package template

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/store"
	"orchids-api/web"
)

func TestRenderIndexCountsOnlyVisibleAccounts(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "template_test:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	web := &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderWeb, ClientCookie: "sso=web", Enabled: true}
	console := &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, GrokSSOParentID: 1, ClientCookie: "sso=web", Enabled: true}
	standaloneConsole := &store.Account{AccountType: "grok", CredentialType: "sso", GrokProvider: grok.ProviderConsole, ClientCookie: "sso=standalone", Enabled: false}
	for _, acc := range []*store.Account{web, console, standaloneConsole} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
	}
	console.GrokSSOParentID = web.ID
	if err := s.UpdateAccount(context.Background(), console); err != nil {
		t.Fatalf("UpdateAccount(console) error = %v", err)
	}

	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=accounts", nil)
	if err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, s); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `id="footerTotal">2</span>`) || !strings.Contains(body, `id="footerNormal">1</span>`) || !strings.Contains(body, `id="footerAbnormal">1</span>`) {
		t.Fatalf("dashboard counts included linked Console child: %s", body)
	}
}
func TestRendererParsesAndRendersEmbeddedTemplates(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=accounts", nil)
	if err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
}

// TestTutorialPageListsEveryChannel proves the rendered tutorial page carries a
// quick-reference row for every channel the page's own script knows about.
//
// The table is plain markup, so adding a channel never fails to compile: Qoder
// shipped with four rows and no card, and the operator's tutorial simply did not
// mention it. This asserts on the rendered HTML so the markup, the script and the
// channel list cannot drift apart again.
func TestTutorialPageListsEveryChannel(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=tutorial", nil)
	if err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
	page := recorder.Body.String()

	// The channel list lives in the page's script; every entry must have a row.
	js, err := web.TemplateFS.ReadFile("templates/pages/tutorial.html")
	if err != nil {
		t.Fatalf("read tutorial template: %v", err)
	}
	_ = js
	scriptBytes, err := os.ReadFile(filepath.Join("..", "..", "web", "static", "js", "tutorial.js"))
	if err != nil {
		t.Fatalf("read tutorial script: %v", err)
	}
	keys := regexp.MustCompile(`key:\s*'([a-z0-9_-]+)'`).FindAllStringSubmatch(string(scriptBytes), -1)
	if len(keys) < 5 {
		t.Fatalf("parsed %d channels from the tutorial script, want at least 5", len(keys))
	}
	for _, match := range keys {
		key := match[1]
		if !strings.Contains(page, `badge-`+key) {
			t.Errorf("the rendered tutorial page has no row for channel %q", key)
		}
		// The row's copyable address must have been filled in for this channel.
		if !strings.Contains(page, `data-api-path="/`+key+`/v1"`) {
			t.Errorf("the rendered tutorial page has no address cell for channel %q", key)
		}
	}
	if !strings.Contains(page, `badge-qoder`) {
		t.Error("the rendered tutorial page does not mention the Qoder channel")
	}
}
