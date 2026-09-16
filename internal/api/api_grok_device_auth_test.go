package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// grokLoginTestAPI is the API the constructor builds, without a store: enough
// for a handler test to seed one device-login transaction of its choosing.
func grokLoginTestAPI() *API {
	return &API{grokLogins: newDeviceLoginRegistry(identityDeviceLogin, nil, "Grok authorization expired")}
}

func TestHandleGrokDeviceAuthorizationStatusRedactsDeviceCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := grokLoginTestAPI()
	a.grokLogins.admit("login-id", &deviceLogin{
		deviceCode: "must-not-be-exposed", userCode: "ABCD-1234", verifyURI: "https://auth.x.ai/device",
		expiresAt: time.Now().Add(time.Minute), status: "pending", cancel: cancel,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/grok/device-auth/login-id", nil).WithContext(ctx)
	a.HandleGrokDeviceAuthorization(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "must-not-be-exposed") || !strings.Contains(body, "ABCD-1234") {
		t.Fatalf("unexpected response: %s", body)
	}
}

func TestHandleGrokDeviceAuthorizationDeleteCancelsAndForgets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := grokLoginTestAPI()
	a.grokLogins.admit("login-id", &deviceLogin{
		deviceCode: "device-secret", expiresAt: time.Now().Add(time.Minute), status: "pending", cancel: cancel,
	})
	rec := httptest.NewRecorder()
	a.HandleGrokDeviceAuthorization(rec, httptest.NewRequest(http.MethodDelete, "/api/grok/device-auth/login-id", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var remained bool
	a.grokLogins.update("login-id", func(*deviceLogin) { remained = true })
	if remained {
		t.Fatal("cancelled login remained in memory")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel function was not called")
	}
}
