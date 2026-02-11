package tiktoken

import (
	"math"
	"unicode"
)

// EstimateTokens 估算文本的 token 数量
// 使用近似算法：
// - 英文/数字按每 4 个字符约 1 token
// - CJK 字符约 2 tokens/char
// - 标点符号按 1 token 计
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}

	tokens := 0
	inWord := false
	wordLength := 0

	// Fast path: byte scan for ASCII
	// Only decode rune when we hit high-bit byte
	n := len(text)
	for i := 0; i < n; i++ {
		b := text[i]
		if b < 128 { // ASCII
			// 0-9, A-Z, a-z
			// '0' = 48, '9' = 57
			// 'A' = 65, 'Z' = 90
			// 'a' = 97, 'z' = 122
			isAlphaNum := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')

			if isAlphaNum {
				if !inWord {
					inWord = true
					wordLength = 0
				}
				wordLength++
			} else {
				// Symbol or whitespace
				if inWord {
					tokens += estimateWordTokens(wordLength)
					inWord = false
				}
				// Space \t \n \r usually merge or ignore, others count as 1
				if b != ' ' && b != '\t' && b != '\n' && b != '\r' {
					tokens++
				}
			}
		} else {
			// Non-ASCII, finish current word first
			if inWord {
				tokens += estimateWordTokens(wordLength)
				inWord = false
			}

			// Simple fallback: just count this byte as part of a multi-byte sequence?
			// Ideally we decode rune to check CJK.
			// Getting rune from string at index i is tricky without proper decoding.
			// Let's assume non-ASCII bytes usually mean 1.5 token per char roughly,
			// or simpler: just count every 3 bytes as 2 tokens (CJK)?
			// The original logic was: IsCJK ? 2 : 1
			// To keep it strictly correct with original logic, we need to decode.
			// But maybe we can skip full decoding if we just accept an approximation?

			// Let's do partial optimization: advance loop for multi-byte
			// But Go's for range loop does proper decoding.
			// Mixing byte loop and range logic is hard.
			// Simplest hybrid: if we hit non-ASCII, we just assume 2 tokens for that "character"
			// checking high bits to skip bytes?
			// UTF-8: 110xxxxx (2 bytes), 1110xxxx (3 bytes), 11110xxx (4 bytes)

			if (b & 0xE0) == 0xC0 { // 2 bytes
				i++
				tokens++ // Non-CJK non-ascii often 1 token
			} else if (b & 0xF0) == 0xE0 { // 3 bytes (common for CJK)
				i += 2
				tokens += 2 // CJK
			} else if (b & 0xF8) == 0xF0 { // 4 bytes
				i += 3
				tokens += 2
			} else {
				// Continuation byte or invalid, just skip
			}
		}
	}

	if inWord {
		tokens += estimateWordTokens(wordLength)
	}

	return tokens
}

// estimateWordTokens 估算英文单词的 token 数量
// 近似规则：每 4 个字符约 1 token
func estimateWordTokens(length int) int {
	if length <= 0 {
		return 0
	}
	return (length + 3) / 4
}

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

// IsCJK 判断是否是中日韩字符
func IsCJK(r rune) bool {
	// CJK 统一表意文字
	if r >= 0x4E00 && r <= 0x9FFF {
		return true
	}
	// CJK 扩展 A
	if r >= 0x3400 && r <= 0x4DBF {
		return true
	}
	// CJK 扩展 B-F
	if r >= 0x20000 && r <= 0x2EBEF {
		return true
	}
	// 平假名
	if r >= 0x3040 && r <= 0x309F {
		return true
	}
	// 片假名
	if r >= 0x30A0 && r <= 0x30FF {
		return true
	}
	// 韩文
	if r >= 0xAC00 && r <= 0xD7A3 {
		return true
	}
	// 标点符号
	if r >= 0x3000 && r <= 0x303F {
		return true
	}
	return false
}
