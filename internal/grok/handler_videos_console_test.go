package grok

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/goccy/go-json"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestPrepareConsoleVideoGeneration(t *testing.T) {
	prepared, err := prepareConsoleVideoRequest(consoleVideoAPIRequest{
		Model:       "grok-imagine-video-1.5",
		Prompt:      "a paper boat on a river",
		Duration:    json.RawMessage(`"12"`),
		AspectRatio: "4:3",
		Resolution:  "1080P",
		Image:       &consoleVideoInput{URL: "https://example.com/input.png"},
	}, consoleVideoGenerate)
	if err != nil {
		t.Fatalf("prepareConsoleVideoRequest() error = %v", err)
	}
	if prepared.duration != 12 || prepared.resolution != "1080p" {
		t.Fatalf("prepared = %#v", prepared)
	}
	if prepared.payload["duration"] != 12 || prepared.payload["aspect_ratio"] != "4:3" {
		t.Fatalf("payload = %#v", prepared.payload)
	}

	defaults, err := prepareConsoleVideoRequest(consoleVideoAPIRequest{
		Model: "grok-imagine-video", Prompt: "a quiet forest",
	}, consoleVideoGenerate)
	if err != nil {
		t.Fatalf("default generation error = %v", err)
	}
	if defaults.duration != 8 || defaults.resolution != "720p" || defaults.payload["aspect_ratio"] != "16:9" {
		t.Fatalf("defaults = %#v", defaults)
	}
}

func TestPrepareConsoleVideoGenerationRejectsInvalidCombinations(t *testing.T) {
	tests := []struct {
		name    string
		request consoleVideoAPIRequest
		want    string
	}{
		{
			name:    "base model 1080p",
			request: consoleVideoAPIRequest{Model: "grok-imagine-video", Prompt: "x", Resolution: "1080p"},
			want:    "does not support 1080p",
		},
		{
			name: "image and references",
			request: consoleVideoAPIRequest{Model: "grok-imagine-video", Prompt: "x",
				Image:           &consoleVideoInput{URL: "https://example.com/a.png"},
				ReferenceImages: []consoleVideoInput{{URL: "https://example.com/b.png"}}},
			want: "cannot be combined",
		},
		{
			name: "reference needs prompt",
			request: consoleVideoAPIRequest{Model: "grok-imagine-video",
				ReferenceAudios: []consoleVideoReferenceAudio{{VoiceID: "voice-1"}}},
			want: "requires prompt",
		},
		{
			name: "reference base duration",
			request: consoleVideoAPIRequest{Model: "grok-imagine-video", Prompt: "x", Duration: json.RawMessage(`15`),
				ReferenceImages: []consoleVideoInput{{URL: "https://example.com/a.png"}}},
			want: "at most 10 seconds",
		},
		{
			name:    "invalid file id",
			request: consoleVideoAPIRequest{Model: "grok-imagine-video", Image: &consoleVideoInput{FileID: "file_123"}},
			want:    "file_id is invalid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := prepareConsoleVideoRequest(test.request, consoleVideoGenerate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestPrepareConsoleVideoAcceptsMediaInputFileID(t *testing.T) {
	fileID := "input_abcdefghijklmnopqrstuvwxyz012345"
	prepared, err := prepareConsoleVideoRequest(consoleVideoAPIRequest{
		Model: "grok-imagine-video", Image: &consoleVideoInput{FileID: fileID},
	}, consoleVideoGenerate)
	if err != nil {
		t.Fatalf("prepareConsoleVideoRequest() error = %v", err)
	}
	image := prepared.payload["image"].(map[string]interface{})
	if image["url"] != localMediaInputPrefix+"image:"+fileID {
		t.Fatalf("image payload = %#v", image)
	}
}

func TestMediaInputUploadAndConsoleFileIDResolution(t *testing.T) {
	oldCacheBaseDir := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldCacheBaseDir })
	h, s, mini := setupValidationHandler(t)
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})
	pngData, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "pixel.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(pngData)
	_ = writer.Close()

	validator := func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: 1}, nil
	}
	key := "sk-media-owner"
	upload := middleware.APIKeyAuth(func() bool { return true }, validator, h.HandleMediaInputs)
	req := httptest.NewRequest(http.MethodPost, "/v1/media/inputs", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	upload(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	var response struct {
		FileID string `json:"file_id"`
		Kind   string `json:"kind"`
	}
	if json.Unmarshal(recorder.Body.Bytes(), &response) != nil || !validMediaInputID(response.FileID) || response.Kind != "image" {
		t.Fatalf("upload response=%q", recorder.Body.String())
	}
	digest := sha256.Sum256([]byte(key))
	owner := hex.EncodeToString(digest[:])
	stored, err := s.GetStoredMediaInput(context.Background(), response.FileID, owner)
	if err != nil {
		t.Fatalf("GetStoredMediaInput() error = %v", err)
	}
	if _, err := os.Stat(stored.ContentPath); err != nil {
		t.Fatalf("cached input missing: %v", err)
	}
	payload := map[string]interface{}{
		"image": map[string]interface{}{"url": localMediaInputPrefix + "image:" + response.FileID},
	}
	if err := h.resolveConsoleVideoFileIDs(context.Background(), payload, owner); err != nil {
		t.Fatalf("resolveConsoleVideoFileIDs() error = %v", err)
	}
	resolved := payload["image"].(map[string]interface{})["url"].(string)
	if !strings.HasPrefix(resolved, "data:image/png;base64,") {
		t.Fatalf("resolved image = %q", resolved)
	}
	wrongOwner := strings.Repeat("0", 64)
	buildReferences, err := h.resolveBuildVideoReferences(context.Background(), []string{response.FileID}, owner)
	if err != nil || len(buildReferences) != 1 || buildReferences[0] != resolved {
		t.Fatalf("Build owned input resolution: %v %v", buildReferences, err)
	}
	if _, err := h.resolveBuildVideoReferences(context.Background(), []string{response.FileID}, wrongOwner); err == nil {
		t.Fatal("Build must reject another API key's media input")
	}
	wrongPayload := map[string]interface{}{
		"image": map[string]interface{}{"url": localMediaInputPrefix + "image:" + response.FileID},
	}
	if err := h.resolveConsoleVideoFileIDs(context.Background(), wrongPayload, wrongOwner); err == nil {
		t.Fatal("cross-owner file_id resolution unexpectedly succeeded")
	}
	wrongKind := map[string]interface{}{
		"video": map[string]interface{}{"url": localMediaInputPrefix + "video:" + response.FileID},
	}
	if err := h.resolveConsoleVideoFileIDs(context.Background(), wrongKind, owner); err == nil || !strings.Contains(err.Error(), "video media") {
		t.Fatalf("wrong-kind resolution error = %v", err)
	}

	resource := middleware.APIKeyAuth(func() bool { return true }, validator, h.HandleMediaInputResource)
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v1/media/inputs/"+response.FileID, nil)
	deleteReq.Header.Set("Authorization", "Bearer "+key)
	deleted := httptest.NewRecorder()
	resource(deleted, deleteReq)
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("delete status=%d body=%q", deleted.Code, deleted.Body.String())
	}
	if _, err := os.Stat(stored.ContentPath); !os.IsNotExist(err) {
		t.Fatalf("deleted media file still exists: %v", err)
	}
}

func TestDetectMediaInputTypes(t *testing.T) {
	tests := []struct {
		name, declared, wantKind, wantMIME string
		data                               []byte
		wantErr                            bool
	}{
		{name: "png", data: append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...), wantKind: "image", wantMIME: "image/png"},
		{name: "mp4", data: []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, wantKind: "video", wantMIME: "video/mp4"},
		{name: "quicktime", declared: "video/quicktime", data: []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'q', 't', ' ', ' '}, wantKind: "video", wantMIME: "video/quicktime"},
		{name: "webm", data: []byte{0x1a, 0x45, 0xdf, 0xa3, 0x01}, wantKind: "video", wantMIME: "video/webm"},
		{name: "junk", data: []byte("not media"), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, mimeType, err := detectMediaInput(test.data, test.declared)
			if test.wantErr {
				if err == nil {
					t.Fatalf("detectMediaInput() = %q, %q", kind, mimeType)
				}
				return
			}
			if err != nil || kind != test.wantKind || mimeType != test.wantMIME {
				t.Fatalf("detectMediaInput() = %q, %q, %v", kind, mimeType, err)
			}
		})
	}
}

func TestPrepareConsoleVideoEditAndExtension(t *testing.T) {
	base := consoleVideoAPIRequest{
		Model: "grok-imagine-video", Prompt: "add falling snow",
		Video: &consoleVideoInput{URL: "https://example.com/source.mp4"},
	}
	edit, err := prepareConsoleVideoRequest(base, consoleVideoEdit)
	if err != nil {
		t.Fatalf("edit error = %v", err)
	}
	if _, exists := edit.payload["duration"]; exists {
		t.Fatalf("edit payload unexpectedly contains duration: %#v", edit.payload)
	}

	extend, err := prepareConsoleVideoRequest(base, consoleVideoExtend)
	if err != nil {
		t.Fatalf("extension error = %v", err)
	}
	if extend.duration != 6 || extend.payload["duration"] != 6 {
		t.Fatalf("extension = %#v", extend)
	}

	base.Duration = json.RawMessage(`3`)
	if _, err := prepareConsoleVideoRequest(base, consoleVideoEdit); err == nil || !strings.Contains(err.Error(), "does not support duration") {
		t.Fatalf("edit duration error = %v", err)
	}
	base.Duration = json.RawMessage(`11`)
	if _, err := prepareConsoleVideoRequest(base, consoleVideoExtend); err == nil || !strings.Contains(err.Error(), "from 2 to 10") {
		t.Fatalf("extension duration error = %v", err)
	}
	base.Model = "grok-imagine-video-1.5"
	base.Duration = nil
	if _, err := prepareConsoleVideoRequest(base, consoleVideoEdit); err == nil || !strings.Contains(err.Error(), "only supports grok-imagine-video") {
		t.Fatalf("1.5 edit error = %v", err)
	}
}

func TestConsoleVideoHandlerRejectsUnsupportedAndUnknownParameters(t *testing.T) {
	h := NewHandler(&config.Config{}, nil)
	tests := []struct {
		name        string
		contentType string
		body        string
		status      int
		want        string
	}{
		{
			name: "unsupported output", contentType: "application/json",
			body:   `{"model":"grok-imagine-video","prompt":"x","output":{"upload_url":"https://example.com"}}`,
			status: http.StatusBadRequest, want: `"code":"unsupported_parameter"`,
		},
		{
			name: "unknown field", contentType: "application/json; charset=utf-8",
			body:   `{"model":"grok-imagine-video","prompt":"x","unknown":true}`,
			status: http.StatusBadRequest, want: "invalid video JSON request",
		},
		{
			name: "wrong media type", contentType: "application/json-patch+json",
			body:   `{"model":"grok-imagine-video","prompt":"x"}`,
			status: http.StatusUnsupportedMediaType, want: "require application/json",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(test.body))
			req.Header.Set("Content-Type", test.contentType)
			recorder := httptest.NewRecorder()
			h.HandleConsoleVideosGenerate(recorder, req)
			if recorder.Code != test.status || !strings.Contains(recorder.Body.String(), test.want) {
				t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestStandardVideoResponseShape(t *testing.T) {
	job := &videoJob{
		ID: "video_shape", Model: "grok-imagine-video-1.5", Seconds: 12,
		Status: "completed", Progress: 100, Operation: "generate", StandardAPI: true,
		ContentPath: "unused.mp4",
	}
	payload := job.toMap()
	video, ok := payload["video"].(map[string]interface{})
	if payload["status"] != "done" || payload["progress"] != 100 || !ok {
		t.Fatalf("payload = %#v", payload)
	}
	if video["url"] != "/v1/videos/video_shape/content" || video["duration"] != 12 || video["respect_moderation"] != true {
		t.Fatalf("video = %#v", video)
	}
	job.Operation = "edit"
	if edited := job.toMap()["video"].(map[string]interface{}); edited["duration"] != nil {
		t.Fatalf("edited response unexpectedly has duration: %#v", edited)
	}
}

func TestRunConsoleVideoJobEndToEnd(t *testing.T) {
	oldCacheBaseDir := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldCacheBaseDir })

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/videos/edits":
			if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ") || r.Header.Get("DPoP") == "" {
				t.Errorf("create request = %s headers=%#v", r.Method, r.Header)
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"prompt":"add snow"`) {
				t.Errorf("create body = %s", body)
			}
			writeJSON(w, map[string]interface{}{"request_id": "upstream-1"})
		case "/v1/videos/upstream-1":
			writeJSON(w, map[string]interface{}{
				"status": "done", "progress": 100,
				"video": map[string]interface{}{"url": server.URL + "/asset.mp4"},
			})
		case "/asset.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("mock-video"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	h := NewHandler(&config.Config{GrokConsoleBaseURL: server.URL + "/v1"}, nil)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	token := "console-sso"
	h.client.dpop.store(dpopCacheKey(token), dpopSession{
		accessToken: "access-token", privateKey: key, publicJWK: publicDPoPJWK(&key.PublicKey), expiresAt: time.Now().Add(time.Minute),
	})
	job := &videoJob{ID: "video_test", Model: "grok-imagine-video", CreatedAt: time.Now().Unix(), Status: "queued", Operation: "edit", StandardAPI: true}
	baseCtx, cancel := context.WithTimeout(context.Background(), videoJobTTL)
	defer cancel()
	leaseCtx, lease, acquired, err := h.beginConsoleVideoJobLease(baseCtx, job)
	if err != nil || !acquired {
		t.Fatalf("beginConsoleVideoJobLease() acquired=%v err=%v", acquired, err)
	}
	h.runConsoleVideoJobWithLease(leaseCtx, lease, job, &chatAccountSession{token: token}, consoleVideoEdit, map[string]interface{}{
		"model": "grok-imagine-video", "prompt": "add snow", "video": map[string]interface{}{"url": "https://example.com/source.mp4"},
	})
	if job.Status != "completed" || job.Progress != 100 || job.RemixedFromID != "upstream-1" {
		t.Fatalf("job = %#v", job)
	}
	data, err := os.ReadFile(job.ContentPath)
	if err != nil || string(data) != "mock-video" {
		t.Fatalf("cached video data=%q err=%v path=%s", data, err, job.ContentPath)
	}
	if filepath.Dir(job.ContentPath) != filepath.Join(cacheBaseDir, "video") {
		t.Fatalf("content path = %s", job.ContentPath)
	}
}

func TestRecoverStoredConsoleVideoJobPollsAndCompletes(t *testing.T) {
	oldCacheBaseDir := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldCacheBaseDir })

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/videos/upstream-resume":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ") || r.Header.Get("DPoP") == "" {
				t.Errorf("missing DPoP headers: %#v", r.Header)
			}
			writeJSON(w, map[string]interface{}{
				"status": "done", "progress": 100,
				"video": map[string]interface{}{"url": server.URL + "/resumed.mp4"},
			})
		case "/resumed.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("resumed-video"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "video-resume:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	account := &store.Account{
		Name: "console", AccountType: "grok", GrokProvider: ProviderConsole,
		ClientCookie: "sso=resume-token", Enabled: true, Subscription: "super", Weight: 1,
		GrokModels: []string{"grok-imagine-video"},
	}
	if err := s.CreateAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(&config.Config{GrokConsoleBaseURL: server.URL + "/v1"}, loadbalancer.NewWithCacheTTL(s, 0))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.client.dpop.store(dpopCacheKey(account.ClientCookie), dpopSession{
		accessToken: "access-token", privateKey: key, publicJWK: publicDPoPJWK(&key.PublicKey), expiresAt: time.Now().Add(time.Minute),
	})
	record := &store.StoredVideoJob{
		ID: "video_resume", OwnerHash: "anonymous", AccountID: account.ID, Provider: ProviderConsole,
		Model: "grok-imagine-video", Operation: "edit", StandardAPI: true,
		Status: "in_progress", Progress: 42, UpstreamRequestID: "upstream-resume", CreatedAt: time.Now().Unix(),
	}
	if err := s.SaveStoredVideoJob(context.Background(), record, videoJobTTL); err != nil {
		t.Fatal(err)
	}
	videoJobsMu.Lock()
	delete(videoJobs, record.ID)
	videoJobsMu.Unlock()
	h.recoverStoredConsoleVideoJobs(context.Background())

	deadline := time.Now().Add(5 * time.Second)
	for {
		stored, getErr := s.GetStoredVideoJob(context.Background(), record.ID, record.OwnerHash)
		if getErr == nil && stored.Status == "completed" {
			data, readErr := os.ReadFile(stored.ContentPath)
			if readErr != nil || string(data) != "resumed-video" || stored.RemixedFromID != "upstream-resume" {
				t.Fatalf("stored=%#v data=%q err=%v", stored, data, readErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered job did not complete: err=%v", getErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	videoJobsMu.Lock()
	delete(videoJobs, record.ID)
	videoJobsMu.Unlock()
}

func TestRecoveryFailsTaskWithoutUpstreamRequestID(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})
	record := &store.StoredVideoJob{
		ID: "video_interrupted", OwnerHash: "anonymous", AccountID: 1, Provider: ProviderConsole,
		Model: "grok-imagine-video", StandardAPI: true, Status: "queued", CreatedAt: time.Now().Unix(),
	}
	if err := s.SaveStoredVideoJob(context.Background(), record, videoJobTTL); err != nil {
		t.Fatal(err)
	}
	videoJobsMu.Lock()
	delete(videoJobs, record.ID)
	videoJobsMu.Unlock()
	h.recoverStoredConsoleVideoJobs(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for {
		stored, getErr := s.GetStoredVideoJob(context.Background(), record.ID, record.OwnerHash)
		if getErr == nil && stored.Status == "failed" && stored.ErrorCode == "video_resume_unavailable" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stored = %#v, %v", stored, getErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	videoJobsMu.Lock()
	delete(videoJobs, record.ID)
	videoJobsMu.Unlock()
}

func TestConsoleVideoLeaseLossCancelsWorkerWithoutReleasingNewHolder(t *testing.T) {
	h, s, mini := setupValidationHandler(t)
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})
	oldTTL := consoleVideoLeaseTTL
	consoleVideoLeaseTTL = 120 * time.Millisecond
	t.Cleanup(func() { consoleVideoLeaseTTL = oldTTL })
	job := &videoJob{ID: "video_lease_loss", OwnerHash: "anonymous", CreatedAt: time.Now().Unix()}
	leaseCtx, lease, acquired, err := h.beginConsoleVideoJobLease(context.Background(), job)
	if err != nil || !acquired {
		t.Fatalf("beginConsoleVideoJobLease() = %v, %v", acquired, err)
	}
	if released, err := s.ReleaseVideoJobLease(context.Background(), job.ID, job.OwnerHash, lease.holder); err != nil || !released {
		t.Fatalf("release original lease = %v, %v", released, err)
	}
	if acquired, err := s.AcquireVideoJobLease(context.Background(), job.ID, job.OwnerHash, "new-instance", time.Second); err != nil || !acquired {
		t.Fatalf("new holder acquire = %v, %v", acquired, err)
	}
	select {
	case <-leaseCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel worker context")
	}
	if !lease.Lost() {
		t.Fatal("lease was canceled without recording loss")
	}
	lease.Close()
	if refreshed, err := s.RefreshVideoJobLease(context.Background(), job.ID, job.OwnerHash, "new-instance", time.Second); err != nil || !refreshed {
		t.Fatalf("new holder lease was released by old worker: %v, %v", refreshed, err)
	}
	_, _ = s.ReleaseVideoJobLease(context.Background(), job.ID, job.OwnerHash, "new-instance")
}

func TestTrustedConsoleVideoURL(t *testing.T) {
	tests := []struct {
		url, console string
		want         bool
	}{
		{"https://vidgen.x.ai/a.mp4", "https://console.x.ai/v1", true},
		{"https://cdn.vidgen.x.ai/a.mp4", "https://console.x.ai/v1", true},
		{"https://vidgen.x.ai.evil.example/a.mp4", "https://console.x.ai/v1", false},
		{"https://user@vidgen.x.ai/a.mp4", "https://console.x.ai/v1", false},
		{"http://127.0.0.1:1234/a.mp4", "http://127.0.0.1:1234/v1", true},
		{"http://127.0.0.1:4321/a.mp4", "http://127.0.0.1:1234/v1", false},
	}
	for _, test := range tests {
		if got := trustedConsoleVideoURL(test.url, test.console); got != test.want {
			t.Errorf("trustedConsoleVideoURL(%q, %q) = %v, want %v", test.url, test.console, got, test.want)
		}
	}
}

func TestStandardVideoTaskOwnership(t *testing.T) {
	keyA := "sk-owner-a"
	digest := sha256.Sum256([]byte(keyA))
	job := &videoJob{
		ID: "video_owned", Model: "grok-imagine-video", CreatedAt: time.Now().Unix(),
		Status: "queued", StandardAPI: true, OwnerHash: hex.EncodeToString(digest[:]),
	}
	putVideoJob(job)
	t.Cleanup(func() {
		videoJobsMu.Lock()
		delete(videoJobs, job.ID)
		videoJobsMu.Unlock()
	})

	h := NewHandler(&config.Config{}, nil)
	validator := func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: 1}, nil
	}
	protected := middleware.APIKeyAuth(func() bool { return true }, validator, h.HandleVideosRetrieve)
	request := func(key string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/videos/"+job.ID, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		protected(w, req)
		return w.Code
	}
	if got := request(keyA); got != http.StatusOK {
		t.Fatalf("owner status = %d", got)
	}
	if got := request("sk-owner-b"); got != http.StatusNotFound {
		t.Fatalf("non-owner status = %d", got)
	}
}

func TestVideoTaskRestoresFromRedisAfterMemoryLoss(t *testing.T) {
	oldCacheBaseDir := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldCacheBaseDir })
	videoDir := filepath.Join(cacheBaseDir, "video")
	if err := os.MkdirAll(videoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contentPath := filepath.Join(videoDir, "persisted.mp4")
	if err := os.WriteFile(contentPath, []byte("persisted-video"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, s, mini := setupValidationHandler(t)
	t.Cleanup(func() {
		_ = s.Close()
		mini.Close()
	})
	job := &videoJob{
		ID: "video_persisted", OwnerHash: "anonymous", Model: "grok-imagine-video",
		Status: "completed", Progress: 100, Operation: "generate", StandardAPI: true,
		Seconds: 8, ContentPath: contentPath, CreatedAt: time.Now().Unix(), CompletedAt: time.Now().Unix(),
	}
	putVideoJob(job)
	h.persistVideoJob(context.Background(), job)
	videoJobsMu.Lock()
	delete(videoJobs, job.ID)
	videoJobsMu.Unlock()

	retrieve := httptest.NewRecorder()
	h.HandleVideosRetrieve(retrieve, httptest.NewRequest(http.MethodGet, "/v1/videos/"+job.ID, nil))
	if retrieve.Code != http.StatusOK || !strings.Contains(retrieve.Body.String(), `"status":"done"`) {
		t.Fatalf("retrieve status=%d body=%q", retrieve.Code, retrieve.Body.String())
	}
	videoJobsMu.Lock()
	delete(videoJobs, job.ID)
	videoJobsMu.Unlock()
	content := httptest.NewRecorder()
	h.HandleVideosContent(content, httptest.NewRequest(http.MethodGet, "/v1/videos/"+job.ID+"/content", nil))
	if content.Code != http.StatusOK || content.Body.String() != "persisted-video" {
		t.Fatalf("content status=%d body=%q", content.Code, content.Body.String())
	}
}

func TestPersistedVideoContentPathCannotEscapeCache(t *testing.T) {
	oldCacheBaseDir := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldCacheBaseDir })
	job := runtimeVideoJobFromStored(&store.StoredVideoJob{
		ID: "video_escape", OwnerHash: "anonymous", Status: "completed",
		ContentPath: filepath.Join(cacheBaseDir, "..", "secret.txt"), CreatedAt: time.Now().Unix(),
	})
	if job.ContentPath != "" {
		t.Fatalf("unsafe persisted path was accepted: %q", job.ContentPath)
	}
}
