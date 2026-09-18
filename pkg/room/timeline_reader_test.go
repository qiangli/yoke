package room

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

func readerFixture(t *testing.T) string {
	t.Helper()
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "BASHY_") {
			t.Setenv(k, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	return filepath.Join(Dir(), "timeline.jsonl")
}
func readerLine(kind string) string {
	b, _ := json.Marshal(Event{Type: kind, Body: strings.Repeat("x", 128)})
	return string(b) + "\n"
}
func readerWrite(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
}
func readerLatest(t *testing.T, r *TimelineReader) TimelinePosition {
	t.Helper()
	p, err := r.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestTimelineReaderUnchangedAndAppend(t *testing.T) {
	path := readerFixture(t)
	readerWrite(t, path, strings.Repeat(readerLine(EventNote), 100)+readerLine(EventAck))
	r := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
	if p := readerLatest(t, r); p.Seq != 101 {
		t.Fatal(p)
	}
	before := r.Stats()
	readerLatest(t, r)
	if r.Stats() != before {
		t.Fatalf("unchanged poll did work: before=%+v after=%+v", before, r.Stats())
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(readerLine(EventNote) + readerLine(EventAck))
	f.Close()
	if p := readerLatest(t, r); p.Seq != 103 {
		t.Fatal(p)
	}
	if n := r.Stats().RecordsDecoded - before.RecordsDecoded; n != 2 {
		t.Fatalf("append decoded %d records, want 2", n)
	}
}
func TestTimelineReaderPartialAndLogicalSequence(t *testing.T) {
	path := readerFixture(t)
	line := readerLine(EventAck)
	readerWrite(t, path, "\nnot-json\n"+readerLine(EventNote)+strings.TrimSuffix(line, "\n"))
	r := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
	if p := readerLatest(t, r); p.Seq != 0 {
		t.Fatalf("accepted incomplete record: %+v", p)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString("\n")
	f.Close()
	if p := readerLatest(t, r); p.Seq != 2 {
		t.Fatalf("blank/malformed records changed logical position: %+v", p)
	}
}
func TestTimelineReaderResetAndRotation(t *testing.T) {
	path := readerFixture(t)
	readerWrite(t, path, readerLine(EventAck)+readerLine(EventNote))
	r := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
	before := readerLatest(t, r)
	lock, err := lockfile.Acquire(timelineLockPath(), lockfile.Holder{})
	if err != nil {
		t.Fatal(err)
	}
	if err := rewriteTimeline([]Event{{Type: EventNote}}); err != nil {
		t.Fatal(err)
	}
	if err := writeArchiveWatermark(1); err != nil {
		t.Fatal(err)
	}
	lock.Release()
	if got := readerLatest(t, r); got != before {
		t.Fatalf("rotation lost observed ack: before=%+v after=%+v", before, got)
	}
	readerWrite(t, path, readerLine(EventAck)+readerLine(EventAck)+readerLine(EventAck))
	got := readerLatest(t, r)
	if got.Generation == before.Generation {
		t.Fatalf("shrink/regrow reused old generation: %+v", got)
	}
}
func TestTimelineReaderReplacementAndSameSizeRewrite(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "rewrite", true: "replace"}[replace], func(t *testing.T) {
			path := readerFixture(t)
			readerWrite(t, path, readerLine("not"))
			r := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
			before := readerLatest(t, r)
			if replace {
				readerWrite(t, path+".new", readerLine(EventAck))
				if err := os.Rename(path+".new", path); err != nil {
					t.Fatal(err)
				}
			} else {
				readerWrite(t, path, readerLine(EventAck))
				// Exercise a metadata-visible rewrite, independently of the
				// filesystem clock's granularity. Two writes within one tick
				// can otherwise retain the same inode, size and modification
				// time and are indistinguishable to the unchanged fast path.
				modified := r.info.ModTime().Add(time.Second)
				if err := os.Chtimes(path, modified, modified); err != nil {
					t.Fatal(err)
				}
			}
			got := readerLatest(t, r)
			if got.Generation == before.Generation || got.Seq != 1 {
				t.Fatalf("reset not recognized: before=%+v after=%+v", before, got)
			}
		})
	}
}
func TestTimelineReaderRotationLockAndCancellation(t *testing.T) {
	path := readerFixture(t)
	readerWrite(t, path, readerLine(EventNote)+readerLine(EventAck))
	r := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
	before := readerLatest(t, r)
	lock, err := lockfile.Acquire(timelineLockPath(), lockfile.Holder{})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := rewriteTimeline([]Event{{Type: EventAck}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := r.Latest(ctx); err == nil {
		t.Fatal("read incoherent rewritten file while watermark publication lock held")
	}
	if err := writeArchiveWatermark(1); err != nil {
		t.Fatal(err)
	}
	lock.Release()
	if got := readerLatest(t, r); got != before {
		t.Fatalf("rotation changed ack identity: %+v -> %+v", before, got)
	}
}

func TestTimelineReaderGrowingPartialReadsOnlyAppend(t *testing.T) {
	path := readerFixture(t)
	line := strings.Replace(readerLine(EventAck), strings.Repeat("x", 128), strings.Repeat("x", 64*1024), 1)
	readerWrite(t, path, line[:len(line)-3])
	r := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
	readerLatest(t, r)
	before := r.Stats()
	readerLatest(t, r)
	if r.Stats() != before {
		t.Fatal("unchanged partial reread")
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(line[len(line)-3 : len(line)-2])
	f.Close()
	readerLatest(t, r)
	after := r.Stats()
	if after.BytesDecoded != before.BytesDecoded {
		t.Fatal("partial record decoded before newline")
	}
	if n := after.BytesRead - before.BytesRead; n > 1025 {
		t.Fatalf("partial append reread prior data: %d bytes", n)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(line[len(line)-2:])
	f.Close()
	if p := readerLatest(t, r); p.Seq != 1 {
		t.Fatal(p)
	}
}
func TestTimelineReaderRootChangeAndRestart(t *testing.T) {
	path := readerFixture(t)
	readerWrite(t, path, readerLine(EventAck))
	r := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
	before := readerLatest(t, r)
	fresh := NewTimelineReader(func(e Event) bool { return e.Type == EventAck })
	if p := readerLatest(t, fresh); p.Seq != before.Seq {
		t.Fatal(p)
	}
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	readerWrite(t, filepath.Join(Dir(), "timeline.jsonl"), readerLine(EventAck)+readerLine(EventAck))
	if p := readerLatest(t, r); p.Generation == before.Generation || p.Seq != 2 {
		t.Fatal(p)
	}
}

func TestTimelineReaderConcurrentAppendUsesCapturedSize(t *testing.T) {
	path := readerFixture(t)
	readerWrite(t, path, readerLine(EventNote))
	appended := false
	r := NewTimelineReader(func(e Event) bool {
		if !appended {
			appended = true
			lock, err := lockfile.TryAcquire(timelineLockPath(), lockfile.Holder{})
			if err == nil {
				lock.Release()
				t.Fatal("decode crossed rotation lock boundary")
			}
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(readerLine(EventAck)); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
		return e.Type == EventAck
	})
	if p := readerLatest(t, r); p.Seq != 0 {
		t.Fatalf("snapshot included racing append: %+v", p)
	}
	if p := readerLatest(t, r); p.Seq != 2 {
		t.Fatalf("next snapshot missed append: %+v", p)
	}
}
func TestTimelineReaderCancelledDecodeRetainsCommittedState(t *testing.T) {
	path := readerFixture(t)
	readerWrite(t, path, readerLine(EventAck))
	ctx, cancel := context.WithCancel(context.Background())
	r := NewTimelineReader(func(e Event) bool { cancel(); return true })
	if _, err := r.Latest(ctx); err == nil {
		t.Fatal("cancelled decode succeeded")
	}
	if r.position.Generation != 0 || r.readOffset != 0 || r.offset != 0 {
		t.Fatalf("cancel published state: %+v", r)
	}
	r.match = func(Event) bool { return true }
	if p := readerLatest(t, r); p.Seq != 1 {
		t.Fatal(p)
	}
}

func TestTimelineReaderCancelledPartialCompletionCanRetry(t *testing.T) {
	path := readerFixture(t)
	line := readerLine(EventAck)
	readerWrite(t, path, line[:len(line)-2])
	r := NewTimelineReader(func(Event) bool { return true })
	readerLatest(t, r)
	partial := string(r.partial)
	offset := r.readOffset
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	f.WriteString(line[len(line)-2:])
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.match = func(Event) bool { cancel(); return true }
	if _, err := r.Latest(ctx); err == nil {
		t.Fatal("cancelled completion succeeded")
	}
	if string(r.partial) != partial || r.readOffset != offset {
		t.Fatal("cancel mutated committed partial buffer")
	}
	r.match = func(Event) bool { return true }
	if p := readerLatest(t, r); p.Seq != 1 {
		t.Fatalf("retry lost completion: %+v", p)
	}
}
