// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// JournalSchemaVersion is stamped into every journaled file so a reader can
// refuse a shape it does not understand instead of guessing.
const JournalSchemaVersion = 1

// DefaultKeepRuns bounds the journal. A run directory is small but a log is
// not, and dag runs on fleet hosts that nobody is watching — an unbounded
// journal is a disk-fill bug on a timer, so retention is not optional.
const DefaultKeepRuns = 50

// Journal persists what one run produced: the report, the graph it ran, and
// each attempt's output. It exists because dag is otherwise write-only to the
// terminal — Cache holds fingerprints and durations, RunReport is returned and
// dropped, and per-step output has no file at all. Without this there is no
// history to show, only a live terminal.
//
// Everything here is best-effort: a journal that cannot be written must never
// fail a run that otherwise succeeded. Errors are surfaced to callers that ask
// for them and ignored by the engine.
type Journal struct {
	dir     string // <root>/runs/<docKey>/<runID>
	runID   string
	docPath string
	keep    int
	started time.Time

	mu   sync.Mutex
	logs map[string]*logSink // "<task>\x00<attempt>" -> open file
}

// RunTask is one target's journaled outcome.
//
// Note what is ABSENT and keep it that way: no host and no error text. The
// engine's TaskResult carries both, but RunRecord deliberately carries neither
// (see RecordAttempt) — a record stamped with the machine that produced it
// cannot be compared against one produced elsewhere, and free error text is
// where hostnames and paths leak. Classification lives in Records; prose lives
// in the log file, which is local and never travels.
type RunTask struct {
	Name       string   `json:"name"`
	Status     string   `json:"status"`
	ExitCode   int      `json:"exit_code"`
	DurationMS int64    `json:"duration_ms"`
	UpToDate   bool     `json:"up_to_date,omitempty"`
	Artifacts  []string `json:"artifacts,omitempty"`
	Logs       []string `json:"logs,omitempty"` // journal-relative attempt logs
}

// RunEntry is one journaled run — the durable form of a RunReport.
type RunEntry struct {
	SchemaVersion int         `json:"schema_version"`
	RunID         string      `json:"run_id"`
	File          string      `json:"file"`
	Targets       []string    `json:"targets,omitempty"`
	StartedAt     time.Time   `json:"started_at"`
	FinishedAt    time.Time   `json:"finished_at"`
	DurationMS    int64       `json:"duration_ms"`
	Failed        bool        `json:"failed"`
	Tasks         []RunTask   `json:"tasks,omitempty"`
	Records       []RunRecord `json:"records,omitempty"`
}

// RunGraphNode is one target's shape at the time the run happened.
type RunGraphNode struct {
	Name     string   `json:"name"`
	Requires []string `json:"requires,omitempty"`
	Layer    int      `json:"layer"` // longest-path depth; the render's column
}

// RunGraph is the graph as it existed for this run. It is stored rather than
// re-parsed because the DAG file is editable: showing a six-month-old run
// against today's file would describe something that never ran.
type RunGraph struct {
	SchemaVersion int            `json:"schema_version"`
	Nodes         []RunGraphNode `json:"nodes"`
}

// logSink serializes writes to one attempt's log. stdout and stderr are teed
// into the SAME file to preserve their relative order, so the two streams need
// one lock between them.
type logSink struct {
	mu sync.Mutex
	f  *os.File
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Write(p)
}

// runsRoot is the journal's per-document directory.
func runsRoot(docPath, cacheDir string) string {
	root := ResolveCacheDir(cacheDir)
	if root == "" {
		return ""
	}
	return filepath.Join(root, "runs", docKey(docPath))
}

// OpenJournal creates the run directory for a new run. It returns (nil, nil)
// when no cache directory is available — journaling is as optional as the
// fingerprint cache, and a host without a writable cache dir still runs graphs.
func OpenJournal(docPath, cacheDir string, keep int) (*Journal, error) {
	base := runsRoot(docPath, cacheDir)
	if base == "" {
		return nil, nil
	}
	if keep <= 0 {
		keep = DefaultKeepRuns
	}
	now := time.Now()
	// Nanosecond prefix makes the id lexically sortable == chronologically
	// sortable, which is what lets List/prune work by filename alone.
	runID := fmt.Sprintf("%d-%s", now.UnixNano(), docKey(docPath)[:8])
	dir := filepath.Join(base, runID)
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return nil, err
	}
	return &Journal{
		dir:     dir,
		runID:   runID,
		docPath: docPath,
		keep:    keep,
		started: now,
		logs:    map[string]*logSink{},
	}, nil
}

// RunID is this run's journal identity, as accepted by --show.
func (j *Journal) RunID() string {
	if j == nil {
		return ""
	}
	return j.runID
}

// Dir is the run's directory on disk.
func (j *Journal) Dir() string {
	if j == nil {
		return ""
	}
	return j.dir
}

// attemptLogWriter opens (once) and returns the writer for one attempt's log.
// A nil return means "do not tee" — every caller must tolerate it, because a
// journal that cannot open a file is not a reason to fail a target.
func (j *Journal) attemptLogWriter(task string, attempt int) io.Writer {
	if j == nil {
		return nil
	}
	key := task + "\x00" + fmt.Sprint(attempt)
	j.mu.Lock()
	defer j.mu.Unlock()
	if s, ok := j.logs[key]; ok {
		return s
	}
	f, err := os.OpenFile(filepath.Join(j.dir, attemptLogPath(task, attempt)),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil
	}
	s := &logSink{f: f}
	j.logs[key] = s
	return s
}

// writeAttemptLog writes a complete attempt log in one shot. It is the path for
// targets whose output could not be teed live — see Engine.runOne: a target
// declaring Secrets is captured and REDACTED after the fact, so streaming its
// raw bytes to disk would write the very values redaction exists to remove.
func (j *Journal) writeAttemptLog(task string, attempt int, stdout, stderr string) {
	if j == nil || (stdout == "" && stderr == "") {
		return
	}
	p := filepath.Join(j.dir, attemptLogPath(task, attempt))
	_ = os.WriteFile(p, []byte(stdout+stderr), 0o644)
}

// journalMayTee reports whether a target's output may be streamed to the
// journal as it is produced.
//
// A target declaring Secrets may NOT. Its output is captured and redacted only
// after the body finishes (Engine.runOne), so a live tee would put the raw
// secret on disk — briefly, but on disk. Engine.runOne writes those targets'
// logs after redaction instead. Keep this as a named predicate rather than an
// inline condition: it is a security rule, and a rule that is not named is a
// rule that gets refactored away.
func journalMayTee(t *Task) bool { return len(t.Secrets) == 0 }

// attemptLogPath is the journal-relative path of one attempt's log.
func attemptLogPath(task string, attempt int) string {
	return filepath.Join("logs", fmt.Sprintf("%s.%d.log", safeFileName(task), attempt))
}

// safeFileName maps a target name onto a filename. Target names are free-form
// (they may contain '/', ':', spaces), so anything outside a conservative set
// is replaced — and because that mapping is lossy, a name that needed any
// replacement also carries a hash of the original. Two distinct targets can
// therefore never collide on one log file.
func safeFileName(s string) string {
	var b strings.Builder
	changed := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			changed = true
		}
	}
	out := b.String()
	if out == "" {
		out, changed = "task", true
	}
	if changed {
		sum := sha256.Sum256([]byte(s))
		out += "-" + hex.EncodeToString(sum[:])[:6]
	}
	return out
}

// WriteGraph records the graph this run executed, with each node's topological
// layer precomputed so a renderer needs no graph algorithm.
func (j *Journal) WriteGraph(order []*Node) error {
	if j == nil {
		return nil
	}
	g := RunGraph{SchemaVersion: JournalSchemaVersion}
	layer := make(map[string]int, len(order))
	// order is topological, so every dependency's layer is already known.
	for _, n := range order {
		best := 0
		var reqs []string
		for _, d := range n.Deps {
			reqs = append(reqs, d.Task.Name)
			if l := layer[d.Task.Name] + 1; l > best {
				best = l
			}
		}
		layer[n.Task.Name] = best
		g.Nodes = append(g.Nodes, RunGraphNode{Name: n.Task.Name, Requires: reqs, Layer: best})
	}
	return writeJSONAtomic(filepath.Join(j.dir, "graph.json"), g)
}

// Finish seals the run: closes any open logs, writes report.json, and prunes
// older runs. It is safe to call on a partial report — a run killed mid-flight
// still leaves a readable journal, which is the whole point of writing it.
func (j *Journal) Finish(targets []string, report RunReport) error {
	if j == nil {
		return nil
	}
	j.closeLogs()

	now := time.Now()
	entry := RunEntry{
		SchemaVersion: JournalSchemaVersion,
		RunID:         j.runID,
		File:          j.docPath,
		Targets:       targets,
		StartedAt:     j.started,
		FinishedAt:    now,
		DurationMS:    now.Sub(j.started).Milliseconds(),
		Failed:        report.Failed,
		Records:       report.Records,
	}
	// Attempt counts come from Records, which is the authoritative per-attempt
	// log; Results holds one final outcome per target.
	attemptsOf := map[string][]int{}
	for _, r := range report.Records {
		attemptsOf[r.Task] = append(attemptsOf[r.Task], r.Attempt)
	}
	for _, res := range report.Results {
		t := RunTask{
			Name:       res.Name,
			Status:     res.Status.String(),
			ExitCode:   res.ExitCode,
			DurationMS: res.Duration.Milliseconds(),
			UpToDate:   res.UpToDate,
			Artifacts:  res.Artifacts,
		}
		for _, a := range attemptsOf[res.Name] {
			p := attemptLogPath(res.Name, a)
			if _, err := os.Stat(filepath.Join(j.dir, p)); err == nil {
				t.Logs = append(t.Logs, filepath.ToSlash(p))
			}
		}
		entry.Tasks = append(entry.Tasks, t)
	}
	if err := writeJSONAtomic(filepath.Join(j.dir, "report.json"), entry); err != nil {
		return err
	}
	j.prune()
	return nil
}

// Close releases open log files without writing a report. Finish already closes
// them; this covers the abandoned-run path.
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	j.closeLogs()
	return nil
}

func (j *Journal) closeLogs() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for k, s := range j.logs {
		s.mu.Lock()
		_ = s.f.Close()
		s.mu.Unlock()
		delete(j.logs, k)
	}
}

// prune keeps the newest keep run directories and removes the rest. Run ids
// sort lexically by their nanosecond prefix, so "newest" is a string sort.
func (j *Journal) prune() {
	base := filepath.Dir(j.dir)
	ids, err := runIDs(base)
	if err != nil || len(ids) <= j.keep {
		return
	}
	for _, id := range ids[j.keep:] {
		_ = os.RemoveAll(filepath.Join(base, id))
	}
}

// runIDs lists run directory names newest-first.
func runIDs(base string) ([]string, error) {
	ents, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

// ListRuns returns this document's journaled runs, newest first. A run
// directory whose report is missing or unreadable is skipped rather than
// failing the listing: a killed run leaves exactly that, and it must not
// prevent looking at the ones that completed.
func ListRuns(docPath, cacheDir string, limit int) ([]RunEntry, error) {
	base := runsRoot(docPath, cacheDir)
	if base == "" {
		return nil, nil
	}
	ids, err := runIDs(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RunEntry
	for _, id := range ids {
		if limit > 0 && len(out) >= limit {
			break
		}
		e, rerr := readRunEntry(filepath.Join(base, id))
		if rerr != nil {
			continue
		}
		out = append(out, *e)
	}
	return out, nil
}

// ListAllRuns returns recent runs across EVERY dag document on this host,
// newest first, at most perDoc runs per document. It is the machine-global
// view a steward board needs: the journal is keyed by a hash of the document
// path, so the path itself is recovered from each report's File field rather
// than from the directory name.
//
// A cacheDir that does not exist yet is not an error — it means nothing has
// run, which is a perfectly good answer.
func ListAllRuns(cacheDir string, perDoc int) ([]RunEntry, error) {
	root := ResolveCacheDir(cacheDir)
	if root == "" {
		return nil, nil
	}
	base := filepath.Join(root, "runs")
	docs, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RunEntry
	for _, d := range docs {
		if !d.IsDir() {
			continue
		}
		ids, ierr := runIDs(filepath.Join(base, d.Name()))
		if ierr != nil {
			continue
		}
		n := 0
		for _, id := range ids {
			if perDoc > 0 && n >= perDoc {
				break
			}
			e, rerr := readRunEntry(filepath.Join(base, d.Name(), id))
			if rerr != nil {
				continue
			}
			out = append(out, *e)
			n++
		}
	}
	// Run ids carry a nanosecond prefix, so a descending string sort across
	// documents is a descending chronological sort.
	sort.Slice(out, func(i, j int) bool { return out[i].RunID > out[j].RunID })
	return out, nil
}

// LoadRun reads one journaled run and the graph it executed. The graph is
// optional — a run interrupted before it was written still has a report.
func LoadRun(docPath, cacheDir, runID string) (*RunEntry, *RunGraph, error) {
	base := runsRoot(docPath, cacheDir)
	if base == "" {
		return nil, nil, errf(weavecli.ExitInvalidArg, "no dag cache directory available")
	}
	dir := filepath.Join(base, runID)
	entry, err := readRunEntry(dir)
	if err != nil {
		return nil, nil, err
	}
	var g *RunGraph
	if data, gerr := os.ReadFile(filepath.Join(dir, "graph.json")); gerr == nil {
		var parsed RunGraph
		if json.Unmarshal(data, &parsed) == nil {
			g = &parsed
		}
	}
	return entry, g, nil
}

// ReadRunLog returns one journaled log file's contents. rel must be a path the
// entry itself reported, and is verified to stay inside the run directory —
// this is a read of a name that reached us from JSON, so it is not trusted.
func ReadRunLog(docPath, cacheDir, runID, rel string) ([]byte, error) {
	base := runsRoot(docPath, cacheDir)
	if base == "" {
		return nil, errf(weavecli.ExitInvalidArg, "no dag cache directory available")
	}
	dir := filepath.Join(base, runID)
	p := filepath.Join(dir, filepath.FromSlash(rel))
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return nil, errf(weavecli.ExitInvalidArg, "log path escapes the run directory")
	}
	return os.ReadFile(abs)
}

func readRunEntry(dir string) (*RunEntry, error) {
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		return nil, err
	}
	var e RunEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// --- CLI rendering (the --runs / --show surface) ---

type runsItem struct {
	RunID      string   `json:"run_id"`
	StartedAt  string   `json:"started_at"`
	DurationMS int64    `json:"duration_ms"`
	Failed     bool     `json:"failed"`
	Targets    []string `json:"targets,omitempty"`
	Tasks      int      `json:"tasks"`
}

type runsListResult struct {
	File string     `json:"file"`
	Runs []runsItem `json:"runs"`
}

// runRuns lists this document's journaled runs, newest first. Like --timings it
// reads only what is on disk and executes nothing.
func runRuns(out io.Writer, mode weavecli.OutputMode, doc *Document, cacheDir string, limit int) error {
	entries, err := ListRuns(doc.Path, cacheDir, limit)
	if err != nil {
		return err
	}
	res := runsListResult{File: doc.Path}
	for _, e := range entries {
		res.Runs = append(res.Runs, runsItem{
			RunID:      e.RunID,
			StartedAt:  e.StartedAt.Format(time.RFC3339),
			DurationMS: e.DurationMS,
			Failed:     e.Failed,
			Targets:    e.Targets,
			Tasks:      len(e.Tasks),
		})
	}
	if mode == weavecli.OutputJSON {
		emitOK(out, res)
		return nil
	}
	if len(res.Runs) == 0 {
		fmt.Fprintf(out, "dag: no runs recorded yet for %s\n", doc.Path)
		return nil
	}
	for _, r := range res.Runs {
		status := "ok"
		if r.Failed {
			status = "FAILED"
		}
		fmt.Fprintf(out, "%-26s %-20s %10s  %-6s %s\n",
			r.RunID, r.StartedAt, fmtMS(r.DurationMS), status, strings.Join(r.Targets, " "))
	}
	return nil
}

type showRunResult struct {
	File  string    `json:"file"`
	Run   *RunEntry `json:"run"`
	Graph *RunGraph `json:"graph,omitempty"`
}

type statusResult struct {
	File       string `json:"file"`
	RunID      string `json:"run_id"`
	StartedAt  string `json:"started_at"`
	DurationMS int64  `json:"duration_ms"`
	Failed     bool   `json:"failed"`
	Total      int    `json:"total"`
	OK         int    `json:"ok"`
	Failedn    int    `json:"failed_count"`
	Targets    string `json:"targets,omitempty"`
}

// runStatus prints ONE line about the most recent run and exits non-zero if it
// failed, so it composes in a shell (`dag --status || notify-me`). --runs is
// the list, --show is the detail; this is the glance.
func runStatus(out io.Writer, mode weavecli.OutputMode, doc *Document, cacheDir string) error {
	entries, err := ListRuns(doc.Path, cacheDir, 1)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if mode == weavecli.OutputJSON {
			emitOK(out, statusResult{File: doc.Path})
			return nil
		}
		fmt.Fprintf(out, "dag: no runs recorded yet for %s\n", doc.Path)
		return nil
	}
	e := entries[0]
	res := statusResult{
		File: doc.Path, RunID: e.RunID, StartedAt: e.StartedAt.Format(time.RFC3339),
		DurationMS: e.DurationMS, Failed: e.Failed, Total: len(e.Tasks),
		Targets: strings.Join(e.Targets, " "),
	}
	for _, t := range e.Tasks {
		switch t.Status {
		case "done", "up-to-date":
			res.OK++
		case "failed":
			res.Failedn++
		}
	}
	if mode == weavecli.OutputJSON {
		emitOK(out, res)
	} else if mode != weavecli.OutputQuiet {
		verdict := "ok"
		if res.Failed {
			verdict = "FAILED"
		}
		fmt.Fprintf(out, "%s  %d/%d targets  %s  %s  %s\n",
			verdict, res.OK, res.Total, fmtMS(res.DurationMS),
			e.StartedAt.Format(time.RFC3339), res.RunID)
	}
	// A status verb that reports failure with exit 0 is the absence-of-evidence
	// shape this repo refuses elsewhere; make the verdict actionable.
	if res.Failed {
		return &Error{Code: weavecli.ExitGenericFail, Msg: "last run failed"}
	}
	return nil
}

// runShow prints one journaled run. runID may be "last" for the newest run,
// which is what an operator actually wants after a failure. htmlOut renders the
// standalone page instead (redirect it to a file, or pipe it to a browser).
func runShow(out io.Writer, mode weavecli.OutputMode, doc *Document, cacheDir, runID string, htmlOut bool) error {
	if runID == "last" {
		entries, err := ListRuns(doc.Path, cacheDir, 1)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return errf(weavecli.ExitInvalidArg, "no runs recorded yet for %s", doc.Path)
		}
		runID = entries[0].RunID
	}
	entry, graph, err := LoadRun(doc.Path, cacheDir, runID)
	if err != nil {
		return errf(weavecli.ExitInvalidArg, "no such run %q for %s", runID, doc.Path)
	}
	if htmlOut {
		page, rerr := renderRunHTML(entry, graph)
		if rerr != nil {
			return rerr
		}
		_, werr := out.Write(page)
		return werr
	}
	res := showRunResult{File: doc.Path, Run: entry, Graph: graph}
	if mode == weavecli.OutputJSON {
		emitOK(out, res)
		return nil
	}
	status := "ok"
	if entry.Failed {
		status = "FAILED"
	}
	fmt.Fprintf(out, "run      %s\n", entry.RunID)
	fmt.Fprintf(out, "file     %s\n", entry.File)
	fmt.Fprintf(out, "started  %s\n", entry.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(out, "elapsed  %s\n", fmtMS(entry.DurationMS))
	fmt.Fprintf(out, "result   %s\n", status)
	if len(entry.Targets) > 0 {
		fmt.Fprintf(out, "targets  %s\n", strings.Join(entry.Targets, " "))
	}
	layer := map[string]int{}
	if graph != nil {
		for _, n := range graph.Nodes {
			layer[n.Name] = n.Layer
		}
	}
	fmt.Fprintln(out)
	for _, t := range entry.Tasks {
		mark := t.Status
		if t.UpToDate {
			mark = "up-to-date"
		}
		fmt.Fprintf(out, "  L%-2d %-24s %-12s %8s  exit=%d\n",
			layer[t.Name], t.Name, mark, fmtMS(t.DurationMS), t.ExitCode)
		for _, l := range t.Logs {
			fmt.Fprintf(out, "%29s log: %s\n", "", l)
		}
	}
	return nil
}

// writeJSONAtomic writes v as indented JSON via tmp+rename, so a reader never
// observes a half-written file (the same discipline as Cache.Save).
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
