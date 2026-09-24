package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestStoredVideoJobLifecycleAndIsolation(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "videos-test:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	record := &StoredVideoJob{
		ID: "video_123", OwnerHash: "owner-a", Model: "grok-imagine-video",
		Operation: "generate", Status: "completed", Progress: 100,
		ContentPath: "data/tmp/video/result.mp4",
		CreatedAt:   time.Now().Unix(), CompletedAt: time.Now().Unix(), StandardAPI: true,
	}
	if err := s.SaveStoredVideoJob(ctx, record, time.Hour); err != nil {
		t.Fatalf("SaveStoredVideoJob() error = %v", err)
	}
	got, err := s.GetStoredVideoJob(ctx, record.ID, record.OwnerHash)
	if err != nil || got.Status != "completed" || got.ContentPath != record.ContentPath || !got.StandardAPI || got.ExpiresAt.IsZero() {
		t.Fatalf("GetStoredVideoJob() = %#v, %v", got, err)
	}
	if _, err := s.GetStoredVideoJob(ctx, record.ID, "owner-b"); !errors.Is(err, ErrNoRows) {
		t.Fatalf("cross-owner lookup error = %v, want ErrNoRows", err)
	}
	listed, err := s.ListStoredVideoJobs(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != record.ID {
		t.Fatalf("ListStoredVideoJobs() = %#v, %v", listed, err)
	}
	if err := s.DeleteStoredVideoJob(ctx, record.ID, record.OwnerHash); err != nil {
		t.Fatalf("DeleteStoredVideoJob() error = %v", err)
	}
	if _, err := s.GetStoredVideoJob(ctx, record.ID, record.OwnerHash); !errors.Is(err, ErrNoRows) {
		t.Fatalf("deleted lookup error = %v, want ErrNoRows", err)
	}
	listed, err = s.ListStoredVideoJobs(ctx)
	if err != nil || len(listed) != 0 {
		t.Fatalf("deleted index entries = %#v, %v", listed, err)
	}
}

func TestStoredVideoJobExpires(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "videos-expiry:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SaveStoredVideoJob(context.Background(), &StoredVideoJob{
		ID: "video_expiring", OwnerHash: "owner", Model: "grok-imagine-video", Status: "queued", CreatedAt: time.Now().Unix(),
	}, time.Second); err != nil {
		t.Fatal(err)
	}
	mini.FastForward(2 * time.Second)
	if _, err := s.GetStoredVideoJob(context.Background(), "video_expiring", "owner"); !errors.Is(err, ErrNoRows) {
		t.Fatalf("expired lookup error = %v, want ErrNoRows", err)
	}
	listed, err := s.ListStoredVideoJobs(context.Background())
	if err != nil || len(listed) != 0 {
		t.Fatalf("expired index entries = %#v, %v", listed, err)
	}
}

func TestVideoJobLeaseMutualExclusionAndTakeover(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "videos-lease:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	const id, owner = "video_lease", "owner"
	acquired, err := s.AcquireVideoJobLease(ctx, id, owner, "instance-a", 10*time.Second)
	if err != nil || !acquired {
		t.Fatalf("first acquire = %v, %v", acquired, err)
	}
	if acquired, err := s.AcquireVideoJobLease(ctx, id, owner, "instance-b", 10*time.Second); err != nil || acquired {
		t.Fatalf("competing acquire = %v, %v", acquired, err)
	}
	if refreshed, err := s.RefreshVideoJobLease(ctx, id, owner, "instance-b", 10*time.Second); err != nil || refreshed {
		t.Fatalf("foreign refresh = %v, %v", refreshed, err)
	}
	if refreshed, err := s.RefreshVideoJobLease(ctx, id, owner, "instance-a", 10*time.Second); err != nil || !refreshed {
		t.Fatalf("owner refresh = %v, %v", refreshed, err)
	}
	if released, err := s.ReleaseVideoJobLease(ctx, id, owner, "instance-b"); err != nil || released {
		t.Fatalf("foreign release = %v, %v", released, err)
	}
	mini.FastForward(11 * time.Second)
	if acquired, err := s.AcquireVideoJobLease(ctx, id, owner, "instance-b", 10*time.Second); err != nil || !acquired {
		t.Fatalf("takeover after expiry = %v, %v", acquired, err)
	}
	if released, err := s.ReleaseVideoJobLease(ctx, id, owner, "instance-b"); err != nil || !released {
		t.Fatalf("owner release = %v, %v", released, err)
	}
}
