package webconsole

import (
	"context"
	"encoding/json"
	"github.com/qiangli/yoke/pkg/websession"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	core "github.com/qiangli/yoke/pkg/shape"
)

func TestLocalShapeWishUsesWebIntentAndWaits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_APPS_DIR", t.TempDir())
	source := newNotesSource(t)
	if e := fleet.New(fleet.WithAppValidate(ValidateRegisteredApp)).SaveApp(fleet.App{Name: "notes", Type: fleet.AppTypeShaped, Source: source}); e != nil {
		t.Fatal(e)
	}
	app, ok := fleet.New().App("notes")
	if !ok {
		t.Fatal("missing notes")
	}
	delegate := &intentShaper{}
	caller := core.Caller{Principal: "alice", Actor: "test-actor", RepresentedUser: "alice"}
	got, e := LocalShapeWish(context.Background(), core.App{Name: "notes", SourceDir: app.Source}, caller, core.Options{Home: home, Delegate: delegate}, core.Wish{Request: "make a sign-up sheet"})
	if e != nil {
		t.Fatal(e)
	}
	if got.Status != candNewApp || got.Actor != caller.Actor || got.RepresentedUser != caller.Principal {
		t.Fatalf("local intent metadata %+v", got)
	}
	var next NewApp
	if e = json.Unmarshal(got.NewApp, &next); e != nil {
		t.Fatal(e)
	}
	if next.App != "signup" {
		t.Fatal(next)
	}
	created, ok := fleet.New().App(next.App)
	if !ok {
		t.Fatal("new app not registered")
	}
	a := localShaped(core.App{Name: created.Name, SourceDir: created.Source}, core.Options{Home: home})
	draft, _, e := a.loadDraft(caller.Principal)
	if e != nil {
		t.Fatal(e)
	}
	if draft.busy() || draft.Cursor != 1 || draft.Steps[0].Rev == "" {
		t.Fatalf("local wish returned before new-app generation finished: %+v", draft)
	}
	if _, e = os.Stat(filepath.Join(created.Source, "public", "index.html")); e != nil {
		t.Fatal(e)
	}
}
func TestLocalShapeIncidentExternalAndPrincipalIsolation(t *testing.T) {
	home := t.TempDir()
	app := core.App{Name: "notes", SourceDir: newNotesSource(t)}
	opts := core.Options{Home: home}
	caller := core.Caller{Principal: "alice", Actor: "tester", RepresentedUser: "alice"}
	in, e := LocalShapeReportIncident(context.Background(), app, caller, opts, LocalIncidentReport{Kind: IncidentScript, Path: "/index?secret=omit", Message: "failed token=example-value", Offline: true})
	if e != nil {
		t.Fatal(e)
	}
	if in.State != IncidentExternal || in.Path != "index" {
		t.Fatalf("incident %+v", in)
	}
	got, e := LocalShapeIncidents(context.Background(), app, caller, opts, in.ID)
	if e != nil {
		t.Fatal(e)
	}
	if got.(Incident).ID != in.ID {
		t.Fatal(got)
	}
	caller.Principal = "bob"
	if _, e = LocalShapeIncidents(context.Background(), app, caller, opts, in.ID); !os.IsNotExist(e) {
		t.Fatalf("wrong principal %v", e)
	}
}

func TestShapeTrackingLinksRequireOperatorSession(t *testing.T) {
	home := t.TempDir()
	source := newNotesSource(t)
	pairPath := filepath.Join(home, "pairing.json")
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	var recorded []core.Candidate
	h := newTestHandler(t, Options{Sessions: sessions, Pairing: true, PairStorePath: pairPath, Panels: []Panel{{Name: "notes", Label: "Notes", Path: "/notes/", ShapedSource: source, Auth: AuthSystem, Source: "registered", Available: true}}, Shape: ShapeOptions{Home: home, Track: func(_ context.Context, c *core.Candidate) error { recorded = append(recorded, *c); return nil }}})
	_, secret, e := newPairStore(pairPath).issueTicketForPrincipal([]string{"notes"}, time.Hour, time.Minute, "alice")
	if e != nil {
		t.Fatal(e)
	}
	const peer = "192.0.2.10:5555"
	redeem := do(h, http.MethodGet, pairRedeemPath+"?v="+pairQRVersion+"&t="+url.QueryEscape(secret), peer, nil)
	var paired http.Header
	for _, cookie := range redeem.Result().Cookies() {
		if cookie.Name == sessionCookie && cookie.Value != "" {
			paired = withCookie(cookie)
		}
	}
	if paired == nil {
		t.Fatalf("pairing %d", redeem.Code)
	}
	post := func(from string, headers http.Header, body string) int {
		headers = headers.Clone()
		headers.Set("Content-Type", "application/json")
		return send(h, http.MethodPost, "/api/shape/notes/candidates", from, headers, body).Code
	}
	for _, selector := range []string{`"story":"existing-story"`, `"todo":"existing-todo"`} {
		if code := post(peer, paired, `{"request":"change protected page","protected":true,`+selector+`}`); code != http.StatusForbidden {
			t.Fatalf("paired explicit link status %d", code)
		}
	}
	if len(recorded) != 0 {
		t.Fatal("denied link wrote tracking")
	}
	if code := post(peer, paired, `{"request":"change protected page","protected":true}`); code != http.StatusAccepted {
		t.Fatalf("paired auto tracking %d", code)
	}
	if len(recorded) != 1 || recorded[0].Story != "" || recorded[0].Todo != "" {
		t.Fatal(recorded)
	}
	if code := post(draftPeer, http.Header{}, `{"request":"change protected page","protected":true,"story":"existing-story"}`); code != http.StatusAccepted {
		t.Fatalf("operator explicit link %d", code)
	}
	if len(recorded) != 2 || recorded[1].Story != "existing-story" {
		t.Fatal(recorded)
	}
}
