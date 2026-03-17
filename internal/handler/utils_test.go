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
		{path: "/bolt/v1/messages", want: "bolt"},
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

func TestBuildToolResultNoToolsFallback(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目怎么优化"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\nsrc/\ndocs/\ntest.go"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "核心源码") && !strings.Contains(strings.ToLower(got), "core source") {
		t.Fatalf("expected request to read core source files first, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_TechStackAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目使用了哪些技术架构"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "1→import json\n2→import os\n3→from urllib.request import Request\n4→import socks\n5→def load_media_mapping():\n6→    with open(MEDIA_MAPPING_FILE, \"r\") as f:\n7→        return json.load(f)\n8→ALERTS_FILE = os.path.join(PROJECT_ROOT, \"market_alerts.json\")"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "Python") {
		t.Fatalf("expected Python signal in fallback, got %q", got)
	}
	if !strings.Contains(got, "JSON") {
		t.Fatalf("expected JSON storage signal in fallback, got %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "socks") && !strings.Contains(got, "代理") {
		t.Fatalf("expected networking/proxy signal in fallback, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_WebImplementationAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "网页是如何实现的"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\ndashboard.py\nweb-ui/\nweb-ui/src/\nweb-ui/public/\nweb-ui/dist/\nweb-ui/index.html\nweb-ui/package.json\nweb-ui/vite.config.js\nweb-ui/tailwind.config.js\nweb-ui/postcss.config.js"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "web-ui") {
		t.Fatalf("expected web-ui signal, got %q", got)
	}
	if !strings.Contains(got, "Vite") {
		t.Fatalf("expected Vite signal, got %q", got)
	}
	if !strings.Contains(got, "Tailwind") {
		t.Fatalf("expected Tailwind signal, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_ProjectPurposeAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目是干什么的"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\ndashboard.py\nweb-ui/\nweb-ui/package.json\nweb-ui/src/\nrequirements.txt"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "前端目录") && !strings.Contains(got, "前端") {
		t.Fatalf("expected frontend-app purpose signal, got %q", got)
	}
	if !strings.Contains(got, "后端") && !strings.Contains(got, "脚本层") {
		t.Fatalf("expected backend/script purpose signal, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_BackendImplementationAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "后端是如何实现的"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "api.py\nrequirements.txt\nroutes/\ncontrollers/\nfastapi\nuvicorn\nredis"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "FastAPI") {
		t.Fatalf("expected FastAPI signal, got %q", got)
	}
	if !strings.Contains(got, "API") && !strings.Contains(strings.ToLower(got), "backend") {
		t.Fatalf("expected backend/API role signal, got %q", got)
	}
	if !strings.Contains(got, "Redis") && !strings.Contains(got, "依赖") {
		t.Fatalf("expected storage or dependency signal, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_DataLayerAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "数据层是如何实现的"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "models.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "models/\nprisma/schema.prisma\nmigrations/\nredis\npostgresql\ntypeorm"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "Redis") && !strings.Contains(got, "PostgreSQL") {
		t.Fatalf("expected storage signal, got %q", got)
	}
	if !strings.Contains(got, "schema") && !strings.Contains(got, "migration") && !strings.Contains(got, "结构") {
		t.Fatalf("expected schema or migration signal, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_TestingAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目如何测试"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "tests/\npytest.ini\nplaywright.config.ts\n.github/workflows/test.yml"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "pytest") && !strings.Contains(got, "Playwright") {
		t.Fatalf("expected testing framework signal, got %q", got)
	}
	if !strings.Contains(got, "CI") && !strings.Contains(got, "测试目录") && !strings.Contains(got, "tests") {
		t.Fatalf("expected test layout signal, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_DeploymentAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目怎么部署"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "Dockerfile\ndocker-compose.yml\n.github/workflows/deploy.yml\npackage.json\nvite.config.js\nrequirements.txt"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "Docker") && !strings.Contains(got, "Compose") {
		t.Fatalf("expected docker deployment signal, got %q", got)
	}
	if !strings.Contains(got, "GitHub Actions") && !strings.Contains(got, "工作流") && !strings.Contains(got, "构建发布") {
		t.Fatalf("expected workflow signal, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_OptimizationAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目怎么优化"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "import json\nimport os\nfrom urllib.request import Request\nimport socks\nfastapi\napi.py\nmarket_alerts.json"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "优化") && !strings.Contains(strings.ToLower(got), "optimiz") {
		t.Fatalf("expected optimization guidance, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_OptimizationAnswerFromSpecificFileContent(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "test_caption_cloud.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "# test_caption_cloud.py\nfrom monitor_trump import hf_caption_image\n\nif __name__ == \"__main__\":\n    img = r\"D:\\Github Code HUB\\truth_social_scraper\\media\\images\\769d347258b5.jpg\"\n    print(hf_caption_image(img, timeout=20))"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "少量核心源码") && !strings.Contains(strings.ToLower(got), "small slice of the core source") {
		t.Fatalf("expected request for more implementation context, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_OptimizationAnswerFromDirectoryOverview(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "total 512\ndrwxr-xr-x@ 15 dailin  staff    480 Mar  7 20:26 .\ndrwxr-xr-x@  7 dailin  staff    224 Mar 10 21:44 ..\n-rw-r--r--@  1 dailin  staff   7191 Mar  5 21:41 README.md\n-rw-r--r--@  1 dailin  staff  54313 Mar  5 21:56 api.py\n-rw-r--r--@  1 dailin  staff  75989 Mar  5 21:45 dashboard.py\ndrwxr-xr-x@  9 dailin  staff    288 Mar  5 22:27 web-ui\n-rw-r--r--@  1 dailin  staff   2137 Mar  5 21:41 requirements.txt\n-rw-r--r--@  1 dailin  staff   3024 Mar  5 21:41 dashboard_data.json"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "核心源码") && !strings.Contains(strings.ToLower(got), "core source") {
		t.Fatalf("expected request to inspect core source files first, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_DeepAnalysisSkipsFallback(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我深入分析这个项目，然后给出优化建议"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "total 512\ndrwxr-xr-x@ 15 dailin  staff    480 Mar  7 20:26 .\ndrwxr-xr-x@  7 dailin  staff    224 Mar 10 21:44 ..\n-rw-r--r--@  1 dailin  staff   7191 Mar  5 21:41 README.md\n-rw-r--r--@  1 dailin  staff  54313 Mar  5 21:56 api.py\n-rw-r--r--@  1 dailin  staff  75989 Mar  5 21:45 dashboard.py"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "核心源码") && !strings.Contains(strings.ToLower(got), "core source") {
		t.Fatalf("expected request to inspect core source files first, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_OptimizationRecoversFromForeignFileMiss(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}},
			{Type: "text", Text: "Let me first understand the project structure."},
		}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "total 512\ndrwxr-xr-x@ 15 dailin  staff    480 Mar  7 20:26 .\ndrwxr-xr-x@  7 dailin  staff    224 Mar 10 21:44 ..\n-rw-r--r--@  1 dailin  staff   7191 Mar  5 21:41 README.md\n-rw-r--r--@  1 dailin  staff  54313 Mar  5 21:56 api.py\n-rw-r--r--@  1 dailin  staff  75989 Mar  5 21:45 dashboard.py\ndrwxr-xr-x@  9 dailin  staff    288 Mar  5 22:27 web-ui\n-rw-r--r--@  1 dailin  staff   2137 Mar  5 21:41 requirements.txt\n-rw-r--r--@  1 dailin  staff   3024 Mar  5 21:41 dashboard_data.json"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "text", Text: "Let me first understand the project structure and code."},
			{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "/Users/jianxinhe/projects/trump-monitor/test_caption_cloud.py"}},
		}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "File does not exist. Note: your current working directory is /Users/dailin/Documents/GitHub/truth_social_scraper."}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "核心源码") && !strings.Contains(strings.ToLower(got), "core source") {
		t.Fatalf("expected request to inspect core source files after recovery, got %q", got)
	}
}

func TestShouldKeepToolsForWarpToolResultFollowup_RecoversMalformedReadPaths(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\nrequirements.txt\nutils.py\nmonitor_trump.py\ndashboard.py"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": ":/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"}},
			{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "</Users/dailin/Documents/GitHub/truth_social_scraper/utils.py"}},
		}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "tool_2", Content: "File does not exist. Note: your current working directory is /Users/dailin/Documents/GitHub/truth_social_scraper."},
			{Type: "tool_result", ToolUseID: "tool_3", Content: "File does not exist. Note: your current working directory is /Users/dailin/Documents/GitHub/truth_social_scraper."},
		}}},
	}

	if !shouldKeepToolsForWarpToolResultFollowup(messages) {
		t.Fatalf("expected malformed read path failures to keep tools for recovery")
	}
}

func TestShouldKeepToolsForWarpToolResultFollowup_OptimizationKeepsToolsAfterExploratoryPreface(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "text", Text: "Let me first understand the project structure and codebase."},
			{Type: "tool_use", ID: "tool_ls", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}},
		}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "tool_ls", Content: "README.md\napi.py\nmonitor_trump.py\nutils.py\ndashboard.py\nrequirements.txt\nweb-ui"},
		}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "text", Text: "Let me first thoroughly understand the codebase before proposing optimizations."},
			{Type: "tool_use", ID: "tool_readme", Name: "Read", Input: map[string]interface{}{"file_path": "README.md"}},
			{Type: "tool_use", ID: "tool_api", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}},
			{Type: "tool_use", ID: "tool_monitor", Name: "Read", Input: map[string]interface{}{"file_path": "monitor_trump.py"}},
			{Type: "tool_use", ID: "tool_utils", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}},
			{Type: "tool_use", ID: "tool_dashboard", Name: "Read", Input: map[string]interface{}{"file_path": "dashboard.py"}},
			{Type: "tool_use", ID: "tool_req", Name: "Read", Input: map[string]interface{}{"file_path": "requirements.txt"}},
		}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "tool_readme", Content: "# Truth Social Monitor\nFastAPI\nStreamlit"},
			{Type: "tool_result", ToolUseID: "tool_api", Content: "import json\nimport requests\nfrom fastapi import FastAPI\napp = FastAPI()"},
			{Type: "tool_result", ToolUseID: "tool_monitor", Content: "from openai import OpenAI\nfrom huggingface_hub import InferenceClient\nimport requests"},
			{Type: "tool_result", ToolUseID: "tool_utils", Content: "import json\nimport os\nALERTS_FILE = 'alerts.json'\ndef load_json(path):\n    with open(path) as f:\n        return json.load(f)"},
			{Type: "tool_result", ToolUseID: "tool_dashboard", Content: "import streamlit as st\ndef render_dashboard():\n    return st.title('dashboard')"},
			{Type: "tool_result", ToolUseID: "tool_req", Content: "fastapi\nstreamlit\nrequests\nopenai"},
		}}},
	}

	if !shouldKeepToolsForWarpToolResultFollowup(messages) {
		t.Fatalf("expected exploratory optimization preface to keep tools even after several file reads")
	}
}

func TestBuildToolResultNoToolsFallback_OptimizationUsesAggregatedEvidence(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "README.md\napi.py\nrequirements.txt\nweb-ui/\ndashboard.py"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_2", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_2", Content: "import requests\nfrom fastapi import FastAPI\nimport json\n\ndef load_data(path):\n    with open(path) as f:\n        return json.load(f)"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_3", Name: "Read", Input: map[string]interface{}{"file_path": "requirements.txt"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_3", Content: "fastapi\nrequests"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_4", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_4", Content: "import json\nimport requests\n\ndef fetch(url):\n    return requests.get(url).json()\n\ndef load_data(path):\n    return json.load(open(path))"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "统一超时") && !strings.Contains(got, "数据读写") && !strings.Contains(got, "边界") {
		t.Fatalf("expected aggregated optimization guidance, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_OptimizationUsesProjectSpecificFileSuggestions(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_ls", Name: "Bash", Input: map[string]interface{}{"command": "ls -la"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_ls", Content: "README.md\napi.py\nmonitor_trump.py\nutils.py\ndashboard.py\ntest_caption_cloud.py\nweb-ui\nrequirements.txt"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_api", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_api", Content: "from fastapi import FastAPI\nfrom threading import Thread\nfrom utils import ALERTS_FILE, DASHBOARD_JSON_FILE\n@app.get('/api/alerts')\ndef list_alerts():\n    with open(ALERTS_FILE, 'r') as f:\n        return json.load(f)\nThread(target=_analyze_media_background, daemon=True).start()"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_utils", Name: "Read", Input: map[string]interface{}{"file_path": "utils.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_utils", Content: "ALERTS_FILE = os.path.join(PROJECT_ROOT, 'market_alerts.json')\nMEDIA_MAPPING_FILE = os.path.join(PROJECT_ROOT, 'media_mapping.json')\nDASHBOARD_JSON_FILE = os.path.join(PROJECT_ROOT, 'dashboard_data.json')\nwith open(ALERTS_FILE, 'r', encoding='utf-8') as f:\n    alerts_data = json.load(f)\nwith open(MEDIA_MAPPING_FILE, 'w', encoding='utf-8') as f:\n    json.dump(mapping, f, indent=2, ensure_ascii=False)"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_monitor", Name: "Read", Input: map[string]interface{}{"file_path": "monitor_trump.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_monitor", Content: "from openai import OpenAI\nfrom huggingface_hub import InferenceClient\nresponse_chat = requests.post(api_url_chat, headers=headers_chat, json=payload_chat, timeout=timeout)\nwith urlopen(req, timeout=timeout) as resp:\n    return resp.read()\nexcept Exception as e:\n    print(f'[AI][HF] caption failed: {e}')"}}}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_dashboard", Name: "Read", Input: map[string]interface{}{"file_path": "dashboard.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_dashboard", Content: "import streamlit as st\nst.components.v1.html('<script>sessionStorage.setItem(\\'api_url_detected\\', \\'true\\')</script>')\nif 'api_base_url' not in st.session_state:\n    st.session_state['api_base_url'] = 'http://localhost:8000'"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	for _, marker := range []string{"`api.py`", "`monitor_trump.py`", "`dashboard.py`", "`utils.py`"} {
		if !strings.Contains(got, marker) {
			t.Fatalf("expected project-specific optimization guidance mentioning %s, got %q", marker, got)
		}
	}
	if !strings.Contains(got, "结构化日志") && !strings.Contains(strings.ToLower(got), "structured logging") {
		t.Fatalf("expected concrete client/error-handling suggestion, got %q", got)
	}
}

func TestBuildToolGateMessage_OptimizationFollowupAvoidsShortNonCodeFraming(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "帮我优化这个项目"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "import requests\nfrom fastapi import FastAPI\napp = FastAPI()"}}}},
	}

	got := buildToolGateMessage(messages, false)
	if strings.Contains(strings.ToLower(got), "short, non-code request") {
		t.Fatalf("expected project-specific tool-gate wording, got %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "project optimization") {
		t.Fatalf("expected optimization-specific tool-gate wording, got %q", got)
	}
	for _, want := range []string{
		"Tool access is unavailable for this turn",
		"will be ignored",
		"Do NOT call tools",
		"do not describe a plan",
		"do not say you will first analyze or review the codebase",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected stronger direct-answer tool-gate guidance %q, got %q", want, got)
		}
	}
}

func TestBuildToolResultNoToolsFallback_SecurityRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些安全风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "app.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: ".env\napi_key=\nverify=False\nyaml.load(data)\nDEBUG=True"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "安全风险") && !strings.Contains(strings.ToLower(got), "security") {
		t.Fatalf("expected security-risk phrasing, got %q", got)
	}
	if !strings.Contains(got, "TLS") && !strings.Contains(got, "密钥") {
		t.Fatalf("expected concrete security signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_PermissionRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些权限风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "Dockerfile"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "USER root\nchmod 777 /app\n--privileged"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "权限") && !strings.Contains(strings.ToLower(got), "permission") {
		t.Fatalf("expected permission-risk phrasing, got %q", got)
	}
	if !strings.Contains(got, "root") && !strings.Contains(got, "权限过宽") {
		t.Fatalf("expected concrete permission signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_DependencyRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些依赖风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "requirements.txt"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "requirements.txt\nrequests>=2.0\ngit+https://example.com/pkg.git#egg=test\nlatest\npackage-lock.json"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "依赖") && !strings.Contains(strings.ToLower(got), "dependency") {
		t.Fatalf("expected dependency-risk phrasing, got %q", got)
	}
	if !strings.Contains(got, "未严格锁定") && !strings.Contains(got, "远程源") && !strings.Contains(got, "锁文件") {
		t.Fatalf("expected concrete dependency signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_PerformanceBottleneckAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些性能瓶颈"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "worker.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "for item in items:\n    resp = requests.get(url)\n    data = json.load(open(path))\n    time.sleep(1)"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "性能") && !strings.Contains(strings.ToLower(got), "performance") {
		t.Fatalf("expected performance phrasing, got %q", got)
	}
	if !strings.Contains(got, "同步网络请求") && !strings.Contains(got, "阻塞") && !strings.Contains(got, "JSON 文件读写") {
		t.Fatalf("expected concrete performance signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_CodeSmellAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些代码异味"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "app.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "import requests\nimport json\nTARGET_URL = 'http://example.com'\nprint('debug')\nexcept Exception:\nTODO: clean this up\njson.load(open(path))"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "代码异味") && !strings.Contains(strings.ToLower(got), "code smell") {
		t.Fatalf("expected code-smell phrasing, got %q", got)
	}
	if !strings.Contains(got, "错误处理过宽") && !strings.Contains(got, "硬编码") && !strings.Contains(got, "调试输出") {
		t.Fatalf("expected concrete code-smell signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_MaintainabilityRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些可维护性风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "service.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "global STATE\napi_key = os.getenv('API_KEY')\nrequests.get(url)\njson.load(open(path))\nexcept Exception:\nBASE_URL = 'http://internal.example'"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "可维护性") && !strings.Contains(strings.ToLower(got), "maintainability") {
		t.Fatalf("expected maintainability phrasing, got %q", got)
	}
	if !strings.Contains(got, "分层边界不清") && !strings.Contains(got, "共享可变状态") && !strings.Contains(got, "硬编码") {
		t.Fatalf("expected concrete maintainability signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_ConfigRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些配置风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": ".env"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "DEBUG=true\nAPI_KEY=abc\nBASE_URL=http://internal.example\nverify=False"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "配置") && !strings.Contains(strings.ToLower(got), "config") {
		t.Fatalf("expected config-risk phrasing, got %q", got)
	}
	if !strings.Contains(got, "调试配置") && !strings.Contains(got, "敏感配置") && !strings.Contains(got, "硬编码") {
		t.Fatalf("expected concrete config-risk signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_ObservabilityGapAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些可观测性缺口"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "api.py"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "fastapi\nprint('start')\nrequests.get(url)\nexcept Exception:"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "可观测性") && !strings.Contains(strings.ToLower(got), "observability") {
		t.Fatalf("expected observability phrasing, got %q", got)
	}
	if !strings.Contains(got, "print/console") && !strings.Contains(got, "metrics") && !strings.Contains(got, "trace") {
		t.Fatalf("expected concrete observability signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_ReleaseRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些发布风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": ".github/workflows/release.yml"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "FROM node:latest\non: push\nkubectl apply -f deploy.yaml\ncurl | bash"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "发布") && !strings.Contains(strings.ToLower(got), "release") {
		t.Fatalf("expected release-risk phrasing, got %q", got)
	}
	if !strings.Contains(got, "可变标签") && !strings.Contains(got, "主干提交") && !strings.Contains(got, "远程脚本") {
		t.Fatalf("expected concrete release-risk signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_CompatibilityRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些兼容性风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "setup.sh"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "#!/bin/bash\nbrew install ffmpeg\npython3.8\nC:\\\\app\\\\data\nplugin.dylib"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "兼容性") && !strings.Contains(strings.ToLower(got), "compatibility") {
		t.Fatalf("expected compatibility-risk phrasing, got %q", got)
	}
	if !strings.Contains(got, "平台假设") && !strings.Contains(got, "版本绑定") && !strings.Contains(got, "平台相关二进制") {
		t.Fatalf("expected concrete compatibility signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_OperationalRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些运维风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": "deploy.sh"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "nohup python app.py &\nssh prod\nrsync -av . prod:/srv/app\nkill -9 1234\ncrontab -e"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "运维") && !strings.Contains(strings.ToLower(got), "operational") {
		t.Fatalf("expected operational-risk phrasing, got %q", got)
	}
	if !strings.Contains(got, "人工方式") && !strings.Contains(got, "人工远程命令") && !strings.Contains(got, "计划任务") {
		t.Fatalf("expected concrete operational signals, got %q", got)
	}
}

func TestBuildToolResultNoToolsFallback_RecoveryRollbackRiskAnswer(t *testing.T) {
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "这个项目有哪些恢复与回滚风险"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "tool_1", Name: "Read", Input: map[string]interface{}{"file_path": ".github/workflows/release.yml"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "tool_1", Content: "kubectl apply -f deploy.yaml\nalembic upgrade head\nDROP TABLE users"}}}},
	}

	got := buildToolResultNoToolsFallback(messages)
	if !strings.Contains(got, "恢复与回滚") && !strings.Contains(strings.ToLower(got), "rollback") {
		t.Fatalf("expected recovery/rollback phrasing, got %q", got)
	}
	if !strings.Contains(got, "破坏性变更") && !strings.Contains(got, "回滚") && !strings.Contains(got, "备份") {
		t.Fatalf("expected concrete recovery/rollback signals, got %q", got)
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
