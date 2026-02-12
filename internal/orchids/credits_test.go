package orchids

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFetchCreditsRaw(t *testing.T) {
	// Use a fresh session JWT to test - replace with a valid one
	sessionJWT := "" // Set via env or manually
	if sessionJWT == "" {
		t.Skip("No session JWT provided, skipping live test")
	}

	ctx := context.Background()
	req, err := http.NewRequestWithContext(ctx, "POST", orchidsAppURL, strings.NewReader(orchidsActionID))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Accept", "text/x-component")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("User-Agent", orchidsUserAgent)
	req.Header.Set("Next-Action", orchidsActionID)
	req.Header.Set("Next-Router-State-Tree", `["",{"children":["__PAGE__",{},null,null]},null,null,true]`)
	req.Header.Set("Origin", "https://www.orchids.app")
	req.Header.Set("Referer", "https://www.orchids.app/")
	req.AddCookie(&http.Cookie{Name: "__session", Value: sessionJWT})

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	fmt.Println("Status:", resp.StatusCode)
	fmt.Println("Content-Type:", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	fmt.Println("=== RAW RESPONSE ===")
	fmt.Println(string(body))
	fmt.Println("=== END ===")

	// Try parsing
	info, err := parseRSCCredits(string(body))
	if err != nil {
		t.Log("Parse error:", err)
	} else {
		fmt.Printf("Credits: %.0f, Plan: %s\n", info.Credits, info.Plan)
	}
}
