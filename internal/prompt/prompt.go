package prompt

import (
	"fmt"
	"github.com/goccy/go-json"
	"strings"
)

// NOTE:
// This package intentionally contains ONLY shared schema/types used across the codebase
// (Orchids AIClient, Warp, caching, and handlers).
//
// Legacy prompt-building implementations (BuildPromptV2*, formatting, summarization, etc.)
// have been removed in favor of AIClient-only routing.

// ImageSource 表示图片来源
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url,omitempty"`
}

// CacheControl 缓存控制
type CacheControl struct {
	Type string `json:"type"`
}

// ContentBlock 表示消息内容中的一个块
type ContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
	URL    string       `json:"url,omitempty"`

	// tool_use 字段
	ID       string      `json:"id,omitempty"`
	Name     string      `json:"name,omitempty"`
	Input    interface{} `json:"input,omitempty"`
	Thinking string      `json:"thinking,omitempty"`

	// tool_result 字段
	ToolUseID    string        `json:"tool_use_id,omitempty"`
	Content      interface{}   `json:"content,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
	HasInput     bool          `json:"has_input,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// MessageContent 联合类型：string 或 ContentBlock[]
type MessageContent struct {
	Text   string
	Blocks []ContentBlock
}

func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		mc.Text = ""
		mc.Blocks = nil
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		mc.Text = text
		mc.Blocks = nil
		return nil
	}

	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err == nil {
		mc.Text = ""
		mc.Blocks = blocks
		return nil
	}

	return fmt.Errorf("content must be string or array of content blocks")
}

func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if mc.Blocks != nil {
		return json.Marshal(mc.Blocks)
	}
	return json.Marshal(mc.Text)
}

func (mc *MessageContent) IsString() bool            { return mc.Blocks == nil }
func (mc *MessageContent) GetText() string           { return mc.Text }
func (mc *MessageContent) GetBlocks() []ContentBlock { return mc.Blocks }

// ExtractText returns the concatenated text content of the message.
func (mc *MessageContent) ExtractText() string {
	if mc.IsString() {
		return strings.TrimSpace(mc.GetText())
	}
	var parts []string
	for _, block := range mc.GetBlocks() {
		if block.Type == "text" {
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	importStrings := false
	_ = importStrings // to satisfy compilation statically
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// ExtractText is a helper to extract text directly from the prompt.Message.
func (m *Message) ExtractText() string {
	return m.Content.ExtractText()
}

// Message 消息结构
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
}

type openAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIToolCallFunction `json:"function"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string           `json:"role"`
		Content    json.RawMessage  `json:"content"`
		ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
		ToolCallID string           `json:"tool_call_id,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	m.Role = raw.Role
	content := raw.Content
	if len(content) == 0 {
		content = json.RawMessage("null")
	}
	if err := json.Unmarshal(content, &m.Content); err != nil {
		return err
	}

	switch strings.ToLower(strings.TrimSpace(raw.Role)) {
	case "assistant":
		if len(raw.ToolCalls) == 0 {
			return nil
		}
		blocks := make([]ContentBlock, 0, len(raw.ToolCalls)+1)
		if m.Content.IsString() {
			if text := m.Content.GetText(); strings.TrimSpace(text) != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		} else if len(m.Content.GetBlocks()) > 0 {
			blocks = append(blocks, m.Content.GetBlocks()...)
		}
		for _, call := range raw.ToolCalls {
			name := strings.TrimSpace(call.Function.Name)
			if name == "" {
				continue
			}
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    strings.TrimSpace(call.ID),
				Name:  name,
				Input: decodeOpenAIToolArguments(call.Function.Arguments),
			})
		}
		m.Content = MessageContent{Blocks: blocks}
	case "tool":
		m.Role = "user"
		m.Content = MessageContent{Blocks: []ContentBlock{{
			Type:      "tool_result",
			ToolUseID: strings.TrimSpace(raw.ToolCallID),
			Content:   toolResultContentFromMessageContent(m.Content),
		}}}
	}

	return nil
}

func decodeOpenAIToolArguments(raw string) interface{} {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]interface{}{}
	}

	var value interface{}
	if err := json.Unmarshal([]byte(trimmed), &value); err == nil {
		switch typed := value.(type) {
		case nil:
			return map[string]interface{}{}
		case map[string]interface{}:
			return typed
		default:
			return map[string]interface{}{"value": typed}
		}
	}

	return map[string]interface{}{"raw": trimmed}
}

func toolResultContentFromMessageContent(content MessageContent) string {
	if content.IsString() {
		return content.GetText()
	}
	return content.ExtractText()
}

// SystemItem 系统提示词项
type SystemItem struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}
