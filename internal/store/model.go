package store

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ModelStatus 表示模型状态。
//
// 管理端前端使用字符串状态：available/maintenance/offline。
// 老数据/老客户端可能仍然使用 bool（true/false）。这里做兼容解析。
type ModelStatus string

const (
	ModelStatusAvailable   ModelStatus = "available"
	ModelStatusMaintenance ModelStatus = "maintenance"
	ModelStatusOffline     ModelStatus = "offline"
)

// Enabled 表示该模型是否可用于对外 /v1/models 列表。
func (s ModelStatus) Enabled() bool {
	return s == ModelStatusAvailable
}

func (s *ModelStatus) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		*s = ModelStatusOffline
		return nil
	}

	// 兼容 bool：true => available, false => offline
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*s = ModelStatusAvailable
		} else {
			*s = ModelStatusOffline
		}
		return nil
	}

	// 字符串状态
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		switch strings.ToLower(strings.TrimSpace(str)) {
		case "available", "enabled", "true", "on", "1":
			*s = ModelStatusAvailable
		case "maintenance", "maint":
			*s = ModelStatusMaintenance
		case "offline", "disabled", "false", "off", "0":
			*s = ModelStatusOffline
		default:
			*s = ModelStatusOffline
		}
		return nil
	}

	// 兜底：非法值视为 offline
	*s = ModelStatusOffline
	return nil
}

func (s ModelStatus) MarshalJSON() ([]byte, error) {
	// 始终输出字符串，保证前后端一致。
	if s == "" {
		s = ModelStatusOffline
	}
	return json.Marshal(string(s))
}

type Model struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`    // e.g., "orchids", "kiro"
	ModelID   string `json:"model_id"`   // e.g., "claude-3-5-sonnet"
	Name      string `json:"name"`       // e.g., "Claude 3.5 Sonnet"
	Status    ModelStatus `json:"status"` // Enabled/Disabled
	IsDefault bool   `json:"is_default"` // Is default for this channel
	SortOrder int    `json:"sort_order"`
}
