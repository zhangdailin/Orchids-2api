package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type Account struct {
	Name         string `json:"name"`
	AccountType  string `json:"account_type"`
	Token        string `json:"token"`
	SessionID    string `json:"session_id"`
	ClientCookie string `json:"client_cookie,omitempty"`
	Enabled      bool   `json:"enabled"`
}

func main() {
	host := flag.String("host", "http://localhost:3002", "API Host URL")
	user := flag.String("user", "admin", "Admin username")
	pass := flag.String("pass", "admin123", "Admin password")
	jsonStr := flag.String("json", "", "JSON string of the account to add")
	file := flag.String("file", "", "Path to JSON file containing account(s)")
	flag.Parse()

	if *jsonStr == "" && *file == "" {
		fmt.Println("Error: Must provide either -json or -file")
		fmt.Println("Usage:")
		fmt.Println("  go run ./cmd/account-tool -json '{\"name\":\"...\"}'")
		fmt.Println("  go run ./cmd/account-tool -file accounts.json")
		os.Exit(1)
	}

	var accounts []Account

	// Handle raw JSON string (single account)
	if *jsonStr != "" {
		var acc Account
		if err := json.Unmarshal([]byte(*jsonStr), &acc); err != nil {
			fmt.Printf("Error parsing JSON string: %v\n", err)
			os.Exit(1)
		}
		accounts = append(accounts, acc)
	}

	// Handle file input (single account or array)
	if *file != "" {
		data, err := os.ReadFile(*file)
		if err != nil {
			fmt.Printf("Error reading file: %v\n", err)
			os.Exit(1)
		}

		// Try parsing as array first
		var list []Account
		if err := json.Unmarshal(data, &list); err == nil {
			accounts = append(accounts, list...)
		} else {
			// Try parsing as single object
			var acc Account
			if err := json.Unmarshal(data, &acc); err != nil {
				fmt.Printf("Error parsing file as JSON (array or object): %v\n", err)
				os.Exit(1)
			}
			accounts = append(accounts, acc)
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}

	for _, acc := range accounts {
		if strings.TrimSpace(acc.AccountType) == "" {
			acc.AccountType = "warp" // Default to warp for this use case
		}
		
		fmt.Printf("Importing account: %s (%s)...\n", acc.Name, acc.AccountType)
		
		payload, _ := json.Marshal(acc)
		req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/accounts", *host), bytes.NewBuffer(payload))
		if err != nil {
			fmt.Printf("Error creating request: %v\n", err)
			continue
		}

		req.SetBasicAuth(*user, *pass)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("Error sending request: %v\n", err)
			continue
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			fmt.Printf("Success! Account %s added.\n", acc.Name)
		} else {
			fmt.Printf("Failed. Status: %s\nResponse: %s\n", resp.Status, string(body))
		}
	}
}
