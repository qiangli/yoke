package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

const testInbox = "https://inbox.example/u/42/inbox"

// deliverFixture is a registered fake tool "fakeclaw" whose "send" command is
// a tiny script appending its argv to a log, so delivery runs through the
// real toolcmd path.
type deliverFixture struct {
	te       *testEnv
	log      string
	failFlag string
	now      time.Time
	server   map[string]bool
}

func newDeliverFixture(t *testing.T, te *testEnv) *deliverFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake tool is a shell script")
	}
	root := fleettest.Ring(t)
	work := t.TempDir()
	fx := &deliverFixture{
		te:       te,
		log:      filepath.Join(work, "calls.log"),
		failFlag: filepath.Join(work, "fail"),
		now:      time.Date(2030, 1, 2, 12, 0, 0, 0, time.Local),
		server:   map[string]bool{},
	}
	script := filepath.Join(work, "fakeclaw")
	body := fmt.Sprintf("#!/bin/sh\n[ -e %q ] && exit 3\nfor a in \"$@\"; do printf '%%s\\001' \"$a\"; done >> %q\nprintf '\\002' >> %q\n",
		fx.failFlag, fx.log, fx.log)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := fleet.Tool{Name: "fakeclaw", Kind: fleet.ToolKindCLI}
	tool.CLI.Binary = script
	tool.Commands = []fleet.ToolCommand{{
		Name: "send", Slash: "/send {args}", Mode: "print", Confirm: true,
		Exec: script + " send {args}",
	}}
	if err := fleet.New(fleet.WithRoot(root)).SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	te.env.Now = func() time.Time { return fx.now }
	te.env.inboxURL = func(context.Context) string { return testInbox }
	te.env.serverRead = func(context.Context) (map[string]bool, error) { return fx.server, nil }
	return fx
}

// calls returns every recorded invocation's argv.
func (fx *deliverFixture) calls(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(fx.log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out [][]string
	for _, rec := range strings.Split(string(b), "\x02") {
		if rec == "" {
			continue
		}
		out = append(out, strings.Split(strings.TrimSuffix(rec, "\x01"), "\x01"))
	}
	return out
}

func (fx *deliverFixture) writeState(t *testing.T, name string) {
	t.Helper()
	if err := saveChannelState(fx.te.env.dir(), map[string]channelState{name: {Seq: 1}}); err != nil {
		t.Fatal(err)
	}
}

// message extracts the --message value of one recorded call.
func messageOf(t *testing.T, argv []string) string {
	t.Helper()
	for i, a := range argv {
		if a == "--message" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatalf("no --message in %q", argv)
	return ""
}

type dlvPost struct{ id, author, headline string }

// spool appends posts with the next seqs and returns their seqs.
func dlvSpool(t *testing.T, dir string, posts ...dlvPost) []int64 {
	t.Helper()
	existing, err := ReadSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	next := int64(len(existing)) + 1
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(spoolPath(dir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var seqs []int64
	for _, p := range posts {
		e := SpoolEntry{
			Seq: next, ID: p.id, Source: "https://feed.example/alice", Rel: "posts/" + p.id + ".md",
			Status: StatusVerified, Fetched: "2030-01-01T00:00:00Z", Created: "2030-01-01T00:00:00Z",
			Author: p.author, Headline: p.headline,
		}
		line, _ := json.Marshal(e)
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, next)
		next++
	}
	return seqs
}

func addChannel(te *testEnv, name string, extra ...string) {
	te.t.Helper()
	te.mustRun(append([]string{"channel", "add", name, "--cmd", "fakeclaw:send"}, extra...)...)
}

func TestDeliverOneBatchPerChannelNoResend(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	dir := te.env.dir()
	addChannel(te, "wide", "--args", "--to wide")
	addChannel(te, "narrow", "--args", "--to 'n n'", "--max", "2")
	dlvSpool(t, dir,
		dlvPost{"p1", "alice", "First post"},
		dlvPost{"p2", "bob", "Second post"},
		dlvPost{"p3", "carol", "Third post"})

	te.mustRun("deliver")
	calls := fx.calls(t)
	if len(calls) != 2 {
		t.Fatalf("want one call per channel, got %d: %q", len(calls), calls)
	}
	wide, narrow := calls[0], calls[1]
	if got := strings.Join(wide[:4], " "); got != "send --to wide --message" {
		t.Fatalf("wide argv = %q", wide)
	}
	if got := strings.Join(narrow[:4], "|"); got != "send|--to|n n|--message" {
		t.Fatalf("narrow argv = %q (args must keep their quoting)", narrow)
	}
	wmsg, nmsg := messageOf(t, wide), messageOf(t, narrow)
	for _, h := range []string{"First post", "Second post", "Third post"} {
		if !strings.Contains(wmsg, h) {
			t.Errorf("wide message missing %q:\n%s", h, wmsg)
		}
	}
	if !strings.Contains(wmsg, "https://feed.example/alice/posts/p1.md") {
		t.Errorf("message lacks the post link:\n%s", wmsg)
	}
	if strings.Contains(wmsg, "more") {
		t.Errorf("wide message collapsed items:\n%s", wmsg)
	}
	if n := strings.Count(nmsg, "post "); n != 2 || !strings.Contains(nmsg, "+1 more → "+testInbox) {
		t.Errorf("narrow message must show 2 items and +1 more with the inbox link:\n%s", nmsg)
	}

	// Same instant: no resend.
	te.mustRun("deliver")
	if n := len(fx.calls(t)); n != 2 {
		t.Fatalf("delivered items were resent: %d calls", n)
	}

	// A new post waits out the interval, then only it goes out.
	dlvSpool(t, dir, dlvPost{"p4", "alice", "Fourth post"})
	fx.now = fx.now.Add(time.Hour)
	te.mustRun("deliver")
	if n := len(fx.calls(t)); n != 2 {
		t.Fatalf("sent inside the interval: %d calls", n)
	}
	fx.now = fx.now.Add(4 * time.Hour)
	te.mustRun("deliver")
	fx.now = fx.now.Add(5 * time.Hour)
	te.mustRun("deliver")
	calls = fx.calls(t)
	if len(calls) != 4 {
		t.Fatalf("want 4 calls, got %d", len(calls))
	}
	for _, c := range calls[2:] {
		m := messageOf(t, c)
		if !strings.Contains(m, "Fourth post") || strings.Contains(m, "First post") || strings.Contains(m, "Third post") {
			t.Errorf("second batch = %q", m)
		}
	}
}

func TestDeliverDropsReadItems(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	dir := te.env.dir()
	addChannel(te, "c")
	seqs := dlvSpool(t, dir,
		dlvPost{"p1", "alice", "Read via cursor"},
		dlvPost{"p2", "bob", "Read on the web"},
		dlvPost{"p3", "carol", "Still unread"})
	if err := WriteCursor(dir, "tester", seqs[0]); err != nil {
		t.Fatal(err)
	}
	fx.server["p2"] = true

	te.mustRun("deliver")
	calls := fx.calls(t)
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	m := messageOf(t, calls[0])
	if !strings.Contains(m, "Still unread") || strings.Contains(m, "Read via cursor") || strings.Contains(m, "Read on the web") {
		t.Fatalf("message = %q", m)
	}
}

func TestDeliverAllReadSendsNothing(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	addChannel(te, "c")
	seqs := dlvSpool(t, te.env.dir(), dlvPost{"p1", "alice", "One"})
	if err := WriteCursor(te.env.dir(), "tester", seqs[0]); err != nil {
		t.Fatal(err)
	}
	te.mustRun("deliver")
	if n := len(fx.calls(t)); n != 0 {
		t.Fatalf("sent %d calls for read-only items", n)
	}
}

func TestDeliverServerReadErrorFallsBackToCursor(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	te.env.serverRead = func(context.Context) (map[string]bool, error) { return nil, errors.New("offline") }
	addChannel(te, "c")
	dlvSpool(t, te.env.dir(), dlvPost{"p1", "alice", "One"})
	_, stderr, err := te.run("deliver")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "offline") {
		t.Errorf("no warning about the unavailable server set: %q", stderr)
	}
	if n := len(fx.calls(t)); n != 1 {
		t.Fatalf("got %d calls", n)
	}
}

func TestDeliverFailureLeavesStateUnchanged(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	dir := te.env.dir()
	addChannel(te, "c")
	dlvSpool(t, dir, dlvPost{"p1", "alice", "First post"}, dlvPost{"p2", "bob", "Second post"})

	if err := os.WriteFile(fx.failFlag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := te.run("deliver"); err == nil {
		t.Fatal("a failing command must fail deliver")
	}
	if _, err := os.Stat(channelsStatePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("state was written after a failed send: %v", err)
	}

	// A prior position survives a failed send untouched.
	if err := saveChannelState(dir, map[string]channelState{"c": {Seq: 1, Last: "2020-01-01T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(channelsStatePath(dir))
	if _, _, err := te.run("deliver"); err == nil {
		t.Fatal("a failing command must fail deliver")
	}
	after, _ := os.ReadFile(channelsStatePath(dir))
	if string(before) != string(after) {
		t.Fatalf("state changed after a failed send:\n%s\n%s", before, after)
	}

	// Recovery: the same item is sent, the delivered one is not.
	if err := os.Remove(fx.failFlag); err != nil {
		t.Fatal(err)
	}
	te.mustRun("deliver")
	calls := fx.calls(t)
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	if m := messageOf(t, calls[0]); !strings.Contains(m, "Second post") || strings.Contains(m, "First post") {
		t.Fatalf("message = %q", m)
	}
	st, _ := loadChannelState(dir)
	if st["c"].Seq != 2 || st["c"].Last == "" {
		t.Fatalf("state after success = %+v", st["c"])
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(channelsStatePath(dir)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("channels-state.json: %v %v", fi, err)
		}
	}
}

func TestDeliverDryRun(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	dir := te.env.dir()
	addChannel(te, "c")
	dlvSpool(t, dir, dlvPost{"p1", "alice", "First post"})

	out := te.mustRun("deliver", "--dry-run")
	if !strings.Contains(out, "First post") || !strings.Contains(out, "channel c: would send 1 item") {
		t.Fatalf("dry-run output = %q", out)
	}
	if n := len(fx.calls(t)); n != 0 {
		t.Fatalf("dry-run ran the command %d times", n)
	}
	if _, err := os.Stat(channelsStatePath(dir)); !os.IsNotExist(err) {
		t.Fatal("dry-run wrote state")
	}
	te.mustRun("deliver")
	if n := len(fx.calls(t)); n != 1 {
		t.Fatalf("the dry-run consumed the batch: %d calls", n)
	}
}

func TestDeliverQuietHoursImmediateAuthor(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	dir := te.env.dir()
	addChannel(te, "c", "--immediate", "alice")
	dlvSpool(t, dir,
		dlvPost{"p1", "bob", "Bob waits"},
		dlvPost{"p2", "alice", "Alice breaks through"},
		dlvPost{"p3", "carol", "Carol waits"})

	fx.now = time.Date(2030, 1, 2, 23, 0, 0, 0, time.Local)
	te.mustRun("deliver")
	calls := fx.calls(t)
	if len(calls) != 1 {
		t.Fatalf("got %d calls", len(calls))
	}
	if m := messageOf(t, calls[0]); !strings.Contains(m, "Alice breaks through") || strings.Contains(m, "waits") {
		t.Fatalf("quiet-hours message = %q", m)
	}
	te.mustRun("deliver")
	if n := len(fx.calls(t)); n != 1 {
		t.Fatalf("immediate post resent: %d calls", n)
	}

	fx.now = time.Date(2030, 1, 3, 12, 0, 0, 0, time.Local)
	te.mustRun("deliver")
	calls = fx.calls(t)
	if len(calls) != 2 {
		t.Fatalf("got %d calls", len(calls))
	}
	if m := messageOf(t, calls[1]); !strings.Contains(m, "Bob waits") || !strings.Contains(m, "Carol waits") || strings.Contains(m, "Alice") {
		t.Fatalf("daytime message = %q", m)
	}
	te.mustRun("deliver")
	fx.now = fx.now.Add(6 * time.Hour)
	te.mustRun("deliver")
	if n := len(fx.calls(t)); n != 2 {
		t.Fatalf("everything was consumed, yet %d calls", n)
	}
}

func TestDeliverDefaultWiring_ServerReadAndInboxDryRun(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	const srvInbox = "https://inbox.example/u/9"
	var calls int
	var gotIDs []string
	var gotAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/feed/read" {
			t.Errorf("expected /api/v1/feed/read, got %s", r.URL.Path)
		}
		gotIDs = r.URL.Query()["ids"]
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ServerReadResponse{
			Read:  []string{"p4"},
			Inbox: srvInbox,
		})
	}))
	defer srv.Close()

	// Default wiring: no explicit serverRead/inboxURL funcs.
	te.env.Cloud = func() (string, string) { return srv.URL, "deliver-token" }
	te.env.serverRead = nil
	te.env.inboxURL = nil

	addChannel(te, "c", "--max", "2")
	dlvSpool(t, te.env.dir(),
		dlvPost{"p1", "alice", "First post"},
		dlvPost{"p2", "bob", "Second post"},
		dlvPost{"p3", "carol", "Third post"},
		dlvPost{"p4", "dave", "Fourth read on web"})

	out := te.mustRun("deliver", "--dry-run")
	if calls != 1 {
		t.Errorf("expected one shared server round trip, got %d calls", calls)
	}
	if gotAuth != "Bearer deliver-token" {
		t.Errorf("expected Bearer deliver-token, got %q", gotAuth)
	}
	if len(gotIDs) != 4 {
		t.Errorf("expected 4 candidate ids queried, got %v", gotIDs)
	}
	if !strings.Contains(out, "First post") || !strings.Contains(out, "Second post") {
		t.Errorf("dry-run missing visible items:\n%s", out)
	}
	if strings.Contains(out, "Fourth read on web") {
		t.Errorf("server-read post was not dropped:\n%s", out)
	}
	if !strings.Contains(out, "+1 more → "+srvInbox) {
		t.Errorf("inbox URL did not flow into the +N more link:\n%s", out)
	}
	if n := len(fx.calls(t)); n != 0 {
		t.Fatalf("dry-run ran the command %d times", n)
	}
}

func TestDeliverDefaultWiring_Server500WarnsAndFallsBack(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	te.env.Cloud = func() (string, string) { return srv.URL, "deliver-token" }
	te.env.serverRead = nil
	te.env.inboxURL = nil

	addChannel(te, "c")
	dlvSpool(t, te.env.dir(), dlvPost{"p1", "alice", "Only post"})

	out, stderr, err := te.run("deliver", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "server read set unavailable") {
		t.Errorf("expected a warning about the unavailable server set, got %q", stderr)
	}
	if !strings.Contains(out, "Only post") {
		t.Errorf("fallback to the local cursor dropped the post:\n%s", out)
	}
	if n := len(fx.calls(t)); n != 0 {
		t.Fatalf("dry-run ran the command %d times", n)
	}
}

func TestDeliverChannelSelectionAndOff(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	addChannel(te, "a")
	addChannel(te, "b")
	addChannel(te, "dormant", "--off")
	dlvSpool(t, te.env.dir(), dlvPost{"p1", "alice", "One"})

	if _, _, err := te.run("deliver", "--channel", "nope"); err == nil {
		t.Fatal("unknown channel accepted")
	}
	te.mustRun("deliver", "--channel", "b")
	if n := len(fx.calls(t)); n != 1 {
		t.Fatalf("--channel b made %d calls", n)
	}
	te.mustRun("deliver")
	// a sends; b already did; dormant never does.
	if n := len(fx.calls(t)); n != 2 {
		t.Fatalf("got %d calls, want 2", n)
	}
	st, _ := loadChannelState(te.env.dir())
	if _, ok := st["dormant"]; ok {
		t.Fatal("an off channel was delivered to")
	}
}
