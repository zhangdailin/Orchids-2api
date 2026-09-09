package grok

import (
	"context"
	"testing"
	"time"
)

func TestBuildChatSummaryBoundary(t *testing.T) {
	for _, operation := range []string{"", "messages", "responses"} {
		for _, build := range []bool{false, true} {
			for _, effort := range []string{"", "low", "xhigh", "none"} {
				req := &ChatCompletionsRequest{Model: "grok-4.6", sourceOperation: operation, Messages: []ChatMessage{{Role: "user", Content: "hello"}}}
				if effort != "" {
					req.ReasoningEffort = &effort
				}
				payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.6", ConsoleModel: "grok-4.6"}, req, build)
				if err != nil {
					t.Fatal(err)
				}
				r, _ := payload["reasoning"].(map[string]interface{})
				if (r["summary"] == "auto") != (build && operation == "" && effort != "none") {
					t.Fatalf("build=%v operation=%q reasoning=%v", build, operation, r)
				}
				if effort == "" && r["effort"] != nil || effort != "" && r["effort"] != effort {
					t.Fatalf("changed effort: %v", r)
				}
			}
		}
	}
}

func TestReasoningDiagnosticsReachAttemptAndOutcome(t *testing.T) {
	for _, effort := range []string{"", "low", "xhigh", "private-secret"} {
		r := map[string]interface{}{"summary": "concise"}
		if effort != "" {
			r["effort"] = effort
		}
		ctx := withReasoningDiagnostics(context.Background(), map[string]interface{}{"reasoning": r})
		log := &parityAuditLog{}
		h := &Handler{auditLogger: log}
		h.auditAttempt(ctx, nil, ProviderBuild, 1, time.Now(), nil)
		h.auditChatOutcome(ctx, nil, &ChatCompletionsRequest{}, chatOutcome{Finish: "stop"})
		if len(log.events) != 2 {
			t.Fatal(log.events)
		}
		for _, event := range log.events {
			want := effort
			if effort == "private-secret" {
				want = "other"
			}
			if effort == "" {
				if _, exists := event.Metadata["reasoning_effort"]; exists {
					t.Fatal(event.Metadata)
				}
			} else if event.Metadata["reasoning_effort"] != want {
				t.Fatal(event.Metadata)
			}
			if event.Metadata["reasoning_summary"] != "concise" {
				t.Fatal(event.Metadata)
			}
		}
	}
}
