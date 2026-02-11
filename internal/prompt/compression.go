package prompt

// CollapseRepeatedErrors detects and merges consecutive identical error loops.
func CollapseRepeatedErrors(messages []Message) []Message {
	if len(messages) < 4 {
		return messages
	}

	out := make([]Message, 0, len(messages))
	hashes := make([]string, len(messages))
	hasHash := make([]bool, len(messages))
	getHash := func(i int) string {
		if !hasHash[i] {
			hashes[i] = messageHash(messages[i])
			hasHash[i] = true
		}
		return hashes[i]
	}
	i := 0
	for i < len(messages) {
		// Look for pattern: [Assistant A, User Error A] followed by [Assistant A, User Error A]
		if i+3 < len(messages) {
			a1 := messages[i]
			u1 := messages[i+1]
			a2 := messages[i+2]
			u2 := messages[i+3]

			if a1.Role == "assistant" && u1.Role == "user" &&
				a2.Role == "assistant" && u2.Role == "user" {

				if isErrorResult(u1) && isErrorResult(u2) {
					// Check if they are effectively identical loops
					if getHash(i) == getHash(i+2) && getHash(i+1) == getHash(i+3) {
						// Skip the first pair (deduplicate), effectively collapsing the loop
						i += 2
						continue
					}
				}
			}
		}
		out = append(out, messages[i])
		i++
	}
	return out
}

func isErrorResult(m Message) bool {
	if m.Content.IsString() {
		return false
	}
	for _, b := range m.Content.GetBlocks() {
		if b.IsError {
			return true
		}
	}
	return false
}
