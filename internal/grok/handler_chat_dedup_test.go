package grok

import (
	"github.com/goccy/go-json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollapseDuplicatedLongChunk(t *testing.T) {
	dup := "Hi! How can I help you today?Hi! How can I help you today?"
	if got := collapseDuplicatedLongChunk(dup); got != "Hi! How can I help you today?" {
		t.Fatalf("collapseDuplicatedLongChunk()=%q", got)
	}

	shortDup := "haha" + "haha"
	if got := collapseDuplicatedLongChunk(shortDup); got != shortDup {
		t.Fatalf("short duplicated chunk should not collapse, got=%q", got)
	}
}

func TestStreamChat_DedupsGreetingRepeat(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()

	dup := "Hi! How can I help you today?Hi! How can I help you today?"
	body := strings.NewReader(
		`{"result":{"response":{"token":"` + dup + `"}}}` +
			`{"result":{"response":{"token":"` + dup + `"}}}`,
	)

	h.streamChat(rec, "grok-420", ModelSpec{ID: "grok-420"}, "", "", true, body)
	contents := extractStreamTextContents(t, rec.Body.String())
	combined := strings.Join(contents, "")
	if strings.Count(combined, "Hi! How can I help you today?") != 1 {
		t.Fatalf("expected greeting once, combined=%q raw=%q", combined, rec.Body.String())
	}
	if strings.Contains(combined, dup) {
		t.Fatalf("unexpected duplicated greeting in stream, combined=%q", combined)
	}
}

func extractStreamTextContents(t *testing.T, raw string) []string {
	t.Helper()
	lines := strings.Split(raw, "\n")
	out := make([]string, 0, 4)
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" || strings.TrimSpace(payload) == "" {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		choices, ok := obj["choices"].([]interface{})
		if !ok || len(choices) == 0 {
			continue
		}
		choice, ok := choices[0].(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		content, _ := delta["content"].(string)
		if strings.TrimSpace(content) != "" {
			out = append(out, content)
		}
	}
	return out
}
