// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package execlog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, w *Writer, ep string, at time.Time, argv []string, exit int, pid int) {
	t.Helper()
	body := Scrub(nil, argv, "/w", TemplateOpts{})
	rec := Record{At: at, Cmd: argv[0], PID: pid, Observed: true}
	if exit >= 0 {
		rec.Exit = &exit
	}
	if err := w.Append(rec, body, ep); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	w := Open(root)
	now := time.Now().UTC()

	write(t, w, "ep-a", now, []string{"go", "build", "./..."}, 0, 100)
	write(t, w, "ep-a", now, []string{"go", "test", "./..."}, 1, 100)
	_ = w.Close()

	recs, cov, err := Read(root, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if cov.Records != 2 || cov.Days != 1 {
		t.Errorf("coverage wrong: %+v", cov)
	}
	if recs[0].Seq >= recs[1].Seq {
		t.Errorf("seq must be monotonic: %d then %d", recs[0].Seq, recs[1].Seq)
	}
	if recs[0].Schema != Schema || recs[0].Stage != "episode" {
		t.Errorf("envelope not stamped: %+v", recs[0])
	}

	failed, _, _ := Read(root, Query{Failed: true})
	if len(failed) != 1 || failed[0].Cmd != "go" || *failed[0].Exit != 1 {
		t.Errorf("failed filter wrong: %+v", failed)
	}
}

// TestUnobservedExitIsNotZero pins the rule that keeps an abstain from being
// recorded as a pass. "The command ran and returned 0" and "we could not
// observe an exit" are different claims and must stay distinguishable on disk.
func TestUnobservedExitIsNotZero(t *testing.T) {
	root := t.TempDir()
	w := Open(root)
	body := Scrub(nil, []string{"ssh", "host"}, "/w", TemplateOpts{})
	if err := w.Append(Record{Cmd: "ssh", Observed: false}, body, "ep-x"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	recs, _, _ := Read(root, Query{})
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].Exit != nil {
		t.Errorf("unobserved exit must be null on disk, got %d", *recs[0].Exit)
	}
	if recs[0].Observed {
		t.Error("Observed must stay false")
	}
	// And the failure filter must not claim it as a success either.
	failed, _, _ := Read(root, Query{Failed: true})
	if len(failed) != 0 {
		t.Errorf("an unobserved record is not evidence of failure: %+v", failed)
	}
}

// TestLostRecordsAreCounted is the disclosure that makes an always-on recorder
// honest: a gap in the sequence is loss, and loss must be countable rather than
// indistinguishable from "it never ran".
func TestLostRecordsAreCounted(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, time.Now().UTC().Format("2006-01-02"))
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	// seq 1, 2 and 5 landed; 3 and 4 died with the process.
	body := `{"schema":"bashy-execlog-v1","episode":"ep-a","pid":7,"seq":%d,"cmd":"ls"}` + "\n"
	var out []byte
	for _, s := range []int{1, 2, 5} {
		out = append(out, []byte(fmt.Sprintf(body, s))...)
	}
	if err := os.WriteFile(filepath.Join(day, "ep-a.jsonl"), out, 0o600); err != nil {
		t.Fatal(err)
	}

	_, cov, err := Read(root, Query{})
	if err != nil {
		t.Fatal(err)
	}
	if cov.Lost != 2 {
		t.Errorf("want 2 lost records, got %d (coverage %+v)", cov.Lost, cov)
	}
}

// TestMalformedIsCountedNotSwallowed — a corrupt line is evidence the store was
// damaged. Skipping it silently makes a partial corpus look whole.
func TestMalformedIsCountedNotSwallowed(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, time.Now().UTC().Format("2006-01-02"))
	_ = os.MkdirAll(day, 0o700)
	content := `{"schema":"bashy-execlog-v1","cmd":"ls","seq":1}` + "\n" +
		"{not json\n" +
		`{"schema":"bashy-execlog-v1","cmd":"cat","seq":2}` + "\n"
	_ = os.WriteFile(filepath.Join(day, "ep-a.jsonl"), []byte(content), 0o600)

	recs, cov, _ := Read(root, Query{})
	if len(recs) != 2 {
		t.Errorf("want 2 good records, got %d", len(recs))
	}
	if cov.Malformed != 1 {
		t.Errorf("want 1 malformed counted, got %d", cov.Malformed)
	}
}

// TestPruneLeavesTombstone — after pruning, the corpus must be able to say that
// it was pruned. Otherwise a later "no failures found" is a claim about a
// corpus that no longer exists.
func TestPruneLeavesTombstone(t *testing.T) {
	root := t.TempDir()
	old := time.Now().UTC().AddDate(0, 0, -30)

	w := Open(root)
	write(t, w, "ep-old", old, []string{"go", "test"}, 1, 1)
	write(t, w, "ep-old", old, []string{"go", "build"}, 0, 1)
	_ = w.Close()

	stones, err := Prune(root, PruneOpts{KeepDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if len(stones) != 1 || stones[0].Records != 2 {
		t.Fatalf("want one tombstone covering 2 records, got %+v", stones)
	}

	recs, cov, _ := Read(root, Query{})
	if len(recs) != 0 {
		t.Errorf("pruned records should be gone, got %d", len(recs))
	}
	if cov.Pruned != 2 {
		t.Errorf("coverage must report 2 pruned, got %d (%+v)", cov.Pruned, cov)
	}
}

// TestPruneNeverTouchesToday — today's file is the one every live writer holds
// open. Deleting it orphans their writes on unix and fails on Windows.
func TestPruneNeverTouchesToday(t *testing.T) {
	root := t.TempDir()
	w := Open(root)
	write(t, w, "ep-now", time.Now().UTC(), []string{"ls"}, 0, 1)
	_ = w.Close()

	if _, err := Prune(root, PruneOpts{KeepDays: 0, Before: time.Now().UTC().AddDate(0, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	recs, _, _ := Read(root, Query{})
	if len(recs) != 1 {
		t.Errorf("today's records must survive any prune, got %d", len(recs))
	}
}

// TestConcurrentEpisodesDoNotInterleaveCausally — two shells sharing an
// inherited episode write to one file. Ordering their records by wall time
// would present the merge as one causal chain.
func TestConcurrentEpisodesDoNotInterleaveCausally(t *testing.T) {
	root := t.TempDir()
	w := Open(root)
	base := time.Now().UTC()
	// pid 1 and pid 2 interleave in wall-clock order.
	write(t, w, "ep-s", base.Add(0*time.Millisecond), []string{"go", "build"}, 0, 1)
	write(t, w, "ep-s", base.Add(1*time.Millisecond), []string{"npm", "install"}, 0, 2)
	write(t, w, "ep-s", base.Add(2*time.Millisecond), []string{"go", "test"}, 0, 1)
	_ = w.Close()

	recs, _, _ := Read(root, Query{})
	if len(recs) != 3 {
		t.Fatalf("want 3, got %d", len(recs))
	}
	// Records must be grouped by pid, so a `then` builder never bridges them.
	for i := 1; i < len(recs); i++ {
		if recs[i].PID == recs[i-1].PID && recs[i].Seq < recs[i-1].Seq {
			t.Errorf("within a pid, seq must ascend: %+v", recs)
		}
	}
	if recs[0].PID != 1 || recs[1].PID != 1 || recs[2].PID != 2 {
		t.Errorf("records must group by pid, got pids %d,%d,%d",
			recs[0].PID, recs[1].PID, recs[2].PID)
	}
}

func TestArgvCapDisclosesTruncation(t *testing.T) {
	big := make([]string, 4000)
	for i := range big {
		big[i] = "aaaaaaaaaa"
	}
	body := Scrub(nil, append([]string{"grep"}, big...), "/w", TemplateOpts{})
	if !body.truncated {
		t.Fatal("an over-cap argv must set Truncated, not clip silently")
	}
	if n := argvBytes(body.argv); n > maxArgv {
		t.Errorf("argv still over cap: %d > %d", n, maxArgv)
	}
}

// writeRaw drops literal JSONL lines into a day file, for cases the Writer
// cannot produce on purpose — a sequence gap, a corrupt line.
func writeRaw(t *testing.T, root, day, episode string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, episode+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
