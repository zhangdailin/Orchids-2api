package workbuddy

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestMonitorStreamIdle_CancelsSilentBody(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	ctx, cancelContext := context.WithCancel(context.Background())
	defer cancelContext()
	cancel := func() {
		cancelContext()
		_ = writer.CloseWithError(context.Canceled)
	}
	body := monitorStreamIdle(reader, 30*time.Millisecond, cancel)
	defer body.Close()

	started := time.Now()
	_, err := body.Read(make([]byte, 1))
	if !errors.Is(err, errStreamIdleTimeout) {
		t.Fatalf("Read() error = %v, want %v", err, errStreamIdleTimeout)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("idle cancellation took %v", elapsed)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("request context was not canceled")
	}
}

func TestMonitorStreamIdle_RenewsOnActivity(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := monitorStreamIdle(reader, 80*time.Millisecond, cancel)
	defer body.Close()

	go func() {
		defer writer.Close()
		for i := 0; i < 5; i++ {
			time.Sleep(30 * time.Millisecond)
			_, _ = writer.Write([]byte{'x'})
		}
	}()

	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(raw) != "xxxxx" {
		t.Fatalf("body = %q, want xxxxx", raw)
	}
	select {
	case <-ctx.Done():
		t.Fatal("active stream was canceled")
	default:
	}
}
