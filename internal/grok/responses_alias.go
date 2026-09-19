package grok

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/goccy/go-json"
)

const maxBuildAliasResponseBytes = 128 << 20

// rewriteBuildToolAliasResponse restores request-scoped namespace and special
// tool identities before a native Build Responses payload crosses the public
// API boundary.
func rewriteBuildToolAliasResponse(source io.ReadCloser, contentType string, aliases map[string]buildToolAliasIdentity) io.ReadCloser {
	reader, writer := io.Pipe()
	closed := &sourceClosingPipe{PipeReader: reader, source: source}
	go func() {
		defer closed.closeSource()
		var err error
		if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
			err = rewriteBuildToolAliasSSE(writer, source, aliases)
		} else {
			err = rewriteBuildToolAliasJSONBody(writer, source, aliases)
		}
		_ = writer.CloseWithError(err)
	}()
	return closed
}

type sourceClosingPipe struct {
	*io.PipeReader
	source io.Closer
	once   sync.Once
}

func (r *sourceClosingPipe) closeSource() { r.once.Do(func() { _ = r.source.Close() }) }
func (r *sourceClosingPipe) Close() error { err := r.PipeReader.Close(); r.closeSource(); return err }

func rewriteBuildToolAliasJSONBody(dst io.Writer, source io.Reader, aliases map[string]buildToolAliasIdentity) error {
	raw, err := io.ReadAll(io.LimitReader(source, maxBuildAliasResponseBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxBuildAliasResponseBytes {
		return fmt.Errorf("Grok Build response exceeds 128 MiB")
	}
	converted := rewriteBuildToolAliasesJSON(raw, aliases)
	_, err = dst.Write(converted)
	return err
}

func rewriteBuildToolAliasSSE(dst io.Writer, source io.Reader, aliases map[string]buildToolAliasIdentity) error {
	calls := map[string]*strings.Builder{}
	// callNames remembers which tool a call_id belongs to, so an arguments event
	// (which carries no name) can still be normalized against that tool's schema.
	callNames := map[string]string{}
	rememberName := func(item map[string]interface{}, payload map[string]interface{}) {
		name := firstNonEmpty(interfaceString(item["name"]), interfaceString(payload["name"]))
		if name == "" {
			return
		}
		for _, key := range []string{interfaceString(item["id"]), interfaceString(item["call_id"]), interfaceString(payload["item_id"]), interfaceString(payload["call_id"])} {
			if key != "" {
				callNames[key] = name
			}
		}
	}
	// normalizeArguments rewrites float-form integers the model produced for an
	// integer-typed argument; a strict client decoder rejects those.
	normalizeArguments := func(nameKey string, raw interface{}) interface{} {
		text, ok := raw.(string)
		if !ok || text == "" {
			return raw
		}
		name := firstNonEmpty(callNames[nameKey], nameKey)
		normalized, changed := normalizeAliasedFunctionArguments(name, text, aliases)
		if !changed {
			return raw
		}
		return normalized
	}
	return consumeCompatibleSSE(source, func(frame compatibleSSEEvent) error {
		if !frame.HasData() || string(frame.Data()) == "[DONE]" {
			return frame.writeTo(dst)
		}
		var payload map[string]interface{}
		if json.Unmarshal(frame.Data(), &payload) != nil || payload == nil {
			return frame.writeTo(dst)
		}
		kind := firstNonEmpty(interfaceString(payload["type"]), frame.Event)
		item, _ := payload["item"].(map[string]interface{})
		id, callID := interfaceString(item["id"]), interfaceString(item["call_id"])
		switch kind {
		case "response.output_item.added", "response.output_item.done", "response.function_call_arguments.delta", "response.function_call_arguments.done":
			rememberName(item, payload)
		}
		if identity, ok := aliases[interfaceString(item["name"])]; ok && (identity.Kind == "tool_search" || identity.Kind == "custom") {
			call := calls[firstNonEmpty(id, callID)]
			if call == nil {
				call = &strings.Builder{}
			}
			if id != "" {
				calls[id] = call
			}
			if callID != "" {
				calls[callID] = call
			}
		}
		if call := calls[firstNonEmpty(interfaceString(payload["item_id"]), interfaceString(payload["call_id"]))]; call != nil {
			switch kind {
			case "response.function_call_arguments.delta", "response.function_call_arguments.done":
				value := streamString(payload["delta"])
				if kind == "response.function_call_arguments.done" {
					value = streamString(payload["arguments"])
					if value != "" {
						call.Reset()
					}
				}
				if call.Len()+len(value) > upstreamMaxEventBytes {
					return fmt.Errorf("tool search arguments exceed 8 MiB")
				}
				call.WriteString(value)
				return nil // Internal search arguments become one public tool_search_call.
			}
		}
		callKey := firstNonEmpty(interfaceString(payload["item_id"]), interfaceString(payload["call_id"]))
		if callKey != "" && interfaceString(payload["type"]) == "response.function_call_arguments.done" {
			payload["arguments"] = normalizeArguments(callKey, payload["arguments"])
		}
		if kind == "response.output_item.done" {
			if call := calls[firstNonEmpty(id, callID)]; call != nil && call.Len() > 0 && interfaceString(item["arguments"]) == "" {
				item["arguments"] = call.String()
			}
			if len(item) > 0 {
				item["arguments"] = normalizeArguments(firstNonEmpty(id, callID), item["arguments"])
			}
			delete(calls, id)
			delete(calls, callID)
		}
		restoreBuildVisibleTools(payload, aliases)
		rewriteBuildToolAliasValue(payload, aliases)
		converted, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		frame.data = []string{string(converted)}
		return frame.writeTo(dst)
	})
}

func rewriteBuildToolAliasesJSON(raw []byte, aliases map[string]buildToolAliasIdentity) []byte {
	var value interface{}
	if json.Unmarshal(raw, &value) != nil {
		return raw
	}
	restoreBuildVisibleTools(value, aliases)
	rewriteBuildToolAliasValue(value, aliases)
	converted, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return converted
}

func rewriteBuildToolAliasValue(value interface{}, aliases map[string]buildToolAliasIdentity) {
	switch typed := value.(type) {
	case map[string]interface{}:
		for _, child := range typed {
			rewriteBuildToolAliasValue(child, aliases)
		}
		name := strings.TrimSpace(fmt.Sprint(typed["name"]))
		identity, ok := aliases[name]
		if !ok {
			return
		}
		kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(typed["type"])))
		if !strings.Contains(kind, "function_call") {
			return
		}
		switch identity.Kind {
		case "function":
			typed["name"] = identity.Name
			if identity.Namespace != "" {
				typed["namespace"] = identity.Namespace
			}
			if arguments, ok := typed["arguments"].(string); ok {
				if normalized, changed := normalizeFunctionArguments(arguments, aliasParameterSchema(identity)); changed {
					typed["arguments"] = normalized
				}
			}
		case "tool_search":
			typed["type"] = "tool_search_call"
			typed["execution"] = "client"
			if arguments, ok := typed["arguments"].(string); ok {
				var decoded interface{}
				if json.Unmarshal([]byte(arguments), &decoded) == nil {
					typed["arguments"] = decoded
				}
			}
			delete(typed, "name")
		case "custom":
			// A freeform/grammar tool is emulated as a function taking one
			// string; the model's {"input": "..."} wrapper is unwrapped again so
			// the client sees the custom_tool_call it declared.
			typed["type"] = "custom_tool_call"
			typed["name"] = identity.Name
			if input, ok := decodeCustomToolInputValue(typed["arguments"]); ok {
				typed["input"] = input
			}
			delete(typed, "arguments")
		case "apply_patch":
			typed["type"] = "apply_patch_call"
			delete(typed, "name")
			// apply_patch forces a "patch" argument; expose the patch object the
			// client's apply_patch tool actually declared instead of leaving a
			// raw argument string on a call with no operation.
			if operation, ok := decodeApplyPatchOperation(typed["arguments"]); ok {
				typed["operation"] = operation
				delete(typed, "arguments")
			}
		}
	case []interface{}:
		for _, child := range typed {
			rewriteBuildToolAliasValue(child, aliases)
		}
	}
}

func restoreBuildVisibleTools(value interface{}, aliases map[string]buildToolAliasIdentity) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if tools, ok := typed["tools"].([]interface{}); ok {
			typed["tools"] = restoreBuildToolDeclarations(tools, aliases)
		}
		for key, child := range typed {
			if key != "tools" {
				restoreBuildVisibleTools(child, aliases)
			}
		}
	case []interface{}:
		for _, child := range typed {
			restoreBuildVisibleTools(child, aliases)
		}
	}
}

func restoreBuildToolDeclarations(tools []interface{}, aliases map[string]buildToolAliasIdentity) []interface{} {
	out := make([]interface{}, 0, len(tools))
	namespaceIndexes := map[string]int{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]interface{})
		if tool == nil || !strings.EqualFold(parseLooseStringAny(tool["type"]), "function") {
			out = append(out, raw)
			continue
		}
		identity, ok := aliases[parseLooseStringAny(tool["name"])]
		if !ok {
			out = append(out, raw)
			continue
		}
		declaration := cloneStringInterfaceMap(identity.Declaration)
		if declaration == nil {
			declaration = cloneStringInterfaceMap(tool)
		}
		if identity.Kind != "function" || identity.Namespace == "" {
			out = append(out, declaration)
			continue
		}
		if index, exists := namespaceIndexes[identity.Namespace]; exists {
			namespace := out[index].(map[string]interface{})
			namespace["tools"] = append(interfaceSlice(namespace["tools"]), declaration)
			continue
		}
		namespaceIndexes[identity.Namespace] = len(out)
		out = append(out, map[string]interface{}{"type": "namespace", "name": identity.Namespace, "tools": []interface{}{declaration}})
	}
	return out
}

// decodeCustomToolInputValue unwraps the {"input": "..."} object the emulated
// custom-tool function returns. A client that already sends the bare string is
// passed through unchanged.
func decodeCustomToolInputValue(arguments interface{}) (string, bool) {
	switch typed := arguments.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return "", false
		}
		var wrapper map[string]interface{}
		if json.Unmarshal([]byte(trimmed), &wrapper) == nil {
			if input, ok := wrapper["input"].(string); ok {
				return input, true
			}
		}
		return trimmed, true
	case map[string]interface{}:
		if input, ok := typed["input"].(string); ok {
			return input, true
		}
	}
	return "", false
}

// decodeApplyPatchOperation extracts the structured operation from the
// emulated apply_patch function arguments so the restored apply_patch_call
// carries `operation` instead of an opaque argument string.
func decodeApplyPatchOperation(arguments interface{}) (map[string]interface{}, bool) {
	raw, ok := arguments.(string)
	if !ok {
		if direct, isMap := arguments.(map[string]interface{}); isMap {
			if operation, hasOperation := direct["operation"].(map[string]interface{}); hasOperation {
				return operation, true
			}
			return direct, true
		}
		return nil, false
	}
	var wrapper map[string]interface{}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &wrapper) != nil {
		return nil, false
	}
	operation, ok := wrapper["operation"].(map[string]interface{})
	if !ok || len(operation) == 0 {
		return nil, false
	}
	if strings.TrimSpace(parseLooseStringAny(operation["type"])) == "" {
		return nil, false
	}
	return operation, true
}
