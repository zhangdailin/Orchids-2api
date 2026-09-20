package grok

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"orchids-api/internal/config"
)

func TestMGWEndpointAndHeadersKeepSSOInCookie(t *testing.T) {
	endpoint, origin, err := mgwEndpoint("https://grok.example/base", "user-id")
	if err != nil {
		t.Fatalf("mgwEndpoint() error = %v", err)
	}
	if endpoint != "wss://grok.example/ws/mgw/?uid=user-id" || origin != "https://grok.example" {
		t.Fatalf("endpoint=%q origin=%q", endpoint, origin)
	}
	headers := mgwHeaders(New(nil), origin, "user-id", "sso-token")
	if headers.Get("Authorization") != "" {
		t.Fatalf("MGW must not turn SSO into Authorization: %q", headers.Get("Authorization"))
	}
	for _, want := range []string{"sso=sso-token", "sso-rw=sso-token", "x-userid=user-id"} {
		if !strings.Contains(headers.Get("Cookie"), want) {
			t.Fatalf("Cookie=%q missing %q", headers.Get("Cookie"), want)
		}
	}
}

func TestMGWCookieReplacesUntrustedUserID(t *testing.T) {
	cookie := mgwCookie("sso=sso-token; x-userid=untrusted", "chosen-user")
	if strings.Count(cookie, "x-userid=") != 1 || !strings.Contains(cookie, "x-userid=chosen-user") {
		t.Fatalf("Cookie=%q", cookie)
	}
}

// signingOffConfig points a client at a mock upstream with statsig signing off:
// these tests assert which requests reach the upstream, and signing adds its own
// page read to that list.
func signingOffConfig(baseURL string) *config.Config {
	disabled := ""
	return &config.Config{GrokAPIBaseURL: baseURL, GrokStatsigSignerURL: &disabled}
}

func TestLegacyAppChatTransportFallsBackToREST(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != defaultChatPath {
			t.Fatalf("path=%q want=%q", request.URL.Path, defaultChatPath)
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatalf("legacy SSO request has Authorization header")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"modelResponse":{"message":"ok"}}`))
	}))
	defer server.Close()

	client := New(signingOffConfig(server.URL))
	response, err := client.doChat(context.Background(), "sso-token", map[string]interface{}{"message": "hello"})
	if err != nil {
		t.Fatalf("doChat() error = %v", err)
	}
	defer response.Body.Close()
	if calls != 1 {
		t.Fatalf("REST calls=%d want 1", calls)
	}
}

func TestMGWOpenStreamsProtocolFrames(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != mgwPath || request.URL.Query().Get("uid") != "user-1" {
			t.Fatalf("MGW request path=%q uid=%q", request.URL.Path, request.URL.Query().Get("uid"))
		}
		if request.Header.Get("Authorization") != "" {
			t.Fatal("MGW request must not carry Authorization")
		}
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer connection.Close()

		var initial map[string]interface{}
		if err := connection.ReadJSON(&initial); err != nil {
			t.Errorf("read initial event: %v", err)
			return
		}
		event, _ := initial["event"].(map[string]interface{})
		if event["type"] != "session.create" {
			t.Errorf("initial event type=%v", event["type"])
			return
		}
		_ = connection.WriteJSON(map[string]interface{}{"session_id": "session-1", "event": map[string]interface{}{"type": "session.created"}})
		_ = connection.WriteJSON(map[string]interface{}{"session_id": "session-1", "event": map[string]interface{}{"type": "conversation.attached", "conversation": map[string]interface{}{"id": "session-1"}}})

		for i := 0; i < 2; i++ {
			var value map[string]interface{}
			if err := connection.ReadJSON(&value); err != nil {
				t.Errorf("read follow-up %d: %v", i, err)
				return
			}
		}
		_ = connection.WriteJSON(map[string]interface{}{"session_id": "session-1", "event": map[string]interface{}{"type": "response.done", "response": map[string]interface{}{"status": "completed"}}})
	}))
	defer server.Close()

	client := New(&config.Config{GrokAPIBaseURL: server.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := (&mgwWebSocketTransport{client: client}).Open(ctx, MGWChatRequest{UserID: "user-1", Model: "grok-test", Prompt: "hello", Token: "sso-token"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	var events []map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode stream line %q: %v", line, err)
		}
		events = append(events, event)
	}
	if len(events) < 3 {
		t.Fatalf("stream events=%d want at least 3: %s", len(events), body)
	}
}
