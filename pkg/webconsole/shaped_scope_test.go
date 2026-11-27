package webconsole

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/coopauth"
	core "github.com/qiangli/yoke/pkg/shape"
	"github.com/qiangli/yoke/pkg/shape/evidence"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
)

type passScopeCheckRunner struct{}

func (passScopeCheckRunner) RunCheck(context.Context, shapeverify.CheckRun) (shapeverify.Outcome, error) {
	return shapeverify.Outcome{Status: evidence.StatusPass}, nil
}

type fixedWebShaper struct {
	calls int
}

func (f *fixedWebShaper) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	f.calls++
	body := "<!doctype html><h1>Shaped by member</h1>\n"
	if err := os.WriteFile(filepath.Join(req.Dir, "public", "index.html"), []byte(body), 0o600); err != nil {
		return DelegateResult{}, err
	}
	return DelegateResult{
		Agent:  "fake:shaper",
		Output: "COVERED:\n- updated heading\nNOT COVERED:\n- (none)\n",
	}, nil
}

func newManagedScopedApp(t *testing.T, home, name, manifest string) (string, string) {
	t.Helper()
	dir := filepath.Join(home, "src", name)
	if err := os.MkdirAll(filepath.Join(dir, "public"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "checks"), 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("<!doctype html><h1>Initial</h1>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contract.bsh"), []byte("@effects(\"read\")\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checks", "smoke.bsh"), []byte("test -f public/index.html\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shaped.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	wt, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "owner", Email: "o@example.test", When: time.Now()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("pristine"), h)); err != nil {
		t.Fatal(err)
	}
	return dir, h.String()
}

func TestLocalShapeWishScopesAndOwnerQueue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	const manifest = `{
		"scopes": {
			"edit": {"circles": ["company"], "groups": ["support"]},
			"shape": {"circles": ["contractor"], "groups": ["support"]}
		}
	}`
	source, _ := newManagedScopedApp(t, home, "site", manifest)
	shaper := &fixedWebShaper{}
	opts := core.Options{
		Home:      home,
		Delegate:  shaper,
		Verifier:  core.ProductionVerifier{Home: home, Runner: passScopeCheckRunner{}},
		Activator: &core.ProductionActivator{Home: home},
	}
	app := core.App{Name: "site", SourceDir: source, Owner: "o@example.test"}

	// Edit-only member A: LocalShapeWish files a request on O's queue rather than running the shaper.
	callerA := core.Caller{Principal: "a@example.test", Actor: "site-agent", RepresentedUser: "a@example.test", Tier: "member", Circle: "company", Groups: []string{"support"}}
	reqA, err := LocalShapeWish(context.Background(), app, callerA, opts, core.Wish{Request: "add FAQ section", Page: "/faq"})
	if err != nil {
		t.Fatalf("LocalShapeWish(A): %v", err)
	}
	if reqA.Status != core.Requested || reqA.Kind != core.KindRequest || reqA.Principal != "a@example.test" || reqA.ActivationPrincipal != "o@example.test" {
		t.Fatalf("A request = %+v", reqA)
	}
	if shaper.calls != 0 {
		t.Fatalf("edit-only wish called delegate %d times", shaper.calls)
	}

	// Shape-scoped member C: LocalShapeWish builds candidate on O's line; O approves; history records C as author.
	callerC := core.Caller{Principal: "c@example.test", Actor: "site-agent", RepresentedUser: "c@example.test", Tier: "member", Circle: "contractor", Groups: []string{"support"}}
	candC, err := LocalShapeWish(context.Background(), app, callerC, opts, core.Wish{Request: "update heading"})
	if err != nil {
		t.Fatalf("LocalShapeWish(C): %v", err)
	}
	if candC.Status != core.Ready || candC.Principal != "c@example.test" || candC.ActivationPrincipal != "o@example.test" {
		t.Fatalf("C candidate = %+v", candC)
	}

	// Account and anonymous callers are refused.
	for _, bad := range []core.Caller{
		{Principal: "visitor:v1", Actor: "site-agent", Tier: "account"},
		{Principal: "anonymous", Actor: "site-agent", Tier: "anonymous"},
	} {
		if _, err := LocalShapeWish(context.Background(), app, bad, opts, core.Wish{Request: "change code"}); !errors.Is(err, core.ErrUnauthorized) {
			t.Fatalf("caller %+v err = %v, want ErrUnauthorized", bad, err)
		}
	}

	svc := core.Service{App: app, Options: opts}
	ownerCaller := core.Caller{Principal: "o@example.test", Actor: "owner", RepresentedUser: "o@example.test", Tier: "owner"}
	statusO, err := svc.Status(context.Background(), ownerCaller)
	if err != nil || len(statusO.Candidates) != 2 {
		t.Fatalf("owner queue = %+v, %v; want A's request + C's candidate", statusO, err)
	}
	if _, err := svc.Approve(context.Background(), ownerCaller, candC.ID); err != nil {
		t.Fatalf("owner approve C: %v", err)
	}
}

func signedMemberHeaders(secret, prefix, kind, circle, groups, user string) http.Header {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	h.Set(coopauth.HdrForwardedPrefix, prefix)
	h.Set(coopauth.HdrRemoteUser, user)
	h.Set(coopauth.HdrRemoteEmail, user)
	h.Set(coopauth.HdrIdentityTs, ts)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(user + "\n\n" + prefix + "\n" + ts))
	h.Set(coopauth.HdrIdentitySig, hex.EncodeToString(mac.Sum(nil)))

	h.Set(coopauth.HdrRelationship, kind)
	if circle != "" {
		h.Set(coopauth.HdrCircle, circle)
	}
	if groups != "" {
		h.Set(coopauth.HdrGroups, groups)
	}
	rmac := hmac.New(sha256.New, []byte(secret))
	rmac.Write([]byte("v2\n" + kind + "\n" + circle + "\n" + groups + "\n\n" + user + "\n" + prefix + "\n" + ts))
	h.Set(coopauth.HdrRelationshipSig, hex.EncodeToString(rmac.Sum(nil)))
	return h
}

func TestWebconsoleOutOfScopeWishQueuesOnOwnerAndOwnerApprovesMemberCandidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	const secret = "test-sso-secret-key"
	const prefix = "/matrix/h/host/app/apps"
	if err := os.MkdirAll(filepath.Join(xdg, "matrix"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfgBytes, _ := json.Marshal(map[string]any{
		"oauth2_email": "o@example.test",
		"service_sso":  map[string]string{"apps": secret},
	})
	if err := os.WriteFile(filepath.Join(xdg, "matrix", "agent.json"), cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	const manifest = `{
		"scopes": {
			"edit": {"circles": ["company"], "groups": ["support"]},
			"shape": {"circles": ["contractor"], "groups": ["support"]}
		}
	}`
	source, _ := newManagedScopedApp(t, home, "site", manifest)
	shaper := &fixedWebShaper{}
	h := newTestHandler(t, Options{
		Operators: []string{"o@example.test"},
		Panels: []Panel{{
			Name: "site", Label: "Site", Path: "/site/", ShapedSource: source,
			Auth: AuthSystem, Source: "registered", Available: true,
		}},
		Shape: ShapeOptions{
			Home:      home,
			Owner:     "cloud:o@example.test",
			Delegate:  shaper,
			Verifier:  productionVerifier{home: home, runner: passScopeCheckRunner{}},
			Activator: &productionActivator{home: home},
		},
	})

	hdrO := signedMemberHeaders(secret, prefix, "owner", "", "", "o@example.test")
	hdrA := signedMemberHeaders(secret, prefix, "member", "company", "support", "a@example.test")
	hdrC := signedMemberHeaders(secret, prefix, "member", "contractor", "support", "c@example.test")

	postJSON := func(hdr http.Header, path, body string) *http.Response {
		hdr = hdr.Clone()
		hdr.Set("Content-Type", "application/json")
		return send(h, http.MethodPost, path, "127.0.0.1:4100", hdr, body).Result()
	}

	// A (edit only) posts a wish -> 202 Accepted, filed as a request on O's queue.
	respA := postJSON(hdrA, "/api/shape/site/candidates", `{"request":"wish beyond edit scope"}`)
	if respA.StatusCode != http.StatusAccepted {
		t.Fatalf("A wish status = %d, want 202", respA.StatusCode)
	}
	var filedA Candidate
	_ = json.NewDecoder(respA.Body).Decode(&filedA)
	if filedA.Status != candRequested || filedA.Kind != candKindRequest || filedA.Principal != "cloud:a@example.test" || filedA.ActivationPrincipal != "cloud:o@example.test" {
		t.Fatalf("A filed request = %+v", filedA)
	}

	// C (shape scope) posts a wish -> builds candidate on O's line.
	respC := postJSON(hdrC, "/api/shape/site/candidates", `{"request":"member code change"}`)
	if respC.StatusCode != http.StatusAccepted {
		t.Fatalf("C wish status = %d, want 202", respC.StatusCode)
	}
	var candC Candidate
	_ = json.NewDecoder(respC.Body).Decode(&candC)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		poll := send(h, http.MethodGet, "/api/shape/site/candidates/"+candC.ID, "127.0.0.1:4100", hdrC, "")
		if poll.Code == http.StatusOK {
			var cur Candidate
			_ = json.Unmarshal(poll.Body.Bytes(), &cur)
			if cur.Status == candReady {
				candC = cur
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if candC.Status != candReady {
		t.Fatalf("C candidate did not become ready: %+v", candC)
	}

	// C cannot approve; O sees both A's request and C's candidate on GET /api/shape/site and approves C's candidate.
	if r := postJSON(hdrC, "/api/shape/site/candidates/"+candC.ID+"/approve", `{}`); r.StatusCode != http.StatusForbidden {
		t.Fatalf("C approve status = %d, want 403", r.StatusCode)
	}
	stateW := send(h, http.MethodGet, "/api/shape/site", "127.0.0.1:4100", hdrO, "")
	if stateW.Code != http.StatusOK {
		t.Fatalf("O state = %d: %s", stateW.Code, stateW.Body.String())
	}
	var stateBody struct {
		Candidates []Candidate `json:"candidates"`
	}
	_ = json.Unmarshal(stateW.Body.Bytes(), &stateBody)
	if len(stateBody.Candidates) != 2 {
		t.Fatalf("O state candidates = %+v, want 2", stateBody.Candidates)
	}

	approveW := postJSON(hdrO, "/api/shape/site/candidates/"+candC.ID+"/approve", `{}`)
	if approveW.StatusCode != http.StatusOK {
		t.Fatalf("O approve = %d", approveW.StatusCode)
	}

	histW := send(h, http.MethodGet, "/api/shape/site/history", "127.0.0.1:4100", hdrO, "")
	if histW.Code != http.StatusOK {
		t.Fatalf("O history = %d: %s", histW.Code, histW.Body.String())
	}
	var hist historyView
	_ = json.Unmarshal(histW.Body.Bytes(), &hist)
	if len(hist.Versions) != 1 || hist.Versions[0].Author != "cloud:c@example.test" || !strings.Contains(hist.Versions[0].Summary, "member code change") {
		t.Fatalf("O history view = %+v", hist)
	}

	incident := postJSON(hdrO, "/api/shape/site/incidents", `{"kind":"script","message":"offline","offline":true}`)
	var created struct {
		ID string `json:"incident"`
	}
	if incident.StatusCode != http.StatusCreated || json.NewDecoder(incident.Body).Decode(&created) != nil || created.ID == "" {
		t.Fatalf("owner incident status %d, id %q", incident.StatusCode, created.ID)
	}
	rev := hist.Versions[0].Revision
	signedIn := signedMemberHeaders(secret, prefix, "", "", "", "d@example.test")
	signedIn.Del(coopauth.HdrRelationshipSig)
	for _, tc := range []struct {
		name    string
		headers http.Header
		want    int
	}{
		{"signed-in", signedIn, http.StatusForbidden},
		{"account", signedMemberHeaders(secret, prefix, "account", "", "", "e@example.test"), http.StatusForbidden},
		{"outside-scope", signedMemberHeaders(secret, prefix, "member", "guest", "", "b@example.test"), http.StatusForbidden},
		{"edit", hdrA, http.StatusOK},
		{"shape", hdrC, http.StatusOK},
		{"owner", hdrO, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{
				"/api/shape/site/history",
				"/api/shape/site/history/diff?from=base&to=" + rev,
				"/api/shape/site/incidents/" + created.ID,
				"/api/shape/site/ledger",
			} {
				resp := send(h, http.MethodGet, path, "127.0.0.1:4100", tc.headers, "")
				if resp.Code != tc.want {
					t.Errorf("GET %s = %d, want %d", path, resp.Code, tc.want)
				}
			}
		})
	}
	// The ledger: the owner reads all of it; a scoped member reads the same
	// attempts with their own address only and an alias for each problem.
	ownerLedger := send(h, http.MethodGet, "/api/shape/site/ledger?max=-1", "127.0.0.1:4100", hdrO, "").Body.String()
	memberLedger := send(h, http.MethodGet, "/api/shape/site/ledger?max=-1", "127.0.0.1:4100", hdrC, "").Body.String()
	for _, want := range []string{`"key":"cand:` + candC.ID, "cloud:o@example.test", "cloud:c@example.test", "member code change", `"standing":"active"`, `"rev":"` + rev} {
		if !strings.Contains(ownerLedger, want) {
			t.Errorf("owner ledger lacks %q: %s", want, ownerLedger)
		}
	}
	for _, want := range []string{`"key":"p-`, "cloud:c@example.test", "member code change", `"standing":"active"`, `"rev":"` + rev} {
		if !strings.Contains(memberLedger, want) {
			t.Errorf("member ledger lacks %q: %s", want, memberLedger)
		}
	}
	for _, leak := range []string{"cloud:o@example.test", "cloud:a@example.test", `"key":"cand:`} {
		if strings.Contains(memberLedger, leak) {
			t.Errorf("member ledger leaks %q: %s", leak, memberLedger)
		}
	}

	for _, action := range []string{"restore", "undo", "name"} {
		resp := postJSON(hdrC, "/api/shape/site/history/"+action, `{}`)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("member %s = %d, want 403", action, resp.StatusCode)
		}
	}
}
