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
			name: "no explicit session key returns empty",
			req: ClaudeRequest{
				Metadata: map[string]interface{}{
					"user_id": "u1",
				},
			},
			want: "",
		},
		{
			name: "no fallback to host and user agent",
			req:  ClaudeRequest{},
			want: "",
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

func TestExtractWorkdirFromRequestPriority(t *testing.T) {
	baseReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://example.com/warp/v1/messages", nil)
		return r
	}

	tests := []struct {
		name string
		req  ClaudeRequest
		hdr  map[string]string
		want string
		src  string
	}{
		{
			name: "metadata wins",
			req:  ClaudeRequest{Metadata: map[string]interface{}{"workdir": "/meta/path"}},
			hdr:  map[string]string{"X-Workdir": "/header/path"},
			want: "/meta/path",
			src:  "metadata",
		},
		{
			name: "header fallback",
			req:  ClaudeRequest{},
			hdr:  map[string]string{"X-Workdir": "/header/path"},
			want: "/header/path",
			src:  "header",
		},
		{
			name: "system fallback",
			req:  ClaudeRequest{System: SystemItems{{Type: "text", Text: "Primary working directory: /system/path"}}},
			want: "/system/path",
			src:  "system",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := baseReq()
			for k, v := range tt.hdr {
				r.Header.Set(k, v)
			}
			got, src := extractWorkdirFromRequest(r, tt.req)
			if got != tt.want || src != tt.src {
				t.Fatalf("extractWorkdirFromRequest() = (%q,%q), want (%q,%q)", got, src, tt.want, tt.src)
			}
		})
	}
}
