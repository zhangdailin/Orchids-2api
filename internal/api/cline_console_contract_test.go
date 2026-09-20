package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The console renders the account channel strip from a hard-coded list. A channel
// missing from that list has no tab, so its login group in the account modal can
// never be selected — the channel is unreachable from the console no matter how
// correct the server side is. These tests pin the console's own contract, which
// lives in files a Go build never compiles.

// readConsoleScript loads one file out of web/static/js.
func readConsoleScript(name string) (string, error) {
	path := filepath.Join("..", "..", "web", "static", "js", filepath.Base(name))
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// TestAccountsJSListClineInTheChannelStrip asserts the channel is in both console
// lists: the tab order that renders it and the name map that labels it.
func TestAccountsJSListClineInTheChannelStrip(t *testing.T) {
	source, err := readConsoleScript("accounts.js")
	if err != nil {
		t.Fatalf("read accounts.js: %v", err)
	}
	// The strip is rendered from ACCOUNT_PLATFORM_ORDER, so a channel absent
	// from that array has no tab and cannot be selected.
	line := consoleLineContaining(source, "ACCOUNT_PLATFORM_ORDER =")
	if line == "" {
		t.Fatal("accounts.js has no ACCOUNT_PLATFORM_ORDER")
	}
	if !strings.Contains(line, `"cline"`) {
		t.Errorf("the channel strip omits cline: %s", line)
	}
	// The tab label comes from the name map; without it the tab renders the
	// lower-case key, which is not how any other channel is shown.
	nameLine := consoleLineContaining(source, "ACCOUNT_TYPE_NAMES =")
	if nameLine == "" {
		t.Fatal("accounts.js has no ACCOUNT_TYPE_NAMES")
	}
	if !strings.Contains(nameLine, `cline: "Cline"`) {
		t.Errorf("the channel name map omits cline: %s", nameLine)
	}
	if !strings.Contains(source, `case "cline":`) {
		t.Error("accounts.js does not resolve the cline account type")
	}
	// The login lifecycle must be stopped with the others: a transaction left
	// polling after the modal closes keeps exchanging a live device code.
	if !strings.Contains(source, `function stopClineLogin()`) {
		t.Error("accounts.js never stops the Cline login")
	}
	if calls := strings.Count(source, "stopClineLogin();"); calls < 2 {
		t.Errorf("stopClineLogin is called %d times, want at least 2 (open + close)", calls)
	}
	// A bare submit has no credential to send: it must be refused like Qoder's.
	if !strings.Contains(source, `type === "cline" && !id`) {
		t.Error("accounts.js lets a Cline account be submitted without the official login")
	}
}

// TestCommonJSExposesTheClineCredentialLikeTheOtherOAuthChannels pins the read
// contract: the refresh token stays server-side and the visible token is the
// access token, which is what makes the account row show a credential at all.
func TestCommonJSExposesTheClineCredentialLikeTheOtherOAuthChannels(t *testing.T) {
	source, err := readConsoleScript("common.js")
	if err != nil {
		t.Fatalf("read common.js: %v", err)
	}
	if !strings.Contains(source, `type === "cline"`) {
		t.Error("common.js has no cline branch")
	}
	if !strings.Contains(source, `acc.cline_access_token`) {
		t.Error("common.js does not expose the cline access token")
	}
}

// TestModelsJSListClineInTheChannelStrip pins the models page: without the
// channel there, a refreshed Cline catalog has no tab to appear under.
func TestModelsJSListClineInTheChannelStrip(t *testing.T) {
	source, err := readConsoleScript("models.js")
	if err != nil {
		t.Fatalf("read models.js: %v", err)
	}
	if !strings.Contains(source, `"Cline"`) {
		t.Error("models.js has no Cline channel")
	}
	if !strings.Contains(source, "cline_recommended_models") {
		t.Error("models.js does not label the Cline catalog source")
	}
}

// consoleLineContaining returns the first line holding a marker, so an assertion
// can be made about one specific line rather than the whole file.
func consoleLineContaining(source, marker string) string {
	for _, line := range strings.Split(source, "\n") {
		if strings.Contains(line, marker) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
