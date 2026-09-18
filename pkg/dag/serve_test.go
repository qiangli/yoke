// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/qiangli/coreutils/cmds/all"
)

// serveFixture runs a graph through the CLI (so the command's own wiring is
// exercised) and returns the cache root plus the run's coordinates.
func serveFixture(t *testing.T, md string, targets ...string) (root, docKey, runID string) {
	t.Helper()
	root = t.TempDir()
	t.Setenv("DAG_CACHE_DIR", root)

	path := writeDAG(t, md)
	cmd := NewDagCmd()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(append([]string{"--file", path}, targets...))
	_ = cmd.Execute() // callers may pass a deliberately failing graph

	base := filepath.Join(root, "runs")
	docs, err := os.ReadDir(base)
	if err != nil || len(docs) != 1 {
		t.Fatalf("want one document dir under %s: %v (%d)", base, err, len(docs))
	}
	ids, err := runIDs(filepath.Join(base, docs[0].Name()))
	if err != nil || len(ids) == 0 {
		t.Fatalf("no run recorded: %v", err)
	}
	return root, docs[0].Name(), ids[0]
}

// THE regression guard for the live half. The Observer field and
// Journal.Observer() both existed and were both correct while nothing joined
// them, so runs journaled a final report and no events — and a live view showed
// every target stuck on "pending" forever. Nothing below the CLI catches that:
// the wiring is in the command.
func TestCLIWiresTheRunObserver(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n"+
			"### one\n"+block("bash", "echo one")+
			"### two\nRequires: one\n"+block("bash", "echo two"),
		"two")

	events, err := ReadEvents(filepath.Join(root, "runs", docKey, runID))
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events recorded: the CLI did not wire Engine.Observer to the journal")
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
	}
	got := strings.Join(kinds, ",")
	want := "run.start,task.start,task.end,task.start,task.end,run.end"
	if got != want {
		t.Errorf("event sequence = %q, want %q", got, want)
	}
	if events[0].File == "" || len(events[0].Targets) == 0 {
		t.Error("run.start must name the file and targets: a live run has no report to name it from")
	}
	// Same discipline as RunRecord: the stream is a view someone screenshots.
	for _, ev := range events {
		if strings.Contains(ev.Status, "/") || strings.Contains(ev.Task, "\x00") {
			t.Errorf("suspicious event content: %+v", ev)
		}
	}
}

// A run with events but no report is in flight. That distinction is the only
// reason the event log exists, so it is pinned.
func TestLoadRunStateLiveVersusSealed(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n### go\n"+block("bash", "echo go"), "go")
	base := filepath.Join(root, "runs")

	st, err := loadRunState(base, docKey, runID)
	if err != nil {
		t.Fatalf("loadRunState: %v", err)
	}
	if st.Summary.Live {
		t.Error("a run with a report must not be reported as live")
	}
	if st.Summary.Done != 1 || st.Summary.Total != 1 {
		t.Errorf("progress = %d/%d, want 1/1", st.Summary.Done, st.Summary.Total)
	}

	// Remove the report: what is left is exactly the shape of a run still in
	// flight (or one that was killed).
	if err := os.Remove(filepath.Join(base, docKey, runID, "report.json")); err != nil {
		t.Fatal(err)
	}
	// ...and drop the terminal event, so nothing says it finished.
	evPath := filepath.Join(base, docKey, runID, EventsFile)
	data, _ := os.ReadFile(evPath)
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if !strings.Contains(line, EventRunEnd) {
			kept = append(kept, line)
		}
	}
	if err := os.WriteFile(evPath, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err = loadRunState(base, docKey, runID)
	if err != nil {
		t.Fatalf("loadRunState (live): %v", err)
	}
	if !st.Summary.Live {
		t.Error("a run with events but no report must be reported as live")
	}
	if st.Summary.File == "" {
		t.Error("a live run must still be identifiable — from run.start, not the report")
	}
}

// Both coordinates reach the server from a URL and are then joined onto a
// filesystem path.
func TestServeRejectsPathTraversal(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n### go\n"+block("bash", "echo go"), "go")
	srv := httptest.NewServer((&server{root: root}).routes())
	defer srv.Close()

	for _, bad := range []string{
		"/run?doc=../../etc&run=" + runID,
		"/run?doc=" + docKey + "&run=..",
		"/events?doc=/etc&run=" + runID,
		"/log?doc=" + docKey + "&run=" + runID + "&path=../../../../etc/passwd",
	} {
		resp, err := http.Get(srv.URL + bad)
		if err != nil {
			t.Fatalf("GET %s: %v", bad, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s returned 200; it must be refused", bad)
		}
	}

	if safePathElement("..") || safePathElement("a/b") || safePathElement("") {
		t.Error("safePathElement accepted a traversal-capable value")
	}
	if !safePathElement(docKey) || !safePathElement(runID) {
		t.Error("safePathElement rejected the journal's own identifiers")
	}
}

// A log holds whatever a build printed. Served as markup it would execute in
// the viewer's browser.
func TestServeLogIsPlainText(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n### go\n"+block("bash", `echo "<script>alert(1)</script>"`), "go")
	srv := httptest.NewServer((&server{root: root}).routes())
	defer srv.Close()

	st, err := loadRunState(filepath.Join(root, "runs"), docKey, runID)
	if err != nil {
		t.Fatal(err)
	}
	tasks := st.orderedTasks()
	if len(tasks) == 0 || len(tasks[0].Logs) == 0 {
		t.Fatal("no log recorded to serve")
	}
	resp, err := http.Get(srv.URL + "/log?doc=" + docKey + "&run=" + runID + "&path=" + tasks[0].Logs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("log Content-Type = %q, want text/plain", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("log response must set nosniff; a browser sniffing this as HTML would run it")
	}
}

// The exported page is a static artifact and carries no script; the served page
// adds the live listener. Both must render the same server-side markup first,
// so the served page is correct before any script runs.
func TestStaticExportHasNoScriptButLivePageDoes(t *testing.T) {
	entry := &RunEntry{
		RunID: "1-abc", File: "/w/DAG.md", Targets: []string{"build"},
		StartedAt: time.Now(), Tasks: []RunTask{{Name: "build", Status: "running"}},
	}
	graph := &RunGraph{SchemaVersion: JournalSchemaVersion,
		Nodes: []RunGraphNode{{Name: "build", Layer: 0}}}

	static, err := renderRunHTML(entry, graph)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(static), "<script") {
		t.Error("the static export must stay script-free: it is a file people mail around")
	}

	live, err := renderRunPage(entry, graph, true, "doc", "1-abc", 3)
	if err != nil {
		t.Fatal(err)
	}
	s := string(live)
	if !strings.Contains(s, "<script") || !strings.Contains(s, "EventSource") {
		t.Error("the live page must subscribe to the event stream")
	}
	// Resuming from the count already rendered is what stops the first events
	// being applied twice.
	if !strings.Contains(s, "from=3") {
		t.Errorf("live page does not resume the stream from the rendered position:\n%s", s)
	}
	// Both pages must be renderable with the target visible.
	for _, page := range []string{string(static), s} {
		if !strings.Contains(page, "build") {
			t.Error("target missing from the rendered page")
		}
	}
}

// The stream must end when the run ends, not on a timer: a long pipeline that
// goes quiet is still running.
func TestEventStreamEndsWithTheRun(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n### go\n"+block("bash", "echo go"), "go")
	srv := httptest.NewServer((&server{root: root}).routes())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/events?doc="+docKey+"&run="+runID+"&from=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// The run already finished, so the stream must replay and close by itself.
	body := new(bytes.Buffer)
	done := make(chan error, 1)
	go func() { _, err := body.ReadFrom(resp.Body); done <- err }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("event stream did not close after the run ended")
	}
	if !strings.Contains(body.String(), EventRunEnd) {
		t.Errorf("stream did not deliver the terminal event:\n%s", body.String())
	}
	if !strings.HasPrefix(body.String(), "data: ") {
		t.Errorf("stream is not SSE-framed:\n%s", body.String())
	}
}

// Targets that never run a body still have to report. A cache hit or a
// dependency-skip that emits nothing leaves a live viewer showing "pending"
// for the whole run — and on an incremental re-run that is most of the graph,
// which is exactly the common case.
func TestEventsCoverSkippedAndUpToDateTargets(t *testing.T) {
	md := "## Tasks\n\n" +
		"### compile\nSources: in.txt\nGenerates: out.txt\n" + block("bash", "cp in.txt out.txt") +
		"### boom\nRequires: compile\n" + block("bash", "exit 3") +
		"### after\nRequires: boom\n" + block("bash", "echo never")

	for _, jobs := range []string{"1", "4"} {
		t.Run("jobs="+jobs, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DAG_CACHE_DIR", root)
			path := writeDAG(t, md)
			t.Chdir(filepath.Dir(path)) // bodies run in the invoking cwd (make parity)
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "in.txt"), []byte("seed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			run := func() {
				cmd := NewDagCmd()
				cmd.SetOut(new(bytes.Buffer))
				cmd.SetErr(new(bytes.Buffer))
				cmd.SetArgs([]string{"--file", path, "after", "-k", "-j", jobs})
				_ = cmd.Execute() // fails on purpose
			}
			run() // first run populates the cache
			run() // second: compile is up-to-date

			base := filepath.Join(root, "runs")
			docs, _ := os.ReadDir(base)
			ids, _ := runIDs(filepath.Join(base, docs[0].Name()))
			events, err := ReadEvents(filepath.Join(base, docs[0].Name(), ids[0]))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, ev := range events {
				if ev.Kind == EventTaskEnd {
					got[ev.Task] = ev.Status
				}
			}
			for task, want := range map[string]string{
				"compile": "up-to-date", // cache hit — never enters runOne
				"boom":    "failed",
				"after":   "skipped", // dependency failed — never enters runOne
			} {
				if got[task] != want {
					t.Errorf("task %q reported %q, want %q (events: %+v)", task, got[task], want, got)
				}
			}
		})
	}
}

// task.start must carry the log path: only the engine knows the sanitized
// filename, and without it a viewer cannot begin tailing a running target.
func TestTaskStartCarriesLogPath(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n### my/odd name\n"+block("bash", "echo hi"), "my/odd name")

	events, err := ReadEvents(filepath.Join(root, "runs", docKey, runID))
	if err != nil {
		t.Fatal(err)
	}
	var logPath string
	for _, ev := range events {
		if ev.Kind == EventTaskStart {
			logPath = ev.Log
		}
	}
	if logPath == "" {
		t.Fatal("task.start carried no log path")
	}
	if _, err := os.Stat(filepath.Join(root, "runs", docKey, runID, logPath)); err != nil {
		t.Errorf("advertised log path %q does not resolve: %v", logPath, err)
	}
}

// Tailing streams appended lines and terminates once the run is over and the
// file is fully read.
func TestLogTailStreamsAndTerminates(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n### go\n"+block("bash", "echo alpha; echo beta"), "go")
	srv := httptest.NewServer((&server{root: root}).routes())
	defer srv.Close()

	st, err := loadRunState(filepath.Join(root, "runs"), docKey, runID)
	if err != nil {
		t.Fatal(err)
	}
	logs := st.orderedTasks()[0].Logs
	if len(logs) == 0 {
		t.Fatal("no log to tail")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/logtail?doc="+docKey+"&run="+runID+"&path="+logs[0], nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body := new(bytes.Buffer)
	done := make(chan error, 1)
	go func() { _, err := body.ReadFrom(resp.Body); done <- err }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("log tail did not terminate after the run finished")
	}
	s := body.String()
	// Lines are JSON-encoded: log content is arbitrary bytes, and a raw newline
	// would end the SSE frame early.
	if !strings.Contains(s, `{"line":"alpha"}`) || !strings.Contains(s, `{"line":"beta"}`) {
		t.Errorf("tail did not deliver the log lines:\n%s", s)
	}
}

// The tail endpoint takes an untrusted path just like the static reader.
func TestLogTailRejectsEscape(t *testing.T) {
	root, docKey, runID := serveFixture(t,
		"## Tasks\n\n### go\n"+block("bash", "echo go"), "go")
	srv := httptest.NewServer((&server{root: root}).routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/logtail?doc=" + docKey + "&run=" + runID +
		"&path=../../../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("logtail accepted a path escaping the run directory")
	}
}

// A nil observer must be exactly today's engine.
func TestNilObserverIsInert(t *testing.T) {
	e := &Engine{}
	e.emitEvent(Event{Kind: EventRunStart}) // must not panic
	var j *Journal
	if j.Observer() != nil {
		t.Error("a nil journal must not hand out an observer")
	}
	j.emit(Event{Kind: EventRunStart}) // must not panic
}
