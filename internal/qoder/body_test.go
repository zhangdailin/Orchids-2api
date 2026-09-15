package qoder

import (
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// TestEncodeBodyMatchesPrivateAlphabet pins the wire encoding. The signature is
// computed over these bytes, so an alphabet or swap change produces a request
// the gateway rejects as unauthenticated even though every header is present.
func TestEncodeBodyMatchesPrivateAlphabet(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"a":1,"b":"hello"}`)

	// Reference implementation: standard base64, then a positional alphabet
	// substitution, then the outer-third swap.
	standard := base64.StdEncoding.EncodeToString(raw)
	var substituted strings.Builder
	for i := 0; i < len(standard); i++ {
		c := standard[i]
		if c == '=' {
			substituted.WriteByte('$')
			continue
		}
		index := strings.IndexByte("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", c)
		if index < 0 {
			t.Fatalf("standard base64 produced %q, which is outside the standard alphabet", c)
		}
		substituted.WriteByte(bodyAlphabet[index])
	}
	want := swapOuterThirds([]byte(substituted.String()))

	got := EncodeBody(raw)
	if string(got) != string(want) {
		t.Fatalf("EncodeBody() = %q, want %q", got, want)
	}
	for _, b := range got {
		if strings.ContainsRune("+/=", rune(b)) {
			t.Fatalf("EncodeBody() left a standard-alphabet byte %q in %q", b, got)
		}
	}
}

// TestBodyCodecRoundTrip covers the inverse, including payload lengths that make
// the outer thirds unequal in size.
func TestBodyCodecRoundTrip(t *testing.T) {
	t.Parallel()

	for length := 0; length < 200; length++ {
		raw := make([]byte, length)
		for i := range raw {
			raw[i] = byte(i * 7 % 251)
		}
		encoded := EncodeBody(raw)
		decoded, err := decodeBodyForTest(encoded)
		if err != nil {
			t.Fatalf("DecodeBody(len=%d) error = %v", length, err)
		}
		if string(decoded) != string(raw) {
			t.Fatalf("round trip mismatch at length %d", length)
		}
	}
}

// TestDecodeBodyRejectsLineBreaks pins the strict framing: a wrapped body is a
// different byte sequence and so a different signature.
func TestDecodeBodyRejectsLineBreaks(t *testing.T) {
	t.Parallel()

	encoded := EncodeBody([]byte(`{"a":1}`))
	if _, err := decodeBodyForTest(append(encoded[:4], append([]byte{'\n'}, encoded[4:]...)...)); err == nil {
		t.Fatal("DecodeBody() error = nil for a body containing a line break")
	}
}

// TestSignRequestSeparators pins the five-field, four-newline MD5 formula with
// no trailing separator. The trailing byte is the classic mistake here: a
// trailing newline yields a signature that is 32 valid-looking hex characters
// the gateway rejects.
func TestSignRequestSeparators(t *testing.T) {
	t.Parallel()

	const (
		payload = "eyJ2ZXJzaW9uIjoidjEifQ=="
		key     = "AAAABBBBCCCCDDDD"
		seconds = "1700000000"
		body    = "encoded-body"
		path    = "/api/v2/service/pro/sse/agent_chat_generation"
	)
	want := fmt.Sprintf("%x", md5.Sum([]byte(payload+"\n"+key+"\n"+seconds+"\n"+body+"\n"+path)))

	if got := signRequest(payload, key, seconds, body, path); got != want {
		t.Fatalf("signRequest() = %q, want %q", got, want)
	}
	if strings.HasSuffix(signRequest(payload, key, seconds, body, path), "\n") {
		t.Fatal("signature carries a trailing separator")
	}
}

// TestComposeBearer pins the authorization prefix.
func TestComposeBearer(t *testing.T) {
	t.Parallel()

	if got, want := composeBearer("payload", "sig"), "Bearer COSY.payload.sig"; got != want {
		t.Fatalf("composeBearer() = %q, want %q", got, want)
	}
}

// TestSignPathStripsAlgoAndQuery pins the signed path: the covered path drops
// the /algo gateway prefix and never includes the query string.
func TestSignPathStripsAlgoAndQuery(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"https://api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1": "/api/v2/service/pro/sse/agent_chat_generation",
		"https://gateway.qoder.com.cn/algo/api/v2/quota/usage?Encode=1":                                                                    "/api/v2/quota/usage",
		"/algo/api/v2/quota/usage?Encode=1": "/api/v2/quota/usage",
	}
	for raw, want := range cases {
		if got := signPath(raw); got != want {
			t.Errorf("signPath(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestChatURLCarriesFixedAgentQuery pins the query the gateway reads instead of
// the body's agent_id.
func TestChatURLCarriesFixedAgentQuery(t *testing.T) {
	t.Parallel()

	got := chatURL("https://api2.qoder.sh")
	want := "https://api2.qoder.sh/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	if got != want {
		t.Fatalf("chatURL() = %q, want %q", got, want)
	}
}
