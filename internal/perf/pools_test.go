package perf

import (
	"bytes"
	"strings"
	"testing"
)

func TestStringBuilderPool(t *testing.T) {
	sb := AcquireStringBuilder()
	sb.WriteString("hello")
	ReleaseStringBuilder(sb)

	sb2 := AcquireStringBuilder()
	defer ReleaseStringBuilder(sb2)
	if sb2.Len() != 0 {
		t.Fatalf("expected reset builder, got len=%d", sb2.Len())
	}
}

func TestMapPool(t *testing.T) {
	m := AcquireMap()
	m["k"] = "v"
	ReleaseMap(m)

	m2 := AcquireMap()
	defer ReleaseMap(m2)
	if len(m2) != 0 {
		t.Fatalf("expected cleared map, got len=%d", len(m2))
	}
}

func TestByteBufferPool(t *testing.T) {
	buf := AcquireByteBuffer()
	_, _ = buf.WriteString("abc")
	ReleaseByteBuffer(buf)

	buf2 := AcquireByteBuffer()
	defer ReleaseByteBuffer(buf2)
	if buf2.Len() != 0 {
		t.Fatalf("expected reset buffer, got len=%d", buf2.Len())
	}
}

func TestBufioReaderPool(t *testing.T) {
	r := AcquireBufioReader(strings.NewReader("ok"))
	defer ReleaseBufioReader(r)

	tmp := make([]byte, 2)
	n, err := r.Read(tmp)
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	if n != 2 || !bytes.Equal(tmp, []byte("ok")) {
		t.Fatalf("unexpected read: n=%d data=%q", n, string(tmp))
	}
}
