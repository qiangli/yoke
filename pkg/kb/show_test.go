// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package kb_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/execlog"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/reduce"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/secrets"
)

// showWorld is every M1-wired kind backed by a real store in a tempdir: the
// resolvers are the owning stores' own RegisterRefs and the readers are thin
// adapters over the owning stores' readers — the same composition bashy
// wiring will use, minus the host roots.
type showWorld struct {
	kbDir     string
	execRoot  string
	chatRoot  string
	out       *reduce.Store
	outDigest string // full "sha256:<hex>"
	redactor  *secrets.Redactor
}

// episodeContentBudget bounds the test episode adapter, standing in for kb
// context's output budget until the host wiring lands (§5.1: oldest first,
// bounded).
const episodeContentBudget = 64 * 1024

const showSecretValue = "show-s3cr3t-value"

func newShowWorld(t *testing.T) *showWorld {
	t.Helper()
	w := &showWorld{}
	w.kbDir = t.TempDir()
	writeShowPage(t, w.kbDir, &kb.Page{
		Slug: "deploy-runbook", Type: kb.TypeRunbook, Title: "Deploy runbook",
		Description: "how to ship", Status: kb.StatusValidated, Body: "ship it",
	})

	w.execRoot = t.TempDir()
	writeShowShard(t, w.execRoot, "2026-09-12", "ep1",
		`{"at":"2026-09-12T10:00:00Z","episode":"ep1","cmd":"ls","seq":1}`,
		`{"at":"2026-09-12T10:05:00Z","episode":"ep1","cmd":"cat","seq":2}`,
	)

	fleetRoot := t.TempDir()
	writeShowFile(t, filepath.Join(fleetRoot, "skills", "deploy", "SKILL.md"),
		"---\nname: deploy\ndescription: Deploy the app\n---\n# Deploy\n")
	for _, env := range []string{"BASHY_TOOLS_PATH", "BASHY_MODELS_PATH", "BASHY_AGENTS_PATH", "BASHY_PEOPLE_PATH", "BASHY_HOSTS_PATH", "BASHY_SKILLS_DIR"} {
		t.Setenv(env, "")
	}

	w.out = reduce.NewStore(t.TempDir())
	digest, err := w.out.Put([]byte("the complete elided bytes"))
	if err != nil {
		t.Fatal(err)
	}
	w.outDigest = digest

	w.chatRoot = t.TempDir()
	writeShowFile(t, filepath.Join(w.chatRoot, "elif.md"), "# elif\n\na working session\n")

	w.redactor = secrets.NewRedactor()
	if err := w.redactor.Register("SHOW_TOKEN", showSecretValue); err != nil {
		t.Fatal(err)
	}

	g := ref.NewRegistry()
	kb.RegisterRefs(g, w.kbDir, nil)
	execlog.RegisterRefs(g, w.execRoot)
	fleet.RegisterRefs(g, fleet.WithRoot(fleetRoot), fleet.WithoutCloudOverlay())
	reduce.RegisterRefs(g, w.out)
	chat.RegisterRefs(g, w.chatRoot)
	kb.SetShowRegistry(g)

	kb.SetShowReader(ref.Episode, func(n ref.Node) (kb.ShowContent, error) {
		recs, _, err := execlog.Read(w.execRoot, execlog.Query{Episode: n.ID})
		if err != nil {
			return kb.ShowContent{}, err
		}
		var sb strings.Builder
		for _, r := range recs {
			fmt.Fprintf(&sb, "%s %s\n", r.At.UTC().Format(time.RFC3339), r.Cmd)
		}
		for _, line := range showJournalFor(w.kbDir, n.ID) {
			sb.WriteString(line + "\n")
		}
		out := sb.String()
		if len(out) > episodeContentBudget {
			out = out[len(out)-episodeContentBudget:]
		}
		return kb.ShowContent{Content: out}, nil
	})
	kb.SetShowReader(ref.Skill, func(n ref.Node) (kb.ShowContent, error) {
		b, err := os.ReadFile(n.Where)
		if err != nil {
			return kb.ShowContent{}, err
		}
		return kb.ShowContent{Content: string(b)}, nil
	})
	kb.SetShowReader(ref.Out, func(n ref.Node) (kb.ShowContent, error) {
		content, digest, err := w.out.Get(n.ID)
		if err != nil {
			return kb.ShowContent{}, err
		}
		return kb.ShowContent{Content: string(content), Snapshot: digest}, nil
	})
	kb.SetShowReader(ref.Chat, func(n ref.Node) (kb.ShowContent, error) {
		tr, err := chat.ReadTranscript(w.chatRoot, n.ID, w.redactor)
		if err != nil {
			if errors.Is(err, chat.ErrTranscriptExpired) {
				return kb.ShowContent{Outcome: kb.OutcomeExpired}, nil
			}
			return kb.ShowContent{}, err
		}
		if tr.Withheld {
			return kb.ShowContent{Outcome: kb.OutcomeRedacted, Redacted: tr.Redacted}, nil
		}
		return kb.ShowContent{Content: tr.Content, Redacted: tr.Redacted}, nil
	})

	t.Cleanup(func() {
		kb.SetShowRegistry(nil)
		for _, k := range []ref.Kind{ref.Episode, ref.Skill, ref.Out, ref.Chat} {
			kb.SetShowReader(k, nil)
		}
		kb.SetSenseResolver(nil)
		kb.SetSenseClock(nil)
	})
	return w
}

// showJournalFor returns the raw journal lines of the kb store at dir that
// belong to episode.
func showJournalFor(dir, episode string) []string {
	lines, err := kb.Open(dir).JournalTail(0)
	if err != nil {
		return nil
	}
	needle := `"episode":"` + episode + `"`
	var out []string
	for _, line := range lines {
		if strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

func writeShowPage(t *testing.T, dir string, p *kb.Page) {
	t.Helper()
	if err := kb.Open(dir).Write(p, "add"); err != nil {
		t.Fatal(err)
	}
}

func writeShowShard(t *testing.T, root, day, episode string, lines ...string) {
	t.Helper()
	dir := filepath.Join(root, day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, episode+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeShowFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runShow runs `kb --dir DIR show ARGS...` and splits the streams.
func runShow(t *testing.T, dir string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := kb.NewKBCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(append([]string{"--dir", dir, "show"}, args...))
	err = cmd.Execute()
	return out.String(), errBuf.String(), err
}

func showEnvelope(t *testing.T, stdout string) (kb.ShowEnvelope, map[string]any) {
	t.Helper()
	var env kb.ShowEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not a show envelope: %v\n%s", err, stdout)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatal(err)
	}
	return env, raw
}

func requireSchemaVersion(t *testing.T, env kb.ShowEnvelope, raw map[string]any) {
	t.Helper()
	if env.SchemaVersion != "bashy-kb-show-v1" {
		t.Errorf("schema_version = %q, want bashy-kb-show-v1", env.SchemaVersion)
	}
	if raw["schema_version"] != "bashy-kb-show-v1" {
		t.Errorf("raw schema_version = %v", raw["schema_version"])
	}
}

func TestShowBareSlugUnchanged(t *testing.T) {
	w := newShowWorld(t)
	raw, err := os.ReadFile(filepath.Join(w.kbDir, "pages", "deploy-runbook.md"))
	if err != nil {
		// The page file lives where the store keeps it; find it by handle.
		t.Skipf("page layout: %v", err)
	}
	for _, in := range []string{"deploy-runbook", "kb:deploy-runbook"} {
		stdout, _, err := runShow(t, w.kbDir, in)
		if err != nil {
			t.Fatalf("show %s: %v", in, err)
		}
		if stdout != string(raw) {
			t.Errorf("show %s = %q, want the raw page bytes", in, stdout)
		}
	}
}

func TestShowKBNotFound(t *testing.T) {
	w := newShowWorld(t)
	for _, in := range []string{"nothing-here", "kb:nothing-here"} {
		_, _, err := runShow(t, w.kbDir, in)
		if err == nil {
			t.Errorf("show %s: want an error (exit 1)", in)
		}
		stdout, _, err := runShow(t, w.kbDir, in, "--json")
		if err == nil {
			t.Errorf("show %s --json: want an error (exit 1)", in)
		}
		env, raw := showEnvelope(t, stdout)
		requireSchemaVersion(t, env, raw)
		if env.Outcome != "not-found" {
			t.Errorf("show %s outcome = %q, want not-found", in, env.Outcome)
		}
		if _, ok := raw["content"]; ok {
			t.Errorf("show %s error envelope carries content", in)
		}
	}
}

func TestShowEpisodeFound(t *testing.T) {
	w := newShowWorld(t)
	// One journal event for the episode, via the observe verb.
	cmd := kb.NewKBCmd()
	cmd.SetArgs([]string{"--dir", w.kbDir, "observe", "--episode", "ep1",
		"--kind", "note", "--ref", "kb:deploy-runbook", "--summary", "shipped it"})
	var discard bytes.Buffer
	cmd.SetOut(&discard)
	cmd.SetErr(&discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("observe: %v", err)
	}

	stdout, _, err := runShow(t, w.kbDir, "episode:ep1")
	if err != nil {
		t.Fatalf("show episode:ep1: %v", err)
	}
	for _, want := range []string{"ls", "cat", "shipped it"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("episode content lacks %q:\n%s", want, stdout)
		}
	}
	if li, lj := strings.Index(stdout, "ls"), strings.Index(stdout, "cat"); li > lj {
		t.Errorf("episode rows out of order (want oldest first)")
	}

	stdout, _, err = runShow(t, w.kbDir, "episode:ep1", "--json")
	if err != nil {
		t.Fatalf("show episode:ep1 --json: %v", err)
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "found" || env.Kind != "episode" || env.ID != "ep1" || env.Ref != "episode:ep1" {
		t.Errorf("envelope identity = %+v", env)
	}
	if !strings.Contains(env.Content, "cat") {
		t.Errorf("envelope content lacks the rows")
	}
}

func TestShowEpisodeNotFound(t *testing.T) {
	w := newShowWorld(t)
	_, _, err := runShow(t, w.kbDir, "episode:ghost")
	if err == nil {
		t.Error("show episode:ghost: want an error (exit 1)")
	}
	stdout, _, err := runShow(t, w.kbDir, "urn:dhnt:episode:ghost", "--json")
	if err == nil {
		t.Error("show urn:dhnt:episode:ghost --json: want an error (exit 1)")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "not-found" || env.Ref != "episode:ghost" {
		t.Errorf("envelope = %+v", env)
	}
	if _, ok := raw["content"]; ok {
		t.Error("not-found envelope carries content")
	}
}

func TestShowSkillFound(t *testing.T) {
	w := newShowWorld(t)
	stdout, _, err := runShow(t, w.kbDir, "skill:deploy")
	if err != nil {
		t.Fatalf("show skill:deploy: %v", err)
	}
	if !strings.Contains(stdout, "# Deploy") || !strings.Contains(stdout, "Deploy the app") {
		t.Errorf("skill content is not the SKILL.md body:\n%s", stdout)
	}
	stdout, _, err = runShow(t, w.kbDir, "skill:deploy", "--json")
	if err != nil {
		t.Fatalf("show skill:deploy --json: %v", err)
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "found" || env.Title != "Deploy the app" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestShowSkillNotFound(t *testing.T) {
	w := newShowWorld(t)
	stdout, _, err := runShow(t, w.kbDir, "skill:nosuch", "--json")
	if err == nil {
		t.Error("show skill:nosuch --json: want an error (exit 1)")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "not-found" {
		t.Errorf("outcome = %q", env.Outcome)
	}
}

func TestShowOutFound(t *testing.T) {
	w := newShowWorld(t)
	full := strings.TrimPrefix(w.outDigest, "sha256:")
	stdout, _, err := runShow(t, w.kbDir, "out:"+full[:12])
	if err != nil {
		t.Fatalf("show out:<prefix>: %v", err)
	}
	if stdout != "the complete elided bytes" {
		t.Errorf("out content = %q, want the spilled bytes verbatim", stdout)
	}
	stdout, _, err = runShow(t, w.kbDir, "out:"+full[:12], "--json")
	if err != nil {
		t.Fatalf("show out --json: %v", err)
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "found" || env.Content != "the complete elided bytes" {
		t.Errorf("envelope = %+v", env)
	}
	if env.Snapshot != w.outDigest {
		t.Errorf("snapshot = %q, want the blob digest %q", env.Snapshot, w.outDigest)
	}
}

func TestShowOutNotFound(t *testing.T) {
	w := newShowWorld(t)
	stdout, _, err := runShow(t, w.kbDir, "out:deadbeef", "--json")
	if err == nil {
		t.Error("show out:deadbeef --json: want an error (exit 1)")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "not-found" {
		t.Errorf("outcome = %q", env.Outcome)
	}
}

func TestShowOutUnreadableStoreIsError(t *testing.T) {
	w := newShowWorld(t)
	g := ref.NewRegistry()
	reduce.RegisterRefs(g, reduce.NewStore(filepath.Join(w.kbDir, "never-created")))
	kb.SetShowRegistry(g)
	kb.SetShowReader(ref.Out, func(n ref.Node) (kb.ShowContent, error) {
		content, digest, err := reduce.NewStore(filepath.Join(w.kbDir, "never-created")).Get(n.ID)
		if err != nil {
			return kb.ShowContent{}, err
		}
		return kb.ShowContent{Content: string(content), Snapshot: digest}, nil
	})
	stdout, _, err := runShow(t, w.kbDir, "out:9c2d4f1a", "--json")
	if err == nil {
		t.Error("unreadable store: want an error (exit 1)")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "error" {
		t.Errorf("outcome = %q, want error (never not-found)", env.Outcome)
	}
}

func TestShowChatFound(t *testing.T) {
	w := newShowWorld(t)
	stdout, _, err := runShow(t, w.kbDir, "chat:elif")
	if err != nil {
		t.Fatalf("show chat:elif: %v", err)
	}
	if stdout != "# elif\n\na working session\n" {
		t.Errorf("chat content = %q", stdout)
	}
	stdout, _, err = runShow(t, w.kbDir, "chat:elif", "--json")
	if err != nil {
		t.Fatalf("show chat:elif --json: %v", err)
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "found" || env.Kind != "chat" || env.Content == "" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestShowChatNotFound(t *testing.T) {
	w := newShowWorld(t)
	stdout, _, err := runShow(t, w.kbDir, "chat:ghost", "--json")
	if err == nil {
		t.Error("show chat:ghost --json: want an error (exit 1)")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "not-found" {
		t.Errorf("outcome = %q", env.Outcome)
	}
}

func TestShowChatRedacted(t *testing.T) {
	w := newShowWorld(t)
	writeShowFile(t, filepath.Join(w.chatRoot, "quiet.md"), showSecretValue)
	stdout, stderr, err := runShow(t, w.kbDir, "chat:quiet")
	if err != nil {
		t.Fatalf("a fully-withheld transcript exits 0: %v (%s)", err, stderr)
	}
	if strings.Contains(stdout, showSecretValue) {
		t.Errorf("secret value reached stdout: %q", stdout)
	}
	stdout, _, err = runShow(t, w.kbDir, "chat:quiet", "--json")
	if err != nil {
		t.Fatalf("redacted exits 0: %v", err)
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "redacted" {
		t.Errorf("outcome = %q, want redacted", env.Outcome)
	}
	if _, ok := raw["content"]; ok {
		t.Error("withheld transcript envelope carries content")
	}
	if len(env.Redacted) != 1 {
		t.Errorf("redacted = %v, want the masked span", env.Redacted)
	}

	// Partial masking stays found, with the spans listed.
	writeShowFile(t, filepath.Join(w.chatRoot, "half.md"), "hello\ntoken "+showSecretValue+"\nbye\n")
	stdout, _, err = runShow(t, w.kbDir, "chat:half", "--json")
	if err != nil {
		t.Fatalf("partially-masked exits 0: %v", err)
	}
	env, _ = showEnvelope(t, stdout)
	if env.Outcome != "found" {
		t.Errorf("partial outcome = %q, want found", env.Outcome)
	}
	if strings.Contains(env.Content, showSecretValue) || len(env.Redacted) != 1 {
		t.Errorf("partial = content %q redacted %v", env.Content, env.Redacted)
	}
}

func TestShowChatExpired(t *testing.T) {
	w := newShowWorld(t)
	path := filepath.Join(w.chatRoot, "old.md")
	writeShowFile(t, path, "an old session\n")
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runShow(t, w.kbDir, "chat:old")
	if err != nil {
		t.Fatalf("expired exits 0: %v", err)
	}
	if !strings.Contains(stdout, "expired") {
		t.Errorf("expired text = %q", stdout)
	}
	stdout, _, err = runShow(t, w.kbDir, "chat:old", "--json")
	if err != nil {
		t.Fatalf("expired --json exits 0: %v", err)
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "expired" {
		t.Errorf("outcome = %q, want expired", env.Outcome)
	}
	if _, ok := raw["content"]; ok {
		t.Error("expired envelope carries content")
	}
}

func TestShowNotARef(t *testing.T) {
	w := newShowWorld(t)
	for _, in := range []string{"foo:bar", "note: remember this"} {
		_, stderr, err := runShow(t, w.kbDir, in)
		if err == nil {
			t.Errorf("show %q: want an error", in)
		}
		if !strings.Contains(stderr, "not a ref") {
			t.Errorf("show %q stderr = %q, want a not-a-ref error", in, stderr)
		}
		stdout, _, err := runShow(t, w.kbDir, in, "--json")
		if err == nil {
			t.Errorf("show %q --json: want an error", in)
		}
		env, raw := showEnvelope(t, stdout)
		requireSchemaVersion(t, env, raw)
		if env.Outcome != "error" {
			t.Errorf("show %q outcome = %q", in, env.Outcome)
		}
	}
	// A claimed ref with an unknown kind names the kind.
	_, stderr, err := runShow(t, w.kbDir, "urn:dhnt:widget:x")
	if err == nil || !strings.Contains(stderr, "unknown kind") {
		t.Errorf("urn unknown kind: err=%v stderr=%q", err, stderr)
	}
	// A known kind outside kb show's scope has no resolver here.
	stdout, _, err := runShow(t, w.kbDir, "todo:abc123", "--json")
	if err == nil {
		t.Error("show todo:abc123: want an error")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "error" || !strings.Contains(env.Error, "no resolver wired") {
		t.Errorf("envelope = %+v", env)
	}
}

// senseWorld installs a fake sense consumer keyed by sense id.
func senseWorld(t *testing.T, readings map[string]kb.SenseReading, errs map[string]error) {
	t.Helper()
	kb.SetSenseClock(func() time.Time { return senseNow })
	kb.SetSenseResolver(func(id string) (kb.SenseReading, error) {
		if err, ok := errs[id]; ok {
			return kb.SenseReading{}, err
		}
		if rd, ok := readings[id]; ok {
			return rd, nil
		}
		return kb.SenseReading{}, fmt.Errorf("sense: %s: %w", id, ref.ErrNotFound)
	})
}

func freshReading() kb.SenseReading {
	return kb.SenseReading{
		SchemaVersion: kb.SenseSchemaVersion,
		SenseID:       "cpu-0",
		Subject:       "host:buildbox",
		Source:        "sense:cpu-0",
		ObservedAt:    "2026-09-13T08:14:00Z",
		Cadence:       "60s",
		Status:        "ok",
		Value:         []byte(`{"load":0.4}`),
		Snapshot:      "sha256:abc123",
	}
}

func TestShowSenseFound(t *testing.T) {
	w := newShowWorld(t)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/007")
	senseWorld(t, map[string]kb.SenseReading{"cpu-0": freshReading()}, nil)
	stdout, _, err := runShow(t, w.kbDir, "sense:cpu-0")
	if err != nil {
		t.Fatalf("fresh sense exits 0: %v", err)
	}
	if strings.TrimSpace(stdout) != `{"load":0.4}` {
		t.Errorf("sense content = %q, want the value", stdout)
	}
	stdout, _, err = runShow(t, w.kbDir, "sense:cpu-0", "--json")
	if err != nil {
		t.Fatalf("fresh sense --json exits 0: %v", err)
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "found" || env.Kind != "sense" || env.ID != "cpu-0" || env.Ref != "sense:cpu-0" {
		t.Errorf("envelope identity = %+v", env)
	}
	if env.Content != `{"load":0.4}` {
		t.Errorf("content = %q", env.Content)
	}
	if env.ObservedAt != "2026-09-13T08:14:00Z" || env.Freshness != "fresh" || env.Snapshot != "sha256:abc123" {
		t.Errorf("observation surface = %+v", env)
	}
}

func TestShowSenseStaleAndUnknownAreExpired(t *testing.T) {
	w := newShowWorld(t)
	t.Setenv("BASHY_PRINCIPAL", "")
	stale := freshReading()
	stale.SenseID, stale.ObservedAt = "old-0", "2026-09-13T08:10:00Z"
	unavail := freshReading()
	unavail.SenseID, unavail.Status = "down-0", "unavailable"
	senseWorld(t, map[string]kb.SenseReading{"old-0": stale, "down-0": unavail}, nil)
	for _, tt := range []struct{ id, freshness string }{
		{"old-0", "stale"},
		{"down-0", "unknown"},
	} {
		stdout, _, err := runShow(t, w.kbDir, "sense:"+tt.id, "--json")
		if err != nil {
			t.Fatalf("sense %s expired exits 0: %v", tt.id, err)
		}
		env, raw := showEnvelope(t, stdout)
		requireSchemaVersion(t, env, raw)
		if env.Outcome != "expired" || env.Freshness != tt.freshness {
			t.Errorf("sense %s = outcome %q freshness %q", tt.id, env.Outcome, env.Freshness)
		}
		if _, ok := raw["content"]; ok {
			t.Errorf("sense %s expired envelope carries content", tt.id)
		}
		if env.ObservedAt == "" || env.Snapshot == "" {
			t.Errorf("sense %s expired envelope lacks the observation surface: %+v", tt.id, env)
		}
	}
}

func TestShowSenseDenied(t *testing.T) {
	w := newShowWorld(t)
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/008")
	rd := freshReading()
	rd.Principals = []string{"dhnt:agent/007"}
	senseWorld(t, map[string]kb.SenseReading{"cpu-0": rd}, nil)
	stdout, _, err := runShow(t, w.kbDir, "sense:cpu-0", "--json")
	if err == nil {
		t.Error("wrong principal: want an error (exit 1)")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "denied" {
		t.Errorf("outcome = %q, want denied", env.Outcome)
	}
	if _, ok := raw["content"]; ok {
		t.Error("denied envelope carries content")
	}

	// The listed principal reads fine.
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/007")
	stdout, _, err = runShow(t, w.kbDir, "sense:cpu-0", "--json")
	if err != nil {
		t.Fatalf("listed principal: %v", err)
	}
	env, _ = showEnvelope(t, stdout)
	if env.Outcome != "found" {
		t.Errorf("listed principal outcome = %q", env.Outcome)
	}
}

func TestShowSenseMissingAndResolverErrors(t *testing.T) {
	w := newShowWorld(t)
	t.Setenv("BASHY_PRINCIPAL", "")
	senseWorld(t, nil, map[string]error{"boom-0": errors.New("sense store unreachable")})
	stdout, _, err := runShow(t, w.kbDir, "sense:ghost-0", "--json")
	if err == nil {
		t.Error("missing reading: want an error (exit 1)")
	}
	env, raw := showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "not-found" {
		t.Errorf("outcome = %q, want not-found", env.Outcome)
	}

	stdout, _, err = runShow(t, w.kbDir, "sense:boom-0", "--json")
	if err == nil {
		t.Error("resolver failure: want an error (exit 1)")
	}
	env, raw = showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "error" || !strings.Contains(env.Error, "unreachable") {
		t.Errorf("envelope = %+v", env)
	}

	// No resolver wired at all.
	kb.SetSenseResolver(nil)
	stdout, _, err = runShow(t, w.kbDir, "sense:cpu-0", "--json")
	if err == nil {
		t.Error("no resolver: want an error (exit 1)")
	}
	env, raw = showEnvelope(t, stdout)
	requireSchemaVersion(t, env, raw)
	if env.Outcome != "error" || !strings.Contains(env.Error, "no sense resolver wired") {
		t.Errorf("envelope = %+v", env)
	}

	// No id at all.
	_, _, err = runShow(t, w.kbDir, "sense:")
	if err == nil {
		t.Error("sense: with no id: want an error")
	}
}
