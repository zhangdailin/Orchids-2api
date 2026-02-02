package clerk

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ClientResponse struct {
	Response struct {
		ID                  string `json:"id"`
		LastActiveSessionID string `json:"last_active_session_id"`
		Sessions            []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			User   struct {
				ID             string `json:"id"`
				EmailAddresses []struct {
					EmailAddress string `json:"email_address"`
				} `json:"email_addresses"`
			} `json:"user"`
			LastActiveToken struct {
				JWT string `json:"jwt"`
			} `json:"last_active_token"`
		} `json:"sessions"`
	} `json:"response"`
}

type AccountInfo struct {
	SessionID    string
	ClientCookie string
	ClientUat    string
	ProjectID    string
	UserID       string
	Email        string
	JWT          string
}

func FetchAccountInfo(clientCookie string) (*AccountInfo, error) {
	url := "https://clerk.orchids.app/v1/client?__clerk_api_version=2025-11-10&_clerk_js_version=5.117.0"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Orchids/0.0.57 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")
	req.AddCookie(&http.Cookie{Name: "__client", Value: clientCookie})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch client info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status code %d: %s", resp.StatusCode, string(body))
	}

	var clientResp ClientResponse
	if err := json.NewDecoder(resp.Body).Decode(&clientResp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(clientResp.Response.Sessions) == 0 {
		return nil, fmt.Errorf("no active sessions found")
	}

	session := clientResp.Response.Sessions[0]
	if len(session.User.EmailAddresses) == 0 {
		return nil, fmt.Errorf("no email address found")
	}

	return &AccountInfo{
		SessionID:    clientResp.Response.LastActiveSessionID,
		ClientCookie: clientCookie,
		ClientUat:    fmt.Sprintf("%d", time.Now().Unix()),
		ProjectID:    "280b7bae-cd29-41e4-a0a6-7f603c43b607",
		UserID:       session.User.ID,
		Email:        session.User.EmailAddresses[0].EmailAddress,
		JWT:          session.LastActiveToken.JWT,
	}, nil
}

func NormalizeClientCookie(input string) (string, error) {
	client, _, err := ParseClientCookies(input)
	return client, err
}

func ParseClientCookies(input string) (clientJWT string, sessionJWT string, err error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "", nil
	}

	if strings.Contains(trimmed, "__client=") {
		client, err := extractCookieValue(trimmed, "__client")
		if err != nil {
			return "", "", err
		}
		if !isLikelyJWT(client) {
			return "", "", fmt.Errorf("invalid __client value")
		}
		session, _ := extractCookieValue(trimmed, "__session")
		if session != "" && !isLikelyJWT(session) {
			return "", "", fmt.Errorf("invalid __session value")
		}
		return client, session, nil
	}

	if strings.Contains(trimmed, "|") {
		parts := strings.Split(trimmed, "|")
		jwt := strings.TrimSpace(parts[0])
		if isLikelyJWT(jwt) {
			return jwt, "", nil
		}
		return "", "", fmt.Errorf("invalid JWT before '|'")
	}

	if isLikelyJWT(trimmed) {
		return trimmed, "", nil
	}

	return "", "", fmt.Errorf("unsupported client cookie format")
}

func extractCookieValue(input string, name string) (string, error) {
	key := name + "="
	idx := strings.Index(input, key)
	if idx < 0 {
		return "", nil
	}
	value := input[idx+len(key):]
	if end := strings.Index(value, ";"); end >= 0 {
		value = value[:end]
	}
	return strings.TrimSpace(value), nil
}

func ParseSessionInfoFromJWT(sessionJWT string) (sessionID string, userID string) {
	parts := strings.Split(sessionJWT, ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload := parts[1]
	payload = strings.ReplaceAll(payload, "-", "+")
	payload = strings.ReplaceAll(payload, "_", "/")
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", ""
	}
	var data struct {
		SID string `json:"sid"`
		SUB string `json:"sub"`
		ID  string `json:"id"`
	}
	if err := json.Unmarshal(decoded, &data); err != nil {
		return "", ""
	}
	sid := data.SID
	if sid == "" {
		sid = data.ID
	}
	return sid, data.SUB
}

func isLikelyJWT(value string) bool {
	if value == "" {
		return false
	}
	if strings.Count(value, ".") != 2 {
		return false
	}
	parts := strings.Split(value, ".")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}
