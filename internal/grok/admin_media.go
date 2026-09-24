package grok

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"orchids-api/internal/store"
)

const defaultAdminMediaPageSize = 24

type adminVideoJob struct {
	ID                string    `json:"id"`
	AccountID         int64     `json:"account_id,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	Model             string    `json:"model"`
	Prompt            string    `json:"prompt,omitempty"`
	Seconds           int       `json:"seconds,omitempty"`
	Size              string    `json:"size,omitempty"`
	Quality           string    `json:"quality,omitempty"`
	Status            string    `json:"status"`
	Progress          int       `json:"progress"`
	VideoURL          string    `json:"video_url,omitempty"`
	UpstreamRequestID string    `json:"upstream_request_id,omitempty"`
	RemixedFromID     string    `json:"remixed_from_id,omitempty"`
	Operation         string    `json:"operation,omitempty"`
	StandardAPI       bool      `json:"standard_api,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         int64     `json:"created_at"`
	CompletedAt       int64     `json:"completed_at,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
}

func parseAdminMediaPagination(r *http.Request) (int, int) {
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := parsePositiveInt(r.URL.Query().Get("page_size"), defaultAdminMediaPageSize)
	if pageSize > maxCachePageSize {
		pageSize = maxCachePageSize
	}
	return page, pageSize
}

func containsFold(value, query string) bool {
	return query == "" || strings.Contains(strings.ToLower(value), query)
}

func paginateAdminImages(entries []cacheEntry, page, pageSize int) ([]cacheEntry, int) {
	return paginateCacheEntries(entries, page, pageSize)
}

func paginateAdminVideos(jobs []adminVideoJob, page, pageSize int) ([]adminVideoJob, int) {
	total := len(jobs)
	if total == 0 || page < 1 || pageSize < 1 || page > (total-1)/pageSize+1 {
		return []adminVideoJob{}, total
	}
	start := (page - 1) * pageSize
	end := min(total, start+pageSize)
	return jobs[start:end], total
}

func (h *Handler) HandleAdminMediaImages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	entries, _, err := listCachedEntries("image")
	if err != nil {
		writeGrokError(w, http.StatusInternalServerError, "failed to list images")
		return
	}
	query := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("search"), r.URL.Query().Get("q"))))
	if query != "" {
		filtered := make([]cacheEntry, 0, len(entries))
		for _, entry := range entries {
			if containsFold(entry.Name, query) {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}
	page, pageSize := parseAdminMediaPagination(r)
	items, total := paginateAdminImages(entries, page, pageSize)
	writeJSON(w, map[string]interface{}{"status": "success", "items": items, "total": total, "page": page, "page_size": pageSize})
}

func (h *Handler) HandleAdminMediaImageStats(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	entries, size, err := listCachedEntries("image")
	if err != nil {
		writeGrokError(w, http.StatusInternalServerError, "failed to read image stats")
		return
	}
	writeJSON(w, map[string]interface{}{"status": "success", "count": len(entries), "bytes": size, "size_bytes": size, "size_mb": bytesToMB(size)})
}

func storedJobForAdmin(job *store.StoredVideoJob) adminVideoJob {
	if job == nil {
		return adminVideoJob{}
	}
	return adminVideoJob{
		ID: job.ID, AccountID: job.AccountID, Provider: job.Provider, Model: job.Model,
		Prompt: job.Prompt, Seconds: job.Seconds, Size: job.Size, Quality: job.Quality,
		Status: job.Status, Progress: job.Progress, VideoURL: job.VideoURL,
		UpstreamRequestID: job.UpstreamRequestID, RemixedFromID: job.RemixedFromID,
		Operation: job.Operation, StandardAPI: job.StandardAPI, ErrorCode: job.ErrorCode,
		ErrorMessage: job.ErrorMessage, CreatedAt: job.CreatedAt, CompletedAt: job.CompletedAt,
		ExpiresAt: job.ExpiresAt, UpdatedAt: job.UpdatedAt,
	}
}

func (h *Handler) listAdminVideoJobs(r *http.Request) ([]adminVideoJob, error) {
	jobs, err := h.lb.Store.ListStoredVideoJobs(r.Context())
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("search"), r.URL.Query().Get("q"))))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	out := make([]adminVideoJob, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || (status != "" && strings.ToLower(strings.TrimSpace(job.Status)) != status) {
			continue
		}
		if query != "" && !containsFold(job.ID, query) && !containsFold(job.Prompt, query) && !containsFold(job.Model, query) && !containsFold(job.Provider, query) {
			continue
		}
		out = append(out, storedJobForAdmin(job))
	}
	sort.SliceStable(out, func(i, j int) bool {
		left, right := out[i].UpdatedAt.UnixMilli(), out[j].UpdatedAt.UnixMilli()
		if left == right {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return left > right
	})
	return out, nil
}

func (h *Handler) HandleAdminMediaVideos(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeGrokError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	jobs, err := h.listAdminVideoJobs(r)
	if err != nil {
		writeGrokError(w, http.StatusInternalServerError, "failed to list video jobs")
		return
	}
	page, pageSize := parseAdminMediaPagination(r)
	items, total := paginateAdminVideos(jobs, page, pageSize)
	writeJSON(w, map[string]interface{}{"status": "success", "items": items, "total": total, "page": page, "page_size": pageSize})
}

func (h *Handler) HandleAdminMediaVideoStats(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		writeGrokError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	jobs, err := h.lb.Store.ListStoredVideoJobs(r.Context())
	if err != nil {
		writeGrokError(w, http.StatusInternalServerError, "failed to read video stats")
		return
	}
	statuses := map[string]int{}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(job.Status))
		if status == "" {
			status = "unknown"
		}
		statuses[status]++
	}
	writeJSON(w, map[string]interface{}{"status": "success", "total": len(jobs), "statuses": statuses})
}
