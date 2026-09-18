// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/room"
)

// inboxInTemp points the bus stores at a scratch directory and seeds mail.
//
// A REAL server against a REAL room on disk, for the same reason the board
// panel's tests insist on it: the console has already shipped bugs whose only
// cause was a contract modelled on a mock.
func inboxInTemp(t *testing.T, events ...room.Event) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("USER", "operator")
	for _, e := range events {
		if e.Type == "" {
			e.Type = room.EventNotify
		}
		if err := room.Notify(e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return dir
}

func roomFingerprint(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// THE panel's defining property. Polling this page must not consume mail.
//
// The page repaints every five seconds, so a read path that materialized
// backlog or advanced a cursor — which the CLI's own read path does, correctly,
// for its own single reader — would drain the entire fleet's inbox while nobody
// was even looking at a message. And it would do so INVISIBLY: the mail is not
// lost, just marked handed-over to an agent that was never handed it.
func TestInboxPanelIsReadOnlyOnDisk(t *testing.T) {
	dir := inboxInTemp(t,
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		room.Event{Principal: "cairn", To: "lintel", Topic: "gate", Body: "86/86"},
	)
	h := newTestHandler(t, Options{})

	before := roomFingerprint(t, dir)
	for _, p := range []string{
		"/api/inbox", "/api/inbox/cairn", "/api/inbox/lintel",
		"/api/inbox/operator", "/api/inbox/nobody-here",
		"/api/inbox/cairn?state=unread", "/api/inbox/cairn?q=pick",
	} {
		if w := do(h, "GET", p, "127.0.0.1:5555", nil); w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (%s)", p, w.Code, strings.TrimSpace(w.Body.String()))
		}
	}
	after := roomFingerprint(t, dir)

	if len(before) != len(after) {
		t.Fatalf("the panel changed the file set: %d before, %d after", len(before), len(after))
	}
	for path, sum := range before {
		if after[path] != sum {
			t.Errorf("the panel rewrote %s — a read of somebody else's inbox must not consume it", path)
		}
	}
}

// There is exactly ONE write route and it is NAMELESS.
//
// The panel has one write — mark my own mail read — and no route through which
// any OTHER inbox can be named. That is stronger than a `name == viewer` check
// on a `/{name}/read` route, because a check can be refactored, mis-cased or
// forgotten and the failure is SILENT: marking another agent's mail read looks
// like nothing from anywhere, the message stays durable on the timeline and is
// simply never handed over.
//
// A non-GET on the read routes falls through to the SPA catch-all, so the
// observable form of "no route" is that nothing answers with the panel's own
// payload — the console has no method-based dispatch that could 405 here.
func TestInboxPanelHasNoPerNameWriteRoute(t *testing.T) {
	inboxInTemp(t, room.Event{Principal: "operator", To: "cairn", Topic: "t", Body: "b"})
	h := newTestHandler(t, Options{})
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		for _, p := range []string{
			"/api/inbox", "/api/inbox/cairn",
			// The shapes somebody would reach for to mark ANOTHER inbox read.
			"/api/inbox/cairn/read", "/api/inbox/read/cairn",
		} {
			w := do(h, m, p, "127.0.0.1:5555", nil)
			if strings.Contains(w.Header().Get("Content-Type"), "json") ||
				strings.Contains(w.Body.String(), inboxSchemaVersion) {
				t.Errorf("%s %s answered with the panel's payload; only the nameless "+
					"POST /api/inbox/read may write", m, p)
			}
		}
	}
}

// postJSON to the mark-read route.
func markRead(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/inbox/read", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Marking read touches the VIEWER'S OWN stores and nothing else.
//
// This is the boundary the whole panel is built around, so it is asserted
// positively (the viewer's count actually falls) AND negatively (every other
// name's files are byte-identical afterwards). A write that worked but also
// consumed an agent's mail would pass a test that only checked the first half.
func TestInboxMarkReadTouchesOnlyTheViewersOwnInbox(t *testing.T) {
	dir := inboxInTemp(t,
		room.Event{Principal: "cairn", To: "operator", Topic: "done", Body: "merged"},
		room.Event{Principal: "cairn", To: "operator", Topic: "gate", Body: "86/86"},
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		room.Event{Principal: "operator", To: "lintel", Topic: "gate", Body: "rerun"},
	)
	h := newTestHandler(t, Options{})

	if got, _ := getJSON(t, h, "/api/inbox?summary=1")["viewer_unread"].(float64); got != 2 {
		t.Fatalf("viewer_unread = %v before marking, want 2", got)
	}
	before := roomFingerprint(t, dir)

	w := markRead(h, `{"all":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("mark all read = %d (%s)", w.Code, strings.TrimSpace(w.Body.String()))
	}

	// The viewer's own count falls to zero...
	sum := getJSON(t, h, "/api/inbox?summary=1")
	if got, _ := sum["viewer_unread"].(float64); got != 0 {
		t.Errorf("viewer_unread = %v after marking all read, want 0", got)
	}
	// ...and NOBODY else's does. cairn and lintel were never handed anything.
	if got, _ := sum["unread"].(float64); got != 2 {
		t.Errorf("fleet unread = %v, want 2 — marking my own mail read must not "+
			"consume cairn's or lintel's", got)
	}

	// The negative half: no file belonging to another name changed. The viewer's
	// own cursor and pending buffer are expected to; every other path is not.
	after := roomFingerprint(t, dir)
	for path, sum := range after {
		if before[path] == sum {
			continue
		}
		if !strings.Contains(path, "operator") && !strings.HasSuffix(path, "timeline.jsonl") {
			t.Errorf("marking the viewer's mail read rewrote %s, which is not theirs", path)
		}
	}
	for _, name := range []string{"cairn", "lintel"} {
		for _, sub := range []string{"cursors/" + name, "pending/" + name + ".jsonl"} {
			if before[sub] != after[sub] {
				t.Errorf("%s changed while marking the VIEWER's mail read", sub)
			}
		}
	}
}

// Marking ONE message read consumes exactly that message.
//
// A cursor write would have swallowed everything below it; CommitItem does not,
// which is what makes a per-message control safe to offer at all.
func TestInboxMarkOneReadDoesNotConsumeTheRest(t *testing.T) {
	inboxInTemp(t,
		room.Event{Principal: "cairn", To: "operator", Topic: "one", Body: "first"},
		room.Event{Principal: "cairn", To: "operator", Topic: "two", Body: "second"},
		room.Event{Principal: "cairn", To: "operator", Topic: "three", Body: "third"},
	)
	h := newTestHandler(t, Options{})

	// Take the NEWEST message's seq and mark only it.
	items := getJSON(t, h, "/api/inbox/operator")["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("operator has %d items, want 3", len(items))
	}
	newest := int64(items[2].(map[string]any)["seq"].(float64))

	w := markRead(h, `{"seq":`+strconv.FormatInt(newest, 10)+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("mark one read = %d (%s)", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got, _ := getJSON(t, h, "/api/inbox?summary=1")["viewer_unread"].(float64); got != 2 {
		t.Fatalf("viewer_unread = %v after marking the NEWEST of three read, want 2 — "+
			"a cursor write would have consumed the two below it", got)
	}
}

func TestInboxMarkReadRefusesAnAmbiguousRequest(t *testing.T) {
	inboxInTemp(t, room.Event{Principal: "cairn", To: "operator", Topic: "t", Body: "b"})
	h := newTestHandler(t, Options{})
	for _, body := range []string{`{}`, `{"all":true,"seq":1}`, `{`, `{"seq":0}`} {
		if w := markRead(h, body); w.Code != http.StatusBadRequest {
			t.Errorf("mark read %s = %d, want 400", body, w.Code)
		}
	}
}

// The viewer is the page's fixed point and comes first in the nav.
func TestInboxRosterPutsTheViewerFirst(t *testing.T) {
	inboxInTemp(t,
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		room.Event{Principal: "cairn", To: "operator", Topic: "done", Body: "merged"},
	)
	h := newTestHandler(t, Options{})
	d := getJSON(t, h, "/api/inbox")

	if d["viewer"] != "operator" {
		t.Fatalf("viewer = %v, want operator", d["viewer"])
	}
	groups, _ := d["groups"].([]any)
	if len(groups) == 0 {
		t.Fatal("no groups in the roster")
	}
	first, _ := groups[0].(map[string]any)
	if first["kind"] != bus.InboxKindPerson {
		t.Fatalf("first group = %v, want the viewer's own — the human's row must not move as the fleet talks", first["kind"])
	}
	names, _ := first["names"].([]any)
	if len(names) != 1 || names[0] != "operator" {
		t.Fatalf("first group names = %v, want [operator]", names)
	}
}

// A name that was written to but never registered still has mail waiting, and a
// roster built from the catalog alone would hide exactly the backlog nobody owns.
func TestInboxRosterListsUnregisteredRecipients(t *testing.T) {
	inboxInTemp(t, room.Event{Principal: "cairn", To: "codex-w12", Topic: "issue", Body: "take #12"})
	h := newTestHandler(t, Options{})
	d := getJSON(t, h, "/api/inbox")

	found := false
	for _, raw := range d["holders"].([]any) {
		if h, _ := raw.(map[string]any); h["name"] == "codex-w12" {
			found = true
			if h["unread"].(float64) != 1 {
				t.Errorf("codex-w12 unread = %v, want 1", h["unread"])
			}
		}
	}
	if !found {
		t.Fatal("codex-w12 is missing: an addressed name with no catalog entry is exactly what this page is for")
	}
}

func TestInboxListFiltersAndFacets(t *testing.T) {
	inboxInTemp(t,
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		room.Event{Principal: "lintel", To: "cairn", Topic: "gate", Body: "86/86 green"},
		room.Event{Principal: "lintel", To: "cairn", Topic: "gate", Body: "rerun requested"},
	)
	h := newTestHandler(t, Options{})

	all := getJSON(t, h, "/api/inbox/cairn")
	if all["total"].(float64) != 3 || all["unread"].(float64) != 3 {
		t.Fatalf("cairn = %v total / %v unread, want 3/3", all["total"], all["unread"])
	}
	// Chronological: the API answers oldest-first, which is the order the page
	// renders and the order the conversation happened in.
	items := all["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	var last float64
	for _, raw := range items {
		seq := raw.(map[string]any)["seq"].(float64)
		if seq < last {
			t.Fatalf("items are not chronological: %v after %v", seq, last)
		}
		last = seq
	}

	// Facets come from the WHOLE inbox, not the filtered slice: a dropdown that
	// listed only what the current filter matched could not change the filter.
	filtered := getJSON(t, h, "/api/inbox/cairn?from=lintel")
	if filtered["matched"].(float64) != 2 {
		t.Errorf("from=lintel matched %v, want 2", filtered["matched"])
	}
	facets := filtered["facets"].(map[string]any)["from"].([]any)
	if len(facets) != 2 {
		t.Errorf("from facet has %d entries under a from filter, want 2 (the whole inbox)", len(facets))
	}

	if got := getJSON(t, h, "/api/inbox/cairn?q=green")["matched"].(float64); got != 1 {
		t.Errorf("q=green matched %v, want 1", got)
	}
	if got := getJSON(t, h, "/api/inbox/cairn?state=read")["matched"].(float64); got != 0 {
		t.Errorf("state=read matched %v, want 0 — nothing has been read", got)
	}
}

func TestInboxTileIsDeclaredAndNotDoubleMounted(t *testing.T) {
	var found bool
	for _, p := range Discover() {
		if p.Name == "inbox" {
			found = true
			if p.Path != "/inbox/" || p.Source != "atlas" || !p.Available {
				t.Fatalf("inbox panel = %+v, want an available atlas panel at /inbox/", p)
			}
		}
	}
	if !found {
		t.Fatal("no inbox tile: the atlas WebSurface declaration is what Discover reads")
	}
	// Like the terminal and the board, the page is served at the launcher's root
	// so it keeps the launcher's <base href>; panelHandler must claim nothing.
	s := &server{}
	if h, _ := s.panelHandler(Panel{Name: "inbox", Path: "/inbox/"}); h != nil {
		t.Fatal("inbox must not also be mounted: coopauth.Mount would take the <base href> with it")
	}
}

// A disabled panel must be UNROUTED, not merely untiled. Dropping it from the
// tile list alone would leave every agent's mail readable by anyone who typed
// the path.
func TestDisabledInboxIsAlsoUnrouted(t *testing.T) {
	inboxInTemp(t, room.Event{Principal: "operator", To: "cairn", Topic: "t", Body: "b"})
	h := newTestHandler(t, Options{Disable: []string{"inbox"}})
	for _, p := range []string{"/inbox/", "/api/inbox", "/api/inbox/cairn"} {
		if w := do(h, "GET", p, "127.0.0.1:5555", nil); w.Code == http.StatusOK &&
			strings.Contains(w.Header().Get("Content-Type"), "json") {
			t.Errorf("GET %s still answers with the disabled panel's data", p)
		}
	}
}

// The summary is what the launcher badge reads, and it separates two counts
// that must never be merged.
//
// The panel is a PEEK: looking at an agent's inbox advances nothing, so that
// agent's backlog falls only when the agent itself reads. `viewer_unread` is
// therefore the only count the person looking can clear, and the only one that
// works as a badge. `unread` is the fleet gauge. One field for both would put a
// number on the tile that the reader's own actions can never move.
func TestInboxSummarySeparatesTheViewersCountFromTheFleets(t *testing.T) {
	inboxInTemp(t,
		room.Event{Principal: "cairn", To: "operator", Topic: "done", Body: "merged"},
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		room.Event{Principal: "operator", To: "lintel", Topic: "gate", Body: "rerun"},
		room.Event{Principal: "operator", To: "lintel", Topic: "gate", Body: "still red"},
	)
	h := newTestHandler(t, Options{})
	d := getJSON(t, h, "/api/inbox?summary=1")

	for field, want := range map[string]float64{
		"viewer_unread": 1, // only what is waiting for the operator
		"unread":        4, // every inbox on the host, theirs included
		"total":         4,
		"waiting":       3, // operator, cairn, lintel
	} {
		if got, _ := d[field].(float64); got != want {
			t.Errorf("%s = %v, want %v", field, d[field], want)
		}
	}
	if d["viewer"] != "operator" {
		t.Errorf("viewer = %v", d["viewer"])
	}
	// A summary is six numbers. Shipping 175 holder rows to a poller that wants
	// a badge is the waste the flag exists to avoid.
	if _, ok := d["holders"]; ok {
		t.Error("summary carried the holder list")
	}
	if _, ok := d["groups"]; ok {
		t.Error("summary carried the group list")
	}

	// And the full roster states the same numbers, so the badge and the panel
	// can never disagree about how much is waiting.
	full := getJSON(t, h, "/api/inbox")
	for _, field := range []string{"viewer_unread", "unread", "total", "waiting", "inboxes"} {
		if full[field] != d[field] {
			t.Errorf("%s: roster says %v, summary says %v", field, full[field], d[field])
		}
	}
	if _, ok := full["holders"]; !ok {
		t.Error("the full roster dropped its holders")
	}
}

// Reading the summary is a read like any other on this panel.
func TestInboxSummaryWritesNothing(t *testing.T) {
	dir := inboxInTemp(t,
		room.Event{Principal: "operator", To: "cairn", Topic: "t", Body: "b"},
	)
	h := newTestHandler(t, Options{})
	before := roomFingerprint(t, dir)
	for range 3 {
		if w := do(h, "GET", "/api/inbox?summary=1", "127.0.0.1:5555", nil); w.Code != http.StatusOK {
			t.Fatalf("summary = %d", w.Code)
		}
	}
	for path, sum := range roomFingerprint(t, dir) {
		if before[path] != sum {
			t.Errorf("the summary rewrote %s", path)
		}
	}
}

// The cached snapshot must not outlive the fact it was read from.
//
// The launcher polls this every five seconds, so a cache keyed on a timer would
// either miss every request (any TTL below the poll interval) or report a stale
// count after a message landed. It is keyed on the timeline's stat instead.
func TestInboxSummaryReflectsMailThatArrivesBetweenPolls(t *testing.T) {
	inboxInTemp(t, room.Event{Principal: "cairn", To: "operator", Topic: "one", Body: "first"})
	h := newTestHandler(t, Options{})

	if got, _ := getJSON(t, h, "/api/inbox?summary=1")["viewer_unread"].(float64); got != 1 {
		t.Fatalf("viewer_unread = %v, want 1", got)
	}
	if err := room.Notify(room.Event{
		Type: room.EventNotify, Principal: "cairn", To: "operator", Topic: "two", Body: "second",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := getJSON(t, h, "/api/inbox?summary=1")["viewer_unread"].(float64); got != 2 {
		t.Fatalf("viewer_unread = %v after a second message, want 2 — the snapshot is stale", got)
	}
}
