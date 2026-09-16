package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// warpLoginTestAPI is the API the constructor builds, without a store: enough
// for a handler test to seed one device-login transaction of its choosing.
func warpLoginTestAPI() *API {
	return &API{warpLogins: newDeviceLoginRegistry(identityDeviceLogin, nil, "Warp authorization expired")}
}

func TestHandleWarpDeviceAuthorizationStatusRedactsDeviceCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := warpLoginTestAPI()
	a.warpLogins.admit("login-id", &deviceLogin{
		deviceCode: "must-not-be-exposed",
		userCode:   "ABCD-1234",
		verifyURI:  "https://app.warp.dev/device",
		expiresAt:  time.Now().Add(time.Minute),
		status:     "pending",
		cancel:     cancel,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/warp/device-auth/login-id", nil).WithContext(ctx)
	a.HandleWarpDeviceAuthorization(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "must-not-be-exposed") {
		t.Fatalf("device code leaked in response: %s", body)
	}
	if !strings.Contains(body, "ABCD-1234") || !strings.Contains(body, "verification_uri") {
		t.Fatalf("expected public device details, got %s", body)
	}
}

func TestHandleWarpDeviceAuthorizationDeleteCancelsAndForgets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := warpLoginTestAPI()
	a.warpLogins.admit("login-id", &deviceLogin{
		deviceCode: "device-secret",
		expiresAt:  time.Now().Add(time.Minute),
		status:     "pending",
		cancel:     cancel,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/warp/device-auth/login-id", nil)
	a.HandleWarpDeviceAuthorization(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var remained bool
	a.warpLogins.update("login-id", func(*deviceLogin) { remained = true })
	if remained {
		t.Fatal("cancelled login remained in memory")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel function was not called")
	}
}
