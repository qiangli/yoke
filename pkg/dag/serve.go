// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// DefaultServeAddr is the address `dag --serve` binds with no argument.
// Loopback is not a default, it is the security model: the viewer has no
// authentication because it is not reachable off the machine.
const DefaultServeAddr = "127.0.0.1:7717"

// pollInterval is how often a live run's event log is re-read. Polling rather
// than filesystem notification keeps this package's dependency budget at zero
// for the feature; at one stat per quarter second per watcher the cost is not
// worth a dependency.
const pollInterval = 250 * time.Millisecond

// RunSummary identifies one run the viewer can show.
//
// Live is what report.json cannot tell you: a run directory with events but no
// report is in flight (or was killed). That distinction is the whole reason the
// event log exists.
type RunSummary struct {
	DocKey     string    `json:"doc_key"`
	RunID      string    `json:"run_id"`
	File       string    `json:"file"`
	Targets    []string  `json:"targets,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Live       bool      `json:"live"`
	Failed     bool      `json:"failed"`
	Total      int       `json:"total"`
	Done       int       `json:"done"`
	FailedN    int       `json:"failed_count"`
}

// runState is a run merged from whatever exists on disk: the graph, the event
// log, and the report if the run has finished.
type runState struct {
	Summary RunSummary
	Graph   *RunGraph
	Entry   *RunEntry
	Tasks   map[string]*RunTask
	// EventCount is how many events this state already reflects. The live page
	// resumes its stream from here, so the hand-off from server-side render to
	// client-side stream neither repeats nor drops an event.
	EventCount int
}

// scanRuns lists every run under root, newest first. A directory that holds
// neither a report nor an event log is skipped: it is a run that was created
// and died before writing anything, and there is nothing to show.
func scanRuns(root string, limit int) ([]RunSummary, error) {
	base := filepath.Join(root, "runs")
	docs, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RunSummary
	for _, d := range docs {
		if !d.IsDir() {
			continue
		}
		ids, ierr := runIDs(filepath.Join(base, d.Name()))
		if ierr != nil {
			continue
		}
		for _, id := range ids {
			st, lerr := loadRunState(base, d.Name(), id)
			if lerr != nil {
				continue
			}
			out = append(out, st.Summary)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID > out[j].RunID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// loadRunState merges a run directory into a viewable state. It tolerates every
// partial shape, because a run being watched is partial by definition.
func loadRunState(base, docKey, runID string) (*runState, error) {
	dir := filepath.Join(base, docKey, runID)
	st := &runState{
		Summary: RunSummary{DocKey: docKey, RunID: runID},
		Tasks:   map[string]*RunTask{},
	}

	if data, err := os.ReadFile(filepath.Join(dir, "graph.json")); err == nil {
		var g RunGraph
		if json.Unmarshal(data, &g) == nil {
			st.Graph = &g
			for _, n := range g.Nodes {
				st.Tasks[n.Name] = &RunTask{Name: n.Name, Status: "pending"}
			}
		}
	}

	events, _ := ReadEvents(dir)
	st.EventCount = len(events)
	sealed := false
	for _, ev := range events {
		switch ev.Kind {
		case EventRunStart:
			st.Summary.File, st.Summary.Targets, st.Summary.StartedAt = ev.File, ev.Targets, ev.At
		case EventTaskStart:
			if t := st.task(ev.Task); t != nil {
				t.Status = "running"
			}
		case EventTaskEnd:
			if t := st.task(ev.Task); t != nil {
				t.Status, t.ExitCode, t.DurationMS = ev.Status, ev.ExitCode, ev.DurationMS
			}
		case EventRunEnd:
			sealed = true
			st.Summary.Failed = ev.Failed
			if !st.Summary.StartedAt.IsZero() {
				st.Summary.DurationMS = ev.At.Sub(st.Summary.StartedAt).Milliseconds()
			}
		}
	}

	// The report is authoritative once it exists — it carries artifacts, log
	// paths, and the up-to-date flag that the event stream does not.
	if entry, err := readRunEntry(dir); err == nil {
		st.Entry = entry
		sealed = true
		st.Summary.File, st.Summary.Targets = entry.File, entry.Targets
		st.Summary.StartedAt, st.Summary.DurationMS = entry.StartedAt, entry.DurationMS
		st.Summary.Failed = entry.Failed
		for i := range entry.Tasks {
			t := entry.Tasks[i]
			st.Tasks[t.Name] = &t
		}
	}

	if st.Graph == nil && st.Entry == nil && len(events) == 0 {
		return nil, os.ErrNotExist
	}

	st.Summary.Live = !sealed
	for _, t := range st.Tasks {
		st.Summary.Total++
		switch t.Status {
		case "done", "up-to-date":
			st.Summary.Done++
		case "failed":
			st.Summary.FailedN++
		}
	}
	return st, nil
}

func (s *runState) task(name string) *RunTask {
	if name == "" {
		return nil
	}
	if t, ok := s.Tasks[name]; ok {
		return t
	}
	t := &RunTask{Name: name}
	s.Tasks[name] = t
	return t
}

// orderedTasks returns the run's targets in graph order, falling back to name
// order when no graph was recorded.
func (s *runState) orderedTasks() []RunTask {
	var out []RunTask
	seen := map[string]bool{}
	if s.Graph != nil {
		for _, n := range s.Graph.Nodes {
			if t := s.Tasks[n.Name]; t != nil {
				out = append(out, *t)
				seen[n.Name] = true
			}
		}
	}
	var rest []string
	for name := range s.Tasks {
		if !seen[name] {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	for _, name := range rest {
		out = append(out, *s.Tasks[name])
	}
	return out
}

// entryView adapts a runState into the RunEntry shape the page template reads,
// so a live run and a finished run render through exactly one code path.
func (s *runState) entryView() *RunEntry {
	e := &RunEntry{
		SchemaVersion: JournalSchemaVersion,
		RunID:         s.Summary.RunID,
		File:          s.Summary.File,
		Targets:       s.Summary.Targets,
		StartedAt:     s.Summary.StartedAt,
		DurationMS:    s.Summary.DurationMS,
		Failed:        s.Summary.Failed,
		Tasks:         s.orderedTasks(),
	}
	if s.Entry != nil {
		e.Records = s.Entry.Records
	}
	return e
}

// server is the read-only viewer. It holds no run state of its own: every
// response is derived from the journal on disk.
//
// This is deliberately NOT a runner. A viewer that could start work would be a
// second execution engine with its own scheduling, its own failure modes, and
// its own divergence from the CLI. Runs are started from a terminal or CI; the
// server watches whatever the journal records, from any shell on the machine.
type server struct {
	root string // the dag cache root
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/run", s.handleRun)
	mux.HandleFunc("/events", s.handleEvents)
	mux.HandleFunc("/log", s.handleLog)
	mux.HandleFunc("/logtail", s.handleLogTail)
	return mux
}

// handleLogTail streams a log file as it is appended (SSE).
//
// This is what makes a long stage watchable. Per-target status changes a
// handful of times in three hours; the output is the thing you actually watch,
// so a monitor that shows only status is not monitoring a long job.
//
// Each line is delivered as its own JSON-encoded event. Log content is
// arbitrary bytes from somebody's build, so it is encoded rather than
// interpolated: a raw newline would end the SSE frame early and invalid UTF-8
// would corrupt the stream.
func (s *server) handleLogTail(w http.ResponseWriter, r *http.Request) {
	docKey, runID, ok := runParams(w, r)
	if !ok {
		return
	}
	dir := filepath.Join(s.root, "runs", docKey, runID)
	rel := r.URL.Query().Get("path")
	// Resolve through the same guard the static log reader uses, then keep the
	// verified absolute path — never re-join the untrusted value.
	path, err := resolveWithin(dir, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	var offset int64
	var pending []byte
	for {
		data, size := readFrom(path, offset)
		if len(data) > 0 {
			offset = size
			pending = append(pending, data...)
			// Emit only whole lines; a partial trailing line is held until the
			// rest arrives, so a line is never split across two events.
			for {
				i := indexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := strings.TrimRight(string(pending[:i]), "\r")
				pending = pending[i+1:]
				if enc, err := json.Marshal(map[string]string{"line": line}); err == nil {
					fmt.Fprintf(w, "data: %s\n\n", enc)
				}
			}
			flusher.Flush()
		}
		// Stop once the run is over AND the file has been fully read: there
		// will never be more. While the run is live, silence is normal.
		if _, err := os.Stat(filepath.Join(dir, "report.json")); err == nil && len(data) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// readFrom reads whatever has been appended past offset, returning the new
// bytes and the file's current size. A file that has not appeared yet reads as
// empty rather than as an error: a target's log is created when it starts
// writing, which can be after the viewer subscribed.
func readFrom(path string, offset int64) ([]byte, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() <= offset {
		return nil, offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset
	}
	buf := make([]byte, fi.Size()-offset)
	n, _ := io.ReadFull(f, buf)
	return buf[:n], offset + int64(n)
}

func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	runs, err := scanRuns(s.root, 100)
	if err != nil {
		http.Error(w, "cannot read the run journal", http.StatusInternalServerError)
		return
	}
	live := 0
	for _, x := range runs {
		if x.Live {
			live++
		}
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, runs)
		return
	}
	page, err := renderIndexHTML(runs, live)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	st, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("format") == "json" {
		writeJSON(w, st.entryView())
		return
	}
	page, err := renderRunPage(st.entryView(), st.Graph, st.Summary.Live,
		st.Summary.DocKey, st.Summary.RunID, st.EventCount)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// handleEvents streams a run's events as they are appended (SSE). It ends when
// the run ends or the client goes away — never on a timer, because a long
// pipeline that goes quiet is still running.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	docKey, runID, ok := runParams(w, r)
	if !ok {
		return
	}
	dir := filepath.Join(s.root, "runs", docKey, runID)
	if _, err := os.Stat(dir); err != nil {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	ctx := r.Context()
	for {
		events, _ := ReadEvents(dir)
		for i := from; i < len(events); i++ {
			line, err := json.Marshal(events[i])
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			if events[i].Kind == EventRunEnd {
				flusher.Flush()
				return
			}
		}
		if len(events) > from {
			from = len(events)
			flusher.Flush()
		}
		select {
		case <-ctx.Done(): // client navigated away; stop reading the file
			return
		case <-time.After(pollInterval):
		}
	}
}

func (s *server) handleLog(w http.ResponseWriter, r *http.Request) {
	docKey, runID, ok := runParams(w, r)
	if !ok {
		return
	}
	rel := r.URL.Query().Get("path")
	dir := filepath.Join(s.root, "runs", docKey, runID)
	data, err := readWithin(dir, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// text/plain, never text/html: a log holds whatever a build printed, and
	// serving that as markup would execute it in the viewer's browser.
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func (s *server) lookup(w http.ResponseWriter, r *http.Request) (*runState, bool) {
	docKey, runID, ok := runParams(w, r)
	if !ok {
		return nil, false
	}
	st, err := loadRunState(filepath.Join(s.root, "runs"), docKey, runID)
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	return st, true
}

// runParams extracts and validates the run coordinates. Both reach us from a
// URL, so both are checked to be single path elements before they are joined
// onto a filesystem path.
func runParams(w http.ResponseWriter, r *http.Request) (docKey, runID string, ok bool) {
	docKey = r.URL.Query().Get("doc")
	runID = r.URL.Query().Get("run")
	if !safePathElement(docKey) || !safePathElement(runID) {
		http.Error(w, "bad run reference", http.StatusBadRequest)
		return "", "", false
	}
	return docKey, runID, true
}

// safePathElement accepts only the shapes the journal actually produces: hex
// document keys and "<nanos>-<hex>" run ids. Anything else is refused rather
// than sanitized, because sanitizing a path is how traversal bugs are written.
func safePathElement(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// resolveWithin turns an untrusted relative path into a verified absolute one
// under dir, refusing anything that escapes. Callers keep the RETURNED path;
// re-joining the untrusted value afterwards would defeat the check.
func resolveWithin(dir, rel string) (string, error) {
	if rel == "" {
		return "", os.ErrNotExist
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	p, err := filepath.Abs(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) {
		return "", os.ErrNotExist
	}
	return p, nil
}

// readWithin reads rel under dir, refusing anything that escapes it.
func readWithin(dir, rel string) ([]byte, error) {
	p, err := resolveWithin(dir, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// runServeCmd starts the viewer and blocks until the context is cancelled.
func runServeCmd(ctx context.Context, out io.Writer, addr, cacheDir string) error {
	root := ResolveCacheDir(cacheDir)
	if root == "" {
		return errf(weavecli.ExitPrecondFail, "no dag cache directory available to serve")
	}
	if addr == "" {
		addr = DefaultServeAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errf(weavecli.ExitStateConflict, "cannot listen on %s: %v", addr, err)
	}
	srv := &http.Server{
		Handler:           (&server{root: root}).routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: an SSE stream is meant to stay open for the length
		// of a run, and a run can legitimately take hours.
	}
	fmt.Fprintf(out, "dag: serving the run journal at http://%s\n", ln.Addr())
	fmt.Fprintf(out, "dag: reading %s (read-only; start runs from a terminal as usual)\n", root)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		return nil
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
