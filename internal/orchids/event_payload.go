package orchids


func extractOrchidsErrorPayload(msg map[string]interface{}) (code string, message string) {
	if data, ok := msg["data"].(map[string]interface{}); ok {
		if m, ok := data["message"].(string); ok {
			message = m
		}
		if c, ok := data["code"].(string); ok {
			code = c
		}
	}
	if message == "" {
		if m, ok := msg["message"].(string); ok {
			message = m
		}
	}
	if code == "" {
		if c, ok := msg["code"].(string); ok {
			code = c
		}
	}
	return code, message
}

func extractOrchidsFastErrorPayload(msg orchidsFastErrorMessage) (code string, message string) {
	if msg.Data.Message != "" {
		message = msg.Data.Message
	}
	if msg.Data.Code != "" {
		code = msg.Data.Code
	}
	if message == "" {
		message = msg.Message
	}
	if code == "" {
		code = msg.Code
	}
	return code, message
}

