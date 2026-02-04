package handler

import (
	"fmt"
	"log/slog"

	"orchids-api/internal/prompt"
)

type toolResultRef struct {
	msgIndex   int
	blockIndex int
}

func trimMessages(messages []prompt.Message, maxHistory, maxToolResults int, channel string) ([]prompt.Message, int, int) {
	trimmed := cloneMessages(messages)
	droppedHistory := 0
	droppedToolResults := 0

	if maxHistory > 0 && len(trimmed) > maxHistory {
		droppedHistory = len(trimmed) - maxHistory
		trimmed = trimmed[len(trimmed)-maxHistory:]
	}

	if maxToolResults > 0 {
		var kept []prompt.Message
		remaining := maxToolResults
		for i := len(trimmed) - 1; i >= 0; i-- {
			msg := trimmed[i]
			if msg.Content.Blocks == nil {
				kept = append(kept, msg)
				continue
			}

			blocks := msg.Content.Blocks
			keepFlags := make([]bool, len(blocks))
			for j := len(blocks) - 1; j >= 0; j-- {
				block := blocks[j]
				if block.Type == "tool_result" {
					if remaining > 0 {
						keepFlags[j] = true
						remaining--
					} else {
						droppedToolResults++
					}
					continue
				}
				keepFlags[j] = true
			}

			newBlocks := make([]prompt.ContentBlock, 0, len(blocks))
			for j, keep := range keepFlags {
				if keep {
					newBlocks = append(newBlocks, blocks[j])
				}
			}

			msg.Content.Blocks = newBlocks
			if msg.Content.Text == "" && len(newBlocks) == 0 {
				continue
			}
			kept = append(kept, msg)
		}

		// Reverse back to original order
		for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
			kept[i], kept[j] = kept[j], kept[i]
		}
		trimmed = kept
	}

	if droppedHistory > 0 || droppedToolResults > 0 {
		logTrim(channel, droppedHistory, droppedToolResults)
	}

	return trimmed, droppedHistory, droppedToolResults
}

func splitWarpToolResults(messages []prompt.Message, batchSize int) ([][]prompt.Message, int) {
	if batchSize <= 0 {
		return [][]prompt.Message{cloneMessages(messages)}, 0
	}

	refs := collectToolResultRefs(messages)
	total := len(refs)
	if total <= batchSize {
		return [][]prompt.Message{cloneMessages(messages)}, total
	}

	var batches [][]prompt.Message
	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		keep := make(map[toolResultRef]struct{}, end-start)
		for _, ref := range refs[start:end] {
			keep[ref] = struct{}{}
		}
		batches = append(batches, filterToolResults(messages, keep))
	}

	return batches, total
}

func collectToolResultRefs(messages []prompt.Message) []toolResultRef {
	var refs []toolResultRef
	for i, msg := range messages {
		if msg.Content.Blocks == nil {
			continue
		}
		for j, block := range msg.Content.Blocks {
			if block.Type == "tool_result" {
				refs = append(refs, toolResultRef{msgIndex: i, blockIndex: j})
			}
		}
	}
	return refs
}

func filterToolResults(messages []prompt.Message, keep map[toolResultRef]struct{}) []prompt.Message {
	trimmed := cloneMessages(messages)
	kept := make([]prompt.Message, 0, len(trimmed))

	for i, msg := range trimmed {
		if msg.Content.Blocks == nil {
			kept = append(kept, msg)
			continue
		}
		blocks := msg.Content.Blocks
		newBlocks := make([]prompt.ContentBlock, 0, len(blocks))
		for j, block := range blocks {
			if block.Type == "tool_result" {
				if _, ok := keep[toolResultRef{msgIndex: i, blockIndex: j}]; !ok {
					continue
				}
			}
			newBlocks = append(newBlocks, block)
		}
		msg.Content.Blocks = newBlocks
		if msg.Content.Text == "" && len(newBlocks) == 0 {
			continue
		}
		kept = append(kept, msg)
	}
	return kept
}

func cloneMessages(messages []prompt.Message) []prompt.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]prompt.Message, len(messages))
	for i, msg := range messages {
		out[i] = msg
		if msg.Content.Blocks == nil {
			continue
		}
		blocks := make([]prompt.ContentBlock, len(msg.Content.Blocks))
		copy(blocks, msg.Content.Blocks)
		out[i].Content.Blocks = blocks
	}
	return out
}

func logTrim(channel string, droppedHistory, droppedToolResults int) {
	if droppedHistory > 0 {
		slog.Info("History trimmed", "channel", channel, "dropped_messages", droppedHistory)
	}
	if droppedToolResults > 0 {
		slog.Info("Tool results trimmed", "channel", channel, "dropped_tool_results", droppedToolResults)
	}
}

func compressToolResults(messages []prompt.Message, maxLen int, channel string) ([]prompt.Message, int) {
	if maxLen <= 0 {
		return messages, 0
	}
	compressed := cloneMessages(messages)
	compressedCount := 0

	for i := range compressed {
		msg := &compressed[i]
		if msg.Role != "user" || msg.Content.Blocks == nil {
			continue
		}

		for j := range msg.Content.Blocks {
			block := &msg.Content.Blocks[j]
			if block.Type == "tool_result" {
				content, ok := block.Content.(string)
				if !ok {
					// Handle array of text blocks if necessary, but usually it's string or array of blocks
					// For simplicity, if it's not string, we skip or handle complex structure later
					// Orchids usually puts string content here
					continue
				}
				if len(content) > maxLen {
					truncated := content[:maxLen] + fmt.Sprintf("\n... [truncated %d bytes]", len(content)-maxLen)
					block.Content = truncated
					compressedCount++
				}
			}
		}
	}

	if compressedCount > 0 {
		slog.Info("Context compressed", "channel", channel, "compressed_blocks", compressedCount)
	}

	return compressed, compressedCount
}
