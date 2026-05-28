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
	text := `models: grok-4.2, "grok-5", "grok-420", and alias grok-4-2`
	ids := extractGrokModelIDsFromText(text)
	if slices.Contains(ids, "grok-4.2") {
		t.Fatalf("grok-4.2 should be filtered out: %+v", ids)
	}
	if !slices.Contains(ids, "grok-5") {
		t.Fatalf("expected grok-5 in ids: %+v", ids)
	}
	if slices.Contains(ids, "grok-420") {
		t.Fatalf("grok-420 should be filtered out: %+v", ids)
	}
}

func TestExtractGrokModelIDsFromText_FiltersDocsSlugs(t *testing.T) {
	text := `grok-code-fast-1 grok-code-prompt-engineering grok-business grok-client grok-conv-id grok-imagine-image grok-4-1-fast-reasoning`
	ids := extractGrokModelIDsFromText(text)
	if !slices.Contains(ids, "grok-code-fast-1") {
		t.Fatalf("expected grok-code-fast-1 in ids: %+v", ids)
	}
	for _, blocked := range []string{
		"grok-code-prompt-engineering",
		"grok-business",
		"grok-client",
		"grok-conv-id",
		"grok-imagine-image",
		"grok-4.1-fast-reasoning",
	} {
		if slices.Contains(ids, blocked) {
			t.Fatalf("%s should be filtered out: %+v", blocked, ids)
		}
	}
}

func TestExtractGrokModelIDsFromText_FiltersVolatileVariants(t *testing.T) {
	text := `grok-3-fast-beta grok-3-fast-latest grok-4.20-0309 grok-4.20-0309-reasoning grok-4.20-0309-non-reasoning grok-4.0709 grok-2.1212 grok-5 grok-5-fast`
	ids := extractGrokModelIDsFromText(text)
	for _, wanted := range []string{"grok-5", "grok-5-fast", "grok-4.20-0309-reasoning", "grok-4.20-0309-non-reasoning"} {
		if !slices.Contains(ids, wanted) {
			t.Fatalf("expected %s in ids: %+v", wanted, ids)
		}
	}
	for _, blocked := range []string{
		"grok-3-fast-beta",
		"grok-3-fast-latest",
		"grok-4.20-0309",
		"grok-4.0709",
		"grok-2.1212",
	} {
		if slices.Contains(ids, blocked) {
			t.Fatalf("%s should be filtered out: %+v", blocked, ids)
		}
	}
}

func TestNormalizeGrokPublicSourceText_UnescapesCommonSequences(t *testing.T) {
	got := normalizeGrokPublicSourceText(`grok-4\.1-fast grok\u002d5`)
	if got != "grok-4.1-fast grok-5" {
		t.Fatalf("normalizeGrokPublicSourceText()=%q want %q", got, "grok-4.1-fast grok-5")
	}
}

func TestBuildGrokVersionProbes(t *testing.T) {
	models := []*store.Model{
		{Channel: "Grok", ModelID: "grok-3"},
		{Channel: "Grok", ModelID: "grok-4"},
		{Channel: "Grok", ModelID: "grok-4.1-fast"},
		{Channel: "Grok", ModelID: "grok-420"},
	}
	probes := buildGrokVersionProbes(models)
	if slices.Contains(probes, "grok-4.2") {
		t.Fatalf("grok-4.2 should be filtered out from probes, got %+v", probes)
	}
	if slices.Contains(probes, "grok-420.1") || slices.Contains(probes, "grok-421") {
		t.Fatalf("marketing model grok-420 should not generate numeric probes, got %+v", probes)
	}
	if !slices.Contains(probes, "grok-5") {
		t.Fatalf("expected grok-5 probe, got %+v", probes)
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
