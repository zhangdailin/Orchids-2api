package workbuddy

import (
	"strings"

	"github.com/goccy/go-json"
)

// catalogSnapshotRow is the stored form of one catalog entry.
//
// The snapshot used to be a bare list of model ids, which preserved the
// whitelist but threw away everything the whitelist was observed alongside: the
// input window and the output budget. A client that budgets its context then had
// nothing to read, and a long session looked like an overflow even though the
// model accepted it. Each row is therefore stored as its own JSON object, and a
// bare id written by an older build is still accepted on read.
type catalogSnapshotRow struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	MaxInputTokens  int64  `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
}

// CatalogSnapshot projects the observed catalog onto its stored form.
func CatalogSnapshot(models []WorkBuddyModel) []string {
	if len(models) == 0 {
		return nil
	}
	rows := make([]string, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		raw, err := json.Marshal(catalogSnapshotRow{
			ID:              id,
			Name:            strings.TrimSpace(model.Name),
			MaxInputTokens:  model.MaxInputTokens,
			MaxOutputTokens: model.MaxOutputTokens,
		})
		if err != nil {
			continue
		}
		rows = append(rows, string(raw))
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}

// CatalogContextWindows reads the windows back out of a stored snapshot.
//
// It answers two maps because the public model list reports the input window and
// the output budget separately. A row that carries neither is skipped, so
// "never observed" stays distinguishable from "observed as zero".
func CatalogContextWindows(ids []string) (input map[string]int, output map[string]int) {
	if len(ids) == 0 {
		return nil, nil
	}
	input = make(map[string]int, len(ids))
	output = make(map[string]int, len(ids))
	for _, raw := range ids {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		row := catalogSnapshotRow{}
		if strings.HasPrefix(trimmed, "{") {
			if err := json.Unmarshal([]byte(trimmed), &row); err != nil {
				continue
			}
		} else {
			// A bare id from a snapshot written before the richer form existed.
			// There is no window to recover, but the id still has to resolve.
			row.ID = trimmed
		}
		id := strings.ToLower(strings.TrimSpace(row.ID))
		if id == "" {
			continue
		}
		if row.MaxInputTokens > 0 {
			if existing, ok := input[id]; !ok || int(row.MaxInputTokens) > existing {
				input[id] = int(row.MaxInputTokens)
			}
		}
		if row.MaxOutputTokens > 0 {
			if existing, ok := output[id]; !ok || int(row.MaxOutputTokens) > existing {
				output[id] = int(row.MaxOutputTokens)
			}
		}
	}
	if len(input) == 0 {
		input = nil
	}
	if len(output) == 0 {
		output = nil
	}
	return input, output
}
