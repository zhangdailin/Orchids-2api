package qoder

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchModelsLenientFallsBackAndReportsWhy proves the login-time contract:
// a catalog read that fails yields the built-in list *and* the reason, so the
// caller can install a usable catalog without pretending it was observed.
func TestFetchModelsLenientFallsBackAndReportsWhy(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	catalog, err := client.FetchModelsLenient(context.Background())
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("error = %v, want ErrCatalogUnavailable", err)
	}
	if catalog == nil || catalog.Len() == 0 {
		t.Fatal("no fallback catalog was returned")
	}
	if _, resolveErr := catalog.Resolve("Qwen3.7-Max"); resolveErr != nil {
		t.Fatalf("the fallback catalog cannot resolve its own models: %v", resolveErr)
	}
}

// TestFetchModelsLeavesRefreshStrict proves the lenient path did not weaken model
// refresh: a refresh that cannot read the upstream catalog must still fail
// instead of installing a stale list.
func TestFetchModelsLeavesRefreshStrict(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	catalog, err := client.FetchModels(context.Background())
	if err == nil {
		t.Fatal("FetchModels() error = nil for a rejected read")
	}
	if catalog != nil {
		t.Fatal("FetchModels() returned a catalog for a rejected read")
	}
}

// TestParseCatalogAcceptsWrappedScene proves a deployment that nests the scene
// under an envelope does not present as a broken credential.
func TestParseCatalogAcceptsWrappedScene(t *testing.T) {
	t.Parallel()

	top := []byte(`{"chat":[{"key":"qmodel_latest","display_name":"Qwen3.7-Max","enable":true}]}`)
	wrappedInData := []byte(`{"data":{"chat":[{"key":"qmodel_latest","display_name":"Qwen3.7-Max","enable":true}]}}`)
	wrappedInResult := []byte(`{"result":{"chat":[{"key":"qmodel_latest","display_name":"Qwen3.7-Max","enable":true}]}}`)

	for name, raw := range map[string][]byte{"top level": top, "data": wrappedInData, "result": wrappedInResult} {
		catalog, err := parseCatalog(raw)
		if err != nil {
			t.Fatalf("%s: parseCatalog() error = %v", name, err)
		}
		if entry, err := catalog.Resolve("Qwen3.7-Max"); err != nil || entry.Key != "qmodel_latest" {
			t.Fatalf("%s: Resolve() = %+v, %v", name, entry, err)
		}
	}
}

// TestParseCatalogRejectsAFailedEnvelope proves a 200 envelope carrying a
// business failure is not read as an empty-but-valid catalog.
func TestParseCatalogRejectsAFailedEnvelope(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"statusCodeValue":500,"body":"{\"message\":\"boom\"}"}`)
	if _, err := parseCatalog(raw); err == nil {
		t.Fatal("parseCatalog() error = nil for a failed envelope")
	}
}

// TestParseCatalogRejectsMalformedScene proves a scene of the wrong shape is an
// error, not an empty catalog.
func TestParseCatalogRejectsMalformedScene(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"chat":[]}`,
		`{"chat":{}}`,
		`{"chat":"nope"}`,
		`[]`,
		`{"nothing":"here"}`,
	} {
		if _, err := parseCatalog([]byte(raw)); err == nil {
			t.Errorf("parseCatalog(%s) error = nil, want a failure", raw)
		}
	}
}

// TestCatalogRequestIsAJSONReadWithTheSignedHeaders pins the catalog request
// contract: a GET in the private encoding era (empty body), signed, and asking
// for JSON rather than a stream.
func TestCatalogRequestIsAJSONReadWithTheSignedHeaders(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath, gotAccept, gotAuth string
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")
		gotAuth = r.Header.Get("Authorization")
		raw := make([]byte, 1<<16)
		n, _ := r.Body.Read(raw)
		gotBody = string(raw[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":[{"key":"dfmodel","display_name":"DeepSeek-V4-Flash","enable":true}]}`))
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	client.SetEndpointsForTest(server.URL, server.URL, server.URL, server.URL)

	if _, err := client.FetchModels(context.Background()); err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %s, want GET", gotMethod)
	}
	if gotPath != "/algo/api/v2/model/list" {
		t.Errorf("path = %q, want /algo/api/v2/model/list", gotPath)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
	if !strings.HasPrefix(gotAuth, "Bearer COSY.") {
		t.Errorf("Authorization = %q, want a COSY bearer", gotAuth)
	}
	if gotBody != "" {
		t.Errorf("body = %q, want the empty body the signature covers", gotBody)
	}
}
