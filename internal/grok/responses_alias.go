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
		if identity, ok := aliases[interfaceString(item["name"])]; ok && identity.Kind == "tool_search" {
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
		if kind == "response.output_item.done" {
			if call := calls[firstNonEmpty(id, callID)]; call != nil && call.Len() > 0 && interfaceString(item["arguments"]) == "" {
				item["arguments"] = call.String()
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
		case "apply_patch":
			typed["type"] = "apply_patch_call"
			delete(typed, "name")
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
