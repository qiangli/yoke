// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/websession"
)

// TestShapedRestoreOldestRevertsToBase is the S525.A10 regression proof for
// the live br.restored_page_old failure: after alice approves a single
// change, the History UI's last (oldest) Restore button must revert the
// served page to the base release. Restoring the oldest listed version
// through the same endpoint the UI calls must take effect on /notes/ — a
// forward commit, user data untouched — and undo must bring the change back.
func TestShapedRestoreOldestRevertsToBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, v1, _, _, _, _, _, _ := historyFixture(t, home)

	aliceStore, err := shape.OpenAt(home, "notes", "device:alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := aliceStore.WriteData("notes.txt", []byte("alice-private\n")); err != nil {
		t.Fatal(err)
	}

	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "shape-work"), []string{"/bin/sh"}
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	panel := Panel{Name: "notes", Label: "Notes", Path: "/notes/",
		ShapedSource: source, Auth: AuthSystem, Source: "registered", Available: true}
	pairPath := filepath.Join(home, "pairing.json")
	h := newTestHandler(t, Options{
		RequireLogin: true, Sessions: websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Pairing: true, PairStorePath: pairPath, Panels: []Panel{panel}, Shape: opts,
	})
	pairs := newPairStore(pairPath)
	alice := pairedPrincipal(t, h, pairs, "alice")
	const peer = "192.0.2.20:5555"

	get := func(who http.Header, path string, want int) string {
		t.Helper()
		w := do(h, http.MethodGet, path, peer, who)
		if w.Code != want {
			t.Fatalf("GET %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
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
	activeOf := func() string {
		t.Helper()
		st, err := shape.OpenAt(home, "notes", "device:alice")
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		return got.ActiveRevision
	}

	// One approved change, the way the approve handler records it.
	setupGate := shape.GateFunc(func(candidate, base, principal string) (shape.EvidenceRef, error) {
		return shape.EvidenceRef{Verdict: "pass", EvidenceID: "ev-setup"}, nil
	})
	if _, err := src.Activate(home, "notes", v1, "device:alice", src.ActivateOptions{Gate: setupGate}); err != nil {
		t.Fatalf("activate v1: %v", err)
	}
	if body := get(alice, "/notes/", 200); !strings.Contains(body, "<!-- v1 -->") {
		t.Fatalf("served page after approval lacks the change: %.200q", body)
	}

	// The history offers the base release as the oldest restorable version.
	var hv historyTestView
	if err := json.Unmarshal([]byte(get(alice, "/api/shape/notes/history", 200)), &hv); err != nil {
		t.Fatal(err)
	}
	if len(hv.Versions) != 1 || hv.Versions[0].Revision != v1 {
		t.Fatalf("versions = %+v, want the single approval of v1", hv.Versions)
	}
	var oldest string
	if hv.Base != "" {
		oldest = hv.Base
	} else {
		oldest = hv.Versions[len(hv.Versions)-1].Revision
	}

	// Restoring the oldest listed version takes effect on the served page.
	var restored struct {
		Revision string `json:"revision"`
		Active   string `json:"active"`
	}
	if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/history/restore",
		`{"rev":`+jsonQuote(oldest)+`}`, 200)), &restored); err != nil {
		t.Fatal(err)
	}
	if got := activeOf(); got != restored.Revision || got != restored.Active {
		t.Fatalf("after restore active = %s, want revision+active %s", got, restored.Revision)
	}
	if body := get(alice, "/notes/", 200); strings.Contains(body, "<!-- v1 -->") {
		t.Fatalf("served page still has the approved change after restoring oldest: %.200q", body)
	}

	// The base release previews and diffs like any listed version.
	if body := get(alice, "/shape/notes/history/"+oldest+"/", 200); strings.Contains(body, "<!-- v1 -->") {
		t.Fatalf("base preview shows the approved change: %.200q", body)
	}
	if body := get(alice, "/api/shape/notes/history/diff?from="+oldest+"&to="+v1, 200); !strings.Contains(body, "app-v1") {
		t.Fatalf("base..v1 diff lacks the change: %.200q", body)
	}

	// Undo brings the approved change back; user data is untouched throughout.
	var undone struct {
		Revision string `json:"revision"`
		Active   string `json:"active"`
	}
	if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/history/undo", "", 200)), &undone); err != nil {
		t.Fatal(err)
	}
	if undone.Revision != v1 || undone.Active != v1 || activeOf() != v1 {
		t.Fatalf("after undo active = %+v, want v1", undone)
	}
	if body := get(alice, "/notes/", 200); !strings.Contains(body, "<!-- v1 -->") {
		t.Fatalf("served page lacks the change after undo: %.200q", body)
	}
	if b, err := aliceStore.ReadData("notes.txt"); err != nil || string(b) != "alice-private\n" {
		t.Fatalf("user data = %q, want it untouched", b)
	}

	// An unknown version is a visible refusal, never a silent 2xx.
	if body := postJSON(alice, "/api/shape/notes/history/restore", `{"rev":"no-such-version"}`, http.StatusNotFound); strings.TrimSpace(body) == "" {
		t.Fatal("unknown-version restore names no reason")
	}

	// The "base" label restores the same release as its SHA.
	if err := json.Unmarshal([]byte(postJSON(alice, "/api/shape/notes/history/restore",
		`{"rev":"base"}`, 200)), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Revision != oldest || activeOf() != oldest {
		t.Fatalf("restore base = %+v, want the base release %s", restored, oldest)
	}
	if body := get(alice, "/notes/", 200); strings.Contains(body, "<!-- v1 -->") {
		t.Fatalf("served page still has the approved change after restoring base: %.200q", body)
	}
}
