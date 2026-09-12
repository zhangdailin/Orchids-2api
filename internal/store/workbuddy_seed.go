package store

import (
	"context"
	"strconv"

	"orchids-api/internal/modelpolicy"
)

// buildWorkBuddySeedModels seeds the WorkBuddy international channel catalog.
// The authoritative set is discovered per account from GET /v3/config
// (`data.agents[name=cli].models`); this seed keeps the channel usable before
// the first refresh.
func buildWorkBuddySeedModels() []Model {
	modelIDs := modelpolicy.LatestWorkBuddyModelIDs()

	models := make([]Model, 0, len(modelIDs))
	for i, modelID := range modelIDs {
		models = append(models, Model{
			ID:        strconv.Itoa(130 + i),
			Channel:   "WorkBuddy",
			ModelID:   modelID,
			Name:      modelID,
			Status:    ModelStatusAvailable,
			IsDefault: modelID == modelpolicy.DefaultWorkBuddyModelID,
			SortOrder: i,
		})
	}
	return models
}

// reconcileLatestWorkBuddyModels drops seeded WorkBuddy rows that left the
// upstream cli whitelist. Refreshed catalog ids are always kept: they are
// verified against the live endpoint, which is a stronger signal than this
// compiled-in seed list.
func (s *Store) reconcileLatestWorkBuddyModels(ctx context.Context) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return
	}
	for _, model := range models {
		if model == nil || !isWorkBuddyChannel(model.Channel) {
			continue
		}
		if model.Origin == "discovery" || model.Verified {
			continue
		}
		if modelpolicy.IsLatestWorkBuddyModelID(model.ModelID) {
			continue
		}
		if err := s.DeleteModel(ctx, model.ID); err != nil {
			continue
		}
	}
}

func isWorkBuddyChannel(channel string) bool {
	return normalizeChannelName(channel) == "workbuddy"
}

// normalizeChannelName lowercases and strips separators so that "WorkBuddy",
// "workbuddy" and "work-buddy" compare equal.
func normalizeChannelName(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch r {
		case ' ', '_', '-':
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}
