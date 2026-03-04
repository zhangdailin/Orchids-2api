package tiktoken

import (
	"math"
	"unicode"
)

// EstimateTextTokens 简单估算：CJK 字符约 1.5 token/char，ASCII 单词约 1 token/word
func EstimateTextTokens(text string) int {
	if text == "" {
		return 0
	}

	var tokens float64
	inWord := false

	for _, r := range text {
		if r < 128 {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				if !inWord {
					inWord = true
				}
			} else {
				if inWord {
					tokens += 1
					inWord = false
				}
				if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
					tokens += 1
				}
			}
			continue
		}

		if inWord {
			tokens += 1
			inWord = false
		}
		tokens += 1.5
	}

	if inWord {
		tokens += 1
	}

	return int(math.Round(tokens))
}
