package grok

import "net/http"

// recoverChatReasoning runs only after a recognized pre-generation decode 400.
// Native Responses and client compaction remain outside this recovery path.
func recoverChatReasoning(payload map[string]interface{}, initial error, call func(string) (*http.Response, error)) (*http.Response, error) {
	if payloadHasCompactionInput(payload) {
		return nil, initial
	}
	var response *http.Response
	failure := initial
	if stripInjectedReasoningReplay(payload) {
		response, failure = call("reasoning_encrypted_content_retry")
		if failure == nil || !isReasoningReplayDecodeError(failure) {
			return response, failure
		}
	}
	if interfaceString(payload["previous_response_id"]) != "" || interfaceString(payload["prompt_cache_key"]) == "" {
		return response, failure
	}
	delete(payload, "prompt_cache_key")
	return call("reasoning_session_reset")
}
