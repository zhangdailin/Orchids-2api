package grok

import (
	"path/filepath"
	"testing"

	"orchids-api/internal/config"
)

// TestCacheBaseDirIsTheResolvedDirectory covers the accessor the media sweeper
// depends on: it walks the tree the media inputs are written to, so it needs the
// path this package actually resolved rather than the one the operator configured.
//
// The distinction matters because the default ("data/tmp" when media_dir is unset)
// is applied inside ConfigureMediaStorage, not by the config loader — a caller that
// read cfg.MediaDir itself would get an empty string and sweep nothing.
func TestCacheBaseDirIsTheResolvedDirectory(t *testing.T) {
	original := cacheBaseDir
	t.Cleanup(func() { cacheBaseDir = original })

	configured := t.TempDir()
	if err := ConfigureMediaStorage(&config.Config{
		MediaDir:           configured,
		DeploymentReplicas: 1,
	}); err != nil {
		t.Fatalf("ConfigureMediaStorage() error = %v", err)
	}
	if got, want := CacheBaseDir(), filepath.Clean(configured); got != want {
		t.Fatalf("CacheBaseDir() = %q, want %q", got, want)
	}

	if err := ConfigureMediaStorage(&config.Config{DeploymentReplicas: 1}); err != nil {
		t.Fatalf("ConfigureMediaStorage() error = %v", err)
	}
	if got, want := CacheBaseDir(), filepath.Join("data", "tmp"); got != want {
		t.Fatalf("CacheBaseDir() = %q, want the package default %q", got, want)
	}
}
