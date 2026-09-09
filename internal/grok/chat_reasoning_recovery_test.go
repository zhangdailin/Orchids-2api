package grok

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
)

func validTestReplayCipher() string {
	data := make([]byte, 128)
	for i := range data {
		data[i] = byte(i)
	}
	return base64.RawStdEncoding.EncodeToString(data)
}

func TestReplayCipherValidation(t *testing.T) {
	if !validReplayCipher(validTestReplayCipher()) {
		t.Fatal("valid cipher rejected")
	}
	for _, value := range []string{"cipher", "gAAAAsecret", " " + validTestReplayCipher(), validTestReplayCipher() + "=", base64.RawStdEncoding.EncodeToString(make([]byte, 128))} {
		if validReplayCipher(value) {
			t.Fatal("malformed cipher accepted")
		}
	}
}

func TestChatReasoningRecoveryResetsOnlyAfterDecodeFailure(t *testing.T) {
	initial := fmt.Errorf("grok cli upstream status=400 body=invalid_encrypted_content")
	payload := map[string]interface{}{"prompt_cache_key": "session", "input": []interface{}{map[string]interface{}{"type": "reasoning", "encrypted_content": "opaque", "summary": []interface{}{map[string]interface{}{"text": "remember this"}}}}}
	calls := 0
	response, err := recoverChatReasoning(payload, initial, func(stage string) (*http.Response, error) {
		calls++
		if calls == 1 {
			item := payload["input"].([]interface{})[0].(map[string]interface{})
			if item["type"] != "message" || item["encrypted_content"] != nil {
				t.Fatal(item)
			}
			if payload["prompt_cache_key"] != "session" {
				t.Fatal("removed session before portable retry")
			}
			return nil, initial
		}
		if payload["prompt_cache_key"] != nil || stage != "reasoning_session_reset" {
			t.Fatal(payload, stage)
		}
		return &http.Response{StatusCode: 200}, nil
	})
	if err != nil || response == nil || calls != 2 {
		t.Fatal(response, err, calls)
	}
}

func TestChatReasoningRecoveryPreservesPreviousResponse(t *testing.T) {
	initial := fmt.Errorf("grok cli upstream status=400 body=invalid_encrypted_content")
	payload := map[string]interface{}{"prompt_cache_key": "session", "previous_response_id": "resp_1"}
	_, err := recoverChatReasoning(payload, initial, func(string) (*http.Response, error) { t.Fatal("unsafe reset"); return nil, nil })
	if err != initial || payload["prompt_cache_key"] != "session" {
		t.Fatal(err, payload)
	}
}

func TestChatReasoningRecoveryDoesNotRetryRateLimit(t *testing.T) {
	initial := fmt.Errorf("grok cli upstream status=400 body=invalid_encrypted_content")
	limited := fmt.Errorf("grok cli upstream status=429 body=rate limited")
	payload := map[string]interface{}{"prompt_cache_key": "session", "input": []interface{}{map[string]interface{}{"type": "reasoning", "encrypted_content": "opaque"}}}
	calls := 0
	_, err := recoverChatReasoning(payload, initial, func(string) (*http.Response, error) { calls++; return nil, limited })
	if err != limited || calls != 1 {
		t.Fatal(err, calls)
	}
}
