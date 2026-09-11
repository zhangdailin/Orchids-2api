package grok

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func parityBuildHandler(t *testing.T, server *httptest.Server) (*Handler, *store.Account) {
	t.Helper()
	mini := miniredis.RunT(t)
	database, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "parity:", CredentialEncryptionKey: bytes.Repeat([]byte{42}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cfg := &config.Config{GrokCLIBaseURL: server.URL + "/v1"}
	h := NewHandler(cfg, loadbalancer.NewWithCacheTTL(database, time.Second))
	h.cliClient.httpClient = server.Client()
	h.cliClient.oauth.httpClient = server.Client()
	acc := &store.Account{ID: 1, Enabled: true, AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", OAuthAccessToken: jwtWithClaims(t, `{"sub":"parity-user","team_id":"parity-team"}`), OAuthExpiresAt: time.Now().Add(time.Hour)}
	return h, acc
}

func TestRelayNativeContextAndUpstreamDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"answer_without_reasoning", "/responses", `{"id":"resp_plain","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"A complete answer does not have to contain any reasoning."}]}]}`, 200},
		// Build reuses the compaction wording for opaque reasoning and session
		// failures, so only this wording combined with a real compaction item is
		// a genuine compaction rejection and passed through untouched.
		{"compaction_blob_rejected", "/responses", `{"error":{"message":"could not decode the compaction blob"}}`, 400},
		{"native_compact", "/responses/compact", `{"object":"response.compaction","output":[{"type":"compaction","encrypted_content":"native-opaque-result"}]}`, 200},
		{"unsupported_compact", "/responses/compact", `{"error":{"code":"not_found"}}`, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received map[string]interface{}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1"+tc.path {
					t.Errorf("unexpected fallback path %s", r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			h, acc := parityBuildHandler(t, server)
			acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-4.6"}, time.Now()
			if err := h.lb.Store.CreateAccount(context.Background(), acc); err != nil {
				t.Fatal(err)
			}
			if err := h.lb.Store.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6", Status: store.ModelStatusAvailable, Verified: true}); err != nil {
				t.Fatal(err)
			}
			payload := map[string]interface{}{
				"model": "grok-4.6", "stream": false, "prompt_cache_key": "client-session",
				"reasoning":          map[string]interface{}{"effort": "future-effort"},
				"context_management": []interface{}{map[string]interface{}{"type": "compaction", "compact_threshold": float64(50000)}},
				"input": []interface{}{
					map[string]interface{}{"type": "reasoning", "encrypted_content": "client-reasoning"},
					map[string]interface{}{"type": "compaction", "encrypted_content": "client-native-compaction"},
					map[string]interface{}{"role": "user", "content": strings.Repeat("完整上下文，不要摘要。", 2000)},
				},
			}
			body, _ := json.Marshal(payload)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1"+tc.path, bytes.NewReader(body))
			if tc.path == "/responses/compact" {
				h.HandleResponsesCompact(rec, req)
			} else {
				h.HandleResponses(rec, req)
			}
			if rec.Code != tc.status || calls != 1 {
				t.Fatalf("status=%d want=%d calls=%d body=%s", rec.Code, tc.status, calls, rec.Body.String())
			}
			// Session keys are tenant-scoped routing metadata; content is not.
			if interfaceString(received["prompt_cache_key"]) == "" {
				t.Fatal("session key lost")
			}
			received["prompt_cache_key"] = payload["prompt_cache_key"]
			if !reflect.DeepEqual(received, payload) {
				for key, want := range payload {
					if !reflect.DeepEqual(received[key], want) {
						t.Errorf("request field %q changed", key)
					}
				}
				for key := range received {
					if _, ok := payload[key]; !ok {
						t.Errorf("unexpected request field %q", key)
					}
				}
			}
			if tc.status == 200 && !strings.Contains(rec.Body.String(), tc.body) {
				t.Fatalf("response changed: %s", rec.Body.String())
			}
			stored, err := h.lb.Store.GetAccount(context.Background(), acc.ID)
			if err != nil || !stored.Enabled || stored.StatusCode != "" {
				t.Fatalf("relay penalized valid account: status=%q error=%v", stored.StatusCode, err)
			}
		})
	}
}

// An opaque-reasoning rejection stays recoverable even when the request also
// carries a client compaction item: recovery rewrites only the reasoning item's
// cipher and never the client-held compaction state.
func TestRelayNativeResponsesRecoversOpaqueReasoning(t *testing.T) {
	var bodies []map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&received)
		bodies = append(bodies, received)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"invalid_encrypted_content","message":"could not decrypt the provided encrypted_content"}}`)
	}))
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-4.6"}, time.Now()
	if err := h.lb.Store.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	if err := h.lb.Store.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: "grok-4.6", Name: "Grok 4.6", Status: store.ModelStatusAvailable, Verified: true}); err != nil {
		t.Fatal(err)
	}
	payload := map[string]interface{}{
		"model": "grok-4.6", "stream": false, "prompt_cache_key": "client-session",
		"input": []interface{}{
			map[string]interface{}{"type": "reasoning", "encrypted_content": "client-reasoning"},
			map[string]interface{}{"type": "compaction", "encrypted_content": "client-native-compaction"},
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	h.HandleResponses(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))

	if len(bodies) < 2 {
		t.Fatalf("expected a recovery retry, calls=%d", len(bodies))
	}
	for index, received := range bodies[1:] {
		items, _ := received["input"].([]interface{})
		reasoningSeen := false
		for _, raw := range items {
			item, _ := raw.(map[string]interface{})
			switch interfaceString(item["type"]) {
			case "reasoning":
				reasoningSeen = true
				if interfaceString(item["encrypted_content"]) != "" {
					t.Fatalf("retry %d still carried opaque reasoning: %v", index, item)
				}
			case "compaction":
				if interfaceString(item["encrypted_content"]) != "client-native-compaction" {
					t.Fatalf("retry %d rewrote client compaction state: %v", index, item)
				}
			}
		}
		if reasoningSeen {
			t.Fatalf("retry %d kept an empty reasoning item instead of dropping it: %v", index, items)
		}
	}
}

func TestRelayImageRetriesPreservePrompt(t *testing.T) {
	calls := 0
	h := &Handler{client: &Client{cfg: &config.Config{}, httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		// Drawing is the website's image mode trigger, not a semantic rewrite.
		if payload["message"] != "Drawing: 美女图片" {
			t.Fatalf("prompt was rewritten: %v", payload["message"])
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}\n"))}, nil
	})}}}
	spec, _ := ResolveModel("grok-imagine-image-lite")
	_, err := h.collectAppChatImageURLs(context.Background(), &chatAccountSession{token: "test-token"}, spec, ImagesGenerationsRequest{Model: spec.ID, Prompt: "美女图片", N: 1}, false)
	if err == nil || calls != 4 {
		t.Fatalf("expected unchanged prompt through bounded attempts: calls=%d err=%v", calls, err)
	}
}

// Sampling stays client-owned. Effort is normalized to the levels the selected
// model actually accepts: grok-4.5 has no xhigh wire contract, so both xhigh and
// the client-only max alias land on high, while unknown values pass through.
func TestRelayChatSamplingAndEffortAreClientOwned(t *testing.T) {
	for _, tc := range []struct{ effort, want string }{
		{"max", "high"},
		{"xhigh", "high"},
		{"minimal", "low"},
		{"low", "low"},
		{"future-effort", "future-effort"},
	} {
		effort := tc.effort
		temperature, topP := 3.0, 1.5
		req := &ChatCompletionsRequest{Model: "grok-4.5", Messages: []ChatMessage{{Role: "user", Content: "original"}}, ReasoningEffort: &effort, Temperature: &temperature, TopP: &topP}
		if err := req.Validate(); err != nil {
			t.Fatal(err)
		}
		payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{ID: req.Model, UpstreamModel: req.Model, Upstream: UpstreamCLI}, req, true)
		if err != nil {
			t.Fatal(err)
		}
		if payload["reasoning"].(map[string]interface{})["effort"] != tc.want || payload["temperature"] != temperature || payload["top_p"] != topP {
			t.Fatalf("effort=%q payload=%v", tc.effort, payload)
		}
	}
}

// A model that does advertise xhigh keeps it, and the client-only max alias
// maps onto it. Composer never receives an effort but keeps its summary.
func TestRelayBuildEffortAliasesFollowModelContract(t *testing.T) {
	for _, tc := range []struct{ model, effort, want string }{
		{"grok-4.6", "max", "xhigh"},
		{"grok-4.6", "xhigh", "xhigh"},
		{"grok-4.5", "minimal", "low"},
		{"grok-composer-2.5-fast", "high", ""},
	} {
		effort := tc.effort
		req := &ChatCompletionsRequest{Model: tc.model, Messages: []ChatMessage{{Role: "user", Content: "hi"}}, ReasoningEffort: &effort}
		payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{ID: tc.model, UpstreamModel: tc.model, Upstream: UpstreamCLI}, req, true)
		if err != nil {
			t.Fatal(err)
		}
		reasoning, _ := payload["reasoning"].(map[string]interface{})
		if tc.want == "" {
			if _, exists := reasoning["effort"]; exists {
				t.Fatalf("%s kept effort %v", tc.model, reasoning)
			}
			if reasoning["summary"] != "concise" {
				t.Fatalf("%s dropped its summary: %v", tc.model, reasoning)
			}
			continue
		}
		if reasoning["effort"] != tc.want {
			t.Fatalf("%s effort=%v want %s", tc.model, reasoning["effort"], tc.want)
		}
	}
}

func TestRelayRepeatedResponsesDeltasArePreserved(t *testing.T) {
	const count = 300 // Exceeds both former text and reasoning repetition limits.
	text, thought := "repeat this answer ", "repeat this thought "
	stream := strings.Repeat(parityText(text), count) + strings.Repeat(parityFrame("response.reasoning_text.delta", map[string]interface{}{"delta": thought}), count) + parityTerminal("response.completed")
	rec := httptest.NewRecorder()
	_, _, result := copyNativeCLIResponseAndCaptureModel(rec, strings.NewReader(stream), "text/event-stream", "grok-4.6")
	if result.Err != nil || strings.Count(rec.Body.String(), text) != count || strings.Count(rec.Body.String(), thought) != count {
		t.Fatalf("native stream repetition suppressed: text=%d thought=%d err=%v", strings.Count(rec.Body.String(), text), strings.Count(rec.Body.String(), thought), result.Err)
	}
	converted, result := parityRun(t, stream)
	if result.Err != nil || strings.Count(converted, text) != count || strings.Count(converted, thought) != count {
		t.Fatalf("converted stream repetition suppressed: text=%d thought=%d err=%v", strings.Count(converted, text), strings.Count(converted, thought), result.Err)
	}
}

func TestRelayCollectedWebTextIsNotCollapsed(t *testing.T) {
	text := strings.Repeat("This intentional repeated sentence must remain. ", 4)
	frame, _ := json.Marshal(map[string]interface{}{"result": map[string]interface{}{"response": map[string]interface{}{"modelResponse": map[string]interface{}{"message": text}}}})
	w := httptest.NewRecorder()
	(&Handler{}).collectChat(w, &ChatCompletionsRequest{Messages: []ChatMessage{{Role: "user", Content: "repeat"}}}, "grok-4.20-0309", ModelSpec{ID: "grok-4.20-0309"}, "", "", false, nil, nil, strings.NewReader(string(frame)), nil)
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Choices) != 1 || strings.TrimSpace(response.Choices[0].Message.Content) != strings.TrimSpace(text) {
		t.Fatalf("repeated collected text changed: %s", w.Body.String())
	}
}
