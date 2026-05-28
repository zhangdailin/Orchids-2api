package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
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
			want: "u1",
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

func TestChannelFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{path: "/orchids/v1/messages", want: "orchids"},
		{path: "/warp/v1/messages", want: "warp"},
		{path: "/puter/v1/messages", want: "puter"},
		{path: "/grok/v1/chat/completions", want: "grok"},
		{path: "/v1/messages", want: ""},
	}
	for _, tt := range tests {
		if got := channelFromPath(tt.path); got != tt.want {
			t.Fatalf("channelFromPath(%q)=%q want %q", tt.path, got, tt.want)
		}
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
			req:  ClaudeRequest{System: SystemItems{{Type: "text", Text: "cwd: /system/path"}}},
			want: "/system/path",
			src:  "system",
		},
		{
			name: "message environment fallback",
			req: ClaudeRequest{Messages: []prompt.Message{
				{
					Role: "user",
					Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
						{
							Type: "text",
							Text: "<system-reminder>\n# Environment\n - Primary working directory: /Users/dailin/Documents/GitHub/truth_social_scraper\n# auto memory\ngitStatus: dirty\nCurrent branch: main",
						},
					}},
				},
			}},
			want: "/Users/dailin/Documents/GitHub/truth_social_scraper",
			src:  "messages",
		},
		{
			name: "extract claude environment primary working directory block",
			req: ClaudeRequest{System: SystemItems{{
				Type: "text",
				Text: "# Environment\n - Primary working directory: /stale/project\n# auto memory\ngitStatus:",
			}}},
			want: "/stale/project",
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

func TestIsTopicClassifierRequest(t *testing.T) {
	req := ClaudeRequest{
		System: SystemItems{
			{
				Type: "text",
				Text: "Analyze if this message indicates a new conversation topic. Format your response as a JSON object with two fields: 'isNewTopic' and 'title'.",
			},
		},
	}
	if !isTopicClassifierRequest(req) {
		t.Fatalf("expected topic classifier request to be detected")
	}

	nonClassifier := ClaudeRequest{
		System: SystemItems{{Type: "text", Text: "You are Claude Code"}},
	}
	if isTopicClassifierRequest(nonClassifier) {
		t.Fatalf("expected non-topic-classifier request")
	}
}

func TestIsTitleGenerationRequest(t *testing.T) {
	req := ClaudeRequest{
		System: SystemItems{
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude."},
			{
				Type: "text",
				Text: "Generate a concise, sentence-case title (3-7 words) that captures the main topic or goal of this coding session.\n\nReturn JSON with a single \"title\" field.",
			},
		},
	}
	if !isTitleGenerationRequest(req) {
		t.Fatalf("expected title generation request to be detected")
	}

	nonTitle := ClaudeRequest{
		System: SystemItems{{
			Type: "text",
			Text: "Analyze if this message indicates a new conversation topic. Format your response as a JSON object with two fields: 'isNewTopic' and 'title'.",
		}},
	}
	if isTitleGenerationRequest(nonTitle) {
		t.Fatalf("expected non-title-generation request")
	}
}

func TestClassifyTopicRequest(t *testing.T) {
	tests := []struct {
		name      string
		messages  []prompt.Message
		wantIsNew bool
	}{
		{
			name: "single user message treated as new topic",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			},
			wantIsNew: true,
		},
		{
			name: "same user message treated as same topic",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "好的"}},
				{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
			},
			wantIsNew: false,
		},
		{
			name: "greeting treated as same topic",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我用python写一个计算器"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "好的"}},
				{Role: "user", Content: prompt.MessageContent{Text: "hi"}},
			},
			wantIsNew: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := ClaudeRequest{Messages: tt.messages}
			gotNew, title := classifyTopicRequest(req)
			if gotNew != tt.wantIsNew {
				t.Fatalf("classifyTopicRequest() isNewTopic = %v, want %v", gotNew, tt.wantIsNew)
			}
			if gotNew && strings.TrimSpace(title) == "" {
				t.Fatalf("expected non-empty title for new topic")
			}
			if !gotNew && title != "" {
				t.Fatalf("expected empty title when not a new topic, got %q", title)
			}
		})
	}
}

func TestBuildLocalSuggestion(t *testing.T) {
	tests := []struct {
		name     string
		messages []prompt.Message
		want     string
	}{
		{
			name: "chinese follow up offer returns chinese suggestion",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "继续处理这个问题"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "已经定位完了。如果你要，我下一步可以直接帮你提交修复。"}},
				{Role: "user", Content: prompt.MessageContent{Text: "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}},
			},
			want: "可以",
		},
		{
			name: "non obvious next step stays silent",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "当前运行的目录"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "当前运行目录：`/Users/dailin/Documents/GitHub/TEST`"}},
				{Role: "user", Content: prompt.MessageContent{Text: "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}},
			},
			want: "",
		},
		{
			name: "english follow up offer returns english suggestion",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "check the logs"}},
				{Role: "assistant", Content: prompt.MessageContent{Text: "I found the issue. If you'd like, I can restart the server and verify it."}},
				{Role: "user", Content: prompt.MessageContent{Text: "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"}},
			},
			want: "go ahead",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildLocalSuggestion(tt.messages); got != tt.want {
				t.Fatalf("buildLocalSuggestion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripSystemRemindersForMode_StripsLocalCommandMetadata(t *testing.T) {
	text := "<local-command-caveat>Caveat</local-command-caveat>\n<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>\n<local-command-stdout>Set model to opus</local-command-stdout>\n[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]"
	got := stripSystemRemindersForMode(text)
	if strings.Contains(got, "<local-command-caveat>") || strings.Contains(got, "/model") || strings.Contains(got, "Set model to opus") {
		t.Fatalf("stripSystemRemindersForMode() should strip local command metadata, got %q", got)
	}
	if !strings.Contains(got, "[SUGGESTION MODE: Suggest what the user might naturally type next into Claude Code.]") {
		t.Fatalf("stripSystemRemindersForMode() should keep suggestion marker, got %q", got)
	}
}

func TestShouldKeepToolsForWarpToolResultFollowup(t *testing.T) {
	tests := []struct {
		name     string
		messages []prompt.Message
		want     bool
	}{
		{
			name: "keeps tools for project optimization after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目怎么优化"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "1→README.md\n2→src/\n3→docs/\n4→test.py"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for project optimization after ls long listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "drwxr-xr-x@ 15 dailin  staff    480 Mar  7 20:26 .\ndrwxr-xr-x@  7 dailin  staff    224 Mar 10 21:26 ..\n-rw-r--r--@  1 dailin  staff   7191 Mar  5 21:41 README.md\n-rw-r--r--@  1 dailin  staff  54313 Mar  5 21:56 api.py\n-rw-r--r--@  1 dailin  staff    401 Mar  5 21:41 requirements.txt\ndrwxr-xr-x@ 15 dailin  staff    480 Mar  7 20:26 web-ui"}}}},
			},
			want: true,
		},
		{
			name: "does not keep tools for simple directory answer",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "当前运行的目录"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "pwd"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "/tmp/project"}}}},
			},
			want: false,
		},
		{
			name: "keeps tools after first shallow file read for optimization follow-up",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目怎么优化"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "test.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "1→import time\n2→def main():\n3→    return 1"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for optimization after readme and requirements without source reads",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\nrequirements.txt\nmonitor_trump.py\ndashboard.py"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "# Truth Social Monitor\nFastAPI\nStreamlit"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "requirements.txt"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_3", Content: "fastapi\nstreamlit\nrequests\nhttpx\npandas"}}}},
			},
			want: true,
		},
		{
			name: "stops tools for optimization once directory and core files are covered",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\nrequirements.txt\nweb-ui/"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "# truth_social_scraper\nFastAPI\nweb-ui"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_3", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_4", Name: "Read", Input: map[string]interface{}{"file_path": "dashboard.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_4", Content: "def render_dashboard():\n    return load_json('dashboard_data.json')"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_5", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_5", Content: "def load_json(path):\n    with open(path) as f:\n        return json.load(f)"}}}},
			},
			want: false,
		},
		{
			name: "keeps tools for optimization after readme requirements and one core source file",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\ndashboard.py\nmonitor_trump.py\nrequirements.txt\nutils.py"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "# Truth Social Monitor\nFastAPI\nStreamlit"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "requirements.txt"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_3", Content: "fastapi\nstreamlit\nrequests\nhttpx\npandas"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_4", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_4", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
			},
			want: true,
		},
		{
			name: "stops tools for deep optimization analysis once enough files are covered",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "仔细看，深入分析怎么优化这里"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\nrequirements.txt"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "# truth_social_scraper\nFastAPI\nweb-ui"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_3", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_4", Name: "Read", Input: map[string]interface{}{"file_path": "dashboard.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_4", Content: "import streamlit as st\nimport pandas as pd"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_5", Name: "Read", Input: map[string]interface{}{"file_path": "requirements.txt"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_5", Content: "requests\nfastapi"}}}},
			},
			want: false,
		},
		{
			name: "keeps tools when optimization follow-up only repeats one core file",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\nrequirements.txt\nweb-ui/"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "# truth_social_scraper\nFastAPI\nweb-ui"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_3", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_4", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_4", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
			},
			want: true,
		},
		{
			name: "stops tools when optimization repeats after multiple implementation files are covered",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\ndashboard.py\nrequirements.txt\nutils.py"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "# truth_social_scraper\nFastAPI\nweb-ui"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_3", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_4", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_4", Content: "import json\nimport requests\n\ndef fetch(url):\n    return requests.get(url).json()"}}}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_5", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_5", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
			},
			want: false,
		},
		{
			name: "does not keep tools for project purpose questions that can be answered locally",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目是干什么的"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\ndashboard.py\nweb-ui/\nweb-ui/package.json\nweb-ui/src/\nrequirements.txt"}}}},
			},
			want: false,
		},
		{
			name: "does not keep tools for testing questions that can be answered locally",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目如何测试"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "tests/\npytest.ini\nplaywright.config.ts\n.github/workflows/test.yml"}}}},
			},
			want: false,
		},
		{
			name: "does not keep tools for dependency risk questions that can be answered locally",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些依赖风险"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "requirements.txt"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "requirements.txt\nrequests>=2.0\nflask==2.3.0\npackage-lock.json"}}}},
			},
			want: false,
		},
		{
			name: "keeps tools for performance bottleneck question after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些性能瓶颈"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\nsrc/\napi.py\nrequirements.txt"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for config risk question after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些配置风险"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: ".env\nconfig/\nconfig.yaml\nREADME.md"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for observability gap question after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些可观测性缺口"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "api.py\nDockerfile\n.github/workflows/\nREADME.md"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for release risk question after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些发布风险"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "Dockerfile\n.github/\ndeploy/\nREADME.md"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for compatibility risk question after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些兼容性风险"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "scripts/\nrequirements.txt\nREADME.md\nDockerfile"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for operational risk question after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些运维风险"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "scripts/\ndeploy/\nDockerfile\nREADME.md"}}}},
			},
			want: true,
		},
		{
			name: "keeps tools for recovery risk question after directory listing",
			messages: []prompt.Message{
				{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些恢复与回滚风险"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
				{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "deploy/\nmigrations/\nDockerfile\nREADME.md"}}}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldKeepToolsForWarpToolResultFollowup(tt.messages); got != tt.want {
				t.Fatalf("shouldKeepToolsForWarpToolResultFollowup() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLastUserIsToolResultFollowup_AllowsTextAlongsideToolResult(t *testing.T) {
	messages := []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "tool_1", Content: "import flask"},
			{Type: "text", Text: "这个项目使用了哪些技术架构"},
		}}},
	}

	if !lastUserIsToolResultFollowup(messages) {
		t.Fatalf("expected tool_result+text to be recognized as follow-up")
	}
}

func TestExplicitlyRequestsDeepAnalysis(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"matches chinese keywords", "请帮我深入分析这个项目", true},
		{"matches english keywords", "can you do a deep analysis", true},
		{"does not match normal opt", "帮我优化这个项目", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := explicitlyRequestsDeepAnalysis(tt.input); got != tt.want {
				t.Fatalf("explicitlyRequestsDeepAnalysis(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestLooksLikeToolResultFailure_RecognizesEditValidationError(t *testing.T) {
	if !looksLikeToolResultFailure("File has not been read yet. Read it first before writing to it.") {
		t.Fatalf("expected edit validation failure to be recognized")
	}
	if !looksLikeToolResultFailure("old_string not found in file") {
		t.Fatalf("expected old_string-not-found failure to be recognized")
	}
	if looksLikeToolResultFailure("Done") {
		t.Fatalf("did not expect successful tool result to be treated as failure")
	}
}
