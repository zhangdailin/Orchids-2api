package store

type Model struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`    // e.g., "orchids", "kiro"
	ModelID   string `json:"model_id"`   // e.g., "claude-3-5-sonnet"
	Name      string `json:"name"`       // e.g., "Claude 3.5 Sonnet"
	Status    bool   `json:"status"`     // Enabled/Disabled
	IsDefault bool   `json:"is_default"` // Is default for this channel
	SortOrder int    `json:"sort_order"`
}
