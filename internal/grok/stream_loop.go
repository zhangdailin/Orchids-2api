package grok

import (
	"errors"
	"fmt"
)

// Upstream doom-loop guard, ported from chenyme/grok2api
// (conversation/stream.go streamRepeatTracker).
//
// A model that degenerates into emitting the same delta forever burns account
// quota and floods the caller's context, and the semantic idle watchdog cannot
// catch it: repeated generated deltas look exactly like healthy output. Only a
// repeat counter distinguishes the two.
var errGrokUpstreamOutputLoop = errors.New("upstream output loop")

// The thresholds are grok2api's: more than 128 identical visible deltas or more
// than 256 identical reasoning deltas is a degenerate upstream repeating itself.
// They count repeats of one value, not volume, so a long answer made of distinct
// chunks is never affected.
const (
	contentDoomLoopThreshold   = 128
	reasoningDoomLoopThreshold = 256
)

type streamRepeatTracker struct {
	lastContentDelta  string
	contentRepeat     int
	lastReasoningText string
	reasoningRepeat   int
}

// observe inspects one upstream event and reports a loop error once a delta has
// repeated past its threshold. Only generated deltas are tracked; any other
// event is ignored.
func (t *streamRepeatTracker) observe(event map[string]interface{}, eventName string) error {
	if t == nil || event == nil {
		return nil
	}
	kind := firstNonEmpty(interfaceString(event["type"]), eventName)
	switch kind {
	case "response.output_text.delta":
		return t.trackContent(streamString(event["delta"]))
	case "response.reasoning_summary_text.delta":
		return t.trackReasoning(streamString(event["delta"])+"\x00summary", "model reasoning summary loop detected")
	case "response.reasoning_text.delta":
		return t.trackReasoning(streamString(event["delta"]), "model reasoning loop detected")
	default:
		return nil
	}
}

func (t *streamRepeatTracker) trackContent(delta string) error {
	if delta == "" {
		return nil
	}
	if delta != t.lastContentDelta {
		t.lastContentDelta = delta
		t.contentRepeat = 1
		return nil
	}
	t.contentRepeat++
	if t.contentRepeat > contentDoomLoopThreshold {
		return fmt.Errorf("%w (repeated content delta %d times)", errGrokUpstreamOutputLoop, t.contentRepeat)
	}
	return nil
}

func (t *streamRepeatTracker) trackReasoning(delta, message string) error {
	if delta == "" {
		return nil
	}
	if delta != t.lastReasoningText {
		t.lastReasoningText = delta
		t.reasoningRepeat = 1
		return nil
	}
	t.reasoningRepeat++
	if t.reasoningRepeat > reasoningDoomLoopThreshold {
		return fmt.Errorf("%w: %s (repeated delta %d times)", errGrokUpstreamOutputLoop, message, t.reasoningRepeat)
	}
	return nil
}
