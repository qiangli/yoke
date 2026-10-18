package apiadapters

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/external/saasclients"
)

func itoa(n int) string { return strconv.Itoa(n) }

const testCanary = "CANARY"

func v1() string { return SchemaVersion }

// ---- loopback fakes (recorded contract shapes only, no live services) ----

type fakeSlackData struct {
	mu    sync.Mutex
	msgs  map[string]string
	posts int64
}

func (f *fakeSlackData) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testCanary {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/") {
	case "conversations.history":
		if r.URL.Query().Get("channel") != "C1" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "channel_not_found"})
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{
				map[string]any{"ts": "1.0", "user": "U1", "text": "hi"},
				map[string]any{"ts": "2.0", "user": "U2", "text": "yo"},
			}, "has_more": true, "response_metadata": map[string]any{"next_cursor": "c1"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []any{
			map[string]any{"ts": "3.0", "user": "U1", "text": "done"},
		}, "has_more": false, "response_metadata": map[string]any{}})
	case "conversations.list":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": []any{
			map[string]any{"id": "C1", "name": "general"},
		}, "response_metadata": map[string]any{}})
	case "chat.postMessage":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.posts++
		ts := "1700000000.000001"
		f.msgs[ts] = in["text"]
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": ts})
	case "chat.delete":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, ok := f.msgs[in["ts"]]; !ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "message_not_found"})
			return
		}
		delete(f.msgs, in["ts"])
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case "limited":
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
	case "echo":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "missing_scope", "detail": testCanary})
	}
}

type notionPageRec struct {
	id       string
	archived bool
	props    json.RawMessage
}

type fakeNotion struct {
	mu    sync.Mutex
	pages map[string]*notionPageRec
	seq   int
	hits  map[string]*atomic.Int64
	flaky *atomic.Int64
}

func newFakeNotion() *fakeNotion {
	return &fakeNotion{pages: map[string]*notionPageRec{}, hits: map[string]*atomic.Int64{}}
}

func (f *fakeNotion) hit(path string) {
	f.mu.Lock()
	c, ok := f.hits[path]
	if !ok {
		c = &atomic.Int64{}
		f.hits[path] = c
	}
	f.mu.Unlock()
	c.Add(1)
}

func (f *fakeNotion) count(path string) int64 {
	f.mu.Lock()
	c := f.hits[path]
	f.mu.Unlock()
	if c == nil {
		return 0
	}
	return c.Load()
}

func (f *fakeNotion) unauthorized(w http.ResponseWriter, r *http.Request) {
	// Echo the PRESENTED credential: the realistic leak shape is a
	// provider repeating what the caller sent, which redaction must mask.
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	w.WriteHeader(401)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"object": "error", "status": 401, "code": "unauthorized",
		"message": "rejecting token " + presented,
	})
}

func (f *fakeNotion) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testCanary {
		f.unauthorized(w, r)
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/")
	if r.Header.Get("Notion-Version") != NotionVersion &&
		(strings.HasPrefix(trimmed, "pages") || strings.HasPrefix(trimmed, "databases") ||
			strings.HasPrefix(trimmed, "v1/pages") || strings.HasPrefix(trimmed, "v1/databases")) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "error", "status": 400, "code": "validation_error", "message": "missing version",
		})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/")
	rest := strings.TrimPrefix(p, "v1/")
	if !strings.HasPrefix(p, "v1/") {
		rest = p // tests use the versioned base; accept both
	}
	f.hit(rest)
	f.mu.Lock()
	defer f.mu.Unlock()
	enc := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	errObj := func(status int, code, msg string) {
		w.WriteHeader(status)
		enc(map[string]any{"object": "error", "status": status, "code": code, "message": msg})
	}
	notionDoc := func(rec *notionPageRec) map[string]any {
		return map[string]any{"object": "page", "id": rec.id,
			"created_time": "2026-10-18T00:00:00Z", "archived": rec.archived,
			"url": "https://notion.so/" + rec.id, "properties": json.RawMessage(rec.props)}
	}
	switch {
	case r.Method == "POST" && rest == "pages":
		var in struct {
			Parent     NotionParent    `json:"parent"`
			Properties json.RawMessage `json:"properties"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Parent.Type == "database_id" && in.Parent.DatabaseID == "boom" {
			errObj(500, "internal_error", "boom "+testCanary)
			return
		}
		if in.Parent.Type != "database_id" && in.Parent.Type != "page_id" || len(in.Properties) == 0 {
			errObj(400, "validation_error", "bad parent or properties")
			return
		}
		f.seq++
		id := "pg-" + itoa(f.seq)
		rec := &notionPageRec{id: id, props: in.Properties}
		f.pages[id] = rec
		w.WriteHeader(200)
		enc(notionDoc(rec))
	case r.Method == "GET" && strings.HasPrefix(rest, "pages/"):
		id := strings.TrimPrefix(rest, "pages/")
		if id == "flaky" {
			n := 0
			if f.flaky != nil {
				n = int(f.flaky.Add(1))
			}
			if n <= 1 {
				w.Header().Set("Retry-After", "0")
				errObj(429, "rate_limited", "slow down")
				return
			}
			w.WriteHeader(200)
			enc(map[string]any{"object": "page", "id": "flaky"})
			return
		}
		if id == "down" {
			errObj(500, "internal_error", "down")
			return
		}
		rec, ok := f.pages[id]
		if !ok {
			errObj(404, "object_not_found", "gone "+testCanary)
			return
		}
		w.WriteHeader(200)
		enc(notionDoc(rec))
	case r.Method == "PATCH" && strings.HasPrefix(rest, "pages/"):
		id := strings.TrimPrefix(rest, "pages/")
		rec, ok := f.pages[id]
		if !ok {
			errObj(404, "object_not_found", "gone")
			return
		}
		var in map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&in)
		if raw, ok := in["archived"]; ok {
			var arch bool
			_ = json.Unmarshal(raw, &arch)
			rec.archived = arch
		}
		if raw, ok := in["properties"]; ok {
			rec.props = raw
		}
		w.WriteHeader(200)
		enc(notionDoc(rec))
	case r.Method == "POST" && strings.HasPrefix(rest, "databases/") && strings.HasSuffix(rest, "/query"):
		var in struct {
			PageSize    int    `json:"page_size"`
			StartCursor string `json:"start_cursor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		all := []string{"q1", "q2", "q3"}
		start := 0
		if in.StartCursor != "" {
			for i, id := range all {
				if id == in.StartCursor {
					start = i + 1
				}
			}
		}
		size := in.PageSize
		if size <= 0 {
			size = 100
		}
		end := start + size
		if end > len(all) {
			end = len(all)
		}
		var out []any
		for _, id := range all[start:end] {
			out = append(out, map[string]any{"object": "page", "id": id, "properties": map[string]any{}})
		}
		if out == nil {
			out = []any{}
		}
		next := ""
		more := false
		if end < len(all) {
			next, more = all[end-1], true
		}
		w.WriteHeader(200)
		enc(map[string]any{"object": "list", "results": out, "next_cursor": next, "has_more": more})
	default:
		errObj(400, "invalid_request_url", "no such path")
	}
}

type fakeAirtable struct {
	mu    sync.Mutex
	rows  map[string]map[string]json.RawMessage
	seq   int
	posts map[string]*atomic.Int64
}

func newFakeAirtable() *fakeAirtable {
	return &fakeAirtable{rows: map[string]map[string]json.RawMessage{}, posts: map[string]*atomic.Int64{}}
}

func (f *fakeAirtable) postCount(path string) int64 {
	f.mu.Lock()
	c := f.posts[path]
	f.mu.Unlock()
	if c == nil {
		return 0
	}
	return c.Load()
}

func (f *fakeAirtable) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testCanary {
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"type": "AUTHENTICATION_REQUIRED", "message": "bad token " + presented}})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "NOT_FOUND", "message": "nope"}})
		return
	}
	base, table := parts[0], parts[1]
	id := ""
	if len(parts) > 2 {
		id = parts[2]
	}
	key := base + "/" + table
	f.mu.Lock()
	defer f.mu.Unlock()
	enc := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	rows := f.rows[key]
	if rows == nil {
		rows = map[string]json.RawMessage{}
		f.rows[key] = rows
	}
	switch r.Method {
	case "GET":
		if id == "" {
			size := 100
			if s := r.URL.Query().Get("pageSize"); s != "" {
				if ps, cerr := strconv.Atoi(s); cerr == nil && ps > 0 {
					size = ps
				}
			}
			var ids []string
			for rid := range rows {
				ids = append(ids, rid)
			}
			// deterministic order for the fake
			for i := 0; i < len(ids); i++ {
				for j := i + 1; j < len(ids); j++ {
					if ids[j] < ids[i] {
						ids[i], ids[j] = ids[j], ids[i]
					}
				}
			}
			start := 0
			if off := r.URL.Query().Get("offset"); off != "" {
				for i, rid := range ids {
					if rid == off {
						start = i + 1
					}
				}
			}
			end := start + size
			if end > len(ids) {
				end = len(ids)
			}
			var out []any
			for _, rid := range ids[start:end] {
				out = append(out, map[string]any{"id": rid, "createdTime": "2026-10-18T00:00:00Z", "fields": rows[rid]})
			}
			if out == nil {
				out = []any{}
			}
			resp := map[string]any{"records": out}
			if end < len(ids) {
				resp["offset"] = ids[end-1]
			}
			enc(resp)
			return
		}
		fields, ok := rows[id]
		if !ok {
			w.WriteHeader(404)
			enc(map[string]any{"error": map[string]any{"type": "NOT_FOUND", "message": "gone " + testCanary}})
			return
		}
		enc(map[string]any{"id": id, "createdTime": "2026-10-18T00:00:00Z", "fields": fields})
	case "POST":
		c, ok := f.posts[key]
		if !ok {
			c = &atomic.Int64{}
			f.posts[key] = c
		}
		c.Add(1)
		if table == "boom" {
			w.WriteHeader(500)
			enc(map[string]any{"error": map[string]any{"type": "UNKNOWN", "message": "boom"}})
			return
		}
		var in struct {
			Fields json.RawMessage `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in.Fields) == 0 {
			w.WriteHeader(422)
			enc(map[string]any{"error": map[string]any{"type": "INVALID_REQUEST", "message": "fields required"}})
			return
		}
		f.seq++
		rid := "rec-" + itoa(f.seq)
		rows[rid] = in.Fields
		w.WriteHeader(200)
		enc(map[string]any{"id": rid, "createdTime": "2026-10-18T00:00:00Z", "fields": in.Fields})
	case "PATCH":
		var in struct {
			Fields json.RawMessage `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, ok := rows[id]; !ok {
			w.WriteHeader(404)
			enc(map[string]any{"error": map[string]any{"type": "NOT_FOUND", "message": "gone"}})
			return
		}
		if len(in.Fields) == 0 {
			w.WriteHeader(422)
			enc(map[string]any{"error": map[string]any{"type": "INVALID_REQUEST", "message": "fields required"}})
			return
		}
		rows[id] = in.Fields
		enc(map[string]any{"id": id, "fields": in.Fields})
	case "DELETE":
		if _, ok := rows[id]; !ok {
			w.WriteHeader(404)
			enc(map[string]any{"error": map[string]any{"type": "NOT_FOUND", "message": "gone"}})
			return
		}
		delete(rows, id)
		enc(map[string]any{"id": id, "deleted": true})
	}
}

// ---- test scaffolding ----

func fastThrottle() *Throttle { return NewThrottle(time.Millisecond) }

func notionScope() saasclients.Scope {
	return saasclients.Scope{Client: "notion", Target: "ws-1", Sandbox: true}
}

func airtableScope() saasclients.Scope {
	return saasclients.Scope{Client: "airtable", Target: "app-1", Sandbox: true}
}

func slackScope(sandbox bool) saasclients.Scope {
	return saasclients.Scope{Client: "slack", Target: "T1", Sandbox: sandbox}
}

func TestVersionGate(t *testing.T) {
	n := NewNotion("http://loop.invalid", testCanary)
	if _, err := n.PageGet(context.Background(), NotionPageGetInput{SchemaVersion: "v9", PageID: "x"}); err == nil || err.Code != gws.CodeUnsupported {
		t.Errorf("notion version: %v", err)
	}
	a := NewAirtable("http://loop.invalid", testCanary)
	if _, err := a.RecordGet(context.Background(), AirtableRecordGetInput{SchemaVersion: "", Base: "b", Table: "t", RecordID: "r"}); err == nil || err.Code != gws.CodeUnsupported {
		t.Errorf("airtable version: %v", err)
	}
	s := NewSlack("http://loop.invalid", testCanary)
	b := gws.Binding{Target: "slack:history", Query: "C1"}
	if _, err := s.History(context.Background(), SlackHistoryInput{SchemaVersion: "bogus", Channel: "C1"}, b, b); err == nil || err.Code != gws.CodeUnsupported {
		t.Errorf("slack version: %v", err)
	}
}

func TestSlackHistoryPaginationAndBinding(t *testing.T) {
	f := &fakeSlackData{msgs: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := NewSlack(srv.URL, testCanary)
	s.Throttle = fastThrottle()
	ctx := context.Background()
	b := gws.Binding{Target: "slack:history", Query: "channel=C1"}

	p1, err := s.History(ctx, SlackHistoryInput{SchemaVersion: v1(), Channel: "C1"}, b, b)
	if err != nil || len(p1.Messages) != 2 || !p1.HasMore || p1.NextCursor == "" {
		t.Fatalf("page 1: %+v %v", p1, err)
	}
	if p1.Messages[0].Text != "hi" || p1.Messages[0].TS == "" {
		t.Errorf("typed message: %+v", p1.Messages[0])
	}
	p2, err := s.History(ctx, SlackHistoryInput{SchemaVersion: v1(), Channel: "C1", Cursor: p1.NextCursor}, b, p1.Binding)
	if err != nil || len(p2.Messages) != 1 || p2.HasMore || p2.NextCursor != "" {
		t.Fatalf("page 2: %+v %v", p2, err)
	}
	// A cursor presented under a different binding must fail typed, never replay.
	other := gws.Binding{Target: "slack:history", Query: "channel=C2"}
	if _, err := s.History(ctx, SlackHistoryInput{SchemaVersion: v1(), Channel: "C1", Cursor: p1.NextCursor}, other, p1.Binding); err == nil || err.Code != gws.CodeCursorMismatch {
		t.Errorf("cross-binding cursor: %v", err)
	}
	// Oversize page fails before any network effect.
	if _, err := s.History(ctx, SlackHistoryInput{SchemaVersion: v1(), Channel: "C1", Limit: 9999}, b, b); err == nil || err.Code != gws.CodeOutputLimit {
		t.Errorf("oversize limit: %v", err)
	}
	if _, err := s.History(ctx, SlackHistoryInput{SchemaVersion: v1()}, b, b); err == nil || err.Code != gws.CodeInvalidInput {
		t.Errorf("missing channel: %v", err)
	}
}

func TestSlackChannelListAndPostDelete(t *testing.T) {
	f := &fakeSlackData{msgs: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := NewSlack(srv.URL, testCanary)
	s.Throttle = fastThrottle()
	ctx := context.Background()
	b := gws.Binding{Target: "slack:channels", Query: ""}

	out, err := s.ChannelList(ctx, SlackChannelListInput{SchemaVersion: v1()}, b, b)
	if err != nil || len(out.Channels) != 1 || out.Channels[0].ID != "C1" || out.HasMore {
		t.Fatalf("channels: %+v %v", out, err)
	}

	sb := slackScope(true)
	dry := SlackPostInput{SchemaVersion: v1(), Channel: "C1", Text: "hi", Key: "k1", DryRun: true}
	if _, err := s.Post(ctx, sb, dry); err != nil || len(f.msgs) != 0 {
		t.Fatalf("dry run posted: %v", err)
	}
	ts, err := s.Post(ctx, sb, SlackPostInput{SchemaVersion: v1(), Channel: "C1", Text: "hi", Key: "k1"})
	if err != nil || ts == "" || len(f.msgs) != 1 {
		t.Fatalf("post: %q %v", ts, err)
	}
	if _, err := s.Post(ctx, sb, SlackPostInput{SchemaVersion: v1(), Channel: "C1", Text: "hi", Key: "k1"}); err == nil || err.Code != gws.CodePartialMutation {
		t.Fatalf("replay: %v", err)
	}
	if err := s.Delete(ctx, sb, SlackDeleteInput{SchemaVersion: v1(), Channel: "C1", TS: ts}); err != nil || len(f.msgs) != 0 {
		t.Fatalf("cleanup delete: %v left=%d", err, len(f.msgs))
	}
	if err := s.Delete(ctx, sb, SlackDeleteInput{SchemaVersion: v1(), Channel: "C1", TS: ts}); err == nil || err.Code != gws.CodeNotFound {
		t.Fatalf("second delete: %v", err)
	}
	// Non-sandbox scope never reaches the provider.
	prod := slackScope(false)
	if _, err := s.Post(ctx, prod, SlackPostInput{SchemaVersion: v1(), Channel: "C1", Text: "x", Key: "k2"}); err == nil || err.Code != gws.CodePermissionDenied {
		t.Fatalf("non-sandbox post: %v", err)
	}
	if len(f.msgs) != 0 {
		t.Fatal("non-sandbox post reached the provider")
	}
	// Unknown channel maps to typed not-found via the shared classifier.
	if _, err := s.History(ctx, SlackHistoryInput{SchemaVersion: v1(), Channel: "CX"}, b, b); err == nil || err.Code != gws.CodeNotFound {
		t.Errorf("unknown channel: %v", err)
	}
}

func TestNotionPageLifecycleWithCleanup(t *testing.T) {
	n := NewNotion("http://loop.invalid", testCanary)
	srv := httptest.NewServer(newFakeNotion())
	defer srv.Close()
	n.API.BaseURL = srv.URL
	n.Throttle = fastThrottle()
	ctx := context.Background()
	sc := notionScope()
	props := json.RawMessage(`{"Name":{"title":[{"text":{"content":"Fixture"}}]}}`)

	dry, err := n.PageCreate(ctx, sc, NotionPageCreateInput{SchemaVersion: v1(),
		Parent:     NotionParent{Type: "database_id", DatabaseID: "db-1"},
		Properties: props, Key: "nc-1", DryRun: true})
	if err != nil || dry.ID != "" {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	mk := func(key string) (NotionPage, *gws.Error) {
		return n.PageCreate(ctx, sc, NotionPageCreateInput{SchemaVersion: v1(),
			Parent:     NotionParent{Type: "database_id", DatabaseID: "db-1"},
			Properties: props, Key: key})
	}
	p1, err := mk("nc-1")
	if err != nil || p1.ID == "" || p1.URL == "" {
		t.Fatalf("create: %+v %v", p1, err)
	}
	if _, err := mk("nc-1"); err == nil || err.Code != gws.CodePartialMutation {
		t.Fatalf("replay: %v", err)
	}
	p2, err := mk("nc-2")
	if err != nil || p2.ID == p1.ID {
		t.Fatalf("second create collided: %+v %v", p2, err)
	}
	got, err := n.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: p1.ID})
	if err != nil || got.ID != p1.ID {
		t.Fatalf("get: %+v %v", got, err)
	}
	upd, err := n.PageUpdate(ctx, sc, NotionPageUpdateInput{SchemaVersion: v1(), PageID: p1.ID,
		Properties: json.RawMessage(`{"Name":{"title":[{"text":{"content":"Renamed"}}]}}`)})
	if err != nil || upd.ID != p1.ID {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := n.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "pg-missing"}); err == nil || err.Code != gws.CodeNotFound || err.ProviderCode != "object_not_found" {
		t.Fatalf("missing page: %+v", err)
	}
	if err := n.PageArchive(ctx, sc, NotionPageArchiveInput{SchemaVersion: v1(), PageID: p1.ID}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	archived, err := n.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: p1.ID})
	if err != nil || !archived.Archived {
		t.Fatalf("archived flag: %+v %v", archived, err)
	}
	if err := n.PageArchive(ctx, sc, NotionPageArchiveInput{SchemaVersion: v1(), PageID: p2.ID}); err != nil {
		t.Fatalf("cleanup archive: %v", err)
	}
	// An empty key derives deterministically from the payload: the first
	// create succeeds, the identical repeat is a ledger replay.
	d1, err := mk("")
	if err != nil || d1.ID == "" {
		t.Fatalf("derived-key create: %+v %v", d1, err)
	}
	if _, err := mk(""); err == nil || err.Code != gws.CodePartialMutation {
		t.Fatalf("derived-key replay: %v", err)
	}
	if err := n.PageArchive(ctx, sc, NotionPageArchiveInput{SchemaVersion: v1(), PageID: d1.ID}); err != nil {
		t.Fatalf("cleanup archive: %v", err)
	}
	// Input validation happens before effects.
	if _, err := n.PageCreate(ctx, sc, NotionPageCreateInput{SchemaVersion: v1(),
		Parent: NotionParent{Type: "bogus"}, Properties: props}); err == nil || err.Code != gws.CodeInvalidInput {
		t.Errorf("bad parent: %v", err)
	}
	if _, err := n.PageCreate(ctx, sc, NotionPageCreateInput{SchemaVersion: v1(),
		Parent: NotionParent{Type: "database_id", DatabaseID: "db-1"}}); err == nil || err.Code != gws.CodeInvalidInput {
		t.Errorf("missing properties: %v", err)
	}
}

func TestNotionQueryPagination(t *testing.T) {
	f := newFakeNotion()
	srv := httptest.NewServer(f)
	defer srv.Close()
	n := NewNotion(srv.URL, testCanary)
	n.Throttle = fastThrottle()
	ctx := context.Background()
	b := gws.Binding{Target: "notion:query", Query: "db-1"}

	p1, err := n.DatabaseQuery(ctx, NotionQueryInput{SchemaVersion: v1(), DatabaseID: "db-1", PageSize: 2}, b, b)
	if err != nil || len(p1.Pages) != 2 || !p1.HasMore || p1.NextCursor == "" {
		t.Fatalf("query page 1: %+v %v", p1, err)
	}
	if p1.Pages[0].ID == "" {
		t.Errorf("untyped page: %+v", p1.Pages[0])
	}
	p2, err := n.DatabaseQuery(ctx, NotionQueryInput{SchemaVersion: v1(), DatabaseID: "db-1", PageSize: 2, StartCursor: p1.NextCursor}, b, p1.Binding)
	if err != nil || len(p2.Pages) != 1 || p2.HasMore {
		t.Fatalf("query page 2: %+v %v", p2, err)
	}
	other := gws.Binding{Target: "notion:query", Query: "db-2"}
	if _, err := n.DatabaseQuery(ctx, NotionQueryInput{SchemaVersion: v1(), DatabaseID: "db-1", StartCursor: p1.NextCursor}, other, p1.Binding); err == nil || err.Code != gws.CodeCursorMismatch {
		t.Errorf("cross-binding cursor: %v", err)
	}
	if _, err := n.DatabaseQuery(ctx, NotionQueryInput{SchemaVersion: v1(), DatabaseID: "db-1", PageSize: 101}, b, b); err == nil || err.Code != gws.CodeOutputLimit {
		t.Errorf("oversize page: %v", err)
	}
}

func TestAirtableRecordLifecycleWithCleanup(t *testing.T) {
	f := newFakeAirtable()
	srv := httptest.NewServer(f)
	defer srv.Close()
	a := NewAirtable(srv.URL, testCanary)
	a.Throttle = fastThrottle()
	ctx := context.Background()
	sc := airtableScope()
	fields := json.RawMessage(`{"Name":"Fixture","Count":3}`)

	dry, err := a.RecordCreate(ctx, sc, AirtableRecordCreateInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", Fields: fields, Key: "ac-1", DryRun: true})
	if err != nil || dry.ID != "" {
		t.Fatalf("dry run: %+v %v", dry, err)
	}
	mk := func(key string) (AirtableRecord, *gws.Error) {
		return a.RecordCreate(ctx, sc, AirtableRecordCreateInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", Fields: fields, Key: key})
	}
	r1, err := mk("ac-1")
	if err != nil || r1.ID == "" {
		t.Fatalf("create: %+v %v", r1, err)
	}
	if _, err := mk("ac-1"); err == nil || err.Code != gws.CodePartialMutation {
		t.Fatalf("replay: %v", err)
	}
	r2, err := mk("ac-2")
	if err != nil || r2.ID == r1.ID {
		t.Fatalf("second create collided: %+v %v", r2, err)
	}
	// Precision: the count field must survive the round trip verbatim.
	if !bytes.Contains(r1.Fields, []byte(`"Count":3`)) {
		t.Errorf("fields not preserved: %s", r1.Fields)
	}
	got, err := a.RecordGet(ctx, AirtableRecordGetInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: r1.ID})
	if err != nil || got.ID != r1.ID {
		t.Fatalf("get: %+v %v", got, err)
	}
	upd, err := a.RecordUpdate(ctx, sc, AirtableRecordUpdateInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: r1.ID, Fields: json.RawMessage(`{"Name":"Renamed"}`)})
	if err != nil || upd.ID != r1.ID {
		t.Fatalf("update: %+v %v", upd, err)
	}
	b := gws.Binding{Target: "airtable:list", Query: "app-1/Things"}
	l1, err := a.RecordList(ctx, AirtableRecordListInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", PageSize: 1}, b, b)
	if err != nil || len(l1.Records) != 1 || l1.Offset == "" {
		t.Fatalf("list page 1: %+v %v", l1, err)
	}
	l2, err := a.RecordList(ctx, AirtableRecordListInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", PageSize: 1, Offset: l1.Offset}, b, l1.Binding)
	if err != nil || len(l2.Records) != 1 || l2.Offset != "" {
		t.Fatalf("list page 2: %+v %v", l2, err)
	}
	if err := a.RecordDelete(ctx, sc, AirtableRecordDeleteInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: r1.ID}); err != nil {
		t.Fatalf("cleanup delete r1: %v", err)
	}
	if err := a.RecordDelete(ctx, sc, AirtableRecordDeleteInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: r2.ID}); err != nil {
		t.Fatalf("cleanup delete r2: %v", err)
	}
	if err := a.RecordDelete(ctx, sc, AirtableRecordDeleteInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: r1.ID}); err == nil || err.Code != gws.CodeNotFound || err.ProviderCode != "NOT_FOUND" {
		t.Fatalf("second delete: %+v", err)
	}
	if _, err := a.RecordGet(ctx, AirtableRecordGetInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: "rec-missing"}); err == nil || err.Code != gws.CodeNotFound {
		t.Fatalf("missing record: %v", err)
	}
	d1, err := mk("")
	if err != nil || d1.ID == "" {
		t.Fatalf("derived-key create: %+v %v", d1, err)
	}
	if _, err := mk(""); err == nil || err.Code != gws.CodePartialMutation {
		t.Fatalf("derived-key replay: %v", err)
	}
	if err := a.RecordDelete(ctx, sc, AirtableRecordDeleteInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: d1.ID}); err != nil {
		t.Fatalf("cleanup delete: %v", err)
	}
	if _, err := a.RecordCreate(ctx, sc, AirtableRecordCreateInput{SchemaVersion: v1(), Base: "", Table: "Things", Fields: fields}); err == nil || err.Code != gws.CodeInvalidInput {
		t.Errorf("missing base: %v", err)
	}
}

func TestTypedErrorPaths(t *testing.T) {
	ctx := context.Background()
	nf := newFakeNotion()
	nf.flaky = &atomic.Int64{}
	nsrv := httptest.NewServer(nf)
	defer nsrv.Close()
	n := NewNotion(nsrv.URL, testCanary)
	n.Throttle = fastThrottle()

	// Wrong token: documented unauthorized shape -> expired credentials.
	bad := NewNotion(nsrv.URL, "wrong")
	bad.Throttle = fastThrottle()
	if _, err := bad.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "pg-1"}); err == nil || err.Code != gws.CodeExpiredCredentials {
		t.Errorf("notion invalid token: %+v", err)
	}
	// Persistent provider failure on a read.
	if _, err := n.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "down"}); err == nil || err.Code != gws.CodeProviderFailure {
		t.Errorf("notion 500: %+v", err)
	} else if n := nf.count("pages/down"); n != DefaultAttempts {
		t.Errorf("read retried %d times, want %d", n, DefaultAttempts)
	}
	// Missing endpoint/token fails before transport.
	if _, err := NewNotion("", "").PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "x"}); err == nil || err.Code != gws.CodeMissingCredentials {
		t.Errorf("no endpoint: %v", err)
	}
	// Cancelled context reports cancellation with unknown remote outcome.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := n.PageGet(cctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "pg-1"}); err == nil || err.Code != gws.CodeCancelled {
		t.Errorf("cancel: %v", err)
	}
	// Oversize response is an explicit output-limit error.
	small := NewNotion(nsrv.URL, testCanary)
	small.API.MaxBytes = 16
	small.Throttle = fastThrottle()
	if _, err := small.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "flaky"}); err == nil || err.Code != gws.CodeOutputLimit {
		t.Errorf("bounded output: %v", err)
	}

	af := newFakeAirtable()
	asrv := httptest.NewServer(af)
	defer asrv.Close()
	a := NewAirtable(asrv.URL, testCanary)
	a.Throttle = fastThrottle()
	badA := NewAirtable(asrv.URL, "wrong")
	badA.Throttle = fastThrottle()
	if _, err := badA.RecordGet(ctx, AirtableRecordGetInput{SchemaVersion: v1(), Base: "b", Table: "t", RecordID: "r"}); err == nil || err.Code != gws.CodeExpiredCredentials || err.ProviderCode != "AUTHENTICATION_REQUIRED" {
		t.Errorf("airtable invalid token: %+v", err)
	}

	f := &fakeSlackData{msgs: map[string]string{}}
	ssrv := httptest.NewServer(f)
	defer ssrv.Close()
	badS := NewSlack(ssrv.URL, "wrong")
	badS.Throttle = fastThrottle()
	b := gws.Binding{Target: "slack:history", Query: "C1"}
	if _, err := badS.History(ctx, SlackHistoryInput{SchemaVersion: v1(), Channel: "C1"}, b, b); err == nil || err.Code != gws.CodeExpiredCredentials {
		t.Errorf("slack invalid token: %v", err)
	}
	s := NewSlack(ssrv.URL, testCanary)
	s.Throttle = fastThrottle()
	if _, err := s.API.Do(ctx, "POST", "limited", nil, nil, nil); err == nil || err.Code != gws.CodeRateLimit || err.RetryAfter != "0" || !err.Retryable {
		t.Errorf("slack rate limit: %+v", err)
	}
}

func TestReadRetryAndMutationNoRetry(t *testing.T) {
	ctx := context.Background()
	nf := newFakeNotion()
	nf.flaky = &atomic.Int64{}
	srv := httptest.NewServer(nf)
	defer srv.Close()
	n := NewNotion(srv.URL, testCanary)
	n.Throttle = fastThrottle()

	// Flaky read: 429 with Retry-After then success — exactly two hits.
	got, err := n.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "flaky"})
	if err != nil || got.ID != "flaky" {
		t.Fatalf("flaky read: %+v %v", got, err)
	}
	if c := nf.count("pages/flaky"); c != 2 {
		t.Errorf("flaky read hit %d times, want 2", c)
	}

	// Mutations never retry: a failing create hits the provider exactly
	// once, and the failed ledger key is released so a later intent with
	// the same key may proceed.
	af := newFakeAirtable()
	asrv := httptest.NewServer(af)
	defer asrv.Close()
	a := NewAirtable(asrv.URL, testCanary)
	a.Throttle = fastThrottle()
	sc := airtableScope()
	fields := json.RawMessage(`{"Name":"Boom"}`)
	boom := AirtableRecordCreateInput{SchemaVersion: v1(), Base: "app-1", Table: "boom", Fields: fields, Key: "k-boom"}
	if _, err := a.RecordCreate(ctx, sc, boom); err == nil || err.Code != gws.CodeProviderFailure {
		t.Fatalf("boom create: %v", err)
	}
	if c := af.postCount("app-1/boom"); c != 1 {
		t.Fatalf("mutation retried: %d posts", c)
	}
	ok := AirtableRecordCreateInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", Fields: fields, Key: "k-boom"}
	rec, err := a.RecordCreate(ctx, sc, ok)
	if err != nil || rec.ID == "" {
		t.Fatalf("released key blocked: %v", err)
	}
	if err := a.RecordDelete(ctx, sc, AirtableRecordDeleteInput{SchemaVersion: v1(), Base: "app-1", Table: "Things", RecordID: rec.ID}); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestRedactionCanary(t *testing.T) {
	ctx := context.Background()
	nf := newFakeNotion()
	srv := httptest.NewServer(nf)
	defer srv.Close()
	n := NewNotion(srv.URL, testCanary)
	n.Throttle = fastThrottle()

	// The fakes echo the token inside provider error bodies; no error
	// surface — message, loom-v2 envelope, provider code — may carry it.
	_, nerr := n.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "pg-missing"})
	if nerr == nil {
		t.Fatal("want a not-found error")
	}
	af := newFakeAirtable()
	asrv := httptest.NewServer(af)
	defer asrv.Close()
	a := NewAirtable(asrv.URL, testCanary)
	a.Throttle = fastThrottle()
	_, aerr := a.RecordGet(ctx, AirtableRecordGetInput{SchemaVersion: v1(), Base: "b", Table: "t", RecordID: "rec-missing"})
	if aerr == nil {
		t.Fatal("want a not-found error")
	}
	badN := NewNotion(srv.URL, "wrong")
	badN.Throttle = fastThrottle()
	_, uerr := badN.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "x"})
	if uerr == nil {
		t.Fatal("want an auth error")
	}
	for _, c := range []struct {
		name   string
		err    *gws.Error
		secret string
	}{
		{"notion", nerr, testCanary},
		{"airtable", aerr, testCanary},
		{"unauthorized", uerr, "wrong"},
	} {
		var buf bytes.Buffer
		c.err.EmitError(&buf, c.name)
		if all := c.err.Message + buf.String() + c.err.ProviderCode; strings.Contains(all, c.secret) {
			t.Errorf("%s leaked the token: %s", c.name, all)
		}
	}
	// The Slack fake echoes the token inside a provider error body.
	ssrv := httptest.NewServer(&fakeSlackData{msgs: map[string]string{}})
	defer ssrv.Close()
	s := NewSlack(ssrv.URL, testCanary)
	s.Throttle = fastThrottle()
	_, serr := s.API.Do(ctx, "GET", "echo", nil, nil, nil)
	if serr == nil {
		t.Fatal("want a permission error from echo")
	}
	var sbuf bytes.Buffer
	serr.EmitError(&sbuf, "slack")
	if all := serr.Message + sbuf.String(); strings.Contains(all, testCanary) {
		t.Errorf("slack leaked the token: %s", all)
	}
	args, masked := gws.RedactArgs([]string{"x", "--token", testCanary}, testCanary)
	if masked == 0 || strings.Contains(strings.Join(args, " "), testCanary) {
		t.Errorf("argv canary: %v", args)
	}
}

func TestThrottleEnforcesInterval(t *testing.T) {
	srv := httptest.NewServer(newFakeNotion())
	defer srv.Close()
	n := NewNotion(srv.URL, testCanary)
	n.Throttle = NewThrottle(50 * time.Millisecond)
	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 2; i++ {
		if _, err := n.PageGet(ctx, NotionPageGetInput{SchemaVersion: v1(), PageID: "pg-missing"}); err == nil {
			t.Fatal("want not-found")
		}
	}
	// The second dispatch must wait out the interval (slowness only
	// lengthens this; the assertion is a lower bound).
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("throttle did not gate: %v", elapsed)
	}
}

func TestEmitPageEnvelope(t *testing.T) {
	var buf bytes.Buffer
	items := []any{
		SlackMessage{TS: "1.0", User: "U1", Text: "hi"},
		SlackMessage{TS: "2.0", User: "U2", Text: "yo"},
	}
	if code := EmitPage(&buf, "slack-history", items, "c1"); code != 0 {
		t.Fatalf("emit partial: %d", code)
	}
	var env struct {
		SchemaVersion string `json:"schema_version"`
		Command       string `json:"command"`
		Status        string `json:"status"`
		Items         []any  `json:"items"`
		NextToken     string `json:"next_page_token"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &env); err != nil {
		t.Fatalf("unreadable envelope: %v", err)
	}
	if env.SchemaVersion != "loom-v2" || env.Status != "partial" || env.NextToken != "c1" || len(env.Items) != 2 {
		t.Errorf("partial envelope: %+v", env)
	}
	buf.Reset()
	if code := EmitPage(&buf, "slack-history", items, ""); code != 0 {
		t.Fatalf("emit ok: %d", code)
	}
	var env2 struct {
		Status string `json:"status"`
		Total  int    `json:"total"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &env2); err != nil {
		t.Fatalf("unreadable envelope: %v", err)
	}
	if env2.Status != "ok" || env2.Total != 2 {
		t.Errorf("complete envelope: %+v", env2)
	}
}

func TestSharedScopeAndOpsReuse(t *testing.T) {
	// Notion and Airtable resolve through the shared saasclients secret
	// facility: explicit target + credential file, no ambient fallback.
	for _, client := range []string{"notion", "airtable"} {
		if _, err := saasclients.ResolveScope(client, func(string) string { return "" }); err == nil || err.Code != gws.CodeMissingCredentials {
			t.Errorf("%s ambient scope accepted: %v", client, err)
		}
	}
	// The capability rows live in the shared Ops table with a strategy
	// for every mutation.
	for _, op := range []string{"page-get", "database-query", "page-create", "page-update", "page-archive"} {
		o, ok := saasclients.LookupOp("notion", op)
		if !ok || o.Via != saasclients.ViaAPI {
			t.Errorf("notion %s: %+v", op, o)
		}
	}
	for _, op := range []string{"record-list", "record-get", "record-create", "record-update", "record-delete"} {
		o, ok := saasclients.LookupOp("airtable", op)
		if !ok || o.Via != saasclients.ViaAPI {
			t.Errorf("airtable %s: %+v", op, o)
		}
	}
	// Slack data actions stay on the merged Web API adapter.
	for _, op := range []string{"history", "channel-list", "message-post", "message-delete"} {
		o, ok := saasclients.LookupOp("slack", op)
		if !ok || o.Via != saasclients.ViaAPI {
			t.Errorf("slack %s: %+v", op, o)
		}
	}
}
