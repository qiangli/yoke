// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/websession"
)

// isolateNeedsStore gives a test its own ask store and room timeline, and
// makes the host owner the human the requests are for.
func isolateNeedsStore(t *testing.T) {
	t.Helper()
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", currentOSUser())
	t.Setenv("BASHY_AGENT", "mgr-741")
}

func raiseNeed(t *testing.T, spec ask.NeedSpec) ask.Request {
	t.Helper()
	r, err := ask.RaiseNeed(spec)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func postAnswer(h http.Handler, id, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/sprint/needs/"+id, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBoardOverviewCarriesNeedsYou(t *testing.T) {
	isolateNeedsStore(t)
	open := raiseNeed(t, ask.NeedSpec{Kind: ask.NeedDecision, Title: "Ship the check-in hotfix?",
		Options: []ask.ApprovalOption{{ID: "ship", Name: "Ship now"}, {ID: "hold"}}, Recommend: "ship",
		Risk: []string{"deploy"}, Sprint: "741"})
	h, s := newBoardTestServer(t)
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = fakeBoard(t), time.Now()
	s.boards.mu.Unlock()

	d := getJSON(t, h, "/api/sprint")
	ns, _ := d["needs_you"].([]any)
	if len(ns) != 1 {
		t.Fatalf("needs_you = %v, want the one open request", d["needs_you"])
	}
	n := ns[0].(map[string]any)
	if n["id"] != open.ID || n["title"] != "Ship the check-in hotfix?" || n["recommend"] != "ship" ||
		n["state"] != "open" || !strings.Contains(n["answer"].(string), "approve allow") {
		t.Fatalf("need view: %v", n)
	}
	if d["can_answer"] != false {
		t.Fatal("an unauthenticated loopback viewer must not be offered answers")
	}

	// Loopback without a sign-in may read the alert but not answer it.
	if w := postAnswer(h, open.ID, `{"verdict":"allow","option":"ship"}`, nil); w.Code != http.StatusForbidden {
		t.Fatalf("loopback answer = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if _, ok := ask.LoadDecision(open.ID); ok {
		t.Fatal("a refused answer must not decide the request")
	}
}

func TestBoardAnswerSignedInOwner(t *testing.T) {
	isolateNeedsStore(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	s, h, closer, err := newHandler(Options{Ctx: context.Background(), RequireLogin: true, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer() })
	s.boards.mu.Lock()
	s.boards.board, s.boards.at = fakeBoard(t), time.Now()
	s.boards.mu.Unlock()
	val, err := sessions.Mint(currentOSUser())
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookie, Value: val}

	r := raiseNeed(t, ask.NeedSpec{Kind: ask.NeedDecision, Title: "Review the lifted specs",
		Options: []ask.ApprovalOption{{ID: "accept"}, {ID: "rework"}}, Sprint: "741"})

	if w := postAnswer(h, r.ID, `{"verdict":"allow","option":"nope"}`, cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown option = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if w := postAnswer(h, r.ID, `{"verdict":"maybe"}`, cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("bad verdict = %d, want 400", w.Code)
	}
	w := postAnswer(h, r.ID, `{"verdict":"allow","option":"rework","reason":"needs examples","by":"someone-else"}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("signed-in answer = %d (%s)", w.Code, w.Body.String())
	}
	d, ok := ask.LoadDecision(r.ID)
	if !ok || d.Via != ask.ViaBoard || d.OptionID != "rework" || d.Reason != "needs examples" ||
		!strings.EqualFold(d.By, currentOSUser()) {
		t.Fatalf("board decision: %+v", d)
	}
	if w := postAnswer(h, r.ID, `{"verdict":"deny"}`, cookie); w.Code != http.StatusConflict {
		t.Fatalf("second answer = %d, want 409", w.Code)
	}
	if w := postAnswer(h, "deadbeef", `{"verdict":"deny"}`, cookie); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", w.Code)
	}

	// The answered request leaves the alert at the next poll.
	req := httptest.NewRequest(http.MethodGet, "/api/sprint", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("overview: %v (%s)", err, rec.Body.String())
	}
	if ns, _ := view["needs_you"].([]any); len(ns) != 0 || view["can_answer"] != true {
		t.Fatalf("after answer: needs_you=%v can_answer=%v", view["needs_you"], view["can_answer"])
	}
}

func TestBoardAnswerRefusesUnsignedCloudCaller(t *testing.T) {
	isolateNeedsStore(t)
	r := raiseNeed(t, ask.NeedSpec{Kind: ask.NeedApproval, Title: "Push the security hotfix", Risk: []string{"security"}})
	h, _ := newBoardTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/sprint/needs/"+r.ID, strings.NewReader(`{"verdict":"allow"}`))
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-Prefix", "/cloud/apps")
	req.Header.Set("Remote-User", "member@example.test")
	req.Header.Set("Remote-Email", "member@example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("a cloud caller without a signed owner relationship answered: %s", w.Body.String())
	}
	if _, ok := ask.LoadDecision(r.ID); ok {
		t.Fatal("refused cloud answer decided the request")
	}
}

// A paired device holds a session but is not the owner's sign-in: it may read
// the alert, never answer (review finding, Sprint 745).
func TestBoardAnswerRefusesPairedDevice(t *testing.T) {
	isolateNeedsStore(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	sessions := websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt"))
	_, h, closer, err := newHandler(Options{Ctx: context.Background(), RequireLogin: true, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closer() })
	val, err := sessions.Mint(deviceSubject(currentOSUser(), "phone1"))
	if err != nil {
		t.Fatal(err)
	}
	r := raiseNeed(t, ask.NeedSpec{Kind: ask.NeedApproval, Title: "Push it?"})
	w := postAnswer(h, r.ID, `{"verdict":"allow"}`, &http.Cookie{Name: sessionCookie, Value: val})
	if w.Code == http.StatusOK {
		t.Fatalf("a paired device answered as the owner: %s", w.Body.String())
	}
	if _, ok := ask.LoadDecision(r.ID); ok {
		t.Fatal("device answer decided the request")
	}
}
