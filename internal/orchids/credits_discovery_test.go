package orchids

import (
	"context"
	"testing"
	"time"
)

func TestExtractCreditsActionIDFromJS(t *testing.T) {
	js := `var n=r(30926);let a=(0,n.createServerReference)("7f876a6c1a4d682f106ecd67a0f77f2aa2d035257c",n.callServer,void 0,n.findSourceMapURL,"getUserProfile")`
	id := extractCreditsActionIDFromJS(js)
	if id != "7f876a6c1a4d682f106ecd67a0f77f2aa2d035257c" {
		t.Fatalf("unexpected id: %q", id)
	}
}

func TestGetCreditsActionID_Cache(t *testing.T) {
	creditsActionIDMu.Lock()
	oldID := cachedCreditsActionID
	oldExp := cachedCreditsActionExp
	creditsActionIDMu.Unlock()
	defer func() {
		creditsActionIDMu.Lock()
		cachedCreditsActionID = oldID
		cachedCreditsActionExp = oldExp
		creditsActionIDMu.Unlock()
	}()

	creditsActionIDMu.Lock()
	cachedCreditsActionID = "cached"
	cachedCreditsActionExp = time.Now().Add(1 * time.Hour)
	creditsActionIDMu.Unlock()

	id, err := getCreditsActionID(context.Background(), false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "cached" {
		t.Fatalf("expected cached id, got %q", id)
	}
}

func TestParseJWTIAT(t *testing.T) {
	// header: {"alg":"none"} => eyJhbGciOiJub25lIn0
	// payload: {"iat":1700000000} => eyJpYXQiOjE3MDAwMDAwMDB9
	jwt := "eyJhbGciOiJub25lIn0.eyJpYXQiOjE3MDAwMDAwMDB9."
	if got := parseJWTIAT(jwt); got != 1700000000 {
		t.Fatalf("unexpected iat: %d", got)
	}
}

func TestParseJWTExp(t *testing.T) {
	// header: {"alg":"none"} => eyJhbGciOiJub25lIn0
	// payload: {"exp":1700000100} => eyJleHAiOjE3MDAwMDAxMDB9
	jwt := "eyJhbGciOiJub25lIn0.eyJleHAiOjE3MDAwMDAxMDB9."
	if got := parseJWTExp(jwt); got != 1700000100 {
		t.Fatalf("unexpected exp: %d", got)
	}
}

func TestIsCreditsSoftError(t *testing.T) {
	err := &creditsSoftError{ActionID: "action-1", Reason: "known-digest-2110345482"}
	if !IsCreditsSoftError(err) {
		t.Fatalf("expected soft error")
	}
}

func TestKnownDigestDetection(t *testing.T) {
	err := &creditsRequestError{StatusCode: 500, BodyPreview: `1:E{"digest":"2110345482"}`}
	if !isKnownDigest2110345482(err) {
		t.Fatalf("expected known digest detection")
	}
}
