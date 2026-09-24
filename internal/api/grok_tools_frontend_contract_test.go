package api

import (
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
