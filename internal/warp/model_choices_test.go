package warp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func TestFetchFeatureAgentModeModelChoices_NormalizesIDsAndDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/graphql/v2" {
			t.Fatalf("path=%q want /graphql/v2", got)
		}
		if got := r.URL.Query().Get("op"); got != "GetFeatureModelChoices" {
			t.Fatalf("op=%q want GetFeatureModelChoices", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if !strings.Contains(string(body), "\"operationName\":\"GetFeatureModelChoices\"") {
			t.Fatalf("request body missing operation name: %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"user": {
					"__typename": "UserOutput",
					"user": {
						"workspaces": [
							{
								"featureModelChoice": {
									"agentMode": {
										"defaultId": "gpt-5.1-medium",
										"choices": [
											{"id": "claude-4.6-sonnet", "displayName": "Claude 4.6 Sonnet"},
											{"id": "gpt-5.1-medium", "displayName": "GPT 5.1 Medium"}
										]
									}
								}
							}
						]
					}
				}
			}
		}`))
	}))
	defer server.Close()

	features, err := fetchFeatureModelChoices(context.Background(), warpRewriteClient(t, server.URL), "jwt")
	if err != nil {
		t.Fatalf("fetchFeatureModelChoices() error: %v", err)
	}
	choices := features.AgentMode.Choices
	defaultID := features.AgentMode.DefaultID
	if defaultID != "gpt-5.1-medium" {
		t.Fatalf("defaultID=%q want gpt-5.1-medium", defaultID)
	}
	gotIDs := []string{choices[0].ID, choices[1].ID}
	wantIDs := []string{"gpt-5.1-medium", "claude-4.6-sonnet"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("choice ids=%+v want %+v", gotIDs, wantIDs)
	}
}

func TestFetchFeatureAgentModeModelChoices_FiltersDisabledAndKeepsRoutingMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{
				"defaultId":"disabled-model","choices":[
					{"id":"disabled-model","displayName":"Disabled","disableReason":"OUT_OF_REQUESTS"},
					{"id":"enabled-model","displayName":"Enabled","baseModelName":"gpt-5.2","reasoningLevel":"high","disableReason":null,"visionSupported":true,"provider":"OPENAI","usageMetadata":{"creditMultiplier":1.5,"requestMultiplier":2},"hostConfigs":[{"enabled":true,"modelRoutingHost":"DIRECT_API"},{"enabled":false,"modelRoutingHost":"AWS_BEDROCK"}],"contextWindow":{"isConfigurable":true,"min":1024,"max":200000,"default":128000}}
				]
			}}}]}}}
		}`))
	}))
	defer server.Close()

	features, err := fetchFeatureModelChoices(context.Background(), warpRewriteClient(t, server.URL), "jwt")
	if err != nil {
		t.Fatalf("fetchFeatureModelChoices() error: %v", err)
	}
	if features.AgentMode.DefaultID != "enabled-model" || len(features.AgentMode.Choices) != 1 {
		t.Fatalf("unexpected enabled choices: %+v", features.AgentMode)
	}
	choice := features.AgentMode.Choices[0]
	if choice.BaseModelName != "gpt-5.2" || choice.RequestMultiplier != 2 || choice.CreditMultiplier == nil || *choice.CreditMultiplier != 1.5 {
		t.Fatalf("usage metadata was not preserved: %+v", choice)
	}
	if !choice.VisionSupported || choice.Provider != "OPENAI" || !slices.Equal(choice.EnabledHosts, []string{"DIRECT_API"}) {
		t.Fatalf("routing metadata was not preserved: %+v", choice)
	}
	if choice.ContextWindow.Max != 200000 || choice.ContextWindow.Default != 128000 {
		t.Fatalf("context metadata was not preserved: %+v", choice.ContextWindow)
	}
}

func TestFetchFeatureModelChoices_IncludesAgentSpecificDefaults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"user": {
					"__typename": "UserOutput",
					"user": {
						"workspaces": [
							{
								"featureModelChoice": {
									"agentMode": {
										"defaultId": "auto-open",
										"choices": [
											{"id": "auto-open", "displayName": "Auto Open"}
										]
									},
									"coding": {
										"defaultId": "auto",
										"choices": [
											{"id": "auto", "displayName": "Auto"}
										]
									},
									"cliAgent": {
										"defaultId": "cli-agent-team-auto",
										"choices": [
											{"id": "cli-agent-team-auto", "displayName": "Team Auto"}
										]
									},
									"computerUseAgent": {
										"defaultId": "computer-use-agent-team-auto",
										"choices": [
											{"id": "computer-use-agent-team-auto", "displayName": "Team Computer Auto"}
										]
									}
								}
							}
						]
					}
				}
			}
		}`))
	}))
	defer server.Close()

	features, err := fetchFeatureModelChoices(context.Background(), warpRewriteClient(t, server.URL), "jwt")
	if err != nil {
		t.Fatalf("fetchFeatureModelChoices() error: %v", err)
	}
	if features.AgentMode.DefaultID != "auto-open" {
		t.Fatalf("agent default=%q want auto-open", features.AgentMode.DefaultID)
	}
	if features.CliAgent.DefaultID != "cli-agent-team-auto" {
		t.Fatalf("cli agent default=%q want cli-agent-team-auto", features.CliAgent.DefaultID)
	}
	if features.ComputerUseAgent.DefaultID != "computer-use-agent-team-auto" {
		t.Fatalf("computer use default=%q want computer-use-agent-team-auto", features.ComputerUseAgent.DefaultID)
	}
	if got := AgentModeModelChoices(features); len(got) != 1 || got[0].ID != "auto-open" {
		t.Fatalf("agent mode choices=%+v want auto-open", got)
	}
}

func TestMergeWarpModelChoices_DedupesAndMovesDefaultFirst(t *testing.T) {
	merged := mergeWarpModelChoices(
		"gpt-5.1-medium",
		[]ModelChoice{
			{ID: "claude-4.6-sonnet", Name: "Claude 4.6 Sonnet"},
			{ID: "gpt-5.1-medium", Name: "GPT 5.1 Medium"},
		},
		[]ModelChoice{
			{ID: "gpt-5-1-medium", Name: "GPT 5.1 Medium"},
			{ID: "auto", Name: "Auto"},
		},
	)

	gotIDs := make([]string, 0, len(merged))
	for _, choice := range merged {
		gotIDs = append(gotIDs, choice.ID)
	}
	wantIDs := []string{"gpt-5.1-medium", "claude-4.6-sonnet", "gpt-5-1-medium", "auto-open"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("merged ids=%+v want %+v", gotIDs, wantIDs)
	}
}

func warpRewriteClient(t *testing.T, targetURL string) *http.Client {
	t.Helper()

	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatalf("parse target url: %v", err)
	}
	base := http.DefaultTransport
	return &http.Client{
		Transport: modelChoiceRoundTripper(func(req *http.Request) (*http.Response, error) {
			clone := req.Clone(req.Context())
			clone.URL.Scheme = target.Scheme
			clone.URL.Host = target.Host
			clone.Host = target.Host
			return base.RoundTrip(clone)
		}),
	}
}

type modelChoiceRoundTripper func(*http.Request) (*http.Response, error)

func (f modelChoiceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
