package main

import (
	"slices"
	"testing"

	"orchids-api/internal/grok"
	"orchids-api/internal/store"
)

func TestNormalizeGrokSSOToken(t *testing.T) {
	raw := "foo=1; sso=abc123; sso-rw=abc123"
	if got := grok.NormalizeSSOToken(raw); got != "abc123" {
		t.Fatalf("NormalizeSSOToken()=%q want abc123", got)
	}
}

func TestExtractGrokModelIDsFromText(t *testing.T) {
	text := `models: grok-4.2, "grok-5", "grok-4.20-beta", and alias grok-4-2`
	ids := extractGrokModelIDsFromText(text)
	if slices.Contains(ids, "grok-4.2") {
		t.Fatalf("grok-4.2 should be filtered out: %+v", ids)
	}
	if !slices.Contains(ids, "grok-5") {
		t.Fatalf("expected grok-5 in ids: %+v", ids)
	}
	if !slices.Contains(ids, "grok-4.20-beta") {
		t.Fatalf("expected grok-4.20-beta in ids: %+v", ids)
	}
}

func TestBuildGrokVersionProbes(t *testing.T) {
	models := []*store.Model{
		{Channel: "Grok", ModelID: "grok-3"},
		{Channel: "Grok", ModelID: "grok-4"},
		{Channel: "Grok", ModelID: "grok-4.1-fast"},
	}
	probes := buildGrokVersionProbes(models)
	if slices.Contains(probes, "grok-4.2") {
		t.Fatalf("grok-4.2 should be filtered out from probes, got %+v", probes)
	}
	if !slices.Contains(probes, "grok-5") {
		t.Fatalf("expected grok-5 probe, got %+v", probes)
	}
}

func TestIsOrchidsAccountType(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty defaults to orchids", in: "", want: true},
		{name: "orchids", in: "orchids", want: true},
		{name: "orchids uppercase", in: "ORCHIDS", want: true},
		{name: "warp", in: "warp", want: false},
		{name: "grok", in: "grok", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOrchidsAccountType(tt.in); got != tt.want {
				t.Fatalf("isOrchidsAccountType(%q)=%v want=%v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLimitProbeModelIDs(t *testing.T) {
	items := []string{"a", "b", "c", "d"}

	limited, didLimit := limitProbeModelIDs(items, 2)
	if !didLimit {
		t.Fatalf("expected didLimit=true")
	}
	if len(limited) != 2 {
		t.Fatalf("unexpected limited result: %+v", limited)
	}
	for _, id := range limited {
		if !slices.Contains(items, id) {
			t.Fatalf("unexpected id %q in limited result: %+v", id, limited)
		}
	}

	all, didLimitAll := limitProbeModelIDs(items, 10)
	if didLimitAll {
		t.Fatalf("expected didLimit=false")
	}
	if len(all) != len(items) {
		t.Fatalf("expected no truncation, got %+v", all)
	}
}

func TestProbeModelWindow(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	got := probeModelWindow(items, 3, 4)
	want := []string{"e", "a", "b"}
	if !slices.Equal(got, want) {
		t.Fatalf("probeModelWindow()=%+v want %+v", got, want)
	}
}
