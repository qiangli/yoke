package chat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/secrets"
)

const testSecretName = "TEST_TOKEN"
const testSecretValue = "s3cr3t-value-long-enough"

// transcriptStore points the sessions root at a tempdir and returns a
// registry resolving over it. Hermetic: nothing touches ~/.bashy.
func transcriptStore(t *testing.T) (string, *ref.Registry) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("BASHY_CHAT_RETENTION", "")
	g := ref.NewRegistry()
	RegisterRefs(g, root)
	return root, g
}

// writeTranscript lands body where sessionLog writes the live capture:
// <root>/logs/<shortHash(id)>.log.
func writeTranscript(t *testing.T, root, id, body string) string {
	t.Helper()
	path := filepath.Join(root, "logs", shortHash(id)+".log")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testRedactor(t *testing.T) *secrets.Redactor {
	t.Helper()
	r := secrets.NewRedactor()
	if err := r.Register(testSecretName, testSecretValue); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRefChatFound(t *testing.T) {
	root, g := transcriptStore(t)
	writeTranscript(t, root, "elif", "# elif\n\na working session\n")
	n, err := g.Resolve("chat:elif")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if n.Kind != ref.Chat || n.Ref != "chat:elif" || n.ID != "elif" {
		t.Errorf("identity = %+v", n)
	}
	if n.Status != "transcript" || !strings.HasSuffix(n.Where, filepath.Join("logs", shortHash("elif")+".log")) {
		t.Errorf("node = %+v, want status transcript and the session capture path", n)
	}
	if n.Open != "bashy kb show chat:elif" {
		t.Errorf("Open = %q", n.Open)
	}
	tr, err := ReadTranscript(root, "elif", testRedactor(t))
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if tr.Content != "# elif\n\na working session\n" || len(tr.Redacted) != 0 || tr.Withheld {
		t.Errorf("transcript = %+v", tr)
	}
}

func TestRefChatNotFound(t *testing.T) {
	root, g := transcriptStore(t)
	if err := os.MkdirAll(filepath.Join(root, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Resolve("chat:ghost"); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("absent session err = %v, want ErrNotFound", err)
	}
	if _, err := ReadTranscript(root, "ghost", nil); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("absent read err = %v, want ErrNotFound", err)
	}
	// A read over a store that was never created still reads not-found and
	// creates nothing.
	missing := filepath.Join(t.TempDir(), "no-such-sessions")
	gMissing := ref.NewRegistry()
	RegisterRefs(gMissing, missing)
	if _, err := gMissing.Resolve("chat:ghost"); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("missing-store resolve err = %v, want ErrNotFound", err)
	}
	if _, err := ReadTranscript(missing, "ghost", nil); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("missing-store read err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("read created %s, want no directories on read", missing)
	}
	// An id that cannot name a file in the store is absent, never an escape.
	for _, id := range []string{"../escape", "a/b", ""} {
		if _, err := g.Resolve("chat:" + id); !errors.Is(err, ref.ErrNotFound) && id != "" {
			t.Errorf("Resolve(chat:%s) err = %v, want ErrNotFound", id, err)
		}
		if _, err := ReadTranscript(root, id, nil); !errors.Is(err, ref.ErrNotFound) {
			t.Errorf("ReadTranscript(%q) err = %v, want ErrNotFound", id, err)
		}
	}
}

func TestRefChatDefaultRootCreatesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("BASHY_CHAT_RETENTION", "")
	// The "" root resolves through SessionsRoot (a fake ~/.bashy here) and
	// still reads not-found without creating anything.
	if _, err := ReadTranscript("", "ghost", nil); !errors.Is(err, ref.ErrNotFound) {
		t.Errorf("default-root read err = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".bashy")); !os.IsNotExist(err) {
		t.Errorf("default-root read created ~/.bashy, want no directories on read")
	}
}

func TestRefChatExpired(t *testing.T) {
	root, g := transcriptStore(t)
	path := writeTranscript(t, root, "old", "an old session\n")
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	// The ref still resolves: the node names the record it expired from.
	if _, err := g.Resolve("chat:old"); err != nil {
		t.Fatalf("past-retention ref must still resolve: %v", err)
	}
	if _, err := ReadTranscript(root, "old", nil); !errors.Is(err, ErrTranscriptExpired) {
		t.Errorf("past-retention read err = %v, want ErrTranscriptExpired (never not-found)", err)
	}
}

func TestRefChatRedacted(t *testing.T) {
	root, _ := transcriptStore(t)
	body := "line one\nthe token is " + testSecretValue + " here\nanother " + testSecretValue + " there\nline four\nlast " + testSecretValue + "\n"
	writeTranscript(t, root, "elif", body)
	tr, err := ReadTranscript(root, "elif", testRedactor(t))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Withheld {
		t.Fatalf("a partially-masked transcript is not withheld: %+v", tr)
	}
	if strings.Contains(tr.Content, testSecretValue) {
		t.Errorf("secret value survived the gate: %q", tr.Content)
	}
	// Adjacent masked lines merge; the lone masked line stands alone.
	want := []string{"secret span: lines 2-3", "secret span: line 5"}
	if strings.Join(tr.Redacted, ";") != strings.Join(want, ";") {
		t.Errorf("redacted = %v, want %v", tr.Redacted, want)
	}
}

func TestRefChatFullyWithheld(t *testing.T) {
	root, _ := transcriptStore(t)
	writeTranscript(t, root, "quiet", testSecretValue)
	tr, err := ReadTranscript(root, "quiet", testRedactor(t))
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Withheld {
		t.Fatalf("an all-secret transcript must be withheld: %+v", tr)
	}
	if tr.Content != "" {
		t.Errorf("withheld content = %q, want none", tr.Content)
	}
	if len(tr.Redacted) != 1 {
		t.Errorf("redacted = %v, want the one masked span", tr.Redacted)
	}
}

func TestRefChatNilRedactorIsNoMask(t *testing.T) {
	root, _ := transcriptStore(t)
	body := "the token is " + testSecretValue + "\n"
	writeTranscript(t, root, "elif", body)
	tr, err := ReadTranscript(root, "elif", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Content != body || len(tr.Redacted) != 0 {
		t.Errorf("nil redactor = %+v, want the bytes unmasked", tr)
	}
}
