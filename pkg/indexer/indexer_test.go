package indexer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/dbstore"
	"github.com/qiangli/yoke/pkg/dbvec"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kbmemory"
)

func fixture(t *testing.T) (*Manager, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_COORD_DIR", filepath.Join(t.TempDir(), "coord"))
	m := New(home)
	if e := m.Init(); e != nil {
		t.Fatal(e)
	}
	return m, t.TempDir()
}
func write(t *testing.T, p, s string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(s), 0600); e != nil {
		t.Fatal(e)
	}
}
func hits(t *testing.T, m *Manager, q string) []kbmemory.Hit {
	t.Helper()
	d, i := m.Targets()
	env := kbmemory.Retrieve(context.Background(), kbmemory.RetrieveQuery{Target: d, Index: i, Text: q, Limit: 20})
	if !env.OK {
		t.Fatalf("retrieve: %+v", env.Error)
	}
	return env.Result.(kbmemory.RetrieveResult).Hits
}
func TestIndexEditsForgetAndReopen(t *testing.T) {
	m, root := fixture(t)
	p := filepath.Join(root, "notes.md")
	write(t, p, "orchidmarker is the initial note")
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if got := hits(t, m, "orchidmarker"); len(got) != 1 || !strings.HasSuffix(got[0].Record.Source.Ref, "notes.md") {
		t.Fatalf("hits %+v", got)
	}
	write(t, p, "cobaltmarker replaced the initial note")
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "orchidmarker")) != 0 || len(hits(t, m, "cobaltmarker")) != 1 {
		t.Fatal("edit retained stale content")
	}
	if e := os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "cobaltmarker")) != 0 {
		t.Fatal("deleted source retained")
	}
	write(t, p, "ambermarker persists")
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	reopened := New(filepath.Dir(m.dir))
	if len(hits(t, reopened, "ambermarker")) != 1 {
		t.Fatal("reopen lost source")
	}
	if e := reopened.Forget(root); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "ambermarker")) != 0 {
		t.Fatal("forget retained entries")
	}
	st, e := os.Stat(m.dir)
	if e != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0700) {
		t.Fatal("index directory must be owner-only")
	}
	if _, e = os.Stat(filepath.Join(filepath.Dir(m.dir), "kb")); !os.IsNotExist(e) {
		t.Fatal("indexed files became curated kb pages")
	}
}
func TestExclusionsAndSecretScrubbing(t *testing.T) {
	m, root := fixture(t)
	t.Setenv("INDEX_TEST_SECRET_TOKEN", "planted-value-must-never-reach-index")
	write(t, filepath.Join(root, "safe.md"), "safemarker INDEX_TEST_SECRET_TOKEN=planted-value-must-never-reach-index")
	for _, p := range []string{".ssh/id_rsa", "node_modules/a.txt", "build/a.txt", ".env", ".bashy/state", "Library/a.txt", "ignored.txt"} {
		write(t, filepath.Join(root, p), "forbiddenmarker")
	}
	write(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	outside := filepath.Join(t.TempDir(), "outside.md")
	write(t, outside, "forbiddenmarker")
	if e := os.Symlink(outside, filepath.Join(root, "outside-link.md")); e != nil && os.Getenv("GOOS") != "windows" {
		t.Log(e)
	}
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "forbiddenmarker")) != 0 {
		t.Fatal("excluded content indexed")
	}
	got := hits(t, m, "safemarker")
	if len(got) != 1 || strings.Contains(got[0].Record.Body, "planted-value") {
		t.Fatalf("secret leak %+v", got)
	}
	docs, e := m.snapshot(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	for _, d := range docs {
		if strings.Contains(string(d.Doc), "planted-value") {
			t.Fatal("secret leaked to source store")
		}
	}
}
func TestVisitPromotionDecayAndExplicitPin(t *testing.T) {
	m, root := fixture(t)
	// Discovery never auto-promotes a temporary directory; exercise identical
	// visit accounting with a fixture canonical root by explicit setup.
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return base }
	if e := m.locked(func() error {
		m.cfg.Roots[root] = &Root{Path: root, State: "candidate", LastVisit: base, Visits: []string{"2026-09-29", "2026-09-30", "2026-10-01"}}
		m.decay()
		return m.save()
	}); e != nil {
		t.Fatal(e)
	}
	st, _ := m.Status()
	if st.Roots[0].State != "watching" {
		t.Fatal(st)
	}
	base = base.AddDate(0, 0, 31)
	if e := m.locked(func() error { m.decay(); return m.save() }); e != nil {
		t.Fatal(e)
	}
	st, _ = m.Status()
	if st.Roots[0].State != "unwatched" {
		t.Fatal(st)
	}
	base = base.AddDate(0, 0, 60)
	if e := m.locked(func() error { m.decay(); return m.save() }); e != nil {
		t.Fatal(e)
	}
	st, _ = m.Status()
	if st.Roots[0].State != "pruned" {
		t.Fatal(st)
	}
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	base = base.AddDate(0, 0, 100)
	if e := m.locked(func() error { m.decay(); return m.save() }); e != nil {
		t.Fatal(e)
	}
	st, _ = m.Status()
	if st.Roots[0].State != "watching" || st.Roots[0].Reason != "explicit" {
		t.Fatal(st)
	}
	// Repeated same-day visits are one day, and future records are ignored by the journal consumer.
	b, _ := json.Marshal(st)
	if len(b) == 0 {
		t.Fatal("empty status")
	}
}
func TestMissingIndexNeverInitializes(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "missing"))
	if _, e := m.Status(); e == nil {
		t.Fatal("missing index read succeeded")
	}
	if _, e := os.Stat(m.dir); !os.IsNotExist(e) {
		t.Fatal("read created index")
	}
}

func TestActualVisitCounting(t *testing.T) {
	m, root := fixture(t)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return base }
	if e := m.locked(func() error { m.observe(root, base); m.observe(root, base); return m.save() }); e != nil {
		t.Fatal(e)
	}
	st, _ := m.Status()
	if len(st.Roots[0].Visits) != 1 || st.Roots[0].State != "candidate" {
		t.Fatal(st)
	}
	for n := 1; n <= 2; n++ {
		base = base.AddDate(0, 0, 1)
		if e := m.locked(func() error { m.observe(root, base); return m.save() }); e != nil {
			t.Fatal(e)
		}
	}
	st, _ = m.Status()
	if st.Roots[0].State != "watching" || len(st.Roots[0].Visits) != 3 {
		t.Fatal(st)
	}
}
func TestPausedForgetStillDeletes(t *testing.T) {
	m, root := fixture(t)
	write(t, filepath.Join(root, "note.md"), "pausedmarker")
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := m.SetPaused(true); e != nil {
		t.Fatal(e)
	}
	if e := m.Forget(root); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "pausedmarker")) != 0 {
		t.Fatal("forget deferred by pause")
	}
}

func TestChunkVectorsUseTailAndFollowDeletion(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	m.Embed = func(ctx context.Context, model string, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			if strings.Contains(text, "tailmarker") {
				out[i] = []float32{0, 1}
			} else {
				out[i] = []float32{1, 0}
			}
		}
		return out, nil
	}
	p := filepath.Join(root, "long.md")
	write(t, p, strings.Repeat("ordinary text ", 400)+"tailmarker")
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.EnableEmbeddings(ctx, "fixture-local"); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	e := m.Query(ctx, "tailmarker", "semantic", 10)
	if !e.OK {
		t.Fatal(e.Error)
	}
	res := e.Result.(kbmemory.RetrieveResult)
	if len(res.Hits) != 1 || res.Mode != "semantic" {
		t.Fatalf("duplicate chunks or missing source: %+v", res)
	}
	if !strings.Contains(res.Hits[0].Record.Body, "tailmarker") {
		t.Fatal("missing tail")
	}
	if e := os.Remove(p); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	e = m.Query(ctx, "tailmarker", "semantic", 10)
	if !e.OK || len(e.Result.(kbmemory.RetrieveResult).Hits) != 0 {
		t.Fatal("deleted vector still recalled", e)
	}
}

func TestGitTrackedDeduplicatedAndCommitRefresh(t *testing.T) {
	m, root := fixture(t)
	repo, e := gogit.PlainInit(root, false)
	if e != nil {
		t.Fatal(e)
	}
	wt, e := repo.Worktree()
	if e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(root, "tracked.md"), "trackedmarker")
	write(t, filepath.Join(root, "ignored.md"), "forbiddenmarker")
	if _, e = wt.Add("tracked.md"); e != nil {
		t.Fatal(e)
	}
	if _, e = wt.Add("ignored.md"); e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(root, ".gitignore"), "ignored.md\n")
	if _, e = wt.Add(".gitignore"); e != nil {
		t.Fatal(e)
	}
	sig := &object.Signature{Name: "Fixture", Email: "fixture@example.invalid", When: time.Now()}
	if _, e = wt.Commit("fixture", &gogit.CommitOptions{Author: sig}); e != nil {
		t.Fatal(e)
	}
	if _, e = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://example.invalid/team/repo.git"}}); e != nil {
		t.Fatal(e)
	}
	clone := filepath.Join(t.TempDir(), "clone")
	if e = os.CopyFS(clone, os.DirFS(root)); e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(root, "untracked.md"), "forbiddenmarker")
	if e = m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e = m.Add(clone); e != nil {
		t.Fatal(e)
	}
	if e = m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "trackedmarker")) != 1 || len(hits(t, m, "forbiddenmarker")) != 0 {
		t.Fatal("git ignored/tracked/dedup policy failed")
	}
	// Whichever checkout was chosen deterministically, refresh that checkout's HEAD.
	st, _ := m.Status()
	canonical := ""
	for _, r := range st.Roots {
		if !strings.HasPrefix(r.Reason, "duplicate of ") {
			canonical = r.Path
		}
	}
	rp, e := gogit.PlainOpen(canonical)
	if e != nil {
		t.Fatal(e)
	}
	tree, e := rp.Worktree()
	if e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(canonical, "tracked.md"), "newcommitmarker")
	if _, e = tree.Add("tracked.md"); e != nil {
		t.Fatal(e)
	}
	if _, e = tree.Commit("update", &gogit.CommitOptions{Author: sig}); e != nil {
		t.Fatal(e)
	}
	if e = m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "newcommitmarker")) != 1 || len(hits(t, m, "trackedmarker")) != 0 {
		t.Fatal("commit refresh retained old text")
	}
}
func TestArchiveIsOneContentAddressedDocument(t *testing.T) {
	m, root := fixture(t)
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, text := range map[string]string{"entry.txt": "archivemarker", ".ssh/id_rsa": "forbiddenmarker", "../../escape": "forbiddenmarker", "ignoredmembermarker.txt": "not public"} {
		w, e := zw.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write([]byte(text)); e != nil {
			t.Fatal(e)
		}
	}
	if e := zw.Close(); e != nil {
		t.Fatal(e)
	}
	write(t, filepath.Join(root, ".bashyignore"), "ignoredmembermarker.txt\n")
	for _, name := range []string{"one.zip", "duplicate.zip"} {
		if e := os.WriteFile(filepath.Join(root, name), b.Bytes(), 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "archivemarker")) != 1 || len(hits(t, m, "forbiddenmarker")) != 0 || len(hits(t, m, "ignoredmembermarker")) != 0 {
		t.Fatal("archive content/dedup/exclusion failed")
	}
	docs, e := m.snapshot(context.Background())
	if e != nil || len(docs) != 1 {
		t.Fatal("archive expanded into multiple documents", len(docs), e)
	}
	if _, e = os.Stat(filepath.Join(root, "entry.txt")); !os.IsNotExist(e) {
		t.Fatal("archive unpacked")
	}
}

func TestSourcePreviewUsesSharedExclusions(t *testing.T) {
	m, root := fixture(t)
	p := filepath.Join(root, "note.md")
	write(t, p, "previewmarker")
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	r, e := m.Source(context.Background(), sourceURI(p))
	if e != nil || r.Body != "previewmarker" {
		t.Fatal(r, e)
	}
	write(t, filepath.Join(root, ".bashyignore"), "note.md\n")
	if _, e = m.Source(context.Background(), sourceURI(p)); e == nil {
		t.Fatal("newly excluded source preview allowed")
	}
}
func TestQueriesAndStatusRemainAvailableDuringEmbeddings(t *testing.T) {
	m, root := fixture(t)
	write(t, filepath.Join(root, "note.md"), "partialmarker")
	entered := make(chan struct{})
	release := make(chan struct{})
	m.Embed = func(ctx context.Context, model string, texts []string) ([][]float32, error) {
		if strings.Contains(texts[0], "partialmarker") && strings.Contains(texts[0], "note.md") {
			select {
			case <-entered:
			default:
				close(entered)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.EnableEmbeddings(context.Background(), "fixture-local"); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- m.Sync(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("embedding not reached")
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	st, e := m.Status()
	if e != nil || st.Phase != "embedding" {
		t.Fatal(st, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r := m.Query(ctx, "partialmarker", "lexical", 10)
	if !r.OK || len(r.Result.(kbmemory.RetrieveResult).Hits) != 1 {
		t.Fatal("query blocked by background ingestion", r)
	}
	close(release)
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not finish")
	}
}

func TestOverlappingRootsHaveOneSourceIdentity(t *testing.T) {
	m, root := fixture(t)
	child := filepath.Join(root, "child")
	write(t, filepath.Join(child, "note.md"), "overlapmarker")
	for _, p := range []string{root, child} {
		if e := m.Add(p); e != nil {
			t.Fatal(e)
		}
	}
	if e := m.Sync(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "overlapmarker")) != 1 {
		t.Fatal("overlapping roots duplicated source")
	}
	if e := m.Forget(root); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "overlapmarker")) != 1 {
		t.Fatal("child source lost with parent")
	}
	if e := m.Forget(child); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "overlapmarker")) != 0 {
		t.Fatal("source survived both roots forgotten")
	}
}
func TestSyncFailureIsVisibleAndPhaseSettles(t *testing.T) {
	m, root := fixture(t)
	write(t, filepath.Join(root, "one.md"), "one")
	write(t, filepath.Join(root, "two.md"), "two")
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.Configure(Config{MaxFiles: 1}); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(context.Background()); e == nil {
		t.Fatal("cap did not fail")
	}
	st, e := m.Status()
	if e != nil || st.Phase != "idle" || st.LastError == "" {
		t.Fatal("sync failure hidden", st, e)
	}
}
func TestInterruptedVectorShrinkCleansTailAcrossHoles(t *testing.T) {
	m, root := fixture(t)
	ctx := context.Background()
	m.Embed = func(context.Context, string, []string) ([][]float32, error) { return [][]float32{{1, 0}}, nil }
	p := filepath.Join(root, "long.md")
	write(t, p, strings.Repeat("long text ", 1000))
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.EnableEmbeddings(ctx, "fixture-local"); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	var id string
	if e := m.locked(func() error {
		docs, e := m.snapshot(ctx)
		if e != nil {
			return e
		}
		for key, d := range docs {
			id = key
			if e = m.put(ctx, key, p, "short", &d); e != nil {
				return e
			}
		}
		docs, e = m.snapshot(ctx)
		if e != nil {
			return e
		}
		d := docs[id]
		out, e := cat(ctx, m.docTarget(), dbvec.OpGet, dbvec.Input{Collection: "indexed", ID: id + "~0"}, false)
		if e != nil {
			return e
		}
		b, _ := json.Marshal(out.Result)
		var item dbvec.Item
		if e = json.Unmarshal(b, &item); e != nil {
			return e
		}
		v, _ := json.Marshal(item.Vector)
		if _, e = cat(ctx, m.docTarget(), dbvec.OpPut, dbvec.Input{Collection: "indexed", ID: id + "~0", Vector: v, Metadata: d.Doc, IfRevision: &item.Revision}, false); e != nil {
			return e
		}
		// Simulate a crash after the first stale tail was removed.
		if _, e = cat(ctx, m.docTarget(), dbvec.OpRm, dbvec.Input{Collection: "indexed", ID: id + "~1"}, false); e != nil {
			return e
		}
		m.cfg.PendingTrim = map[string]int{id: 3}
		return m.save()
	}); e != nil {
		t.Fatal(e)
	}
	write(t, p, "short")
	if e := m.Sync(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := cat(ctx, m.docTarget(), dbvec.OpGet, dbvec.Input{Collection: "indexed", ID: id + "~2"}, false); !errors.Is(e, dbstore.ErrNotFound) {
		t.Fatal("stale tail survived recovery", e)
	}
	st, e := m.Status()
	if e != nil || len(st.Limits.PendingTrim) != 0 {
		t.Fatal(st, e)
	}
}

func TestPauseCancelsActiveEmbeddingWithoutWaitingForStateLock(t *testing.T) {
	m, root := fixture(t)
	write(t, filepath.Join(root, "note.md"), "pausemarker")
	entered := make(chan struct{})
	m.Embed = func(ctx context.Context, model string, texts []string) ([][]float32, error) {
		if strings.Contains(texts[0], "pausemarker") {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return [][]float32{{1, 0}}, nil
	}
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.EnableEmbeddings(context.Background(), "fixture-local"); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- m.Sync(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("embed not reached")
	}
	start := time.Now()
	if e := m.SetPaused(true); e != nil {
		t.Fatal(e)
	}
	if time.Since(start) > time.Second {
		t.Fatal("pause blocked behind ingestion")
	}
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("model request not canceled")
	}
	st, e := m.Status()
	if e != nil || !st.Paused || st.Phase != "idle" || st.LastError != "" {
		t.Fatal(st, e)
	}
	if e := m.Forget(root); e != nil {
		t.Fatal(e)
	}
}

func TestGitInfoAndGlobalExcludes(t *testing.T) {
	m, root := fixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo, e := gogit.PlainInit(root, false)
	if e != nil {
		t.Fatal(e)
	}
	wt, e := repo.Worktree()
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"info.md", "global.md", "visible.md"} {
		write(t, filepath.Join(root, name), "ignorepolicyneedle "+name)
		if _, e = wt.Add(name); e != nil {
			t.Fatal(e)
		}
	}
	write(t, filepath.Join(root, ".git", "info", "exclude"), "info.md\n")
	global := filepath.Join(home, "global-ignore")
	write(t, global, "global.md\n")
	cfg, e := repo.Config()
	if e != nil {
		t.Fatal(e)
	}
	cfg.Raw.Section("core").SetOption("excludesfile", filepath.ToSlash(global))
	if e = repo.SetConfig(cfg); e != nil {
		t.Fatal(e)
	}
	if e = m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e = m.Sync(t.Context()); e != nil {
		t.Fatal(e)
	}
	got := hits(t, m, "ignorepolicyneedle")
	if len(got) != 1 || got[0].Record.Title != "visible.md" {
		t.Fatalf("ignored tracked source included: %#v", got)
	}
}

func TestIgnoreWindowsLineEndings(t *testing.T) {
	m, root := fixture(t)
	write(t, filepath.Join(root, ".gitignore"), "ignored.md\r\n")
	write(t, filepath.Join(root, "ignored.md"), "crlfignoredneedle")
	write(t, filepath.Join(root, "visible.md"), "crlfvisibleneedle")
	if e := m.Add(root); e != nil {
		t.Fatal(e)
	}
	if e := m.Sync(t.Context()); e != nil {
		t.Fatal(e)
	}
	if len(hits(t, m, "crlfignoredneedle")) != 0 || len(hits(t, m, "crlfvisibleneedle")) != 1 {
		t.Fatal("CRLF policy was not applied")
	}
}
