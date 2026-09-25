package errors

import "testing"

// TestClassifySharedQueueRefusalDoesNotRotate pins that every shape of a
// shared/upstream-wide queue refusal is recognised. Falling through to the
// default branch labelled it "unknown" with SwitchAccount=true, so one shared
// refusal was retried against every account in the pool in turn: Qoder's nine
// accounts were all parked as "429" within seconds and the pool went empty.
func TestClassifySharedQueueRefusalDoesNotRotate(t *testing.T) {
	// The exact status_message stored for production account 22: the text blames
	// the credential, the body says the service is unavailable.
	const production = `qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"p3\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"waitTime\":30}"}`

	for name, message := range map[string]string{
		"production message":      production,
		"classified busy form":    "qoder gateway is busy: serviceAvailable=false retryAfterSeconds=29",
		"upstream pool throttled": "qoder API error: the available upstream accounts are rate-limited",
		"service unavailable":     `qoder upstream error: {"serviceAvailable":false}`,
		"queued flag":             `qoder upstream error: {"isQueued":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			class := ClassifyUpstreamError(message)
			if class.Category != "rate_limit" {
				t.Errorf("category = %q, want rate_limit", class.Category)
			}
			if class.SwitchAccount {
				t.Error("SwitchAccount = true; every account meets the identical refusal")
			}
		})
	}
}

// TestClassifyOrdinaryThrottleStillSwitches guards the other direction: an
// account-scoped throttle must keep switching, so the shared-refusal rule cannot
// swallow the channels it does not describe.
func TestClassifyOrdinaryThrottleStillSwitches(t *testing.T) {
	for name, message := range map[string]string{
		"plain 429":         "upstream API error: status=429, too many requests",
		"cline prose cap":   "cline inference cap reached: Try again in 17h 59m",
		"qoder agent limit": "qoder agent limit reached; resets at 2026-09-27T19:47:13Z",
	} {
		t.Run(name, func(t *testing.T) {
			class := ClassifyUpstreamError(message)
			if class.Category != "rate_limit" {
				t.Errorf("category = %q, want rate_limit", class.Category)
			}
			if !class.SwitchAccount {
				t.Error("SwitchAccount = false; an account-scoped throttle must still rotate")
			}
		})
	}
}
