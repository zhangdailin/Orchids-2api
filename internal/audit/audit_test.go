package audit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"
	"github.com/redis/go-redis/v9"
)

func setupRedisLogger(t *testing.T) (*RedisLogger, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	logger := NewRedisLogger(client, "test:", 1000)
	return logger, s
}

func readLoggedEvents(t *testing.T, logger *RedisLogger, count int64) []Event {
	t.Helper()
	msgs, err := logger.client.XRevRangeN(context.Background(), logger.streamKey, "+", "-", count).Result()
	if err != nil {
		t.Fatal(err)
	}
	events := make([]Event, 0, len(msgs))
	for _, msg := range msgs {
		data, ok := msg.Values["data"].(string)
		if !ok {
			t.Fatalf("audit event data has type %T", msg.Values["data"])
		}
		var event Event
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func TestRedisLoggerLog(t *testing.T) {
	logger, _ := setupRedisLogger(t)
	ctx := context.Background()

	logger.Log(ctx, Event{
		Action:    "chat_request",
		AccountID: 1,
		Model:     "claude-sonnet-4-5",
		Status:    "success",
		Duration:  150,
	})

	logger.Log(ctx, Event{
		Action:    "image_generate",
		AccountID: 2,
		Status:    "error",
		Error:     "timeout",
	})

	// Close drains the async queue. A fixed sleep races the writer on loaded CI.
	logger.Close()

	events := readLoggedEvents(t, logger, 10)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Results are reverse-chronological
	if events[0].Action != "image_generate" {
		t.Fatalf("expected image_generate first (newest), got %s", events[0].Action)
	}
	if events[1].Action != "chat_request" {
		t.Fatalf("expected chat_request second (oldest), got %s", events[1].Action)
	}
}

func TestRedisLoggerTimestamp(t *testing.T) {
	logger, _ := setupRedisLogger(t)
	ctx := context.Background()

	before := time.Now()
	logger.Log(ctx, Event{Action: "test", Status: "success"})
	logger.Close()

	events := readLoggedEvents(t, logger, 1)
	if len(events) != 1 {
		t.Fatal("expected 1 event")
	}
	if events[0].Timestamp.Before(before) {
		t.Fatal("timestamp should be after log call")
	}
}

func TestNopLogger(t *testing.T) {
	logger := NewNopLogger()
	ctx := context.Background()

	// Should not panic
	logger.Log(ctx, Event{Action: "test", Status: "success"})
}
