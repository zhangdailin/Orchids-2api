package template

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestTutorialPageListsCline(t *testing.T) {
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
	if !strings.Contains(page, `badge-cline`) {
		t.Error("the rendered tutorial page has no row for cline")
	}
	if !strings.Contains(page, `data-api-path="/cline/v1"`) {
		t.Error("the rendered tutorial page has no address cell for cline")
	}
}

func TestAccountModalOffersClineLogin(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=accounts", nil)
	if err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
	page := recorder.Body.String()
	for _, want := range []string{
		`id="clineLoginGroup"`,
		`id="clineLoginButton"`,
		`ClineLogin.start()`,
		`js/cline-auth.js`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the accounts page is missing %s", want)
		}
	}
}

func TestModelModalOffersClineChannel(t *testing.T) {
	renderer, err := NewRenderer()
	if err != nil {
		t.Fatalf("NewRenderer() error = %v", err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/?tab=models", nil)
	if err := renderer.RenderIndex(recorder, request, &config.Config{AdminPath: "/admin"}, nil); err != nil {
		t.Fatalf("RenderIndex() error = %v", err)
	}
	if page := recorder.Body.String(); !strings.Contains(page, `<option value="Cline">Cline</option>`) {
		t.Error("the model modal has no Cline channel option")
	}
}
