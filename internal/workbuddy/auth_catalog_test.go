package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/store"
)

// fetchCatalogModels runs FetchModels against a stubbed /v3/config body.
func fetchCatalogModels(t *testing.T, body string) ([]string, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client := NewFromAccount(&store.Account{}, nil)
	client.baseURL = srv.URL
	client.httpClient = srv.Client()
	client.creds = Credentials{AccessToken: "access", UID: "uid"}
	models, err := client.FetchModels(context.Background())
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids, err
}

// TestFetchModels_WhitelistMismatchDoesNotDarkenTheChannel is the regression test
// for the whole channel going unusable.
//
// The whitelist and the model list are two upstream lists keyed by id. When they
// stop sharing an identifier — a rename on either side — the intersection is
// empty while the catalog is not. That used to be reported as "no cli models",
// which failed the channel's entire model refresh: no WorkBuddy model could be
// published, so no request could resolve to the channel, and every model of the
// channel failed for a reason that looked like an upstream outage.
func TestFetchModels_WhitelistMismatchDoesNotDarkenTheChannel(t *testing.T) {
	got, err := fetchCatalogModels(t, `{"code":0,"data":{
		"models":[{"id":"hy3","disabled":false},{"id":"hy3-pro","disabled":false},{"id":"dead","disabled":true}],
		"agents":[{"name":"cli","models":["completely-different-id"]}]
	}}`)
	if err != nil {
		t.Fatalf("a whitelist that matches nothing must not fail the read: %v", err)
	}
	// The advertised, enabled list; the disabled entry stays out.
	if strings.Join(got, ",") != "hy3,hy3-pro" {
		t.Fatalf("models = %v, want the advertised enabled list", got)
	}
}

// TestFetchModels_EmptyCatalogIsStillAnError keeps the distinction the fix rests
// on: an account that advertises nothing is a real failure, not a filter
// artifact, and must not be reported as one.
func TestFetchModels_EmptyCatalogIsStillAnError(t *testing.T) {
	_, err := fetchCatalogModels(t, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":["x"]}]}}`)
	if err == nil || !strings.Contains(err.Error(), "advertised no enabled models") {
		t.Fatalf("error = %v, want a report that the upstream advertised nothing", err)
	}
	_, err = fetchCatalogModels(t, `{"code":0,"data":{"models":[{"id":"only","disabled":true}],"agents":[]}}`)
	if err == nil {
		t.Fatal("an all-disabled catalog must still be an error")
	}
}

// TestFetchModels_WhitelistStillFilters pins the restriction the whitelist is for:
// when it does intersect, only its models are returned.
func TestFetchModels_WhitelistStillFilters(t *testing.T) {
	got, err := fetchCatalogModels(t, `{"code":0,"data":{
		"models":[{"id":"a"},{"id":"b"},{"id":"c"}],
		"agents":[{"name":"cli","models":["b"]}]
	}}`)
	if err != nil {
		t.Fatalf("FetchModels() error = %v", err)
	}
	if strings.Join(got, ",") != "b" {
		t.Fatalf("models = %v, want only the whitelisted model", got)
	}
}

// TestFetchModels_NoCLIRestrictionServesTheCatalog preserves the case that already
// worked: no cli agent at all means no restriction to apply.
func TestFetchModels_NoCLIRestrictionServesTheCatalog(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"data":{"models":[{"id":"a"},{"id":"b"}],"agents":[{"name":"web","models":["a"]}]}}`,
		`{"code":0,"data":{"models":[{"id":"a"},{"id":"b"}],"agents":[{"name":"cli","models":[]}]}}`,
		`{"code":0,"data":{"models":[{"id":"a"},{"id":"b"}]}}`,
	} {
		got, err := fetchCatalogModels(t, body)
		if err != nil {
			t.Fatalf("body %s: FetchModels() error = %v", body, err)
		}
		if strings.Join(got, ",") != "a,b" {
			t.Fatalf("body %s: models = %v, want the whole catalog", body, got)
		}
	}
}
