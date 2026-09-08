package puter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// User is the non-secret identity returned for both user and user-app tokens.
type User struct {
	UUID     string `json:"uuid"`
	Username string `json:"username"`
}

// FetchUser validates a browser grant with Puter, without a billable AI probe.
func (c *Client) FetchUser(ctx context.Context) (*User, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.puter.com/whoami", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("Accept", "application/json")
	// Never follow an upstream redirect with a login credential.
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Puter identity request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Puter identity status=%d", resp.StatusCode)
	}
	var user User
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&user); err != nil {
		return nil, fmt.Errorf("invalid Puter identity response")
	}
	user.UUID = strings.TrimSpace(user.UUID)
	user.Username = strings.TrimSpace(user.Username)
	if user.UUID == "" || user.Username == "" {
		return nil, fmt.Errorf("missing Puter identity")
	}
	return &user, nil
}
