package qoder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/goccy/go-json"
)

// The model catalog is a control-plane read on the same COSY-signed surface as
// the chat call. It is therefore reachable with the credential this channel
// already holds: the signature is computed over the runtime key, the encoded
// body and the signed path, none of which are chat specific.
//
// modelListRoutes are the routes the catalog has been observed at, in probe
// order. The read is a GET: the gateway answers POST to this path with
// "Request method 'POST' not supported". The query variant is the one the CLI's
// other control-plane calls use, and is kept as a compatibility fallback for a
// gateway build that expects it.
var modelListRoutes = []struct {
	method string
	path   string
	body   string
}{
	{method: http.MethodGet, path: "/algo/api/v2/model/list"},
	{method: http.MethodGet, path: "/algo/api/v2/model/list?FetchKeys=llm_model_result&Encode=1"},
}

// FetchUpstreamModels reads the account-scoped model catalog from the signed
// upstream control plane.
//
// There is no local fallback. A compiled-in catalog is not an observation of
// what the account may run, so a failed read is reported as a failure; the
// caller decides whether that is worth surfacing.
func (c *Client) FetchUpstreamModels(ctx context.Context) (*Catalog, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	creds, err := c.ensureAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	fields, err := c.ensureRuntimeFields(ctx, creds)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, route := range modelListRoutes {
		catalog, fetchErr := c.fetchModelListOnce(ctx, creds, fields, route.method, route.path, route.body)
		if fetchErr == nil {
			return catalog, nil
		}
		lastErr = fetchErr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no model list route is configured")
	}
	return nil, fmt.Errorf("qoder upstream model list unavailable: %w", lastErr)
}

// fetchModelListOnce performs one signed catalog read.
func (c *Client) fetchModelListOnce(ctx context.Context, creds Credentials, fields RuntimeFields, method, path, rawBody string) (*Catalog, error) {
	body := ""
	if strings.TrimSpace(rawBody) != "" {
		// The signature covers the encoded body, so the wire form is what both
		// the signature and the request must carry.
		body = string(EncodeBody([]byte(rawBody)))
	}
	url := strings.TrimRight(c.endpoints.inference, "/") + path

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, url, reader)
	if err != nil {
		return nil, err
	}
	requestID, err := newUUID(c.entropy)
	if err != nil {
		return nil, err
	}
	if err := c.applyAuthHeaders(req, creds, fields, requestID, "", "", body, signPath(url)); err != nil {
		return nil, err
	}
	// The catalog is a JSON document, not an event stream: overriding the chat
	// path's Accept header keeps a strict gateway from wrapping the reply.
	req.Header.Set("Accept", "application/json")

	resp, err := c.control.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(method, url, resp.StatusCode, raw)
	}

	catalog, parseErr := parseModelList(raw)
	if parseErr != nil {
		// The gateway's own reason travels in the error; the raw body is not
		// reported upwards so an unrelated payload cannot reach a log line.
		return nil, fmt.Errorf("%s %s: %w", method, signPath(url), parseErr)
	}
	if catalog.Len() == 0 {
		return nil, fmt.Errorf("%s %s returned an empty catalog", method, signPath(url))
	}
	return catalog, nil
}

// parseModelList decodes the catalog from the shapes the gateway has used.
//
// The observed response groups the rows by capability under a top-level object
// ("chat" first), and the rows are keyed objects rather than bare identifiers.
// The envelope also nests under `data` (sometimes as a JSON string when the
// request asked for encoding). Every shape is accepted so a change in nesting
// does not silently read as "no models".
func parseModelList(raw []byte) (*Catalog, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("empty response")
	}

	if entries, ok := decodeCatalogEntries(trimmed); ok {
		return newCatalog(entries), nil
	}

	// A failure envelope is worth reporting verbatim: it carries the gateway's
	// own reason, which the caller surfaces instead of a generic parse error.
	var failure struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
		MsgInfo string `json:"msgInfo"`
	}
	if err := json.Unmarshal(trimmed, &failure); err == nil {
		if detail := firstNonEmpty(failure.Message, failure.MsgInfo, failure.Msg); detail != "" {
			return nil, fmt.Errorf("upstream reported: %s", detail)
		}
	}
	return nil, fmt.Errorf("catalog response carried no model list")
}

// catalogGroupKeys are the capability groups the catalog is nested under, in
// preference order. "chat" is the group this channel serves; the rest are
// accepted so a gateway that reorganises the groups is still readable.
var catalogGroupKeys = []string{"chat", "models", "list", "data", "completion", "completions", "embedding"}

// decodeCatalogEntries accepts an array of model rows, or an object wrapping
// them under one of catalogGroupKeys — or, as a last resort, under any field
// whose value decodes into model rows.
//
// A payload that is itself an encoded JSON string is decoded once more, because
// the gateway's Encode=1 mode nests that way. Only rows carrying a key count:
// that is what separates a real catalog from an unrelated array of objects, so
// an unrecognised shape reports "no catalog" instead of a list of blanks.
func decodeCatalogEntries(raw json.RawMessage) ([]modelEntry, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}

	// An encoded payload arrives as a JSON string holding JSON.
	if trimmed[0] == '"' {
		var inner string
		if err := json.Unmarshal(trimmed, &inner); err != nil {
			return nil, false
		}
		return decodeCatalogEntries([]byte(inner))
	}

	if trimmed[0] == '[' {
		var entries []modelEntry
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, false
		}
		return usableCatalogEntries(entries)
	}

	if trimmed[0] != '{' {
		return nil, false
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, false
	}
	for _, key := range catalogGroupKeys {
		payload, ok := fields[key]
		if !ok {
			continue
		}
		if entries, ok := decodeCatalogEntries(payload); ok {
			return entries, true
		}
	}
	// Field order from a map is not stable, so an unlisted group is found by
	// trying each one. A row without a key cannot satisfy usableCatalogEntries,
	// which keeps this from treating an unrelated object array as a catalog.
	for key, payload := range fields {
		if strings.Contains(key, "message") || strings.Contains(key, "msg") {
			continue
		}
		if entries, ok := decodeCatalogEntries(payload); ok {
			return entries, true
		}
	}
	return nil, false
}

// usableCatalogEntries reports the decodable rows that carry a key, and whether
// any did.
func usableCatalogEntries(entries []modelEntry) ([]modelEntry, bool) {
	usable := make([]modelEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.Key) == "" {
			continue
		}
		usable = append(usable, entry)
	}
	return usable, len(usable) > 0
}
