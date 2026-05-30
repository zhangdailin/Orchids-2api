package grok

import "testing"

func TestNormalizeGeneratedImageURLsPrefersGrokAsset(t *testing.T) {
	in := []string{
		"https://example.com/traffic.jpg",
		"https://assets.grok.com/users/u/generated/a/image.jpg",
	}
	got := normalizeGeneratedImageURLs(in, 1)
	if len(got) != 1 {
		t.Fatalf("len=%d want=1", len(got))
	}
	if got[0] != "https://assets.grok.com/users/u/generated/a/image.jpg" {
		t.Fatalf("got=%q want grok asset url", got[0])
	}
}

func TestNormalizeGeneratedImageURLsPrefersFullOverPart(t *testing.T) {
	in := []string{
		"https://assets.grok.com/users/u/generated/a-part-0/image.jpg",
		"https://assets.grok.com/users/u/generated/a/image.jpg",
	}
	got := normalizeGeneratedImageURLs(in, 0)
	if len(got) != 1 {
		t.Fatalf("len=%d want=1", len(got))
	}
	if got[0] != "https://assets.grok.com/users/u/generated/a/image.jpg" {
		t.Fatalf("got=%q want full url", got[0])
	}
}

func TestAppendImageCandidatesPrefersGrokPath(t *testing.T) {
	debugHTTP := []string{"https://example.com/other.jpg"}
	debugAsset := []string{"users/u/generated/a/image.jpg"}
	got := appendImageCandidates(nil, debugHTTP, debugAsset, 1)
	if len(got) != 1 {
		t.Fatalf("len=%d want=1", len(got))
	}
	if got[0] != "https://assets.grok.com/users/u/generated/a/image.jpg" {
		t.Fatalf("got=%q want grok asset url", got[0])
	}
}

func TestAppendImageCandidatesExtractsAssetPathInsideText(t *testing.T) {
	debugAsset := []string{`{"message":"rendered users/u-1/generated/a2/image.png for preview"}`}
	got := appendImageCandidates(nil, nil, debugAsset, 1)
	if len(got) != 1 {
		t.Fatalf("len=%d want=1 got=%#v", len(got), got)
	}
	if got[0] != "https://assets.grok.com/users/u-1/generated/a2/image.png" {
		t.Fatalf("got=%q want grok asset url", got[0])
	}
}

func TestAppendImageCandidatesExtractsJSONArrayText(t *testing.T) {
	debugAsset := []string{`["https://assets.grok.com/users/u-1/generated/a3/image.webp"]`}
	got := appendImageCandidates(nil, nil, debugAsset, 1)
	if len(got) != 1 {
		t.Fatalf("len=%d want=1 got=%#v", len(got), got)
	}
	if got[0] != "https://assets.grok.com/users/u-1/generated/a3/image.webp" {
		t.Fatalf("got=%q want grok asset url", got[0])
	}
}

func TestAppendImageResultURLsAcceptsAssetIDContentURL(t *testing.T) {
	resp := map[string]interface{}{
		"userResponse": map[string]interface{}{
			"fileAttachments": []interface{}{"asset-123"},
		},
	}

	got := normalizeGeneratedImageURLs(appendImageResultURLs(nil, resp), 1)
	if len(got) != 1 {
		t.Fatalf("len=%d want=1 got=%#v", len(got), got)
	}
	if got[0] != "https://assets.grok.com/asset-123/content" {
		t.Fatalf("got=%q want content asset url", got[0])
	}
}

func TestNormalizeGeneratedImageURLsRejectsEmptyExtensionPlaceholder(t *testing.T) {
	got := normalizeGeneratedImageURLs([]string{"https://assets.grok.com/.png"}, 1)
	if len(got) != 0 {
		t.Fatalf("got=%#v want no placeholder urls", got)
	}
}
