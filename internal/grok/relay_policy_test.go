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
		{"invalid_reasoning", "/responses", `{"error":{"code":"invalid_encrypted_content","message":"preserve this rejection"}}`, 400},
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

func TestRelayChatSamplingAndEffortAreClientOwned(t *testing.T) {
	for _, effort := range []string{"max", "xhigh", "future-effort"} {
		temperature, topP := 3.0, 1.5
		req := &ChatCompletionsRequest{Model: "grok-4.5", Messages: []ChatMessage{{Role: "user", Content: "original"}}, ReasoningEffort: &effort, Temperature: &temperature, TopP: &topP}
		if err := req.Validate(); err != nil {
			t.Fatal(err)
		}
		payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{ID: req.Model, UpstreamModel: req.Model, Upstream: UpstreamCLI}, req, true)
		if err != nil {
			t.Fatal(err)
		}
		if payload["reasoning"].(map[string]interface{})["effort"] != effort || payload["temperature"] != temperature || payload["top_p"] != topP {
			t.Fatal(payload)
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
