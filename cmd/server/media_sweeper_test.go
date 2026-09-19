package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The sweeper must reclaim expired media input files and nothing else: generated
// images/videos share the same directories.
func TestMediaInputSweeperOnlyReclaimsNamespacedFiles(t *testing.T) {
	dir := t.TempDir()
	imageDir := filepath.Join(dir, "image")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	expiredInput := filepath.Join(imageDir, mediaInputFilePrefix+strings.Repeat("a", 40)+".png")
	generatedAsset := filepath.Join(imageDir, strings.Repeat("b", 40)+".png")
	unrelated := filepath.Join(imageDir, "note.txt")
	for _, path := range []string{expiredInput, generatedAsset, unrelated} {
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	sweepMediaInputFiles(context.Background(), nil, dir)
	if _, err := os.Stat(expiredInput); !os.IsNotExist(err) {
		t.Fatal("an expired media input file must be reclaimed")
	}
	for _, path := range []string{generatedAsset, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweeper deleted a file it does not own: %s", path)
		}
	}
}

func TestMediaInputSweeperKeepsFreshFiles(t *testing.T) {
	dir := t.TempDir()
	imageDir := filepath.Join(dir, "image")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(imageDir, mediaInputFilePrefix+strings.Repeat("c", 40)+".png")
	if err := os.WriteFile(fresh, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sweepMediaInputFiles(context.Background(), nil, dir)
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a media input still inside its TTL must survive")
	}
}
