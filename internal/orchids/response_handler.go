package orchids

import (
	"github.com/goccy/go-json"
)

type orchidsToolCall struct {
	id    string
	name  string
	input string
}

func marshalOrchidsToolInput(input interface{}) string {
	if input == nil {
		return ""
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(raw)
}


