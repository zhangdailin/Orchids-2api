package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"orchids-api/internal/store"
)

// mediaInputSweepEvery is how often the gateway reconciles cached media input
// files with the store records that own them.
const mediaInputSweepEvery = 1 * time.Hour

// mediaInputSweepTTL mirrors the media input record TTL in the grok package
// (24h). A file younger than this may still have a live record, so it is kept.
const mediaInputSweepTTL = 24 * time.Hour

// startMediaInputSweeper deletes cached media input files whose store record has
// expired.
//
// A media input is written to disk and its metadata to Redis with a 24h TTL.
// When the Redis entry expires the file stays on disk forever, so repeated
// uploads fill the volume even though no record can reach the bytes any more
// (grok2api runs an equivalent cleanup over objects and metadata together).
func startMediaInputSweeper(ctx context.Context, s *store.Store, cacheBaseDir string) {
	base := strings.TrimSpace(cacheBaseDir)
	if s == nil || base == "" {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("Panic in media input sweeper", "error", r)
			}
		}()
		ticker := time.NewTicker(mediaInputSweepEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepMediaInputFiles(ctx, s, base)
			}
		}
	}()
}

// sweepMediaInputFiles removes expired media input files.
//
// It walks only the image and video cache directories and only considers the
// namespaced media input files, so it can never delete generated media (a
// completed video job keeps pointing at its cached copy) or a user's own file.
func sweepMediaInputFiles(ctx context.Context, s *store.Store, base string) {
	for _, kind := range []string{"image", "video"} {
		dir := filepath.Join(base, kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !looksLikeMediaInputName(name) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			// Only reclaim files older than the media input TTL: a fresh upload
			// whose record has not been read yet must survive.
			if time.Since(info.ModTime()) < mediaInputSweepTTL {
				continue
			}
			if err := os.Remove(filepath.Join(dir, name)); err == nil {
				slog.Debug("Media input sweep: removed an orphaned cache file", "kind", kind, "name", name)
			}
		}
	}
}

// mediaInputFilePrefix namespaces uploaded media inputs in the shared cache
// directories. Only these files are reclaimable: generated images and videos
// live in the same directories, and a completed video job keeps pointing at its
// cached copy, so a sweep must never touch anything else.
const mediaInputFilePrefix = "input-"

// looksLikeMediaInputName reports whether a filename is a namespaced media input
// written by the grok cache writer (prefix + content hash + extension).
func looksLikeMediaInputName(name string) bool {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || strings.Contains(trimmed, "..") || strings.ContainsAny(trimmed, `/\`) {
		return false
	}
	stem := strings.TrimPrefix(trimmed, mediaInputFilePrefix)
	if stem == trimmed {
		// No prefix: not a media input, never reclaim it.
		return false
	}
	ext := strings.ToLower(filepath.Ext(stem))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".mp4", ".webm", ".mov", ".bin", ".wav", ".mp3":
	default:
		return false
	}
	hash := strings.TrimSuffix(stem, filepath.Ext(stem))
	return len(hash) >= 32 && isHex(hash)
}

func isHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
