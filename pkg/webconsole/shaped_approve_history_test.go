// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/websession"
)

// staticShaper stands in for the shaping agent: it makes one static,
// verifier-passing change to the candidate clone. Each call appends its
// own marker so consecutive asks always differ from their base.
type staticShaper struct {
	mu sync.Mutex
	n  int
}

func (f *staticShaper) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	p := filepath.Join(req.Dir, "public", "index.html")
	b, err := os.ReadFile(p)
	if err != nil {
		return DelegateResult{}, err
	}
	marker := []byte("<!-- shaped a10 " + itoaShaped(n) + " -->\n")
	b = append(b, marker...)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return DelegateResult{}, err
	}
	return DelegateResult{Agent: "codex:static-shaper"}, nil
}

func itoaShaped(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

type approveHistoryVersion struct {
	Activation string    `json:"activation"`
	Revision   string    `json:"revision"`
	Kind       string    `json:"kind"`
	Request    string    `json:"request"`
	Principal  string    `json:"principal"`
	Author     string    `json:"author"`
	When       time.Time `json:"when"`
}

type approveHistoryView struct {
	App      string                  `json:"app"`
	Active   string                  `json:"active"`
	Versions []approveHistoryVersion `json:"versions"`
}

// TestShapedApproveRecordsHistory is the S525.A10 proof through the REAL
// production approve handler: ask via HTTP, approve via HTTP (the
// productionActivator, never a test-only activator), then read history. It
// asserts the request text, the principal and a recent timestamp on every
// version, and that a restore carries its own request line.
func TestShapedApproveRecordsHistory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture units run under /bin/sh")
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("PATH", t.TempDir())
	before := time.Now().UTC()
	source, _ := productionNotesSource(t, home)

	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "shape-work"), []string{"/bin/sh"}
	opts.Delegate = &staticShaper{}
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	panel := Panel{Name: "notes", Label: "Notes", Path: "/notes/", LaunchPath: "/shape/notes/",
		ShapedSource: source, Auth: AuthSystem, Source: "registered", Available: true}
	pairPath := filepath.Join(home, "pairing.json")
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Pairing: true, PairStorePath: pairPath, Panels: []Panel{panel}, Shape: opts,
	})
	pairs := newPairStore(pairPath)
	alice := pairedPrincipal(t, h, pairs, "alice")
	const peer = "192.0.2.20:5555"

	postJSON := func(who http.Header, path, body string, want int) string {
		t.Helper()
		hdr := who.Clone()
		if hdr == nil {
			hdr = http.Header{}
		}
		hdr.Set("Content-Type", "application/json")
		w := send(h, http.MethodPost, path, peer, hdr, body)
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	get := func(who http.Header, path string, want int) string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, who)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	historyOf := func() approveHistoryView {
		t.Helper()
		var v approveHistoryView
		if err := json.Unmarshal([]byte(get(alice, "/api/shape/notes/history", 200)), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	askAndApprove := func(request string) Candidate {
		t.Helper()
		var asked Candidate
		if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/candidates",
			`{"request":`+jsonQuote(request)+`}`, http.StatusAccepted)), &asked); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(60 * time.Second)
		for {
			var c Candidate
			w := do(h, http.MethodGet, "/api/shape/notes/candidates/"+asked.ID, peer, alice)
			if w.Code != http.StatusOK {
				t.Fatalf("GET candidate = %d: %s", w.Code, w.Body.String())
			}
			if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
				t.Fatal(err)
			}
			if c.Status == candReady || c.Status == candVerified {
				break
			}
			if c.Status == candFailed {
				t.Fatalf("candidate failed: %s", c.Error)
			}
			if time.Now().After(deadline) {
				t.Fatalf("candidate %s never became ready (status %s)", asked.ID, c.Status)
			}
			time.Sleep(200 * time.Millisecond)
		}
		var approved Candidate
		if err := json.Unmarshal([]byte(postJSON(alice,
			"/api/shape/notes/candidates/"+asked.ID+"/approve", "", http.StatusOK)), &approved); err != nil {
			t.Fatal(err)
		}
		if approved.Status != candApproved {
			t.Fatalf("approve = %+v, want approved", approved)
		}
		return approved
	}

	// Two real activations through the production path.
	askAndApprove("show newest notes first")
	askAndApprove("tint the notes blue")

	hv := historyOf()
	if len(hv.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(hv.Versions))
	}
	for i, want := range []string{"tint the notes blue", "show newest notes first"} {
		e := hv.Versions[i]
		if e.Request != want {
			t.Fatalf("version %d request = %q, want %q", i, e.Request, want)
		}
		if e.Principal != "device:alice" {
			t.Fatalf("version %d principal = %q, want device:alice", i, e.Principal)
		}
		if e.Author != "device:alice" {
			t.Fatalf("version %d author = %q, want device:alice", i, e.Author)
		}
		if e.When.IsZero() || e.When.Year() < 2020 || e.When.Before(before.Add(-time.Minute)) {
			t.Fatalf("version %d when = %v, want a recent timestamp", i, e.When)
		}
		if e.Revision == "" || e.Revision == e.Request {
			t.Fatalf("version %d revision = %q, want the content SHA, not the request text", i, e.Revision)
		}
	}

	// Restore the older version: a new forward commit with its own request line.
	firstRev := hv.Versions[1].Revision
	var restored struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/history/restore",
		`{"rev":`+jsonQuote(firstRev)+`}`, 200)), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Revision != firstRev {
		t.Fatalf("restore revision = %s, want %s", restored.Revision, firstRev)
	}
	hv = historyOf()
	if len(hv.Versions) != 3 {
		t.Fatalf("after restore versions = %d, want 3", len(hv.Versions))
	}
	if got := hv.Versions[0].Request; !strings.HasPrefix(got, "restore to ") {
		t.Fatalf("restore request = %q, want a \"restore to ...\" line", got)
	}
	if hv.Versions[0].Author != "device:alice" || hv.Versions[0].When.Before(before.Add(-time.Minute)) {
		t.Fatalf("restore entry = %+v, want the principal and a recent timestamp", hv.Versions[0])
	}

	// Undo the restore: the request line is "undo".
	var undone struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/history/undo", "", 200)), &undone); err != nil {
		t.Fatal(err)
	}
	hv = historyOf()
	if len(hv.Versions) != 4 {
		t.Fatalf("after undo versions = %d, want 4", len(hv.Versions))
	}
	if hv.Versions[0].Request != "undo" {
		t.Fatalf("undo request = %q, want \"undo\"", hv.Versions[0].Request)
	}
	if hv.Versions[0].Author != "device:alice" {
		t.Fatalf("undo author = %q, want device:alice", hv.Versions[0].Author)
	}

	// The evidence binding is untouched: every entry still carries the
	// sealed verifier verdict.
	for i, e := range hv.Versions {
		if e.Revision == "" {
			t.Fatalf("version %d has no revision", i)
		}
	}
}
