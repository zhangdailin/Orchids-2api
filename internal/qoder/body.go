package qoder

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/goccy/go-json"
)

// The gateway reads two things from a chat request that are easy to get wrong.
//
// Body: the JSON is not sent raw. It is standard-base64/base64-encoded with a
// private alphabet and padding character, and then the two outer thirds are
// swapped. The signature is computed over the encoded form, so an
// implementation that signs the JSON is rejected with an auth error that looks
// like a bad token.
//
// Authorization: "Bearer COSY.<payload>.<signature>" where the signature is the
// MD5 of five newline-separated fields, and the covered path is the request
// path without the /algo prefix and without the query string.

// bodyAlphabet is the private base64 alphabet the gateway expects. The
// character set is fixed; it is not chosen here.
const bodyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

// bodyPadding substitutes for '=' in the private alphabet.
const bodyPadding = '$'

// bodyEncoding is the strict private-alphabet encoder.
var bodyEncoding = base64.NewEncoding(bodyAlphabet).WithPadding(bodyPadding).Strict()

// EncodeBody renders the JSON body the way the wire format requires.
//
// bodyEncoding already substitutes the private alphabet (and the '$' padding),
// so only the third swap remains. The swap is a positional reordering over the
// already-encoded bytes, which is why the signature can be computed over the
// result before it is sent.
func EncodeBody(raw []byte) []byte {
	encoded := make([]byte, bodyEncoding.EncodedLen(len(raw)))
	bodyEncoding.Encode(encoded, raw)
	return swapOuterThirds(encoded)
}

// DecodeBody reverses EncodeBody. It is exported for tests and diagnostics; the
// channel never needs to read a body back in production.
func DecodeBody(encoded []byte) ([]byte, error) {
	if bytes.IndexByte(encoded, '\r') >= 0 || bytes.IndexByte(encoded, '\n') >= 0 {
		return nil, fmt.Errorf("encoded body contains a line break")
	}
	decoded := make([]byte, bodyEncoding.DecodedLen(len(encoded)))
	n, err := bodyEncoding.Decode(decoded, swapOuterThirds(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return decoded[:n], nil
}

// swapOuterThirds moves the leading third to the end and the trailing third to
// the front, leaving the middle in place. The middle section absorbs any
// remainder so that all bytes are covered exactly once.
func swapOuterThirds(src []byte) []byte {
	q := len(src) / 3
	out := make([]byte, 0, len(src))
	out = append(out, src[len(src)-q:]...)
	out = append(out, src[q:len(src)-q]...)
	out = append(out, src[:q]...)
	return out
}

// cosyPayload is the decoded COSY payload. Field order and the empty
// ideVersion are part of the wire contract: the payload is base64-encoded
// verbatim, so any difference changes every signature.
type cosyPayload struct {
	Version     string `json:"version"`
	RequestID   string `json:"requestId"`
	Info        string `json:"info"`
	CosyVersion string `json:"cosyVersion"`
	IDEVersion  string `json:"ideVersion"`
}

// buildCOSYPayload renders the payload and its base64 form.
func buildCOSYPayload(requestID, info, cosyVersion string) (string, error) {
	raw, err := json.Marshal(cosyPayload{
		Version:     "v1",
		RequestID:   requestID,
		Info:        info,
		CosyVersion: cosyVersion,
		IDEVersion:  "",
	})
	if err != nil {
		return "", fmt.Errorf("marshal cosy payload: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// signRequest is the request signature: the lowercase hex MD5 of the payload,
// the runtime key, the Unix seconds, the encoded body and the signed path,
// joined by newlines with no trailing separator.
func signRequest(payloadBase64, runtimeKey, unixSeconds, encodedBody, signedPath string) string {
	sum := md5.Sum([]byte(payloadBase64 + "\n" + runtimeKey + "\n" + unixSeconds + "\n" + encodedBody + "\n" + signedPath))
	return hex.EncodeToString(sum[:])
}

// composeBearer renders the Authorization header value.
func composeBearer(payloadBase64, signature string) string {
	return "Bearer COSY." + payloadBase64 + "." + signature
}
