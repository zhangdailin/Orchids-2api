package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"orchids-api/internal/config"
)

// TestHandleKeyByIDRotatesSecret covers the only supported way back to a usable
// secret. The store keeps just the hash, so a key whose secret was not copied
// at creation can never be displayed again; before rotation existed the list
// put a copy button over the masked string, which handed clients "sk-****1234"
// and a request that could never authenticate.
func TestHandleKeyByIDRotatesSecret(t *testing.T) {
	s, mini := newTestStore(t, "api-keys-rotate:")
	defer mini.Close()
	defer s.Close()
	a := New(s, "admin", "pass", &config.Config{})
	ctx := context.Background()

	createReq := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"client"}`))
	createRec := httptest.NewRecorder()
	a.HandleKeys(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created CreateKeyResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if _, err := s.AuthorizeApiKey(ctx, created.Key); err != nil {
		t.Fatalf("freshly created key does not authorize: %v", err)
	}

	rotateReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/keys/%d/rotate", created.ID), nil)
	rotateRec := httptest.NewRecorder()
	a.HandleKeyByID(rotateRec, rotateReq)
	if rotateRec.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", rotateRec.Code, rotateRec.Body.String())
	}
	var rotated CreateKeyResponse
	if err := json.Unmarshal(rotateRec.Body.Bytes(), &rotated); err != nil {
		t.Fatalf("decode rotate response: %v", err)
	}
	if rotated.Key == "" || rotated.Key == created.Key {
		t.Fatalf("rotated key = %q, want a new secret distinct from %q", rotated.Key, created.Key)
	}
	if rotated.ID != created.ID || rotated.Name != created.Name {
		t.Fatalf("rotation changed the key identity: %#v vs %#v", rotated, created)
	}
	if rotated.KeySuffix != rotated.Key[len(rotated.Key)-4:] {
		t.Fatalf("key_suffix %q does not match the secret", rotated.KeySuffix)
	}

	if _, err := s.AuthorizeApiKey(ctx, rotated.Key); err != nil {
		t.Fatalf("rotated key does not authorize: %v", err)
	}
	if _, err := s.AuthorizeApiKey(ctx, created.Key); err == nil {
		t.Fatal("the retired secret still authorizes after rotation")
	}

	// The list must keep withholding the secret; rotation is the reveal path,
	// not the listing endpoint.
	listReq := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	listRec := httptest.NewRecorder()
	a.HandleKeys(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d", listRec.Code)
	}
	body := listRec.Body.String()
	if strings.Contains(body, rotated.Key) || strings.Contains(body, created.Key) || strings.Contains(body, "key_full") {
		t.Fatalf("list leaked the secret: %s", body)
	}
	if !strings.Contains(body, rotated.KeySuffix) {
		t.Fatalf("list does not report the new suffix: %s", body)
	}
}

// TestHandleKeyByIDRejectsUnknownAction pins that the trailing action segment
// is parsed rather than ignored: only reset-usage and rotate dispatch, an
// unknown suffix leaves an unparsable id behind (400), and a bare POST without
// any action is refused outright (405) instead of silently rotating the key.
func TestHandleKeyByIDRejectsUnknownAction(t *testing.T) {
	s, mini := newTestStore(t, "api-keys-action:")
	defer mini.Close()
	defer s.Close()
	a := New(s, "admin", "pass", &config.Config{})

	createReq := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"client"}`))
	createRec := httptest.NewRecorder()
	a.HandleKeys(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRec.Code, createRec.Body.String())
	}
	var created CreateKeyResponse
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{fmt.Sprintf("/api/keys/%d/nonsense", created.ID), http.StatusBadRequest},
		{fmt.Sprintf("/api/keys/%d", created.ID), http.StatusMethodNotAllowed},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		rec := httptest.NewRecorder()
		a.HandleKeyByID(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("POST %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
	}

	// Neither refused request may have replaced the secret.
	if _, err := s.AuthorizeApiKey(context.Background(), created.Key); err != nil {
		t.Fatalf("refused requests rotated the key anyway: %v", err)
	}
}
