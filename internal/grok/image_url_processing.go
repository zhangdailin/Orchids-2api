package grok

import (
	"github.com/goccy/go-json"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

func sanitizeText(s string) string {
	if s == "" {
		return s
	}
	// Drop replacement chars introduced by invalid UTF-8 boundaries.
	s = strings.ReplaceAll(s, "\uFFFD", "")
	return s
}

func formatImageMarkdown(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	// Blank lines around images improve rendering in some clients.
	return "\n\n![](" + u + ")\n\n"
}

var reImageURLInText = regexp.MustCompile(`https?://[^\s"')>]+\.(?:png|jpe?g|webp|gif)(?:\?[^\s"')>]*)?`)
var reGrokAssetPathInText = regexp.MustCompile(`(?i)(?:^|[^\w/])((?:users|user)/[a-z0-9-]+/generated/[a-z0-9-]+(?:-part-0)?/image\.(?:png|jpe?g|webp|gif))`)

func extractImageURLsFromText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m := reImageURLInText.FindAllString(s, -1)
	if len(m) == 0 {
		return nil
	}
	return uniqueStrings(m)
}

func extractGrokAssetPathsFromText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	ms := reGrokAssetPathInText.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if len(m) == 2 {
			out = append(out, m[1])
		}
	}
	return uniqueStrings(out)
}

type scoredURL struct {
	u     string
	score int
}

// preferFullOverPart drops "-part-0" preview variants when the corresponding full URL is present.
// This is part of the stable contract:
// - Never emit -part-0 when full exists.
func preferFullOverPart(urls []string) []string {
	if len(urls) == 0 {
		return urls
	}
	set := map[string]struct{}{}
	for _, u := range urls {
		set[u] = struct{}{}
	}
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if strings.Contains(u, "-part-0/") {
			full := strings.ReplaceAll(u, "-part-0/", "/")
			if _, ok := set[full]; ok {
				continue
			}
		}
		out = append(out, u)
	}
	return out
}

func normalizeImageURLs(urls []string, n int) []string {
	urls = selectImageURLs(urls, false)
	if n > 0 && len(urls) > n {
		urls = urls[:n]
	}
	return urls
}

// normalizeGeneratedImageURLs is stricter than normalizeImageURLs:
// when Grok/local file URLs exist, they are preferred over arbitrary external links.
func normalizeGeneratedImageURLs(urls []string, n int) []string {
	urls = selectImageURLs(urls, true)
	if n > 0 && len(urls) > n {
		urls = urls[:n]
	}
	return urls
}

func selectImageURLs(urls []string, preferGrok bool) []string {
	type scored struct {
		u     string
		score int
	}
	input := uniqueStrings(urls)
	all := make([]scored, 0, len(input))
	preferred := make([]scored, 0, len(input))
	for _, u := range input {
		sc := imageURLScore(u)
		if sc < 0 {
			continue
		}
		item := scored{u: strings.TrimSpace(u), score: sc}
		all = append(all, item)
		if sc >= 700 {
			preferred = append(preferred, item)
		}
	}
	candidates := all
	if preferGrok && len(preferred) > 0 {
		candidates = preferred
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].u < candidates[j].u
		}
		return candidates[i].score > candidates[j].score
	})
	out := make([]string, 0, len(candidates))
	for _, it := range candidates {
		out = append(out, it.u)
	}
	return preferFullOverPart(out)
}

func imageURLScore(raw string) int {
	u := strings.TrimSpace(raw)
	if !isLikelyImageURL(u) {
		return -1
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "/grok/v1/files/image/") || strings.HasPrefix(lu, "/v1/files/image/") {
		return 1000
	}

	score := 100
	if strings.HasPrefix(lu, "http://127.0.0.1:") || strings.HasPrefix(lu, "http://localhost:") {
		if strings.Contains(lu, "/grok/v1/files/image/") || strings.Contains(lu, "/v1/files/image/") {
			score = 980
		}
	}
	if parsed, err := url.Parse(u); err == nil {
		host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
		switch {
		case host == "assets.grok.com":
			score = 900
		case host == "grok.com" || strings.HasSuffix(host, ".grok.com"):
			score = 700
		case strings.Contains(host, "encrypted-tbn"):
			score = 10
		}
		path := strings.ToLower(parsed.EscapedPath())
		if strings.Contains(path, "/generated/") {
			score += 30
		}
		if strings.Contains(path, "/image.") {
			score += 10
		}
		if strings.Contains(path, "-part-0/") {
			score -= 120
		}
		query := strings.ToLower(parsed.RawQuery)
		if strings.Contains(query, "thumbnail") || strings.Contains(query, "thumb") {
			score -= 30
		}
	}
	return score
}

func appendImageCandidates(urls []string, debugHTTP []string, debugAsset []string, n int) []string {
	if n <= 0 {
		n = 4
	}
	candidates := make([]string, 0, len(urls)+len(debugHTTP)+len(debugAsset))
	candidates = append(candidates, urls...)

	// 1) Prefer direct image URLs from observed http strings.
	for _, u := range debugHTTP {
		if isLikelyImageURL(u) {
			candidates = append(candidates, u)
		}
	}

	// 2) Parse JSON card strings or asset-like strings from debugAsset and collect up to n.
	for _, p := range debugAsset {
		p = strings.TrimSpace(p)
		if p == "" || strings.Contains(p, "grok-3") || strings.Contains(p, "grok-4") {
			continue
		}
		if strings.HasPrefix(p, "{") || strings.HasPrefix(p, "[") {
			preferred := extractPreferredImageURLsFromJSONText(p)
			if len(preferred) == 0 {
				preferred = extractImageURLsFromText(p)
			}
			for _, u := range preferred {
				if isLikelyImageURL(u) {
					candidates = append(candidates, u)
				}
			}
			continue
		}

		for _, u := range extractImageURLsFromText(p) {
			if isLikelyImageURL(u) {
				candidates = append(candidates, u)
			}
		}
		for _, assetPath := range extractGrokAssetPathsFromText(p) {
			if isLikelyImageAssetPath(assetPath) {
				candidates = append(candidates, "https://assets.grok.com/"+strings.TrimPrefix(assetPath, "/"))
			}
		}

		if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
			if isLikelyImageURL(p) {
				candidates = append(candidates, p)
			}
		} else if isLikelyImageAssetPath(p) {
			candidates = append(candidates, "https://assets.grok.com/"+strings.TrimPrefix(p, "/"))
		}
	}
	return normalizeGeneratedImageURLs(candidates, n)
}

func extractPreferredImageURLsFromJSONText(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || (!strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[")) {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	var out []scoredURL
	var walk func(x interface{}, keyHint string)
	walk = func(x interface{}, keyHint string) {
		switch t := x.(type) {
		case map[string]interface{}:
			for k, vv := range t {
				walk(vv, k)
			}
		case []interface{}:
			for _, vv := range t {
				walk(vv, keyHint)
			}
		case string:
			u := strings.TrimSpace(t)
			if !isLikelyImageURL(u) {
				for _, assetPath := range extractGrokAssetPathsFromText(u) {
					out = append(out, scoredURL{u: "https://assets.grok.com/" + strings.TrimPrefix(assetPath, "/"), score: 90})
				}
				return
			}
			lk := strings.ToLower(strings.TrimSpace(keyHint))
			score := 50
			if strings.Contains(lk, "original") {
				score = 100
			} else if strings.Contains(lk, "thumbnail") || strings.Contains(lk, "thumb") {
				score = 10
			} else if strings.Contains(lk, "link") {
				score = 5
			}
			out = append(out, scoredURL{u: u, score: score})
		}
	}
	walk(v, "")
	if len(out) == 0 {
		return nil
	}
	// Dedup keeping best score.
	best := map[string]int{}
	for _, it := range out {
		if cur, ok := best[it.u]; !ok || it.score > cur {
			best[it.u] = it.score
		}
	}
	items := make([]scoredURL, 0, len(best))
	for u, sc := range best {
		items = append(items, scoredURL{u: u, score: sc})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score == items[j].score {
			return items[i].u < items[j].u
		}
		return items[i].score > items[j].score
	})
	res := make([]string, 0, len(items))
	for _, it := range items {
		res = append(res, it.u)
	}
	return res
}

func isLikelyImageURL(u string) bool {
	u = strings.TrimSpace(u)
	if u == "" {
		return false
	}
	if strings.HasPrefix(u, "/grok/v1/files/image/") || strings.HasPrefix(u, "/v1/files/image/") {
		return true
	}
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "http://") || strings.HasPrefix(lu, "https://") {
		parsed, err := url.Parse(u)
		if err == nil {
			base := strings.TrimSpace(path.Base(parsed.EscapedPath()))
			if strings.HasPrefix(base, ".") {
				return false
			}
		}
		// Quick allow if it clearly ends with an image extension (ignore query).
		cut := lu
		if q := strings.IndexByte(cut, '?'); q >= 0 {
			cut = cut[:q]
		}
		if strings.HasSuffix(cut, ".jpg") || strings.HasSuffix(cut, ".jpeg") || strings.HasSuffix(cut, ".png") || strings.HasSuffix(cut, ".webp") || strings.HasSuffix(cut, ".gif") {
			return true
		}
		// assets.grok.com generated image paths. Some Grok streams only expose an
		// asset id, which is served through /content instead of a file extension.
		if strings.Contains(lu, "assets.grok.com/") && (strings.Contains(lu, "/generated/") || strings.Contains(lu, "/image") || strings.HasSuffix(lu, "/content")) {
			return true
		}
		return false
	}
	return false
}

func isLikelyImageAssetPath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	if strings.HasPrefix(path.Base(p), ".") {
		return false
	}
	// Reject JSON blobs or echoed prompts.
	if strings.HasPrefix(p, "{") || strings.Contains(p, "Image Generation:") {
		return false
	}
	// Reject anything with whitespace (asset paths/urls shouldn't contain spaces/newlines).
	if strings.ContainsAny(p, " \t\r\n") {
		return false
	}
	lp := strings.ToLower(p)
	if strings.HasSuffix(lp, ".jpg") || strings.HasSuffix(lp, ".jpeg") || strings.HasSuffix(lp, ".png") || strings.HasSuffix(lp, ".webp") || strings.HasSuffix(lp, ".gif") {
		return true
	}
	return false
}
