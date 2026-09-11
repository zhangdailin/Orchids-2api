package grok

import (
	"bytes"
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestListCacheOnlineAccountsUsesOnlyCanonicalVisibleWebSource(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	defer func() {
		_ = s.Close()
		mini.Close()
	}()

	webHighID := &store.Account{ID: 9, Name: "web-high", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Subscription: "basic"}
	webLowID := &store.Account{ID: 3, Name: "web-low", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderWeb, ClientCookie: "sso=shared", Enabled: true, Subscription: "super", StatusCode: "429"}
	if err := s.CreateAccount(context.Background(), webLowID); err != nil {
		t.Fatalf("CreateAccount(web low) error = %v", err)
	}
	if err := s.CreateAccount(context.Background(), webHighID); err != nil {
		t.Fatalf("CreateAccount(web high) error = %v", err)
	}
	hiddenConsole := &store.Account{Name: "hidden-console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, GrokSSOParentID: webLowID.ID, ClientCookie: "sso=shared", Enabled: true, Subscription: "heavy", StatusCode: "401"}
	standaloneConsole := &store.Account{Name: "standalone-console", AccountType: "grok", CredentialType: "sso", GrokProvider: ProviderConsole, ClientCookie: "sso=standalone", Enabled: true}
	for _, acc := range []*store.Account{hiddenConsole, standaloneConsole} {
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatalf("CreateAccount(%s) error = %v", acc.Name, err)
		}
	}

	accounts := h.listCacheOnlineAccounts(httptest.NewRequest(http.MethodGet, "/api/v1/admin/cache", nil))
	if len(accounts) != 1 {
		t.Fatalf("online accounts=%#v want one Web source", accounts)
	}
	if accounts[0]["token"] != "shared" || accounts[0]["pool"] != "ssoSuper" || accounts[0]["status"] != "cooling" {
		t.Fatalf("online account leaked non-canonical/Console state: %#v", accounts[0])
	}
}

func TestHandleAdminCacheEndpoints(t *testing.T) {
	oldBase := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldBase })

	imageDir := filepath.Join(cacheBaseDir, "image")
	videoDir := filepath.Join(cacheBaseDir, "video")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir image: %v", err)
	}
	if err := os.MkdirAll(videoDir, 0o755); err != nil {
		t.Fatalf("mkdir video: %v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "a.jpg"), []byte("abc"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	if err := os.WriteFile(filepath.Join(videoDir, "b.mp4"), []byte("12345"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}

	h := &Handler{}

	summaryReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cache", nil)
	summaryRec := httptest.NewRecorder()
	h.HandleAdminCache(summaryRec, summaryReq)
	if summaryRec.Code != http.StatusOK {
		t.Fatalf("summary status=%d want=200", summaryRec.Code)
	}
	var summaryResp map[string]interface{}
	if err := json.Unmarshal(summaryRec.Body.Bytes(), &summaryResp); err != nil {
		t.Fatalf("decode summary response: %v", err)
	}
	if _, ok := summaryResp["local_image"]; !ok {
		t.Fatalf("summary missing local_image")
	}
	if _, ok := summaryResp["local_video"]; !ok {
		t.Fatalf("summary missing local_video")
	}
	if got, _ := summaryResp["online_scope"].(string); got != "none" {
		t.Fatalf("online_scope=%q want=none", got)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cache/list", nil)
	listRec := httptest.NewRecorder()
	h.HandleAdminCacheList(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status=%d want=200", listRec.Code)
	}
	var listResp struct {
		Items []cacheEntry `json:"items"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("items=%d want=1", len(listResp.Items))
	}
	if listResp.Items[0].MediaType != "image" {
		t.Fatalf("default list media_type=%q want=image", listResp.Items[0].MediaType)
	}
	if listResp.Items[0].SizeBytes <= 0 || listResp.Items[0].MtimeMS <= 0 {
		t.Fatalf("compat fields not set: size_bytes=%d mtime_ms=%d", listResp.Items[0].SizeBytes, listResp.Items[0].MtimeMS)
	}

	listCompatReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cache/list?type=image&page=1&page_size=1", nil)
	listCompatRec := httptest.NewRecorder()
	h.HandleAdminCacheList(listCompatRec, listCompatReq)
	if listCompatRec.Code != http.StatusOK {
		t.Fatalf("list compat status=%d want=200", listCompatRec.Code)
	}
	var listCompatResp struct {
		Total    int          `json:"total"`
		Page     int          `json:"page"`
		PageSize int          `json:"page_size"`
		Items    []cacheEntry `json:"items"`
	}
	if err := json.Unmarshal(listCompatRec.Body.Bytes(), &listCompatResp); err != nil {
		t.Fatalf("decode list compat response: %v", err)
	}
	if listCompatResp.Total != 1 || listCompatResp.Page != 1 || listCompatResp.PageSize != 1 || len(listCompatResp.Items) != 1 {
		t.Fatalf("unexpected list compat payload: total=%d page=%d page_size=%d items=%d", listCompatResp.Total, listCompatResp.Page, listCompatResp.PageSize, len(listCompatResp.Items))
	}

	deleteBody := map[string]interface{}{
		"type": "image",
		"name": "a.jpg",
	}
	deleteRaw, _ := json.Marshal(deleteBody)
	deleteReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/item/delete", bytes.NewReader(deleteRaw))
	deleteRec := httptest.NewRecorder()
	h.HandleAdminCacheItemDelete(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status=%d want=200", deleteRec.Code)
	}
	if _, err := os.Stat(filepath.Join(imageDir, "a.jpg")); !os.IsNotExist(err) {
		t.Fatalf("image cache file should be removed, err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "c.jpg"), []byte("img"), 0o644); err != nil {
		t.Fatalf("write image c.jpg: %v", err)
	}

	clearReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/clear", bytes.NewReader([]byte(`{}`)))
	clearRec := httptest.NewRecorder()
	h.HandleAdminCacheClear(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear status=%d want=200", clearRec.Code)
	}
	var clearResp struct {
		Result struct {
			Count  int     `json:"count"`
			SizeMB float64 `json:"size_mb"`
			SizeB  int64   `json:"size_bytes"`
		} `json:"result"`
	}
	if err := json.Unmarshal(clearRec.Body.Bytes(), &clearResp); err != nil {
		t.Fatalf("decode clear response: %v", err)
	}
	if clearResp.Result.Count != 1 {
		t.Fatalf("clear result count=%d want=1", clearResp.Result.Count)
	}
	remainImages, err := os.ReadDir(imageDir)
	if err != nil {
		t.Fatalf("readdir image: %v", err)
	}
	if len(remainImages) != 0 {
		t.Fatalf("image cache should be empty, remain=%d", len(remainImages))
	}
	remainVideoBefore, err := os.ReadDir(videoDir)
	if err != nil {
		t.Fatalf("readdir video before typed clear: %v", err)
	}
	if len(remainVideoBefore) != 1 {
		t.Fatalf("video cache should remain 1 before typed clear, remain=%d", len(remainVideoBefore))
	}

	clearVideoReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/clear", bytes.NewReader([]byte(`{"type":"video"}`)))
	clearVideoRec := httptest.NewRecorder()
	h.HandleAdminCacheClear(clearVideoRec, clearVideoReq)
	if clearVideoRec.Code != http.StatusOK {
		t.Fatalf("clear video status=%d want=200", clearVideoRec.Code)
	}

	remain, err := os.ReadDir(videoDir)
	if err != nil {
		t.Fatalf("readdir video: %v", err)
	}
	if len(remain) != 0 {
		t.Fatalf("video cache should be empty, remain=%d", len(remain))
	}
}

func TestHandleAdminCache_OnlineSelectedRealtimeCount(t *testing.T) {
	requests := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/assets" {
			http.NotFound(w, r)
			return
		}
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("pageToken") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"assets": []map[string]interface{}{
					{"assetId": "asset-1"},
					{"assetId": "asset-2"},
				},
				"nextPageToken": "next-1",
			})
		case "next-1":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"assets": []map[string]interface{}{
					{"assetId": "asset-3"},
				},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"assets": []map[string]interface{}{},
			})
		}
	}))
	defer upstream.Close()

	h := &Handler{
		client: New(&config.Config{GrokAPIBaseURL: upstream.URL}),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cache?tokens=sso=test-token", nil)
	rec := httptest.NewRecorder()
	h.HandleAdminCache(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		OnlineScope string `json:"online_scope"`
		Online      struct {
			Count  int    `json:"count"`
			Status string `json:"status"`
		} `json:"online"`
		OnlineDetails []struct {
			Token  string `json:"token"`
			Count  int    `json:"count"`
			Status string `json:"status"`
		} `json:"online_details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.OnlineScope != "selected" {
		t.Fatalf("online_scope=%q want=selected", out.OnlineScope)
	}
	if out.Online.Count != 3 || out.Online.Status != "ok" {
		t.Fatalf("online mismatch: count=%d status=%q", out.Online.Count, out.Online.Status)
	}
	if len(out.OnlineDetails) != 1 {
		t.Fatalf("online_details len=%d want=1", len(out.OnlineDetails))
	}
	if out.OnlineDetails[0].Token != "test-token" || out.OnlineDetails[0].Count != 3 || out.OnlineDetails[0].Status != "ok" {
		t.Fatalf("online detail mismatch: %+v", out.OnlineDetails[0])
	}
	if requests != 2 {
		t.Fatalf("upstream requests=%d want=2", requests)
	}
}

func TestCacheOnlineAccountMap_NormalizesTokens(t *testing.T) {
	accounts := []map[string]interface{}{
		{
			"token":               "sso=t1",
			"token_masked":        "masked-t1",
			"last_asset_clear_at": int64(123),
		},
		{
			"token":               "foo=1; sso=t2; bar=2",
			"token_masked":        "masked-t2",
			"last_asset_clear_at": int64(456),
		},
	}

	accountByToken := cacheOnlineAccountMap(accounts)
	if _, ok := accountByToken["sso=t1"]; ok {
		t.Fatalf("map should not keep cookie-style key")
	}
	if _, ok := accountByToken["foo=1; sso=t2; bar=2"]; ok {
		t.Fatalf("map should not keep full cookie key")
	}

	masked, lastClear := onlineAccountInfo(accountByToken, "t1")
	if masked != "masked-t1" || lastClear != int64(123) {
		t.Fatalf("t1 info mismatch masked=%q lastClear=%v", masked, lastClear)
	}
	masked, lastClear = onlineAccountInfo(accountByToken, "t2")
	if masked != "masked-t2" || lastClear != int64(456) {
		t.Fatalf("t2 info mismatch masked=%q lastClear=%v", masked, lastClear)
	}
}

func TestHandleAdminCacheOnlineClear(t *testing.T) {
	type state struct {
		mu     sync.Mutex
		assets map[string][]string
	}
	s := &state{
		assets: map[string][]string{
			"t1": []string{"a1", "a2"},
			"t2": []string{"b1"},
		},
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		token := ""
		for _, part := range strings.Split(cookie, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "sso=") {
				token = strings.TrimPrefix(part, "sso=")
				break
			}
		}
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/assets":
			s.mu.Lock()
			defer s.mu.Unlock()
			items := make([]map[string]interface{}, 0, len(s.assets[token]))
			for _, id := range s.assets[token] {
				items = append(items, map[string]interface{}{"assetId": id})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"assets": items})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/rest/assets-metadata/"):
			s.mu.Lock()
			defer s.mu.Unlock()
			id := strings.TrimPrefix(r.URL.Path, "/rest/assets-metadata/")
			list := s.assets[token]
			next := make([]string, 0, len(list))
			for _, item := range list {
				if item != id {
					next = append(next, item)
				}
			}
			s.assets[token] = next
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := &Handler{
		client: New(&config.Config{GrokAPIBaseURL: upstream.URL}),
	}

	body := []byte(`{"tokens":["sso=t1","sso=t2"]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/online/clear", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleAdminCacheOnlineClear(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Status  string                            `json:"status"`
		Result  map[string]interface{}            `json:"result"`
		Results map[string]map[string]interface{} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q want=success", out.Status)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results len=%d want=2", len(out.Results))
	}
	for _, token := range []string{"t1", "t2"} {
		item, ok := out.Results[token]
		if !ok {
			t.Fatalf("missing result for token %s", token)
		}
		if got, _ := item["status"].(string); got != "success" {
			t.Fatalf("token %s status=%q want=success", token, got)
		}
	}
	if got, _ := out.Result["total"].(float64); int(got) != 3 {
		t.Fatalf("aggregate total=%v want=3", out.Result["total"])
	}
	if got, _ := out.Result["success"].(float64); int(got) != 3 {
		t.Fatalf("aggregate success=%v want=3", out.Result["success"])
	}
	if got, _ := out.Result["failed"].(float64); int(got) != 0 {
		t.Fatalf("aggregate failed=%v want=0", out.Result["failed"])
	}

	reqStats := httptest.NewRequest(http.MethodGet, "/api/v1/admin/cache?tokens=t1,t2", nil)
	recStats := httptest.NewRecorder()
	h.HandleAdminCache(recStats, reqStats)
	if recStats.Code != http.StatusOK {
		t.Fatalf("stats status=%d want=200", recStats.Code)
	}
	var stats struct {
		OnlineDetails []struct {
			Token   string      `json:"token"`
			LastClr interface{} `json:"last_asset_clear_at"`
		} `json:"online_details"`
	}
	if err := json.Unmarshal(recStats.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats response: %v", err)
	}
	if len(stats.OnlineDetails) != 2 {
		t.Fatalf("online_details len=%d want=2", len(stats.OnlineDetails))
	}
	for _, item := range stats.OnlineDetails {
		if item.LastClr == nil {
			t.Fatalf("token %s last_asset_clear_at is nil", item.Token)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.assets["t1"]) != 0 || len(s.assets["t2"]) != 0 {
		t.Fatalf("assets not fully deleted: t1=%v t2=%v", s.assets["t1"], s.assets["t2"])
	}
}

func TestResolveCacheOnlineClearTargets(t *testing.T) {
	tokens, mode, errMsg := resolveCacheOnlineClearTargets(cacheOnlineClearRequest{
		Tokens: []string{"sso=t2", "t1", "  ", "t1"},
	}, nil)
	if errMsg != "" {
		t.Fatalf("unexpected errMsg=%q", errMsg)
	}
	if mode != "batch" {
		t.Fatalf("mode=%q want=batch", mode)
	}
	if len(tokens) != 2 || tokens[0] != "t2" || tokens[1] != "t1" {
		t.Fatalf("tokens=%v want=[t2 t1]", tokens)
	}

	_, _, errMsg = resolveCacheOnlineClearTargets(cacheOnlineClearRequest{
		Tokens: []string{"", "   "},
	}, nil)
	if errMsg != "No tokens provided" {
		t.Fatalf("empty batch err=%q want=%q", errMsg, "No tokens provided")
	}

	tokens, mode, errMsg = resolveCacheOnlineClearTargets(cacheOnlineClearRequest{
		Token: "sso=single",
	}, nil)
	if errMsg != "" {
		t.Fatalf("single token err=%q", errMsg)
	}
	if mode != "single" || len(tokens) != 1 || tokens[0] != "single" {
		t.Fatalf("single token mismatch: mode=%q tokens=%v", mode, tokens)
	}

	tokens, mode, errMsg = resolveCacheOnlineClearTargets(cacheOnlineClearRequest{}, []map[string]interface{}{
		{"token": "sso=fallback-a"},
		{"token": "fallback-b"},
	})
	if errMsg != "" {
		t.Fatalf("fallback err=%q", errMsg)
	}
	if mode != "single" || len(tokens) != 1 || tokens[0] != "fallback-a" {
		t.Fatalf("fallback mismatch: mode=%q tokens=%v", mode, tokens)
	}

	_, _, errMsg = resolveCacheOnlineClearTargets(cacheOnlineClearRequest{}, nil)
	if errMsg != "No available token to perform cleanup" {
		t.Fatalf("missing-token err=%q want=%q", errMsg, "No available token to perform cleanup")
	}
}

func TestHandleAdminCacheOnlineClear_EmptyTokensList(t *testing.T) {
	h := &Handler{client: New(nil)}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/online/clear", bytes.NewReader([]byte(`{"tokens":[]}`)))
	rec := httptest.NewRecorder()
	h.HandleAdminCacheOnlineClear(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want=400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "No tokens provided") {
		t.Fatalf("body=%q want contains %q", rec.Body.String(), "No tokens provided")
	}
}

func TestHandleAdminCacheOnlineClear_SingleErrorStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusForbidden)
	}))
	defer upstream.Close()

	h := &Handler{
		client: New(&config.Config{GrokAPIBaseURL: upstream.URL}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/online/clear", bytes.NewReader([]byte(`{"token":"sso=t1"}`)))
	rec := httptest.NewRecorder()
	h.HandleAdminCacheOnlineClear(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Status != "error" {
		t.Fatalf("status=%q want=error body=%s", out.Status, rec.Body.String())
	}
	if strings.TrimSpace(out.Error) == "" {
		t.Fatalf("error should not be empty, body=%s", rec.Body.String())
	}
}

func TestHandleAdminCacheOnlineLoadAsync(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/rest/assets" {
			http.NotFound(w, r)
			return
		}
		cookie := r.Header.Get("Cookie")
		token := ""
		for _, part := range strings.Split(cookie, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "sso=") {
				token = strings.TrimPrefix(part, "sso=")
				break
			}
		}
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}

		var assets []map[string]interface{}
		switch token {
		case "t1":
			assets = []map[string]interface{}{
				{"assetId": "a1"},
				{"assetId": "a2"},
			}
		case "t2":
			assets = []map[string]interface{}{
				{"assetId": "b1"},
			}
		default:
			assets = []map[string]interface{}{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"assets": assets})
	}))
	defer upstream.Close()

	h := &Handler{
		client: New(&config.Config{GrokAPIBaseURL: upstream.URL}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/online/load/async", bytes.NewReader([]byte(`{"tokens":["sso=t1","t2"]}`)))
	rec := httptest.NewRecorder()
	h.HandleAdminCacheOnlineLoadAsync(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var start struct {
		Status string `json:"status"`
		TaskID string `json:"task_id"`
		Total  int    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if start.Status != "success" || start.TaskID == "" || start.Total != 2 {
		t.Fatalf("unexpected start response: %+v", start)
	}

	snapshot := waitBatchTaskSnapshot(t, start.TaskID, 3*time.Second)
	raw, _ := json.Marshal(snapshot)
	var out struct {
		Status    string `json:"status"`
		Processed int    `json:"processed"`
		Total     int    `json:"total"`
		Result    struct {
			OnlineScope string `json:"online_scope"`
			Online      struct {
				Count  int    `json:"count"`
				Status string `json:"status"`
			} `json:"online"`
			OnlineDetails []struct {
				Token  string `json:"token"`
				Count  int    `json:"count"`
				Status string `json:"status"`
			} `json:"online_details"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode snapshot: %v raw=%s", err, string(raw))
	}
	if out.Status != "done" {
		t.Fatalf("final status=%q want=done", out.Status)
	}
	if out.Total != 2 || out.Processed != 2 {
		t.Fatalf("progress mismatch total=%d processed=%d", out.Total, out.Processed)
	}
	if out.Result.OnlineScope != "selected" {
		t.Fatalf("online_scope=%q want=selected", out.Result.OnlineScope)
	}
	if out.Result.Online.Count != 3 || out.Result.Online.Status != "ok" {
		t.Fatalf("online mismatch: count=%d status=%q", out.Result.Online.Count, out.Result.Online.Status)
	}
	if len(out.Result.OnlineDetails) != 2 {
		t.Fatalf("online_details len=%d want=2", len(out.Result.OnlineDetails))
	}
}

func TestHandleAdminCacheOnlineClearAsync(t *testing.T) {
	type state struct {
		mu     sync.Mutex
		assets map[string][]string
	}
	s := &state{
		assets: map[string][]string{
			"t1": []string{"a1", "a2"},
			"t2": []string{"b1"},
		},
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		token := ""
		for _, part := range strings.Split(cookie, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "sso=") {
				token = strings.TrimPrefix(part, "sso=")
				break
			}
		}
		if token == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/assets":
			s.mu.Lock()
			defer s.mu.Unlock()
			items := make([]map[string]interface{}, 0, len(s.assets[token]))
			for _, id := range s.assets[token] {
				items = append(items, map[string]interface{}{"assetId": id})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"assets": items})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/rest/assets-metadata/"):
			s.mu.Lock()
			defer s.mu.Unlock()
			id := strings.TrimPrefix(r.URL.Path, "/rest/assets-metadata/")
			list := s.assets[token]
			next := make([]string, 0, len(list))
			for _, item := range list {
				if item != id {
					next = append(next, item)
				}
			}
			s.assets[token] = next
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	h := &Handler{
		client: New(&config.Config{GrokAPIBaseURL: upstream.URL}),
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/online/clear/async", bytes.NewReader([]byte(`{"tokens":["sso=t1","t2"]}`)))
	rec := httptest.NewRecorder()
	h.HandleAdminCacheOnlineClearAsync(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=200 body=%s", rec.Code, rec.Body.String())
	}

	var start struct {
		Status string `json:"status"`
		TaskID string `json:"task_id"`
		Total  int    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if start.Status != "success" || start.TaskID == "" || start.Total != 2 {
		t.Fatalf("unexpected start response: %+v", start)
	}

	snapshot := waitBatchTaskSnapshot(t, start.TaskID, 5*time.Second)
	raw, _ := json.Marshal(snapshot)
	var out struct {
		Status    string `json:"status"`
		Processed int    `json:"processed"`
		Total     int    `json:"total"`
		Result    struct {
			Status  string `json:"status"`
			Summary struct {
				Total int `json:"total"`
				OK    int `json:"ok"`
				Fail  int `json:"fail"`
			} `json:"summary"`
			Results map[string]struct {
				Status string                 `json:"status"`
				Result map[string]interface{} `json:"result"`
			} `json:"results"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode snapshot: %v raw=%s", err, string(raw))
	}
	if out.Status != "done" {
		t.Fatalf("final status=%q want=done", out.Status)
	}
	if out.Total != 2 || out.Processed != 2 {
		t.Fatalf("progress mismatch total=%d processed=%d", out.Total, out.Processed)
	}
	if out.Result.Status != "success" {
		t.Fatalf("result.status=%q want=success", out.Result.Status)
	}
	if out.Result.Summary.Total != 2 || out.Result.Summary.OK != 2 || out.Result.Summary.Fail != 0 {
		t.Fatalf("summary mismatch: %+v", out.Result.Summary)
	}
	if len(out.Result.Results) != 2 {
		t.Fatalf("results len=%d want=2", len(out.Result.Results))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.assets["t1"]) != 0 || len(s.assets["t2"]) != 0 {
		t.Fatalf("assets not fully deleted: t1=%v t2=%v", s.assets["t1"], s.assets["t2"])
	}
}

func TestHandleAdminCacheOnlineAsyncMissingTokens(t *testing.T) {
	h := &Handler{client: New(nil)}

	loadReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/online/load/async", bytes.NewReader([]byte(`{}`)))
	loadRec := httptest.NewRecorder()
	h.HandleAdminCacheOnlineLoadAsync(loadRec, loadReq)
	if loadRec.Code != http.StatusBadRequest {
		t.Fatalf("load async status=%d want=400 body=%s", loadRec.Code, loadRec.Body.String())
	}

	clearReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/cache/online/clear/async", bytes.NewReader([]byte(`{}`)))
	clearRec := httptest.NewRecorder()
	h.HandleAdminCacheOnlineClearAsync(clearRec, clearReq)
	if clearRec.Code != http.StatusBadRequest {
		t.Fatalf("clear async status=%d want=400 body=%s", clearRec.Code, clearRec.Body.String())
	}
}

func waitBatchTaskSnapshot(t *testing.T, taskID string, timeout time.Duration) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, ok := getNSFWBatchTask(taskID)
		if ok {
			snapshot := task.snapshot()
			status := strings.ToLower(strings.TrimSpace(fmt.Sprint(snapshot["status"])))
			if status == "done" || status == "cancelled" || status == "error" {
				return snapshot
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s not finished within %s", taskID, timeout)
	return nil
}
