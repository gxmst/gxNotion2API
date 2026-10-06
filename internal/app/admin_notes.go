package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

type noteSummary struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type noteDocument struct {
	noteSummary
	Text    string `json:"text"`
	Partial bool   `json:"partial"`
}
type noteCacheEntry struct {
	At         time.Time
	Payload    any
	Status     int
	RetryUntil time.Time
}

func noteTitle(value any) string {
	var text strings.Builder
	for _, raw := range sliceValue(value) {
		part := sliceValue(raw)
		if len(part) > 0 {
			text.WriteString(stringValue(part[0]))
		}
	}
	return strings.TrimSpace(text.String())
}
func noteBlock(records map[string]any, id, space string) (map[string]any, bool) {
	raw := mapValue(records[id])
	value := unwrapRecordValue(raw)
	actual := firstNonEmpty(stringValue(raw["spaceId"]), stringValue(value["space_id"]))
	if actual != space || value == nil || value["alive"] == false || stringValue(mapValue(raw["value"])["role"]) == "none" {
		return nil, false
	}
	return value, true
}
func parseNoteList(body []byte, space string) ([]noteSummary, error) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil, errors.New("笔记列表格式无法识别")
	}
	records := mapValue(mapValue(payload["recordMap"])["block"])
	notes := []noteSummary{}
	for _, rawID := range sliceValue(payload["pages"]) {
		id := stringValue(rawID)
		value, ok := noteBlock(records, id, space)
		if !ok {
			continue
		}
		title := noteTitle(mapValue(value["properties"])["title"])
		notes = append(notes, noteSummary{ID: id, Title: firstNonEmpty(title, "未命名笔记")})
		if len(notes) >= 100 {
			break
		}
	}
	return notes, nil
}

// Private page pointers come from the current user's scoped space view.
func privateNoteList(records map[string]any, space, user string) []noteSummary {
	notes := []noteSummary{}
	seen := map[string]bool{}
	blocks := mapValue(records["block"])
	for _, raw := range mapValue(records["space_view"]) {
		view := unwrapRecordValue(raw)
		if user == "" || stringValue(view["space_id"]) != space || view["alive"] == false || stringValue(mapValue(mapValue(raw)["value"])["role"]) == "none" {
			continue
		}
		if stringValue(view["parent_id"]) != user {
			continue
		}
		for _, rawID := range sliceValue(view["private_pages"]) {
			id := stringValue(rawID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			title := ""
			if block, ok := noteBlock(blocks, id, space); ok {
				title = noteTitle(mapValue(block["properties"])["title"])
			} else if _, exists := blocks[id]; exists {
				continue
			}
			notes = append(notes, noteSummary{ID: id, Title: firstNonEmpty(title, "私人页面 · "+id[:min(8, len(id))])})
			if len(notes) >= 100 {
				return notes
			}
		}
	}
	return notes
}

func parseNoteDocument(body []byte, id, space string) (noteDocument, error) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return noteDocument{}, errors.New("笔记内容格式无法识别")
	}
	if actual := stringValue(payload["spaceId"]); actual != "" && actual != space {
		return noteDocument{}, errors.New("笔记不属于当前工作区")
	}
	records := mapValue(mapValue(payload["recordMap"])["block"])
	root, ok := noteBlock(records, id, space)
	if !ok {
		return noteDocument{}, errors.New("当前账号无法读取此工作区的笔记")
	}
	doc := noteDocument{noteSummary: noteSummary{ID: id, Title: firstNonEmpty(noteTitle(mapValue(root["properties"])["title"]), "未命名笔记")}}
	lines := []string{}
	visited := map[string]bool{}
	chars := 0
	var walk func(string, int)
	walk = func(blockID string, depth int) {
		if visited[blockID] {
			doc.Partial = true
			return
		}
		if len(visited) >= 200 || depth > 12 || chars >= 12000 {
			doc.Partial = true
			return
		}
		visited[blockID] = true
		block, ok := noteBlock(records, blockID, space)
		if !ok {
			doc.Partial = true
			return
		}
		kind := stringValue(block["type"])
		text := noteTitle(mapValue(block["properties"])["title"])
		if blockID != id {
			switch kind {
			case "header":
				text = "# " + text
			case "sub_header":
				text = "## " + text
			case "sub_sub_header":
				text = "### " + text
			case "bulleted_list", "numbered_list", "to_do":
				text = "- " + text
			case "page":
				text = "[子页面] " + text
				doc.Partial = true
			case "text", "quote", "callout", "toggle", "code", "column", "column_list", "divider":
			default:
				doc.Partial = true
			}
			runes := []rune(text)
			if len(runes) > 12000-chars {
				text = string(runes[:12000-chars])
				doc.Partial = true
			}
			if text != "" {
				lines = append(lines, text)
				chars += len([]rune(text))
			}
		}
		if kind == "page" && blockID != id {
			return
		}
		for _, child := range sliceValue(block["content"]) {
			walk(stringValue(child), depth+1)
		}
	}
	walk(id, 0)
	doc.Text = strings.Join(lines, "\n\n")
	// Bounded single-chunk reads avoid crawling a workspace in the background.
	if len(sliceValue(payload["cursors"])) > 0 {
		doc.Partial = true
	}
	return doc, nil
}

// Explicit, read-only browsing; no transactions or automatic relogin.
func (a *App) handleAdminNotes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.adminAuthOK(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]any{"detail": "method not allowed"})
		return
	}
	email, space := strings.TrimSpace(r.URL.Query().Get("email")), strings.TrimSpace(r.URL.Query().Get("workspace_id"))
	id := strings.TrimSpace(r.URL.Query().Get("page_id"))
	source := r.URL.Query().Get("source")
	if source != "" && source != "shared" && source != "private" {
		writeJSON(w, 400, map[string]any{"detail": "未知笔记范围"})
		return
	}
	if email == "" || space == "" || len(id) > 128 {
		writeJSON(w, 400, map[string]any{"detail": "必须指定账号和工作区"})
		return
	}
	cfg, _, _ := a.State.Snapshot()
	account, _, ok := cfg.FindAccountWorkspace(email, space)
	if !ok {
		writeJSON(w, 404, map[string]any{"detail": "workspace not found"})
		return
	}
	if account.Disabled {
		writeJSON(w, 409, map[string]any{"detail": "账号已禁用"})
		return
	}
	if until := parseOptionalRFC3339(account.CredentialCooldownUntil); until.After(time.Now()) {
		w.Header().Set("Retry-After", until.UTC().Format(http.TimeFormat))
		writeJSON(w, 429, map[string]any{"detail": "账号正在冷却，请稍后读取笔记"})
		return
	}
	if !a.State.notesMu.TryLock() {
		w.Header().Set("Retry-After", "5")
		writeJSON(w, 429, map[string]any{"detail": "正在读取笔记，请稍后再试"})
		return
	}
	defer a.State.notesMu.Unlock()
	key := canonicalEmailKey(email) + "\x00" + space + "\x00" + id
	if id == "" && source == "private" {
		key += "\x00private"
	}
	cached, exists := a.State.notesCache[key]
	age := time.Since(cached.At)
	force := r.URL.Query().Get("refresh") == "1"
	if exists && (cached.RetryUntil.After(time.Now()) || age < 30*time.Second || !force && age < 5*time.Minute) {
		if cached.RetryUntil.After(time.Now()) {
			w.Header().Set("Retry-After", cached.RetryUntil.UTC().Format(http.TimeFormat))
		}
		writeJSON(w, cached.Status, cached.Payload)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client, err := a.notionClientForWorkspace(ctx, email, space)
	if err != nil {
		writeJSON(w, 400, map[string]any{"detail": "无法加载账号登录态"})
		return
	}
	client.FallbackHTTPClient = nil
	var result any
	status := http.StatusOK
	endpoint := "getUserSharedPagesInSpace"
	args := map[string]any{"spaceId": space}
	if id != "" {
		endpoint = "loadCachedPageChunkV2"
		args = map[string]any{"page": map[string]any{"id": id, "spaceId": space}, "cursor": map[string]any{"stack": []any{}}, "verticalColumns": false}
	}
	var body []byte
	if id == "" && source == "private" {
		var records map[string]any
		records, err = client.readModelPolicyRecords(ctx)
		if err == nil {
			result = map[string]any{"items": privateNoteList(records, space, client.Session.UserID), "fetched_at": time.Now().UTC(), "scope": "private_pages"}
		}
	} else {
		body, err = client.postJSON(ctx, client.Config.NotionUpstream().API(endpoint), args, "application/json")
	}
	if err == nil && result == nil {
		if id == "" {
			var notes []noteSummary
			notes, err = parseNoteList(body, space)
			result = map[string]any{"items": notes, "fetched_at": time.Now().UTC(), "scope": "shared_pages"}
		} else {
			var doc noteDocument
			doc, err = parseNoteDocument(body, id, space)
			result = map[string]any{"item": doc, "fetched_at": time.Now().UTC()}
		}
	}
	retryUntil := time.Time{}
	if err != nil {
		status = http.StatusBadGateway
		retryUntil = credentialBackoff(err, time.Now())
		if retryUntil.After(time.Now()) {
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", retryUntil.UTC().Format(http.TimeFormat))
		}
		result = map[string]any{"detail": "笔记读取失败或无权限；没有自动重试，请稍后再试"}
	}
	if a.State.notesCache == nil {
		a.State.notesCache = map[string]noteCacheEntry{}
	}
	if len(a.State.notesCache) >= 100 {
		for k := range a.State.notesCache {
			delete(a.State.notesCache, k)
			break
		}
	}
	a.State.notesCache[key] = noteCacheEntry{At: time.Now(), Payload: result, Status: status, RetryUntil: retryUntil}
	writeJSON(w, status, result)
}
