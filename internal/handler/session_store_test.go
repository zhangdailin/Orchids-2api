package handler

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func setupRedisSessionStore(t *testing.T) (*RedisSessionStore, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	store := NewRedisSessionStore(client, "test:", 30*time.Minute)
	return store, s
}

func TestRedisSessionStoreWorkdir(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()

	// Miss
	_, ok := store.GetWorkdir(ctx, "session1")
	if ok {
		t.Fatal("expected miss")
	}

	// Set and get
	store.SetWorkdir(ctx, "session1", "/home/user")
	dir, ok := store.GetWorkdir(ctx, "session1")
	if !ok || dir != "/home/user" {
		t.Fatalf("expected /home/user, got %q (ok=%v)", dir, ok)
	}
}

func TestRedisSessionStoreConvID(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()

	// Miss
	_, ok := store.GetConvID(ctx, "session1")
	if ok {
		t.Fatal("expected miss")
	}

	// Set and get
	store.SetConvID(ctx, "session1", "conv_abc123")
	id, ok := store.GetConvID(ctx, "session1")
	if !ok || id != "conv_abc123" {
		t.Fatalf("expected conv_abc123, got %q (ok=%v)", id, ok)
	}
}

func TestRedisSessionStoreWarpTaskContext(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()

	if _, ok := store.GetWarpTaskContext(ctx, "session1"); ok {
		t.Fatal("expected task context miss")
	}
	store.SetWarpTaskContext(ctx, "session1", "encoded-task-context")
	got, ok := store.GetWarpTaskContext(ctx, "session1")
	if !ok || got != "encoded-task-context" {
		t.Fatalf("task context=%q ok=%v", got, ok)
	}
}

func TestRedisSessionStoreDelete(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()

	store.SetWorkdir(ctx, "session1", "/tmp")
	store.SetConvID(ctx, "session1", "conv_xyz")

	store.DeleteSession(ctx, "session1")

	_, ok := store.GetWorkdir(ctx, "session1")
	if ok {
		t.Fatal("workdir should be deleted")
	}
	_, ok = store.GetConvID(ctx, "session1")
	if ok {
		t.Fatal("convID should be deleted")
	}
}

func TestRedisSessionStoreTTL(t *testing.T) {
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	store := NewRedisSessionStore(client, "test:", 1*time.Second)
	ctx := context.Background()

	store.SetWorkdir(ctx, "session1", "/tmp")
	s.FastForward(2 * time.Second)

	_, ok := store.GetWorkdir(ctx, "session1")
	if ok {
		t.Fatal("session should have expired")
	}
}

func TestRedisSessionStoreTouch(t *testing.T) {
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	store := NewRedisSessionStore(client, "test:", 3*time.Second)
	ctx := context.Background()

	store.SetWorkdir(ctx, "session1", "/tmp")

	// Advance 2 seconds, then touch
	s.FastForward(2 * time.Second)
	store.Touch(ctx, "session1")

	// Advance another 2 seconds (total 4s from creation, but only 2s from touch)
	s.FastForward(2 * time.Second)

	_, ok := store.GetWorkdir(ctx, "session1")
	if !ok {
		t.Fatal("session should still exist after touch")
	}
}

// --- Memory tests ---

func TestMemorySessionStoreWorkdir(t *testing.T) {
	store := NewMemorySessionStore(30*time.Minute, 100)
	ctx := context.Background()

	_, ok := store.GetWorkdir(ctx, "s1")
	if ok {
		t.Fatal("expected miss")
	}

	store.SetWorkdir(ctx, "s1", "/home")
	dir, ok := store.GetWorkdir(ctx, "s1")
	if !ok || dir != "/home" {
		t.Fatalf("expected /home, got %q", dir)
	}
}

func TestMemorySessionStoreWarpTaskContext(t *testing.T) {
	store := NewMemorySessionStore(30*time.Minute, 100)
	ctx := context.Background()

	if _, ok := store.GetWarpTaskContext(ctx, "s1"); ok {
		t.Fatal("expected task context miss")
	}
	store.SetWarpTaskContext(ctx, "s1", "encoded-task-context")
	got, ok := store.GetWarpTaskContext(ctx, "s1")
	if !ok || got != "encoded-task-context" {
		t.Fatalf("task context=%q ok=%v", got, ok)
	}
}

func TestMemorySessionStoreCleanup(t *testing.T) {
	store := NewMemorySessionStore(100*time.Millisecond, 100)
	ctx := context.Background()

	store.SetWorkdir(ctx, "s1", "/tmp")
	store.SetWarpToolBinding(ctx, "session", "tool_1", WarpToolBinding{ConversationID: "conv_1"})
	time.Sleep(150 * time.Millisecond)
	store.Cleanup(ctx)

	_, ok := store.GetWorkdir(ctx, "s1")
	if ok {
		t.Fatal("session should have been cleaned up")
	}
	if _, ok := store.GetWarpToolBinding(ctx, "session", "tool_1"); ok {
		t.Fatal("tool binding should have been cleaned up")
	}
}

func TestMemorySessionStoreWarpToolBindingAndAccount(t *testing.T) {
	store := NewMemorySessionStore(30*time.Minute, 100)
	ctx := context.Background()
	store.SetAccountID(ctx, "session", 132)
	store.SetWarpToolBinding(ctx, "session", "tool_write", WarpToolBinding{
		ConversationID: "warp_conv",
		AccountID:      132,
		ToolType:       "call_mcp_tool",
		ToolName:       "Write",
		ToolInput:      `{"file_path":"main.go"}`,
		TaskContext:    "dGFzaw",
	})

	if accountID, ok := store.GetAccountID(ctx, "session"); !ok || accountID != 132 {
		t.Fatalf("accountID=%d ok=%v, want 132 true", accountID, ok)
	}
	binding, ok := store.GetWarpToolBinding(ctx, "session", "tool_write")
	if !ok || binding.ConversationID != "warp_conv" || binding.AccountID != 132 || binding.ToolType != "call_mcp_tool" || binding.TaskContext != "dGFzaw" {
		t.Fatalf("binding=%#v ok=%v", binding, ok)
	}
}

func TestRedisSessionStoreWarpToolBindingAndAccount(t *testing.T) {
	store, _ := setupRedisSessionStore(t)
	ctx := context.Background()
	store.SetAccountID(ctx, "session", 132)
	store.SetWarpToolBinding(ctx, "session", "tool:with unsafe key bytes", WarpToolBinding{
		ConversationID: "warp_conv",
		AccountID:      132,
		ToolType:       "call_mcp_tool",
	})

	if accountID, ok := store.GetAccountID(ctx, "session"); !ok || accountID != 132 {
		t.Fatalf("accountID=%d ok=%v, want 132 true", accountID, ok)
	}
	binding, ok := store.GetWarpToolBinding(ctx, "session", "tool:with unsafe key bytes")
	if !ok || binding.ConversationID != "warp_conv" || binding.AccountID != 132 {
		t.Fatalf("binding=%#v ok=%v", binding, ok)
	}
}

func TestMemorySessionStoreWarpToolBindingIsConversationScoped(t *testing.T) {
	store := NewMemorySessionStore(30*time.Minute, 100)
	ctx := context.Background()
	store.SetWarpToolBinding(ctx, "conversation-a", "tool_1", WarpToolBinding{ConversationID: "conv_a"})
	store.SetWarpToolBinding(ctx, "conversation-b", "tool_1", WarpToolBinding{ConversationID: "conv_b"})

	if binding, ok := store.GetWarpToolBinding(ctx, "conversation-a", "tool_1"); !ok || binding.ConversationID != "conv_a" {
		t.Fatalf("conversation-a binding=%#v ok=%v", binding, ok)
	}
	if binding, ok := store.GetWarpToolBinding(ctx, "conversation-b", "tool_1"); !ok || binding.ConversationID != "conv_b" {
		t.Fatalf("conversation-b binding=%#v ok=%v", binding, ok)
	}
}

func TestMemorySessionStoreAllowsAnonymousToolCapability(t *testing.T) {
	store := NewMemorySessionStore(time.Hour, 10)
	ctx := context.Background()
	store.SetWarpToolBinding(ctx, "", "unguessable-tool-id", WarpToolBinding{ConversationID: "warp-conv", AccountID: 7})
	binding, ok := store.GetWarpToolBinding(ctx, "", "unguessable-tool-id")
	if !ok || binding.ConversationID != "warp-conv" || binding.AccountID != 7 {
		t.Fatalf("anonymous capability binding=%+v ok=%v", binding, ok)
	}
	if _, ok := store.GetWarpToolBinding(ctx, "another-session", "unguessable-tool-id"); ok {
		t.Fatal("anonymous capability leaked into an explicit conversation namespace")
	}
}
