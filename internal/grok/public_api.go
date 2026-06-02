package grok

import (
	"bytes"
	"github.com/goccy/go-json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const publicVideoSessionTTL = 10 * time.Minute

type publicVideoSession struct {
	Prompt          string
	AspectRatio     string
	VideoLength     int
	ResolutionName  string
	Preset          string
	ImageURL        string
	ReasoningEffort string
	CreatedAt       time.Time
}

type publicVideoStartRequest struct {
	Prompt          string `json:"prompt"`
	AspectRatio     string `json:"aspect_ratio"`
	VideoLength     int    `json:"video_length"`
	ResolutionName  string `json:"resolution_name"`
	Preset          string `json:"preset"`
	ImageURL        string `json:"image_url"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type publicVideoStopRequest struct {
	TaskIDs []string `json:"task_ids"`
}

var (
	publicVideoSessionsMu sync.Mutex
	publicVideoSessions   = map[string]publicVideoSession{}
)

func cleanupPublicVideoSessionsLocked(now time.Time) {
	for id, session := range publicVideoSessions {
		if now.Sub(session.CreatedAt) > publicVideoSessionTTL {
			delete(publicVideoSessions, id)
		}
	}
}

func createPublicVideoSession(session publicVideoSession) string {
	id := randomHex(16)
	if id == "" {
		id = strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	}
	now := time.Now()

	publicVideoSessionsMu.Lock()
	defer publicVideoSessionsMu.Unlock()
	cleanupPublicVideoSessionsLocked(now)
	session.CreatedAt = now
	publicVideoSessions[id] = session
	return id
}

func getPublicVideoSession(taskID string) (publicVideoSession, bool) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return publicVideoSession{}, false
	}
	now := time.Now()
	publicVideoSessionsMu.Lock()
	defer publicVideoSessionsMu.Unlock()
	cleanupPublicVideoSessionsLocked(now)
	session, ok := publicVideoSessions[id]
	if !ok {
		return publicVideoSession{}, false
	}
	if now.Sub(session.CreatedAt) > publicVideoSessionTTL {
		delete(publicVideoSessions, id)
		return publicVideoSession{}, false
	}
	return session, true
}

func deletePublicVideoSession(taskID string) {
	id := strings.TrimSpace(taskID)
	if id == "" {
		return
	}
	publicVideoSessionsMu.Lock()
	delete(publicVideoSessions, id)
	publicVideoSessionsMu.Unlock()
}

func deletePublicVideoSessions(taskIDs []string) int {
	removed := 0
	publicVideoSessionsMu.Lock()
	for _, raw := range taskIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := publicVideoSessions[id]; ok {
			delete(publicVideoSessions, id)
			removed++
		}
	}
	publicVideoSessionsMu.Unlock()
	return removed
}

func validatePublicImageURL(imageURL string) bool {
	imageURL = strings.TrimSpace(imageURL)
	if imageURL == "" {
		return true
	}
	if strings.HasPrefix(imageURL, "data:") {
		return true
	}
	if strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://") {
		return true
	}
	return false
}

func validateReasoningEffortValue(raw string) bool {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return true
	}
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func (h *Handler) HandlePublicVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status": "success",
	})
}

func (h *Handler) HandlePublicImagineConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	finalMinBytes := 100000
	mediumMinBytes := 30000
	nsfw := false
	if h != nil && h.cfg != nil {
		finalMinBytes = h.cfg.PublicImagineFinalMinBytes()
		mediumMinBytes = h.cfg.PublicImagineMediumMinBytes()
		nsfw = h.cfg.PublicImagineNSFW()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"final_min_bytes":  finalMinBytes,
		"medium_min_bytes": mediumMinBytes,
		"nsfw":             nsfw,
	})
}

func (h *Handler) HandlePublicVideoStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req publicVideoStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		http.Error(w, "Prompt cannot be empty", http.StatusBadRequest)
		return
	}

	videoCfg, err := validateVideoConfig(&VideoConfig{
		AspectRatio:    strings.TrimSpace(req.AspectRatio),
		VideoLength:    req.VideoLength,
		ResolutionName: strings.TrimSpace(req.ResolutionName),
		Preset: func() string {
			preset := strings.TrimSpace(req.Preset)
			if preset == "" {
				return "normal"
			}
			return preset
		}(),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	imageURL := strings.TrimSpace(req.ImageURL)
	if !validatePublicImageURL(imageURL) {
		http.Error(w, "image_url must be a URL or data URI (data:<mime>;base64,...)", http.StatusBadRequest)
		return
	}
	reasoning := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if !validateReasoningEffortValue(reasoning) {
		http.Error(w, "reasoning_effort must be one of [high low medium minimal none xhigh]", http.StatusBadRequest)
		return
	}

	taskID := createPublicVideoSession(publicVideoSession{
		Prompt:          prompt,
		AspectRatio:     videoCfg.AspectRatio,
		VideoLength:     videoCfg.VideoLength,
		ResolutionName:  videoCfg.ResolutionName,
		Preset:          videoCfg.Preset,
		ImageURL:        imageURL,
		ReasoningEffort: reasoning,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"task_id":      taskID,
		"aspect_ratio": videoCfg.AspectRatio,
	})
}

func (h *Handler) HandlePublicVideoStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req publicVideoStopRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	removed := deletePublicVideoSessions(req.TaskIDs)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"removed": removed,
	})
}

func (h *Handler) HandlePublicVideoSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	session, ok := getPublicVideoSession(taskID)
	if !ok {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	defer deletePublicVideoSession(taskID)

	var messages interface{}
	if strings.TrimSpace(session.ImageURL) != "" {
		messages = []map[string]interface{}{
			{
				"role": "user",
				"content": []map[string]interface{}{
					{"type": "text", "text": session.Prompt},
					{"type": "image_url", "image_url": map[string]interface{}{"url": session.ImageURL}},
				},
			},
		}
	} else {
		messages = []map[string]interface{}{
			{"role": "user", "content": session.Prompt},
		}
	}

	payload := map[string]interface{}{
		"model":    "grok-imagine-video",
		"stream":   true,
		"messages": messages,
		"video_config": map[string]interface{}{
			"aspect_ratio":    session.AspectRatio,
			"video_length":    session.VideoLength,
			"resolution_name": session.ResolutionName,
			"preset":          session.Preset,
		},
	}
	if session.ReasoningEffort != "" {
		payload["reasoning_effort"] = session.ReasoningEffort
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "failed to build video request", http.StatusInternalServerError)
		return
	}

	subReq := r.Clone(r.Context())
	subReq.Method = http.MethodPost
	subReq.URL.Path = "/v1/chat/completions"
	subReq.Header = make(http.Header)
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Body = io.NopCloser(bytes.NewReader(raw))
	subReq.ContentLength = int64(len(raw))

	h.HandleChatCompletions(w, subReq)
}
