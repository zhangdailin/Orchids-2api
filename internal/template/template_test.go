package template

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/store"
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

// TestTutorialPageListsEveryChannel proves that the tutorial's single channel
// table includes every public base URL. Channel content intentionally lives in
// the template now; tutorial.js only fills the current origin and handles copy.
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

	for _, key := range []string{"warp", "puter", "workbuddy", "qoder", "grok"} {
		if !strings.Contains(page, `badge-`+key) {
			t.Errorf("the rendered tutorial page has no row for channel %q", key)
		}
		// The row's copyable address must have been filled in for this channel.
		if !strings.Contains(page, `data-api-path="/`+key+`/v1"`) {
			t.Errorf("the rendered tutorial page has no address cell for channel %q", key)
		}
	}
	for _, unrelatedID := range []string{"modelModal", "createKeyModal", "editKeyModal", "showKeyModal", "deleteKeyModal"} {
		if strings.Contains(page, `id="`+unrelatedID+`"`) {
			t.Errorf("tutorial page still includes unrelated modal %q", unrelatedID)
		}
	}
}

func TestPagesRenderOnlyTheirOwnModals(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}

	tests := []struct {
		tab       string
		wantIDs   []string
		forbidIDs []string
	}{
		{"accounts", []string{"accountModal"}, []string{"modelModal", "createKeyModal"}},
		{"keys", []string{"createKeyModal", "editKeyModal", "showKeyModal", "deleteKeyModal"}, []string{"accountModal", "modelModal"}},
		{"models", []string{"modelModal"}, []string{"accountModal", "createKeyModal"}},
	}

	for _, tt := range tests {
		t.Run(tt.tab, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/?tab="+tt.tab, nil)
			if err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil); err != nil {
				t.Fatalf("RenderIndex() error = %v", err)
			}
			page := recorder.Body.String()
			for _, id := range tt.wantIDs {
				if !strings.Contains(page, `id="`+id+`"`) {
					t.Errorf("missing modal %q", id)
				}
			}
			for _, id := range tt.forbidIDs {
				if strings.Contains(page, `id="`+id+`"`) {
					t.Errorf("includes unrelated modal %q", id)
				}
			}
		})
	}
}

func TestSidebarUsesRealLinks(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=ops", nil)
	if err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/console"}, nil); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
	page := recorder.Body.String()
	for _, tab := range []string{"ops", "logs", "accounts", "keys", "models", "grok-tools", "alerts", "tutorial"} {
		if !strings.Contains(page, `href="/console/?tab=`+tab+`"`) {
			t.Errorf("sidebar has no native link for %q", tab)
		}
	}
	if strings.Contains(page, `onclick="switchTab(`) {
		t.Error("sidebar navigation still depends on inline JavaScript")
	}
}
