package store

import (
	"bytes"
	"github.com/goccy/go-json"
	"slices"
	"strings"
	"time"
)

const (
	CapabilityResponses = "responses"
	CapabilityChat      = "chat"
	CapabilityMessages  = "messages"
	CapabilityImage     = "image"
	CapabilityImageEdit = "image_edit"
	CapabilityVideo     = "video"
	CapabilityTTS       = "tts"
	CapabilitySTT       = "stt"
	CapabilityRealtime  = "realtime"
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
	*s = ModelStatusOffline

	// 兼容 bool：true => available, false => offline
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*s = ModelStatusAvailable
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
		}
		return nil
	}
	return nil
}

func (s ModelStatus) MarshalJSON() ([]byte, error) {
	// 始终输出字符串，保证前后端一致。
	if s == "" {
		s = ModelStatusOffline
	}
	return json.Marshal(string(s))
}

// ModelReconcileOptions controls one authoritative catalog publication.
// ProviderScope limits replacement/pruning to one provider plane (for example,
// Grok Build). An empty scope covers the whole channel. When Prune is true the
// upstream snapshot is the truth: every in-scope row absent from it is removed,
// including rows previously created through the admin UI.
type ModelReconcileOptions struct {
	Prune         bool
	ProviderScope string
}

// ModelReconcileResult describes the atomic changes made by a reconciliation.
type ModelReconcileResult struct {
	Added           int      `json:"added"`
	Updated         int      `json:"updated"`
	Deleted         int      `json:"deleted"`
	Protected       int      `json:"protected"`
	AddedModelIDs   []string `json:"added_model_ids,omitempty"`
	UpdatedModelIDs []string `json:"updated_model_ids,omitempty"`
	DeletedModelIDs []string `json:"deleted_model_ids,omitempty"`
	ProtectedIDs    []string `json:"protected_model_ids,omitempty"`
}

type Model struct {
	ID            string      `json:"id"`
	Channel       string      `json:"channel"`  // e.g., "workbuddy", "grok"
	ModelID       string      `json:"model_id"` // e.g., "claude-3-5-sonnet"
	Name          string      `json:"name"`     // e.g., "Claude 3.5 Sonnet"
	Status        ModelStatus `json:"status"`   // Enabled/Disabled
	Verified      bool        `json:"verified,omitempty"`
	IsDefault     bool        `json:"is_default"` // Is default for this channel
	SortOrder     int         `json:"sort_order"`
	Provider      string      `json:"provider,omitempty"`
	UpstreamModel string      `json:"upstream_model,omitempty"`
	Capabilities  []string    `json:"capabilities,omitempty"`
	// BillingTier is the upstream-observed charging class for this route. Empty
	// means unknown; only the exact value "free" may keep an exhausted account
	// eligible. It is deliberately separate from capabilities because charging
	// can change while the model's protocol features do not.
	BillingTier     string  `json:"billing_tier,omitempty"`
	BillingSource   string  `json:"billing_source,omitempty"`
	Origin          string  `json:"origin,omitempty"`
	BoundAccountIDs []int64 `json:"bound_account_ids,omitempty"`
	// CreatedAt records when the route row was first seen. The public model
	// list reports it as `created`; a row stored before this field existed
	// leaves it zero and the caller falls back to the legacy constant.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func (m *Model) NormalizeRoute() {
	if m == nil {
		return
	}
	m.Provider = strings.ToLower(strings.TrimSpace(m.Provider))
	m.UpstreamModel = strings.TrimSpace(m.UpstreamModel)
	m.BillingTier = strings.ToLower(strings.TrimSpace(m.BillingTier))
	m.BillingSource = strings.ToLower(strings.TrimSpace(m.BillingSource))
	m.Origin = strings.ToLower(strings.TrimSpace(m.Origin))
	if m.Origin == "" {
		m.Origin = "manual"
	}
	seen := map[string]struct{}{}
	capabilities := make([]string, 0, len(m.Capabilities))
	for _, value := range m.Capabilities {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		capabilities = append(capabilities, value)
	}
	slices.Sort(capabilities)
	m.Capabilities = capabilities
	ids := append([]int64(nil), m.BoundAccountIDs...)
	slices.Sort(ids)
	m.BoundAccountIDs = slices.CompactFunc(ids, func(a, b int64) bool { return a == b })
}

func (m *Model) SupportsCapability(capability string) bool {
	if m == nil || len(m.Capabilities) == 0 {
		return true
	}
	return slices.Contains(m.Capabilities, strings.ToLower(strings.TrimSpace(capability)))
}

func (m *Model) AllowsAccount(accountID int64) bool {
	return m == nil || len(m.BoundAccountIDs) == 0 || slices.Contains(m.BoundAccountIDs, accountID)
}
