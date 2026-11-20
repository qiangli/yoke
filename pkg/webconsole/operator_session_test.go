// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/shape/contain"
	"github.com/qiangli/yoke/pkg/websession"
)

// TestOperatorSessionCloudCaller is the regression guard for todo
// d871ac00b808. operatorSession decided "this is the host's operator" from
// one fact — the session subject is not a paired device — which was sound
// while the only sessions were the on-host operator and a paired device. Once
// a person the app is merely shared with could reach these routes, that proxy
// admitted them to host-scoped shaping, to linking a story or todo onto a
// wish, and (through registryWriter, which carried the identical predicate)
// to writing the app registry.
//
// The fix judges a cloud-arrived caller by the host's Operators allowlist, and
// reads that allowlist directly rather than asking Identity.IsAdmin(), because
// coopauth falls back to the cloud tier when the allowlist is empty and
// cloudbox stamps every sharee "admin" (coopauth/guard.go).
//
// Both directions are asserted end to end, through the real guard:
// a sharee is refused, and a named operator is not.
func TestOperatorSessionCloudCaller(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := newNotesSource(t)
	notes := Panel{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source,
		Auth: AuthSystem, Source: "registered", Available: true}
	h := newTestHandler(t, Options{
		Ctx: context.Background(), RequireLogin: true,
		Sessions:  websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
		Panels:    []Panel{notes},
		Operators: []string{"Owner@Example.Test"}, // case and space are ignored
		Shape: ShapeOptions{
			Home: home, WorkRoot: filepath.Join(t.TempDir(), "shape-work"),
			Runner: shapedFixtureRunner(t), Level: contain.LevelProcess, Delegate: &fakeShaper{},
		},
	})
	cloud := func(email string) http.Header {
		return http.Header{
			"X-Forwarded-Prefix": {"/cloud/apps"},
			"Remote-User":        {email},
			"Remote-Email":       {email},
			// cloudbox marks every sharee with the admin tier; it must not
			// decide anything here.
			"Remote-Groups": {"admin"},
		}
	}
	const peer = "127.0.0.1:5555"
	wish := func(who http.Header) (int, string) {
		hdr := who.Clone()
		hdr.Set("Content-Type", "application/json")
		w := send(h, http.MethodPost, "/api/shape/notes/candidates", peer, hdr, `{"request":"make the title bigger","story":"abc123"}`)
		return w.Code, w.Body.String()
	}

	// (a) A cloud sharee may not link a story or todo onto a wish.
	if code, body := wish(cloud("sharee@example.test")); code != http.StatusForbidden {
		t.Errorf("a sharee's story-linked wish = %d %s; want 403 — a cloud sharee is not this host's operator", code, strings.TrimSpace(body))
	}

	// (b) A named operator still may, over the same cloud path. Without this
	// the fix would simply have locked the owner out of their own host when
	// they administer it remotely.
	if code, body := wish(cloud("owner@example.test")); code == http.StatusForbidden {
		t.Errorf("the named operator's story-linked wish = %d %s; want it allowed", code, strings.TrimSpace(body))
	}

	// (c) The app registry carries the same predicate, so a sharee cannot
	// write it either. This is the "manage apps" capability.
	hdr := cloud("sharee@example.test")
	hdr.Set("Content-Type", "application/json")
	if w := send(h, http.MethodPost, "/api/apps/registered", peer, hdr, `{"name":"evil","label":"Evil","url":"http://127.0.0.1:1/"}`); w.Code != http.StatusForbidden {
		t.Errorf("a sharee writing the app registry = %d %s; want 403", w.Code, strings.TrimSpace(w.Body.String()))
	}
}

// TestConsoleGuardPolicyPopulated pins the second half of the same cause: the
// console used to build its guard with an empty Policy, and coopauth documents
// what follows — "an EMPTY allowlist falls back to the cloud tier — demo
// convenience only; real apps populate it" — where the cloud tier calls every
// sharee an admin. The policy now carries the host's operators, and says
// explicitly what was previously inferred from the absence of a device
// subject: the on-host caller is the machine's operator.
func TestConsoleGuardPolicyPopulated(t *testing.T) {
	g := consoleGuard(Options{Operators: []string{" Owner@Example.Test "}})
	if !g.Policy.AdminEmails["owner@example.test"] {
		t.Errorf("the operator allowlist = %v; want the host's operator, lowercased and trimmed", g.Policy.AdminEmails)
	}
	if !g.Policy.LoopbackAdmin {
		t.Error("the on-host caller must be admin explicitly, not by the absence of a device subject")
	}
	if n := len(consoleGuard(Options{}).Policy.AdminEmails); n != 0 {
		t.Errorf("a host naming no operators = %d allowlisted; want 0, so no cloud caller is an operator", n)
	}
}
