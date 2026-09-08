package grok

import (
	"fmt"
	"github.com/goccy/go-json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ChatCompletionsRequest struct {
	Model               string                   `json:"model"`
	Messages            []ChatMessage            `json:"messages"`
	Stream              bool                     `json:"stream"`
	StreamProvided      bool                     `json:"-"`
	Thinking            *string                  `json:"thinking,omitempty"`
	ReasoningEffort     *string                  `json:"reasoning_effort,omitempty"`
	Temperature         *float64                 `json:"temperature,omitempty"`
	TopP                *float64                 `json:"top_p,omitempty"`
	MaxTokens           *int                     `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                     `json:"max_completion_tokens,omitempty"`
	ResponseFormat      map[string]interface{}   `json:"response_format,omitempty"`
	SafetyIdentifier    string                   `json:"safety_identifier,omitempty"`
	ResponseText        map[string]interface{}   `json:"text,omitempty"`
	ResponsesTools      []map[string]interface{} `json:"x_responses_tools,omitempty"`
	ResponsesInput      []interface{}            `json:"x_responses_input,omitempty"`
	Include             []string                 `json:"include,omitempty"`
	VideoConfig         *VideoConfig             `json:"video_config,omitempty"`
	ImageConfig         *ImageConfig             `json:"image_config,omitempty"`
	Tools               []ToolDef                `json:"tools,omitempty"`
	ToolChoice          interface{}              `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                    `json:"parallel_tool_calls,omitempty"`
	Stop                []string                 `json:"stop,omitempty"`
	PromptCacheKey      string                   `json:"prompt_cache_key,omitempty"`
	MCPServers          []map[string]interface{} `json:"mcp_servers,omitempty"`
	OutputConfig        map[string]interface{}   `json:"output_config,omitempty"`
	ThinkingConfig      map[string]interface{}   `json:"thinking_config,omitempty"`
	ReasoningReplay     bool                     `json:"-"`
	startedAt           time.Time
}

type ChatMessage struct {
	Role                      string      `json:"role"`
	Content                   interface{} `json:"content"`
	ToolCalls                 []ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID                string      `json:"tool_call_id,omitempty"`
	Name                      string      `json:"name,omitempty"`
	ReasoningContent          string      `json:"reasoning_content,omitempty"`
	ReasoningEncryptedContent string      `json:"reasoning_encrypted_content,omitempty"`
}

type ToolDef struct {
	Type     string                 `json:"type"`
	Function map[string]interface{} `json:"function,omitempty"`
}

type ToolCall struct {
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function map[string]interface{} `json:"function,omitempty"`
}

type VideoConfig struct {
	AspectRatio    string `json:"aspect_ratio"`
	VideoLength    int    `json:"video_length"`
	ResolutionName string `json:"resolution_name"`
	Preset         string `json:"preset"`
	Size           string `json:"size,omitempty"`
}

type VideosRequest struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	Seconds         int    `json:"seconds"`
	Size            string `json:"size"`
	ResolutionName  string `json:"resolution_name"`
	Preset          string `json:"preset"`
	InputReferences []string
}

type videoJob struct {
	ID                string
	Model             string
	Prompt            string
	Seconds           int
	Size              string
	Quality           string
	CreatedAt         int64
	Status            string
	Progress          int
	CompletedAt       int64
	Error             map[string]interface{}
	VideoURL          string
	ContentPath       string
	RemixedFromID     string
	InputReferences   []string
	Operation         string
	StandardAPI       bool
	OwnerHash         string
	AccountID         int64
	Provider          string
	UpstreamRequestID string
	PublicBaseURL     string
	BuildFallback     bool
}

type ImageConfig struct {
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

type ImagesGenerationsRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	N              int             `json:"n"`
	PartialImages  *int            `json:"partial_images,omitempty"`
	Size           string          `json:"size"`
	AspectRatio    string          `json:"aspect_ratio"`
	Resolution     string          `json:"resolution"`
	Quality        string          `json:"quality"`
	Stream         bool            `json:"stream"`
	NSFW           *bool           `json:"nsfw,omitempty"`
	ResponseFormat string          `json:"response_format"`
	StorageOptions json.RawMessage `json:"storage_options,omitempty"`
}

func parseLooseBoolAnyForField(value interface{}, field string) (bool, error) {
	if strings.TrimSpace(field) == "" {
		field = "value"
	}
	errText := field + " must be a boolean"
	switch v := value.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return false, nil
		}
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "y", "on":
			return true, nil
		case "0", "false", "no", "n", "off":
			return false, nil
		default:
			return false, fmt.Errorf("%s", errText)
		}
	case float64:
		if v == 1 {
			return true, nil
		}
		if v == 0 {
			return false, nil
		}
		return false, fmt.Errorf("%s", errText)
	default:
		return false, fmt.Errorf("%s", errText)
	}
}

func parseLooseBoolAny(value interface{}) (bool, error) {
	return parseLooseBoolAnyForField(value, "stream")
}

func parseLooseIntAny(value interface{}) (int, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, err
		}
		return n, nil
	default:
		return 0, fmt.Errorf("invalid integer value")
	}
}

func parseLooseFloatAny(value interface{}) (*float64, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case float64:
		out := v
		return &out, nil
	case int:
		out := float64(v)
		return &out, nil
	case int32:
		out := float64(v)
		return &out, nil
	case int64:
		out := float64(v)
		return &out, nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return nil, nil
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		return &n, nil
	default:
		return nil, fmt.Errorf("invalid float value")
	}
}

func parseLooseStringAny(value interface{}) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func parseVideoInputReferences(value interface{}) []string {
	var out []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case nil:
			return
		case string:
			if s := strings.TrimSpace(x); s != "" {
				out = append(out, s)
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		case map[string]interface{}:
			for _, key := range []string{"image_url", "url", "data"} {
				if s := parseLooseStringAny(x[key]); s != "" {
					out = append(out, s)
					return
				}
			}
		}
	}
	walk(value)
	return uniqueStrings(out)
}

func (v *VideoConfig) UnmarshalJSON(data []byte) error {
	type rawVideoConfig struct {
		AspectRatio    interface{} `json:"aspect_ratio"`
		VideoLength    interface{} `json:"video_length"`
		Seconds        interface{} `json:"seconds"`
		ResolutionName interface{} `json:"resolution_name"`
		Preset         interface{} `json:"preset"`
		Size           interface{} `json:"size"`
	}
	var raw rawVideoConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	videoLength, err := parseLooseIntAny(raw.VideoLength)
	if err != nil {
		return err
	}
	if videoLength == 0 {
		videoLength, err = parseLooseIntAny(raw.Seconds)
		if err != nil {
			return err
		}
	}
	v.AspectRatio = parseLooseStringAny(raw.AspectRatio)
	v.VideoLength = videoLength
	v.ResolutionName = parseLooseStringAny(raw.ResolutionName)
	v.Preset = parseLooseStringAny(raw.Preset)
	v.Size = parseLooseStringAny(raw.Size)
	return nil
}

func (c *ImageConfig) UnmarshalJSON(data []byte) error {
	type rawImageConfig struct {
		N              interface{} `json:"n"`
		Size           interface{} `json:"size"`
		ResponseFormat interface{} `json:"response_format"`
	}
	var raw rawImageConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n, err := parseLooseIntAny(raw.N)
	if err != nil {
		return err
	}
	c.N = n
	c.Size = parseLooseStringAny(raw.Size)
	c.ResponseFormat = parseLooseStringAny(raw.ResponseFormat)
	return nil
}

func (r *ChatCompletionsRequest) UnmarshalJSON(data []byte) error {
	type rawChatRequest struct {
		Model               string                   `json:"model"`
		Messages            []ChatMessage            `json:"messages"`
		Stream              interface{}              `json:"stream"`
		Thinking            *string                  `json:"thinking,omitempty"`
		ReasoningEffort     *string                  `json:"reasoning_effort,omitempty"`
		Temperature         interface{}              `json:"temperature,omitempty"`
		TopP                interface{}              `json:"top_p,omitempty"`
		MaxTokens           interface{}              `json:"max_tokens,omitempty"`
		MaxCompletionTokens interface{}              `json:"max_completion_tokens,omitempty"`
		ResponseFormat      map[string]interface{}   `json:"response_format,omitempty"`
		User                interface{}              `json:"user,omitempty"`
		SafetyIdentifier    interface{}              `json:"safety_identifier,omitempty"`
		ResponseText        map[string]interface{}   `json:"text,omitempty"`
		ResponsesTools      []map[string]interface{} `json:"x_responses_tools,omitempty"`
		Include             []string                 `json:"include,omitempty"`
		VideoConfig         *VideoConfig             `json:"video_config,omitempty"`
		ImageConfig         *ImageConfig             `json:"image_config,omitempty"`
		Tools               []ToolDef                `json:"tools,omitempty"`
		ToolChoice          interface{}              `json:"tool_choice,omitempty"`
		ParallelToolCalls   interface{}              `json:"parallel_tool_calls,omitempty"`
		Stop                interface{}              `json:"stop,omitempty"`
		PromptCacheKey      string                   `json:"prompt_cache_key,omitempty"`
		MCPServers          []map[string]interface{} `json:"mcp_servers,omitempty"`
		OutputConfig        map[string]interface{}   `json:"output_config,omitempty"`
		ThinkingConfig      map[string]interface{}   `json:"thinking_config,omitempty"`
	}

	var raw rawChatRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var rawMap map[string]json.RawMessage
	_ = json.Unmarshal(data, &rawMap)
	streamRaw, streamProvided := rawMap["stream"]
	if streamProvided {
		s := strings.TrimSpace(string(streamRaw))
		if s == "" || strings.EqualFold(s, "null") {
			streamProvided = false
		}
	}
	stream, err := parseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	temp, err := parseLooseFloatAny(raw.Temperature)
	if err != nil {
		return err
	}
	topP, err := parseLooseFloatAny(raw.TopP)
	if err != nil {
		return err
	}
	maxTokens, err := parseLooseIntAny(raw.MaxTokens)
	if err != nil {
		return err
	}
	maxCompletionTokens, err := parseLooseIntAny(raw.MaxCompletionTokens)
	if err != nil {
		return err
	}
	parallelToolCalls, err := parseLooseBoolAnyForField(raw.ParallelToolCalls, "parallel_tool_calls")
	if err != nil {
		return err
	}

	r.Model = raw.Model
	r.Messages = raw.Messages
	r.Stream = stream
	r.StreamProvided = streamProvided
	r.Thinking = raw.Thinking
	r.ReasoningEffort = raw.ReasoningEffort
	r.Temperature = temp
	r.TopP = topP
	if _, ok := rawMap["max_tokens"]; ok {
		r.MaxTokens = &maxTokens
	}
	if _, ok := rawMap["max_completion_tokens"]; ok {
		r.MaxCompletionTokens = &maxCompletionTokens
		if r.MaxTokens == nil {
			r.MaxTokens = &maxCompletionTokens
		}
	}
	r.ResponseFormat = raw.ResponseFormat
	r.SafetyIdentifier = firstNonEmpty(parseLooseStringAny(raw.SafetyIdentifier), parseLooseStringAny(raw.User))
	r.ResponseText = raw.ResponseText
	r.ResponsesTools = append([]map[string]interface{}(nil), raw.ResponsesTools...)
	r.Include = append([]string(nil), raw.Include...)
	r.VideoConfig = raw.VideoConfig
	r.ImageConfig = raw.ImageConfig
	r.Tools = raw.Tools
	r.ToolChoice = raw.ToolChoice
	if _, ok := rawMap["parallel_tool_calls"]; ok {
		r.ParallelToolCalls = &parallelToolCalls
	}
	r.Stop, err = parseStringList(raw.Stop, "stop")
	if err != nil {
		return err
	}
	r.PromptCacheKey = strings.TrimSpace(raw.PromptCacheKey)
	r.MCPServers = raw.MCPServers
	r.OutputConfig = raw.OutputConfig
	r.ThinkingConfig = raw.ThinkingConfig
	return nil
}

func parseStringList(value interface{}, field string) ([]string, error) {
	switch item := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{item}, nil
	case []interface{}:
		out := make([]string, 0, len(item))
		for _, raw := range item {
			text, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string or array of strings", field)
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or array of strings", field)
	}
}

func (r *ImagesGenerationsRequest) UnmarshalJSON(data []byte) error {
	type rawImagesGenerationsRequest struct {
		Model          interface{}     `json:"model"`
		Prompt         interface{}     `json:"prompt"`
		N              interface{}     `json:"n"`
		PartialImages  interface{}     `json:"partial_images"`
		Size           interface{}     `json:"size"`
		AspectRatio    interface{}     `json:"aspect_ratio"`
		Resolution     interface{}     `json:"resolution"`
		Quality        interface{}     `json:"quality"`
		Stream         interface{}     `json:"stream"`
		NSFW           interface{}     `json:"nsfw"`
		ResponseFormat interface{}     `json:"response_format"`
		StorageOptions json.RawMessage `json:"storage_options"`
	}
	var raw rawImagesGenerationsRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n, err := parseLooseIntAny(raw.N)
	if err != nil {
		return err
	}
	var partialImages *int
	if raw.PartialImages != nil {
		value, err := parseLooseIntAny(raw.PartialImages)
		if err != nil {
			return err
		}
		partialImages = &value
	}
	stream, err := parseLooseBoolAny(raw.Stream)
	if err != nil {
		return err
	}
	var nsfw *bool
	if raw.NSFW != nil {
		nsfwVal, err := parseLooseBoolAnyForField(raw.NSFW, "nsfw")
		if err != nil {
			return err
		}
		nsfw = &nsfwVal
	}
	r.Model = parseLooseStringAny(raw.Model)
	r.Prompt = parseLooseStringAny(raw.Prompt)
	r.N = n
	r.PartialImages = partialImages
	r.Size = parseLooseStringAny(raw.Size)
	r.AspectRatio = parseLooseStringAny(raw.AspectRatio)
	r.Resolution = parseLooseStringAny(raw.Resolution)
	r.Quality = parseLooseStringAny(raw.Quality)
	r.Stream = stream
	r.NSFW = nsfw
	r.ResponseFormat = parseLooseStringAny(raw.ResponseFormat)
	r.StorageOptions = append(r.StorageOptions[:0], raw.StorageOptions...)
	return nil
}

func (r *VideosRequest) UnmarshalJSON(data []byte) error {
	type rawVideosRequest struct {
		Model           interface{} `json:"model"`
		Prompt          interface{} `json:"prompt"`
		Seconds         interface{} `json:"seconds"`
		VideoLength     interface{} `json:"video_length"`
		Size            interface{} `json:"size"`
		AspectRatio     interface{} `json:"aspect_ratio"`
		ResolutionName  interface{} `json:"resolution_name"`
		Preset          interface{} `json:"preset"`
		InputReference  interface{} `json:"input_reference"`
		InputReferences interface{} `json:"input_references"`
	}
	var raw rawVideosRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	seconds, err := parseLooseIntAny(raw.Seconds)
	if err != nil {
		return err
	}
	if seconds == 0 {
		seconds, err = parseLooseIntAny(raw.VideoLength)
		if err != nil {
			return err
		}
	}
	r.Model = parseLooseStringAny(raw.Model)
	r.Prompt = parseLooseStringAny(raw.Prompt)
	r.Seconds = seconds
	r.Size = parseLooseStringAny(raw.Size)
	if r.Size == "" {
		r.Size = parseLooseStringAny(raw.AspectRatio)
	}
	r.ResolutionName = parseLooseStringAny(raw.ResolutionName)
	r.Preset = parseLooseStringAny(raw.Preset)
	r.InputReferences = parseVideoInputReferences(raw.InputReferences)
	if len(r.InputReferences) == 0 {
		r.InputReferences = parseVideoInputReferences(raw.InputReference)
	}
	return nil
}

type RateLimitInfo struct {
	Limit        int64
	HasLimit     bool
	Remaining    int64
	HasRemaining bool
	ResetAt      time.Time
	Unit         string
}

const (
	maxToolDefinitions      = 128
	maxToolDescriptionBytes = 16 << 10
)

var grokToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validateToolDefinitions(tools []ToolDef) error {
	seen := make(map[string]struct{}, len(tools))
	for i, tool := range tools {
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			return fmt.Errorf("tools.%d.type must be function", i)
		}
		if tool.Function == nil {
			return fmt.Errorf("tools.%d.function is required", i)
		}
		name, ok := tool.Function["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("tools.%d.function.name must be a non-empty string", i)
		}
		key := strings.TrimSpace(name)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("tools.%d.function.name duplicates %q", i, name)
		}
		seen[key] = struct{}{}
		if parameters, ok := tool.Function["parameters"]; ok && parameters != nil {
			raw, err := json.Marshal(parameters)
			if err != nil {
				return fmt.Errorf("tools.%d.function.parameters must be valid JSON", i)
			}
			var decoded interface{}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return fmt.Errorf("tools.%d.function.parameters must be valid JSON", i)
			}
		}
	}
	return nil
}

// Web emulates tools in a prompt; its limits are not Build/Console contracts.
func validateWebToolDefinitions(tools []ToolDef) error {
	if err := validateToolDefinitions(tools); err != nil {
		return err
	}
	if len(tools) > maxToolDefinitions {
		return fmt.Errorf("tools must contain at most %d items on Web", maxToolDefinitions)
	}
	for i, tool := range tools {
		if !grokToolNamePattern.MatchString(strings.TrimSpace(tool.Function["name"].(string))) {
			return fmt.Errorf("tools.%d.function.name must match [A-Za-z0-9_-]{1,64} on Web", i)
		}
		if description := strings.TrimSpace(parseLooseStringAny(tool.Function["description"])); len(description) > maxToolDescriptionBytes {
			return fmt.Errorf("tools.%d.function.description must be at most %d bytes on Web", i, maxToolDescriptionBytes)
		}
	}
	return nil
}

func (r *ChatCompletionsRequest) Validate() error {
	if strings.TrimSpace(r.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if len(r.Messages) == 0 {
		return fmt.Errorf("messages is required")
	}
	if err := validateChatMessages(r.Messages); err != nil {
		return err
	}
	if err := validateToolDefinitions(r.Tools); err != nil {
		return err
	}
	if r.ToolChoice != nil {
		switch v := r.ToolChoice.(type) {
		case string:
			switch strings.ToLower(strings.TrimSpace(v)) {
			case "auto", "none":
			case "required":
				if len(r.Tools) == 0 {
					return fmt.Errorf("tool_choice required needs at least one defined tool")
				}
			default:
				return fmt.Errorf("tool_choice must be auto, required, none, or a specific function object")
			}
		case map[string]interface{}:
			fn, _ := v["function"].(map[string]interface{})
			name, _ := fn["name"].(string)
			name = strings.TrimSpace(name)
			if strings.TrimSpace(fmt.Sprint(v["type"])) != "function" || name == "" {
				return fmt.Errorf("tool_choice object must have type=function and function.name")
			}
			found := false
			for _, tool := range r.Tools {
				if strings.TrimSpace(fmt.Sprint(tool.Function["name"])) == name {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("tool_choice.function.name must reference a defined tool")
			}
		default:
			return fmt.Errorf("tool_choice must be auto, required, none, or a specific function object")
		}
	}
	if r.ImageConfig != nil {
		r.ImageConfig.Normalize()
		if r.ImageConfig.N < 1 || r.ImageConfig.N > 10 {
			return fmt.Errorf("image_config.n must be between 1 and 10")
		}
		modelID := normalizeModelID(r.Model)
		if modelID == "grok-imagine-image-lite" && r.ImageConfig.N > 4 {
			return fmt.Errorf("image_config.n must be between 1 and 4 for grok-imagine-image-lite")
		}
		if modelID == "grok-imagine-image-edit" && r.ImageConfig.N > 2 {
			return fmt.Errorf("image_config.n must be between 1 and 2 for image edit")
		}
		if r.ImageConfig.ResponseFormat != "" {
			switch normalizeImageResponseFormat(r.ImageConfig.ResponseFormat) {
			case "b64_json", "url":
				// ok
			default:
				return fmt.Errorf("image_config.response_format must be one of b64_json, base64, url")
			}
			r.ImageConfig.ResponseFormat = normalizeImageResponseFormat(r.ImageConfig.ResponseFormat)
		}
		size, err := normalizeImageSize(r.ImageConfig.Size)
		if modelID == "grok-imagine-image-edit" {
			size, err = normalizeImageEditSize(r.ImageConfig.Size)
		}
		if err != nil {
			return err
		}
		r.ImageConfig.Size = size
		if r.Stream && r.ImageConfig.N > 2 {
			return fmt.Errorf("streaming is only supported when image_config.n=1 or n=2")
		}
	}
	return nil
}

func (r *ImagesGenerationsRequest) Normalize() {
	if strings.TrimSpace(r.Model) == "" {
		r.Model = "grok-imagine-image"
	}
	if r.N <= 0 {
		r.N = 1
	}
	if strings.TrimSpace(r.ResponseFormat) == "" {
		r.ResponseFormat = "url"
	}
}

func (c *ImageConfig) Normalize() {
	if c == nil {
		return
	}
	if c.N <= 0 {
		c.N = 1
	}
	if strings.TrimSpace(c.Size) == "" {
		c.Size = "1024x1024"
	}
	if strings.TrimSpace(c.ResponseFormat) == "" {
		c.ResponseFormat = "url"
	}
	c.ResponseFormat = normalizeImageResponseFormat(c.ResponseFormat)
}

func (v *VideoConfig) Normalize() {
	if v == nil {
		return
	}
	if v.VideoLength == 0 {
		v.VideoLength = 6
	}
	if strings.TrimSpace(v.Preset) == "" {
		v.Preset = "custom"
	}
}
