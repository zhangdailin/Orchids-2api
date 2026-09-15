package store

import (
	"strconv"
)

// buildQoderSeedModels seeds the Qoder channel catalog. The authoritative set is
// discovered per account from the signed model list endpoint; this seed keeps the
// channel usable before the first refresh, and exists only as a fallback.
func buildQoderSeedModels() []Model {
	names := qoderSeedModelNames()
	models := make([]Model, 0, len(names))
	for i, name := range names {
		models = append(models, Model{
			ID:        strconv.Itoa(160 + i),
			Channel:   "Qoder",
			ModelID:   name,
			Name:      name,
			Status:    ModelStatusAvailable,
			IsDefault: name == qoderSeedDefaultModel,
			SortOrder: i,
		})
	}
	return models
}

// qoderSeedDefaultModel is the fallback default. It mirrors the CLI's own
// default: a middle tier rather than the flagship, so a catalog outage does not
// silently bill every request at the highest rate.
const qoderSeedDefaultModel = "Qwen3.7-Max"

// qoderSeedModelNames mirrors the built-in catalog in internal/qoder. Keep this
// list in sync when the desktop client ships a new model so a fresh install and
// an account refresh expose the same rows. It is
// duplicated as a plain list rather than imported so the store keeps no
// dependency on an upstream client package.
func qoderSeedModelNames() []string {
	return []string{
		"Auto",
		"Ultimate",
		"Performance",
		"Efficient",
		"Lite",
		"Cantus",
		"Qwen3.8-Max",
		"Qwen3.7-Max",
		"Qwen3.7-Plus",
		"Kimi-K3",
		"Kimi-K2.7-Code",
		"GLM-5.3",
		"GLM-5.3-Flash",
		"GLM-5.2",
		"DeepSeek-V4-Pro",
		"DeepSeek-V4-Flash",
		"Qwen3.8-Flash",
		"MiniMax-M3",
	}
}
