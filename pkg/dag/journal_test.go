// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/qiangli/coreutils/cmds/all"
)

// journaledRun runs md in a temp dir with a journal rooted at cacheDir and
// returns the sealed entry.
func journaledRun(t *testing.T, cacheDir, dir, md string, targets ...string) (*Engine, RunReport) {
	t.Helper()
	docPath := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(docPath, []byte(md), 0o644); err != nil {
		t.Fatalf("write DAG.md: %v", err)
	}
	eng := engineFor(t, dir, md)
	j, err := OpenJournal(docPath, cacheDir, 0)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if j == nil {
		t.Fatal("OpenJournal returned nil journal for an explicit cache dir")
	}
	eng.Journal = j
	report, err := eng.Run(context.Background(), targets...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return eng, report
}

func TestJournalRecordsRunAndLogs(t *testing.T) {
	cacheDir, dir := t.TempDir(), t.TempDir()
	md := "## Tasks\n\n" +
		"### build\n" + block("bash", "echo building-now") +
		"### check\nRequires: build\n" + block("bash", "echo checked-ok")

	eng, report := journaledRun(t, cacheDir, dir, md, "check")
	if report.Failed {
		t.Fatalf("run failed: %+v", report.Results)
	}

	docPath := filepath.Join(dir, "DAG.md")
	entries, err := ListRuns(docPath, cacheDir, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 journaled run, got %d", len(entries))
	}
	got := entries[0]
	if got.RunID != eng.Journal.RunID() {
		t.Errorf("run id = %q, want %q", got.RunID, eng.Journal.RunID())
	}
	if got.Failed {
		t.Error("entry reports failed for a green run")
	}
	if len(got.Tasks) != 2 {
		t.Fatalf("want 2 journaled tasks, got %d", len(got.Tasks))
	}

	// The report must round-trip through LoadRun, and the graph must carry the
	// layering a renderer depends on: check requires build, so it is one deeper.
	entry, graph, err := LoadRun(docPath, cacheDir, got.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if entry.RunID != got.RunID {
		t.Errorf("LoadRun id = %q, want %q", entry.RunID, got.RunID)
	}
	if graph == nil {
		t.Fatal("no graph recorded")
	}
	layer := map[string]int{}
	for _, n := range graph.Nodes {
		layer[n.Name] = n.Layer
	}
	if layer["build"] != 0 || layer["check"] != 1 {
		t.Errorf("layers = %v, want build=0 check=1", layer)
	}

	// Each task's body output must be retrievable from its journaled log.
	for _, task := range entry.Tasks {
		if len(task.Logs) == 0 {
			t.Errorf("task %q recorded no log", task.Name)
			continue
		}
		data, rerr := ReadRunLog(docPath, cacheDir, got.RunID, task.Logs[0])
		if rerr != nil {
			t.Errorf("ReadRunLog(%s): %v", task.Name, rerr)
			continue
		}
		want := map[string]string{"build": "building-now", "check": "checked-ok"}[task.Name]
		if !strings.Contains(string(data), want) {
			t.Errorf("log for %q = %q, want it to contain %q", task.Name, string(data), want)
		}
	}
}

// A journal that records a hostname or a raw error string would make its
// reports non-comparable across machines and is the classic path for a real
// host name to reach a shared artifact. RunRecord refuses both by construction
// (see RecordAttempt); the journal must not reintroduce them.
func TestJournalReportOmitsHostAndErrorText(t *testing.T) {
	cacheDir, dir := t.TempDir(), t.TempDir()
	md := "## Tasks\n\n" +
		"### boom\nHost: secret-box.internal\n" + block("bash", "echo failing-loudly; exit 3")

	journaledRun(t, cacheDir, dir, md, "boom")

	docPath := filepath.Join(dir, "DAG.md")
	entries, err := ListRuns(docPath, cacheDir, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListRuns: %v (%d entries)", err, len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(
		runsRoot(docPath, cacheDir), entries[0].RunID, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	report := string(raw)
	if strings.Contains(report, "secret-box.internal") {
		t.Errorf("report.json leaked the target host:\n%s", report)
	}
	// The body's own output belongs in the log file, not in the record.
	if strings.Contains(report, "failing-loudly") {
		t.Errorf("report.json leaked body output:\n%s", report)
	}
	if !entries[0].Failed {
		t.Error("failed run not marked failed")
	}
	// The classification still has to survive — a report that hides WHY is
	// useless. Records carry it as a stable code.
	if len(entries[0].Records) == 0 {
		t.Error("no run records journaled")
	}
}

// The live-tee rule, tested directly. This is the assertion that actually
// pins the security property: TestJournalRedactsSecretsInLogs only inspects
// the FINAL log contents, and those come out redacted either way because
// runOne rewrites the file afterward. Only this test fails if the guard in
// runAttempt is removed.
func TestJournalMayTeeExcludesSecretTargets(t *testing.T) {
	if !journalMayTee(&Task{Name: "plain"}) {
		t.Error("a target with no secrets must be teed live")
	}
	if journalMayTee(&Task{Name: "leaky", Secrets: []string{"TOKEN"}}) {
		t.Error("a secret-declaring target must NOT be teed live: its output is " +
			"redacted only after capture, so streaming puts the raw secret on disk")
	}
}

// A target declaring Secrets has its output redacted only AFTER capture, so a
// naive live tee would write the secret to disk. The journal must end up with
// the redacted form. (The tee guard itself is pinned by
// TestJournalMayTeeExcludesSecretTargets — this covers the end state.)
func TestJournalRedactsSecretsInLogs(t *testing.T) {
	cacheDir, dir := t.TempDir(), t.TempDir()
	const secret = "hunter2-do-not-log"
	t.Setenv("DAG_TEST_TOKEN", secret)

	md := "## Tasks\n\n" +
		"### leaky\nSecrets: DAG_TEST_TOKEN\n" + block("bash", "echo token is $DAG_TEST_TOKEN")

	docPath := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(docPath, []byte(md), 0o644); err != nil {
		t.Fatalf("write DAG.md: %v", err)
	}
	eng := engineFor(t, dir, md)
	eng.Env = append(os.Environ(), "DAG_TEST_TOKEN="+secret)
	j, err := OpenJournal(docPath, cacheDir, 0)
	if err != nil || j == nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	eng.Journal = j
	if _, err := eng.Run(context.Background(), "leaky"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Nothing anywhere under the journal may contain the secret.
	root := runsRoot(docPath, cacheDir)
	var found []string
	err = filepath.Walk(root, func(p string, info os.FileInfo, werr error) error {
		if werr != nil || info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr == nil && bytes.Contains(data, []byte(secret)) {
			found = append(found, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk journal: %v", err)
	}
	if len(found) > 0 {
		t.Errorf("secret written to journal files: %v", found)
	}

	// Guard against a vacuous pass: the log must actually exist and hold the
	// REDACTED output. If secret-bearing targets were simply never journaled,
	// the walk above would find nothing and prove nothing.
	entries, err := ListRuns(docPath, cacheDir, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListRuns: %v (%d entries)", err, len(entries))
	}
	if len(entries[0].Tasks) != 1 || len(entries[0].Tasks[0].Logs) == 0 {
		t.Fatalf("secret-bearing target recorded no log: %+v", entries[0].Tasks)
	}
	data, err := ReadRunLog(docPath, cacheDir, entries[0].RunID, entries[0].Tasks[0].Logs[0])
	if err != nil {
		t.Fatalf("ReadRunLog: %v", err)
	}
	if !strings.Contains(string(data), "token is ***") {
		t.Errorf("log = %q, want the redacted output", string(data))
	}
}

// Retention is what keeps an unattended fleet host from filling its disk.
func TestJournalPrunesToKeep(t *testing.T) {
	cacheDir, dir := t.TempDir(), t.TempDir()
	md := "## Tasks\n\n### noop\n" + block("bash", "echo ok")
	docPath := filepath.Join(dir, "DAG.md")
	if err := os.WriteFile(docPath, []byte(md), 0o644); err != nil {
		t.Fatalf("write DAG.md: %v", err)
	}

	const keep = 5
	const runs = 12
	var ids []string
	for i := 0; i < runs; i++ {
		eng := engineFor(t, dir, md)
		j, err := OpenJournal(docPath, cacheDir, keep)
		if err != nil || j == nil {
			t.Fatalf("OpenJournal: %v", err)
		}
		eng.Journal = j
		eng.Force = true
		ids = append(ids, j.RunID())
		if _, err := eng.Run(context.Background(), "noop"); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}

	entries, err := ListRuns(docPath, cacheDir, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(entries) != keep {
		t.Fatalf("kept %d runs, want %d", len(entries), keep)
	}
	// The survivors must be the NEWEST ones — pruning the wrong end would
	// silently discard exactly the run someone is about to look at.
	newest := map[string]bool{}
	for _, id := range ids[runs-keep:] {
		newest[id] = true
	}
	for _, e := range entries {
		if !newest[e.RunID] {
			t.Errorf("kept run %q is not among the newest %d", e.RunID, keep)
		}
	}
	// Newest-first ordering is the listing contract.
	if entries[0].RunID != ids[runs-1] {
		t.Errorf("first listed = %q, want newest %q", entries[0].RunID, ids[runs-1])
	}
}

// An unwritable or unavailable cache root must degrade to "no journal", never
// fail the run — the same contract LoadCache has.
func TestJournalDegradesWithoutCacheDir(t *testing.T) {
	t.Setenv("DAG_CACHE_DIR", "")
	dir := t.TempDir()
	docPath := filepath.Join(dir, "DAG.md")

	// An explicit empty cacheDir with no DAG_CACHE_DIR falls through to the user
	// cache dir, which exists on test hosts; the nil-journal path is what a
	// caller sees when ResolveCacheDir yields "". Exercise the nil receiver
	// directly, since that is the contract every call site relies on.
	var nilJournal *Journal
	if nilJournal.RunID() != "" || nilJournal.Dir() != "" {
		t.Error("nil journal should report empty identity")
	}
	if nilJournal.attemptLogWriter("t", 1) != nil {
		t.Error("nil journal should not hand out a writer")
	}
	if err := nilJournal.WriteGraph(nil); err != nil {
		t.Errorf("nil journal WriteGraph: %v", err)
	}
	if err := nilJournal.Finish(nil, RunReport{}); err != nil {
		t.Errorf("nil journal Finish: %v", err)
	}
	if err := nilJournal.Close(); err != nil {
		t.Errorf("nil journal Close: %v", err)
	}
	nilJournal.writeAttemptLog("t", 1, "out", "err") // must not panic
	_ = docPath
}

// The read-only reporters must survive a file with NO default goal and no
// named target. That combination makes the command fall back to listing
// targets, and for a long time --timings landed after that fallback and
// silently printed the target list instead of timings. All three reporters are
// pinned here so the ordering cannot regress again.
func TestReportersRunWithoutDefaultGoal(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DAG_CACHE_DIR", cacheDir)

	// Two targets and no `default:` frontmatter => no default goal.
	md := "## Tasks\n\n" +
		"### alpha\n" + block("bash", "echo alpha-ran") +
		"### beta\nRequires: alpha\n" + block("bash", "echo beta-ran")
	path := writeDAG(t, md)

	// Produce one journaled run so the reporters have something to report.
	run := NewDagCmd()
	run.SetOut(new(bytes.Buffer))
	run.SetErr(new(bytes.Buffer))
	run.SetArgs([]string{"--file", path, "beta"})
	if err := run.Execute(); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
		deny string
	}{
		// "total (T)" only ever appears in the timings report; "requires:" only
		// in the target listing. Asserting both directions is what distinguishes
		// "reported correctly" from "fell through to --list".
		{"timings", []string{"--timings"}, "total (T)", "requires:"},
		{"runs", []string{"--runs"}, "beta", "requires:"},
		{"show", []string{"--show", "last"}, "result   ok", "requires:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewDagCmd()
			out, errOut := new(bytes.Buffer), new(bytes.Buffer)
			cmd.SetOut(out)
			cmd.SetErr(errOut)
			cmd.SetArgs(append([]string{"--file", path}, tc.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute: %v (stderr=%s)", err, errOut.String())
			}
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Errorf("output missing %q:\n%s", tc.want, got)
			}
			if strings.Contains(got, tc.deny) {
				t.Errorf("output looks like the target list (contains %q):\n%s", tc.deny, got)
			}
		})
	}
}

// --status is the glance: one line, and a non-zero exit when the last run
// failed. A status verb that reports failure with exit 0 is the
// absence-of-evidence shape this repo refuses everywhere else.
func TestStatusReportsVerdictAndExitCode(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DAG_CACHE_DIR", cacheDir)

	green := writeDAG(t, "## Tasks\n\n### fine\n"+block("bash", "echo fine"))
	red := writeDAG(t, "## Tasks\n\n### boom\n"+block("bash", "exit 7"))

	// Name the target explicitly: a file with no `default:` frontmatter and no
	// target named "default" lists its targets instead of running anything.
	for _, tc := range []struct{ path, target string }{{green, "fine"}, {red, "boom"}} {
		seed := NewDagCmd()
		seed.SetOut(new(bytes.Buffer))
		seed.SetErr(new(bytes.Buffer))
		seed.SetArgs([]string{"--file", tc.path, tc.target})
		_ = seed.Execute() // the red one fails on purpose
	}

	t.Run("green", func(t *testing.T) {
		cmd := NewDagCmd()
		out := new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs([]string{"--file", green, "--status"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("green --status returned error: %v", err)
		}
		if got := out.String(); !strings.HasPrefix(got, "ok  1/1 targets") {
			t.Errorf("status = %q, want an ok verdict", got)
		}
		if n := strings.Count(strings.TrimSpace(out.String()), "\n"); n != 0 {
			t.Errorf("status printed %d extra lines; it is meant to be one", n)
		}
	})

	t.Run("red", func(t *testing.T) {
		cmd := NewDagCmd()
		out := new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs([]string{"--file", red, "--status"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("red --status exited 0; a failed last run must be actionable")
		}
		if code := ExitCodeOf(err); code == 0 {
			t.Errorf("exit code = %d, want non-zero", code)
		}
		if got := out.String(); !strings.HasPrefix(got, "FAILED") {
			t.Errorf("status = %q, want a FAILED verdict on stdout", got)
		}
	})

	t.Run("no runs", func(t *testing.T) {
		fresh := writeDAG(t, "## Tasks\n\n### never\n"+block("bash", "echo x"))
		cmd := NewDagCmd()
		out := new(bytes.Buffer)
		cmd.SetOut(out)
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs([]string{"--file", fresh, "--status"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("--status with no history should not error: %v", err)
		}
		if !strings.Contains(out.String(), "no runs recorded") {
			t.Errorf("status = %q", out.String())
		}
	})
}

// The HTML view must be self-contained (no network) and must escape whatever
// a DAG file happens to name its targets — a DAG file is untrusted input.
func TestRunHTMLIsSelfContainedAndEscaped(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DAG_CACHE_DIR", cacheDir)

	path := writeDAG(t, "## Tasks\n\n"+
		"### <script>alert(1)</script>\n"+block("bash", "echo xss")+
		"### second\nRequires: <script>alert(1)</script>\n"+block("bash", "echo two"))

	seed := NewDagCmd()
	seed.SetOut(new(bytes.Buffer))
	seed.SetErr(new(bytes.Buffer))
	seed.SetArgs([]string{"--file", path, "second"})
	if err := seed.Execute(); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	cmd := NewDagCmd()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--file", path, "--show", "last", "--html"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("--show --html: %v", err)
	}
	page := out.String()

	if !strings.HasPrefix(page, "<!doctype html>") {
		t.Errorf("not an HTML document: %.60q", page)
	}
	// A target named <script> must never reach the page as markup.
	if strings.Contains(page, "<script>") {
		t.Errorf("unescaped script tag in rendered page:\n%s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Errorf("target name not present in escaped form:\n%s", page)
	}
	// Self-contained: nothing may be fetched at view time.
	for _, bad := range []string{"http://", "https://", "src=", "<script"} {
		if strings.Contains(page, bad) {
			t.Errorf("page is not self-contained, found %q", bad)
		}
	}
	// The layered graph is the point of the view: two layers, L0 and L1.
	if !strings.Contains(page, ">L0</th>") || !strings.Contains(page, ">L1</th>") {
		t.Errorf("layered graph columns missing:\n%s", page)
	}
}

// The machine-global reader is what the steward board consumes; it must span
// documents and sort newest-first across all of them.
func TestListAllRunsSpansDocuments(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("DAG_CACHE_DIR", cacheDir)

	var last string
	for _, body := range []string{"echo one", "echo two", "echo three"} {
		p := writeDAG(t, "## Tasks\n\n### go\n"+block("bash", body))
		cmd := NewDagCmd()
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		cmd.SetArgs([]string{"--file", p, "go"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("seed %q: %v", body, err)
		}
		last = p
	}

	all, err := ListAllRuns(cacheDir, 5)
	if err != nil {
		t.Fatalf("ListAllRuns: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 runs across 3 documents, got %d", len(all))
	}
	if all[0].File != last {
		t.Errorf("newest run is from %q, want %q", all[0].File, last)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].RunID < all[i].RunID {
			t.Errorf("runs not sorted newest-first at %d", i)
		}
	}

	// An empty root is "nothing has run", not an error.
	empty, err := ListAllRuns(t.TempDir(), 5)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty root: got %d runs, err %v", len(empty), err)
	}
}

// Distinct target names must never share a log file, however they are spelled.
func TestSafeFileNameDisambiguates(t *testing.T) {
	cases := []struct{ a, b string }{
		{"a/b", "a:b"},
		{"deploy prod", "deploy-prod"},
		{"x", "x "},
	}
	for _, c := range cases {
		if got, want := safeFileName(c.a), safeFileName(c.b); got == want {
			t.Errorf("safeFileName(%q) == safeFileName(%q) == %q", c.a, c.b, got)
		}
	}
	// A name that needs no escaping stays readable.
	if got := safeFileName("build-all.v2"); got != "build-all.v2" {
		t.Errorf("safeFileName(build-all.v2) = %q, want it unchanged", got)
	}
}

// A log path arriving from JSON is untrusted input; it must not read outside
// the run directory.
func TestReadRunLogRejectsEscape(t *testing.T) {
	cacheDir, dir := t.TempDir(), t.TempDir()
	md := "## Tasks\n\n### noop\n" + block("bash", "echo ok")
	journaledRun(t, cacheDir, dir, md, "noop")

	docPath := filepath.Join(dir, "DAG.md")
	entries, _ := ListRuns(docPath, cacheDir, 0)
	if len(entries) != 1 {
		t.Fatalf("want 1 run, got %d", len(entries))
	}
	if _, err := ReadRunLog(docPath, cacheDir, entries[0].RunID, "../../../etc/passwd"); err == nil {
		t.Error("ReadRunLog accepted a path escaping the run directory")
	}
}

// report.json must be complete-or-absent, never half-written.
func TestJournalReportIsValidJSON(t *testing.T) {
	cacheDir, dir := t.TempDir(), t.TempDir()
	md := "## Tasks\n\n### noop\n" + block("bash", "echo ok")
	journaledRun(t, cacheDir, dir, md, "noop")

	docPath := filepath.Join(dir, "DAG.md")
	entries, _ := ListRuns(docPath, cacheDir, 0)
	raw, err := os.ReadFile(filepath.Join(runsRoot(docPath, cacheDir), entries[0].RunID, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("report.json is not valid JSON: %v", err)
	}
	if probe["schema_version"] == nil {
		t.Error("report.json has no schema_version")
	}
	// No .tmp file may survive a completed write.
	ents, _ := os.ReadDir(filepath.Join(runsRoot(docPath, cacheDir), entries[0].RunID))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
