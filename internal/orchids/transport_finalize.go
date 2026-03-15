package orchids

import (
	"context"
	"fmt"
)

func orchidsFinishReason(state *requestState) string {
	if state.sawToolCall {
		return "tool_use"
	}
	return "end_turn"
}

func finalizeOrchidsTransport(
	ctx context.Context,
	state *requestState,
	writer *SSEWriter,
) error {
	if state.errorMsg != "" {
		return fmt.Errorf("orchids upstream error: %s", state.errorMsg)
	}

	if !state.finishSent {
		state.finishReason = orchidsFinishReason(state)
		if writer != nil {
			_ = writer.WriteMessageEnd()
		}
	}

	return nil
}
