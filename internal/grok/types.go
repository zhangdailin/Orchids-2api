package grok

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type ChatCompletionsRequest struct {
	Model           string          `json:"model"`
	Messages        []ChatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	StreamProvided  bool            `json:"-"`
	Thinking        *string         `json:"thinking,omitempty"`
	ReasoningEffort *string         `json:"reasoning_effort,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	VideoConfig     *VideoConfig    `json:"video_config,omitempty"`
	ImageConfig     *ImageConfig    `json:"image_config,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

type ChatMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type VideoConfig struct {
	AspectRatio    string `json:"aspect_ratio"`
	VideoLength    int    `json:"video_length"`
	ResolutionName string `json:"resolution_name"`
	Preset         string `json:"preset"`
}

type ImageConfig struct {
	N              int    `json:"n"`
	Size           string `json:"size"`
	ResponseFormat string `json:"response_format"`
}

type ImagesGenerationsRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Stream         bool   `json:"stream"`
	NSFW           *bool  `json:"nsfw,omitempty"`
	ResponseFormat string `json:"response_format"`
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

func (v *VideoConfig) UnmarshalJSON(data []byte) error {
	type rawVideoConfig struct {
		AspectRatio    interface{} `json:"aspect_ratio"`
		VideoLength    interface{} `json:"video_length"`
		ResolutionName interface{} `json:"resolution_name"`
		Preset         interface{} `json:"preset"`
	}
	var raw rawVideoConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	videoLength, err := parseLooseIntAny(raw.VideoLength)
	if err != nil {
		return err
	}
	v.AspectRatio = parseLooseStringAny(raw.AspectRatio)
	v.VideoLength = videoLength
	v.ResolutionName = parseLooseStringAny(raw.ResolutionName)
	v.Preset = parseLooseStringAny(raw.Preset)
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
		Model           string          `json:"model"`
		Messages        []ChatMessage   `json:"messages"`
		Stream          interface{}     `json:"stream"`
		Thinking        *string         `json:"thinking,omitempty"`
		ReasoningEffort *string         `json:"reasoning_effort,omitempty"`
		Temperature     interface{}     `json:"temperature,omitempty"`
		TopP            interface{}     `json:"top_p,omitempty"`
		VideoConfig     *VideoConfig    `json:"video_config,omitempty"`
		ImageConfig     *ImageConfig    `json:"image_config,omitempty"`
		Raw             json.RawMessage `json:"-"`
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

	r.Model = raw.Model
	r.Messages = raw.Messages
	r.Stream = stream
	r.StreamProvided = streamProvided
	r.Thinking = raw.Thinking
	r.ReasoningEffort = raw.ReasoningEffort
	r.Temperature = temp
	r.TopP = topP
	r.VideoConfig = raw.VideoConfig
	r.ImageConfig = raw.ImageConfig
	r.Raw = append(r.Raw[:0], data...)
	return nil
}

func (r *ImagesGenerationsRequest) UnmarshalJSON(data []byte) error {
	type rawImagesGenerationsRequest struct {
		Model          interface{} `json:"model"`
		Prompt         interface{} `json:"prompt"`
		N              interface{} `json:"n"`
		Size           interface{} `json:"size"`
		Stream         interface{} `json:"stream"`
		NSFW           interface{} `json:"nsfw"`
		ResponseFormat interface{} `json:"response_format"`
	}
	var raw rawImagesGenerationsRequest
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n, err := parseLooseIntAny(raw.N)
	if err != nil {
		return err
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
	r.Size = parseLooseStringAny(raw.Size)
	r.Stream = stream
	r.NSFW = nsfw
	r.ResponseFormat = parseLooseStringAny(raw.ResponseFormat)
	return nil
}

type RateLimitInfo struct {
	Limit     int64
	Remaining int64
	ResetAt   time.Time
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
	if r.Temperature == nil {
		v := 0.8
		r.Temperature = &v
	} else if *r.Temperature < 0 || *r.Temperature > 2 {
		return fmt.Errorf("temperature must be between 0 and 2")
	}
	if r.TopP == nil {
		v := 0.95
		r.TopP = &v
	} else if *r.TopP < 0 || *r.TopP > 1 {
		return fmt.Errorf("top_p must be between 0 and 1")
	}
	if r.ReasoningEffort != nil {
		v := strings.ToLower(strings.TrimSpace(*r.ReasoningEffort))
		switch v {
		case "none", "minimal", "low", "medium", "high", "xhigh":
		default:
			return fmt.Errorf("reasoning_effort must be one of none/minimal/low/medium/high/xhigh")
		}
	}
	if r.ImageConfig != nil {
		r.ImageConfig.Normalize()
		if r.ImageConfig.N < 1 || r.ImageConfig.N > 10 {
			return fmt.Errorf("image_config.n must be between 1 and 10")
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
		r.Model = "grok-imagine-1.0"
	}
	if r.N <= 0 {
		r.N = 1
	}
	if strings.TrimSpace(r.Size) == "" {
		r.Size = "1024x1024"
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
	if c.ResponseFormat != "" {
		c.ResponseFormat = normalizeImageResponseFormat(c.ResponseFormat)
	}
}

func (v *VideoConfig) Normalize() {
	if v == nil {
		return
	}
	if strings.TrimSpace(v.AspectRatio) == "" {
		v.AspectRatio = "3:2"
	}
	if v.VideoLength == 0 {
		v.VideoLength = 6
	}
	if strings.TrimSpace(v.ResolutionName) == "" {
		v.ResolutionName = "480p"
	}
	if strings.TrimSpace(v.Preset) == "" {
		v.Preset = "custom"
	}
}
