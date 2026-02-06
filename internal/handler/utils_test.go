package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConversationKeyForRequestPriority(t *testing.T) {
	baseReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://example.com/orchids/v1/messages", nil)
		r.RemoteAddr = "203.0.113.9:1234"
		r.Header.Set("User-Agent", "test-agent")
		return r
	}

	tests := []struct {
		name       string
		req        ClaudeRequest
		headerKey  string
		headerVal  string
		remoteAddr string
		userAgent  string
		want       string
	}{
		{
			name: "conversation_id highest priority",
			req: ClaudeRequest{
				ConversationID: "cid",
				Metadata: map[string]interface{}{
					"user_id": "u1",
				},
			},
			headerKey: "X-Conversation-Id",
			headerVal: "header",
			want:      "cid",
		},
		{
			name: "metadata conversation_id before header",
			req: ClaudeRequest{
				Metadata: map[string]interface{}{
					"conversation_id": "meta",
				},
			},
			headerKey: "X-Conversation-Id",
			headerVal: "header",
			want:      "meta",
		},
		{
			name: "header before metadata user_id",
			req: ClaudeRequest{
				Metadata: map[string]interface{}{
					"user_id": "u1",
				},
			},
			headerKey: "X-Conversation-Id",
			headerVal: "header",
			want:      "header",
		},
		{
			name: "metadata user_id fallback",
			req: ClaudeRequest{
				Metadata: map[string]interface{}{
					"user_id": "u1",
				},
			},
			want: "user:u1",
		},
		{
			name: "fallback host and user agent",
			req:  ClaudeRequest{},
			want: "203.0.113.9|test-agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := baseReq()
			if tt.headerKey != "" {
				r.Header.Set(tt.headerKey, tt.headerVal)
			}
			if tt.remoteAddr != "" {
				r.RemoteAddr = tt.remoteAddr
			}
			if tt.userAgent != "" {
				r.Header.Set("User-Agent", tt.userAgent)
			}
			if got := conversationKeyForRequest(r, tt.req); got != tt.want {
				t.Fatalf("conversationKeyForRequest() = %q, want %q", got, tt.want)
			}
		})
	}
}
