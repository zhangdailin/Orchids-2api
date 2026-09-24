package template

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// TestRenderIndexShipsNoCompetingSidebarCount pins the fix for the sidebar that
// read 5 on 账号管理 and 11 on 运维总览. Three rules used to write #footerAbnormal
// (this renderer counted !Enabled, accounts.js counted its own verdict,
// common.js counted a third). The renderer must now ship no number at all, so
// common.js's single predicate over /api/accounts is the only source.
func TestRenderIndexShipsNoCompetingSidebarCount(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "template_test:"})
	if err != nil {
		t.Fatalf("store.New() error = %v", err)
	}
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	accounts := []*store.Account{
		{AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", Enabled: true},
		{AccountType: "grok", CredentialType: "oauth", GrokProvider: "build", Enabled: false},
	}
	for _, acc := range accounts {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount() error = %v", err)
		}
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
	for _, element := range []string{`id="footerTotal"`, `id="footerNormal"`, `id="footerAbnormal"`} {
		if !strings.Contains(body, element+`>—</span>`) {
			t.Errorf("%s must ship the client-owned placeholder, not a server count:\n%s", element, body)
		}
	}
	// A server-side count would have printed the two rows and one disabled row;
	// neither number may appear.
	if strings.Contains(body, `id="footerTotal">2</span>`) || strings.Contains(body, `id="footerAbnormal">1</span>`) {
		t.Fatalf("renderer still publishes a competing account count: %s", body)
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
	for _, tab := range []string{"ops", "logs", "accounts", "keys", "models", "alerts", "tutorial"} {
		if !strings.Contains(page, `href="/console/?tab=`+tab+`"`) {
			t.Errorf("sidebar has no native link for %q", tab)
		}
	}
	if strings.Contains(page, `onclick="switchTab(`) {
		t.Error("sidebar navigation still depends on inline JavaScript")
	}
}
