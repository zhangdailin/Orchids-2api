package orchids

import (
	"bufio"
	"errors"
	"io"
	"net/url"
)

func mapStringValue(msg map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := msg[key].(string); ok {
			return value
		}
	}
	return ""
}

func readLineBytes(reader *bufio.Reader, scratch []byte) ([]byte, []byte, error) {
	scratch = scratch[:0]
	for {
		fragment, err := reader.ReadSlice('\n')
		switch {
		case err == nil:
			if len(scratch) == 0 {
				return fragment, scratch, nil
			}
			scratch = append(scratch, fragment...)
			return scratch, scratch, nil
		case errors.Is(err, bufio.ErrBufferFull):
			scratch = append(scratch, fragment...)
		case errors.Is(err, io.EOF):
			if len(fragment) == 0 && len(scratch) == 0 {
				return nil, scratch, io.EOF
			}
			scratch = append(scratch, fragment...)
			return scratch, scratch, io.EOF
		default:
			return nil, scratch, err
		}
	}
}

func trimTrailingLineBreakBytes(line []byte) []byte {
	for len(line) > 0 {
		last := line[len(line)-1]
		if last != '\n' && last != '\r' {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

func urlEncode(value string) string {
	return url.QueryEscape(value)
}

func truncateTextWithEllipsis(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(text) <= maxLen {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	return string(runes[:maxLen]) + "…[truncated]"
}
