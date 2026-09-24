package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokToolsFrontendPreservesCapabilitiesAndAuthenticatesVideoMedia(t *testing.T) {
	source, err := readConsoleScript("grok-tools.js")
	if err != nil {
		t.Fatal(err)
	}
	checks := []string{
		`if (!Array.isArray(route.capabilities)) route.capabilities = ["chat"]`,
		`status === "completed" || status === "done"`,
		`job?.video?.url || job?.content_url || job?.video_url`,
		`${toolInferencePrefix()}/videos/${encodeURIComponent(taskID)}/content`,
		`window.GrokToolRequest?.headers?.({ Accept: "video/*" })`,
		`video.src = objectURL`,
		`revokeVideoObjectURLs()`,
		`Array.isArray(route?.video_actions)`,
	}
	for _, check := range checks {
		if !strings.Contains(source, check) {
			t.Errorf("grok-tools.js missing %q", check)
		}
	}
	if strings.Contains(source, `.map((item) => ({ ...item, id: String(item.id).trim(), capabilities: ["chat"] }))`) {
		t.Error("client-key catalog still overwrites declared model capabilities")
	}
	if strings.Contains(source, `<source src="${safeUrl}"`) {
		t.Error("video preview still assigns an authentication-free media URL")
	}
}

// TestGrokVideoRatioPickerMatchesAcceptedAspectRatios keeps the console video
// aspect-ratio picker and grok.validConsoleVideoAspectRatio in step. The picker
// used to omit 4:3 and 3:4, so the only ratios a Browser could select were a
// subset of what the backend accepts and the audit's "video ratios" item stayed
// silently open.
func TestGrokVideoRatioPickerMatchesAcceptedAspectRatios(t *testing.T) {
	template, err := os.ReadFile(filepath.Join("..", "..", "web", "templates", "pages", "grok-tools.html"))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := os.ReadFile(filepath.Join("..", "grok", "handler_videos_console.go"))
	if err != nil {
		t.Fatal(err)
	}

	accepted := consoleAspectRatioLiterals(t, string(backend))
	// A parse failure would make every later assertion vacuous, so pin the count
	// the backend switch declares.
	if len(accepted) != 7 {
		t.Fatalf("parsed %d backend aspect ratios (%v), want the 7 in the switch", len(accepted), accepted)
	}

	page := string(template)
	// Scope the search to the video picker so an image ratio cannot satisfy it.
	start := strings.Index(page, `id="videoRatio"`)
	if start < 0 {
		t.Fatal("videoRatio select is missing from the Grok tools page")
	}
	end := strings.Index(page[start:], "</select>")
	if end < 0 {
		t.Fatal("videoRatio select is not closed")
	}
	picker := page[start : start+end]

	for _, ratio := range accepted {
		if !strings.Contains(picker, `value="`+ratio+`"`) {
			t.Errorf("video ratio picker cannot select %s, which the backend accepts", ratio)
		}
	}
}

// consoleAspectRatioLiterals extracts the ratio literals from the backend's
// validConsoleVideoAspectRatio switch.
func consoleAspectRatioLiterals(t *testing.T, source string) []string {
	t.Helper()
	const marker = "func validConsoleVideoAspectRatio"
	start := strings.Index(source, marker)
	if start < 0 {
		t.Fatal("validConsoleVideoAspectRatio is missing")
	}
	body := source[start:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}

	ratios := make([]string, 0, 8)
	seen := map[string]struct{}{}
	// Splitting on the quote character alternates between code and literal, so
	// the ratio literals are exactly the segments shaped like "N:M".
	for _, segment := range strings.Split(body, `"`) {
		left, right, ok := strings.Cut(segment, ":")
		if !ok || left == "" || right == "" {
			continue
		}
		if strings.ContainsFunc(left+right, func(r rune) bool { return r < '0' || r > '9' }) {
			continue
		}
		if _, duplicate := seen[segment]; duplicate {
			continue
		}
		seen[segment] = struct{}{}
		ratios = append(ratios, segment)
	}
	return ratios
}
