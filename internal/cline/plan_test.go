package cline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

// planClient stands up a control plane that answers /users/me/plan with the
// given status and body, so the tier logic is tested against the shapes the
// upstream actually returns: a plan row for a subscriber, and the no-history
// sentence for an account that never subscribed.
func planClient(t *testing.T, status int, body string, capture *http.Header) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			*capture = r.Header.Clone()
		}
		if r.URL.Path != "/users/me/plan" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	return &Client{
		apiBase: server.URL,
		control: server.Client(),
		stream:  server.Client(),
		creds:   Credentials{AccessToken: "access-1", ExpiresAt: time.Now().Add(time.Hour)},
	}
}

// TestPlanReadsASubscriberNamesThePlan proves a subscriber's row becomes the
// tier rather than being flattened to "free". The catalog cannot answer this:
// it publishes the free list to every account, subscriber included.
func TestPlanReadsASubscriberNamesThePlan(t *testing.T) {
	client := planClient(t, http.StatusOK, `{"data":{"displayName":"Cline Pass (Monthly)"},"success":true}`, nil)
	plan, err := client.FetchPlan(context.Background())
	if err != nil {
		t.Fatalf("FetchPlan() error = %v", err)
	}
	if !plan.Explicit {
		t.Error("Explicit = false for a plan row the upstream returned")
	}
	if plan.Name != "Cline Pass (Monthly)" {
		t.Errorf("Name = %q, want the plan name", plan.Name)
	}
}

// TestPlanNoHistoryMeansFree pins the free verdict on the sentence the upstream
// actually sends. It is the answer observed for the live account in this
// deployment, and it is corroborated rather than assumed: a pass-tier model
// answered 403 ENTITLEMENT_ERROR for that account while every free-tier model
// answered 200.
func TestPlanNoHistoryMeansFree(t *testing.T) {
	client := planClient(t, http.StatusNotFound, `{"data":null,"error":"no plan history found for user","success":false}`, nil)
	plan, err := client.FetchPlan(context.Background())
	if err != nil {
		t.Fatalf("FetchPlan() error = %v", err)
	}
	if !plan.Explicit || plan.Name != "free" {
		t.Errorf("Plan = %+v, want {Name: free, Explicit: true}", plan)
	}
}

// TestPlanNeverInventsATier is the guard the whole file exists for. An endpoint
// that is down, or that answers something unrecognised, must leave the tier
// unknown: defaulting it to "free" would label a subscriber's account as free
// and hide the fact that the gateway never actually asked.
func TestPlanNeverInventsATier(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"upstream down":     {http.StatusBadGateway, `{"error":"upstream down"}`},
		"unrecognised 404":  {http.StatusNotFound, `{"error":"Not Found","success":false}`},
		"200 without a row": {http.StatusOK, `{"data":{},"success":true}`},
		"unparseable body":  {http.StatusOK, `not json`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := planClient(t, tc.status, tc.body, nil)
			plan, err := client.FetchPlan(context.Background())
			if err == nil {
				t.Fatalf("FetchPlan() = %+v, want an error so no tier is recorded", plan)
			}
			if plan.Explicit || plan.Name != "" {
				t.Errorf("Plan = %+v, want it empty", plan)
			}
		})
	}
}

// TestPlanRefusedCredentialIsNotAFreeVerdict keeps the tier read from being
// mistaken for a credential verdict. A 401 means the token is gone and the
// operator has to re-authorize; recording "free" there would bury it.
func TestPlanRefusedCredentialIsNotAFreeVerdict(t *testing.T) {
	client := planClient(t, http.StatusUnauthorized, `{"error":"unauthorized"}`, nil)
	plan, err := client.FetchPlan(context.Background())
	if err == nil {
		t.Fatalf("FetchPlan() = %+v, want an error", plan)
	}
	if plan.Name != "" {
		t.Errorf("Name = %q, want it empty", plan.Name)
	}
}

// TestClineAccountCarriesThePlanTier pins the storage contract the console
// reads: the tier lives in its own field so "never probed" and "probed and
// free" stay distinguishable.
func TestClineAccountCarriesThePlanTier(t *testing.T) {
	acc := &store.Account{ID: 7, AccountType: "cline"}
	if acc.ClinePlan != "" {
		t.Errorf("ClinePlan = %q, want empty before a probe", acc.ClinePlan)
	}
	acc.ClinePlan = "free"
	if acc.ClinePlan != "free" {
		t.Errorf("ClinePlan = %q, want free", acc.ClinePlan)
	}
}

// TestPlanRequestSendsTheProductIdentity proves the tier read goes out with the
// same identity block chat needs: without it the upstream answers 403 for a
// product-surface reason that has nothing to do with the credential.
func TestPlanRequestSendsTheProductIdentity(t *testing.T) {
	var seen http.Header
	client := planClient(t, http.StatusOK, `{"data":{"displayName":"Cline Pass"},"success":true}`, &seen)
	if _, err := client.FetchPlan(context.Background()); err != nil {
		t.Fatalf("FetchPlan() error = %v", err)
	}
	for _, name := range []string{"X-CLIENT-TYPE", "X-CORE-VERSION", "User-Agent"} {
		if got := seen.Get(name); got != defaultClientHeaders[name] {
			t.Errorf("header %s = %q, want %q", name, got, defaultClientHeaders[name])
		}
	}
	if got := seen.Get("Authorization"); !strings.HasPrefix(got, "Bearer workos:") {
		t.Errorf("Authorization = %q, want the workos bearer prefix", got)
	}
}
