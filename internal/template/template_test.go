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
