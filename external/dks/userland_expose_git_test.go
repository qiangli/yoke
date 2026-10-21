package dks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// RED (S487.4, reshaped in S487.2): proxy:git requires a valid --allow of
// owner/repo or owner/* entries, registers git+<upstream> with Outpost
// carrying the list, and applies the same ONE object as an http expose: the
// Secret, recording kind git, the allow list, the git upstream and the app
// route. The relay itself runs in the run pod, in plain http mode.
func TestExposeGitRequiresValidAllowAndPassesItToOutpost(t *testing.T) {
	// Missing --allow, and every malformed entry, refuse the whole expose
	// before anything is registered or applied.
	for _, tc := range []struct {
		name  string
		allow []string
	}{
		{"missing", nil},
		{"empty", []string{""}},
		{"no-slash", []string{"octo"}},
		{"empty-repo", []string{"octo/"}},
		{"empty-owner", []string{"/repo"}},
		{"extra-slash", []string{"octo/app/extra"}},
		{"wildcard-repo-name", []string{"octo/ap*"}},
		{"space", []string{"oct o/app"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			et := newExposeTest(t)
			et.selectWorkspace(t, "skel")
			args := []string{"expose", exposeKindGit, "--as", "gitproxy"}
			if tc.allow != nil {
				args = append(args, "--allow", strings.Join(tc.allow, ","))
			}
			code, _, stderr := et.exec(t, args...)
			if code == 0 {
				t.Fatalf("expose proxy:git --allow %v exited 0", tc.allow)
			}
			for _, want := range []string{"--allow", "nothing ran"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not carry %q", stderr, want)
				}
			}
			if len(et.apps.adds) != 0 {
				t.Errorf("a refused git expose registered an app with Outpost: %+v", et.apps.adds)
			}
			if got := applyCalls(et.runner.calls); len(got) != 0 {
				t.Errorf("a refused git expose applied to the plane: %v", got)
			}
		})
	}

	// A valid list lands on Outpost as git+<upstream> with --allow, and the
	// one plane object records what the run's http relay needs.
	et := newExposeTest(t)
	et.selectWorkspace(t, "skel")
	code, _, stderr := et.exec(t, "expose", exposeKindGit, "--as", "gitproxy",
		"--allow", "octo/app,octo/*")
	if code != 0 {
		t.Fatalf("expose proxy:git exited %d: %s", code, stderr)
	}
	if len(et.apps.adds) != 1 {
		t.Fatalf("outpost apps add called %d times, want 1", len(et.apps.adds))
	}
	spec := et.apps.adds[0]
	if want := exposeGitTargetPrefix + defaultGitUpstream; spec.Target != want {
		t.Errorf("registered target = %q, want %q", spec.Target, want)
	}
	if strings.Join(spec.Allow, ",") != "octo/app,octo/*" {
		t.Errorf("registered allow = %q, want the cleaned list", spec.Allow)
	}
	argv := strings.Join(execOutpostApps{}.addArgv(spec), " ")
	if !strings.Contains(argv, "--allow") || !strings.Contains(argv, "octo/app,octo/*") {
		t.Errorf("outpost argv %q carries no --allow list", argv)
	}
	if strings.Contains(argv, testExposeToken) {
		t.Fatal("outpost argv leaks the token")
	}

	objs := appliedObjects(t, et.runner.calls)
	if len(objs) != 1 {
		t.Fatalf("expose applied %d kinds, want exactly the Secret: %v", len(objs), objs)
	}
	for _, gone := range []string{"Deployment", "Service"} {
		if _, ok := objs[gone]; ok {
			t.Errorf("a git expose still applies a %s", gone)
		}
	}
	annots := exposeAnnotations(t, objs["Secret"])
	for annot, want := range map[string]string{
		exposeKindAnnot:     exposeGitKind,
		exposeAllowAnnot:    "octo/app,octo/*",
		exposeUpstreamAnnot: defaultGitUpstream,
		exposePortAnnot:     "8080",
		exposeAppURLAnnot:   testSatellite + "/matrix/h/" + testOutpostHost + "/app/gitproxy/",
	} {
		if got := annots[annot]; got != want {
			t.Errorf("Secret annotation %s = %q, want %q", annot, got, want)
		}
	}
	// A git expose rides the plain http relay mode: Outpost's git app does
	// the smart-HTTP filtering and injects the HOST's credential.
	gitRec := exposeRecord{Name: "gitproxy", Kind: exposeGitKind}
	if got := gitRec.relayMode(); got != dagrun.RelayModeHTTP {
		t.Errorf("a git expose is served by relay mode %q, want %q", got, dagrun.RelayModeHTTP)
	}
	if raw := string(mustJSON(t, objs["Secret"])); strings.Contains(raw, defaultGitUpstream+"/info/refs") {
		t.Errorf("Secret leaks an upstream credential path")
	}

	// --upstream overrides the default git host on both sides.
	et2 := newExposeTest(t)
	et2.selectWorkspace(t, "skel")
	code, _, stderr = et2.exec(t, "expose", exposeKindGit, "--as", "gitproxy",
		"--allow", "octo/app", "--upstream", "https://git.example.com")
	if code != 0 {
		t.Fatalf("expose proxy:git --upstream exited %d: %s", code, stderr)
	}
	if got := et2.apps.adds[0].Target; got != "git+https://git.example.com" {
		t.Errorf("registered target = %q, want git+https://git.example.com", got)
	}

	// An --upstream that is not an http(s) URL refuses the expose.
	et3 := newExposeTest(t)
	et3.selectWorkspace(t, "skel")
	code, _, stderr = et3.exec(t, "expose", exposeKindGit, "--as", "gitproxy",
		"--allow", "octo/app", "--upstream", "ftp://example.com")
	if code == 0 {
		t.Fatal("expose proxy:git with a non-http upstream exited 0")
	}
	if !strings.Contains(stderr, "nothing ran") {
		t.Errorf("stderr %q does not carry %q", stderr, "nothing ran")
	}
	if len(et3.apps.adds) != 0 {
		t.Errorf("a refused git expose registered an app: %+v", et3.apps.adds)
	}

	// exposed shows KIND git with the allow list.
	et4 := newExposeTest(t)
	et4.selectWorkspace(t, "skel")
	now := et4.renv.now()
	et4.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return []exposeRecord{
			{Name: "gitproxy", Kind: exposeGitKind, Target: "octo/app,octo/*",
				Upstream: defaultGitUpstream, Port: exposeHTTPPort, Expires: now.Add(time.Hour)},
		}, nil
	}
	code, stdout, stderr := et4.exec(t, "exposed")
	if code != 0 {
		t.Fatalf("exposed exited %d: %s", code, stderr)
	}
	for _, want := range []string{"gitproxy", "git", "octo/app,octo/*", "live"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("exposed output %q misses %q", stdout, want)
		}
	}
}

// RED (S487.4, reshaped in S487.2): a live git expose reaches runs as
// GIT_CONFIG_* insteadOf entries plus DKS_<NAME>_URL, pointed at the
// LOOPBACK port the pod's own relay serves — never a credential. Several git
// exposes number their entries, take the http ports in name order, and do
// not collide with the egress proxy env.
func TestRunInjectsGitInsteadOfEnvWithoutCredential(t *testing.T) {
	rt := newRunTest(t)
	rt.selectWorkspace(t, "skel")
	now := rt.renv.now()
	recs := []exposeRecord{
		{Name: "git", Kind: exposeGitKind, Target: "octo/app,octo/*", AppURL: testAppURL("git"),
			Upstream: defaultGitUpstream, Port: exposeHTTPPort, Expires: now.Add(time.Hour)},
		{Name: "mirror", Kind: exposeGitKind, Target: "other/*", AppURL: testAppURL("mirror"),
			Upstream: "https://git.example.com", Port: exposeHTTPPort, Expires: now.Add(time.Hour)},
		{Name: "old", Kind: exposeGitKind, Target: "octo/app", AppURL: testAppURL("old"),
			Upstream: defaultGitUpstream, Port: exposeHTTPPort, Expires: now.Add(-time.Second)},
		{Name: "proxy", Kind: exposeEgressKind, Target: "a.example.com", AppURL: testAppURL("proxy"),
			Port: exposeEgressPort, Expires: now.Add(time.Hour)},
	}
	got := exposeRunEnv(recs, now)
	for k, want := range map[string]string{
		"DKS_GIT_URL":        "http://127.0.0.1:18080",
		"DKS_MIRROR_URL":     "http://127.0.0.1:18081",
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "url.http://127.0.0.1:18080/.insteadOf",
		"GIT_CONFIG_VALUE_0": defaultGitUpstream + "/",
		"GIT_CONFIG_KEY_1":   "url.http://127.0.0.1:18081/.insteadOf",
		"GIT_CONFIG_VALUE_1": "https://git.example.com/",
		// The egress proxy env of a live egress expose is untouched by the
		// git entries, and keeps 3128 of its own.
		"HTTPS_PROXY": "http://127.0.0.1:3128",
		"HTTP_PROXY":  "http://127.0.0.1:3128",
	} {
		if got[k] != want {
			t.Errorf("run env %s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["DKS_OLD_URL"]; ok {
		t.Error("an EXPIRED git expose reached the run's environment")
	}

	// End to end through the manifest: a plain `git clone <upstream>/…` in
	// the pod goes through the relay, and NO value carries the expose token
	// — the credential stays in the expose Secret.
	rt.renv.listExposes = func(context.Context, dagrun.Context) ([]exposeRecord, error) {
		return recs[:3], nil // git, mirror, old
	}
	code, stdout, stderr := rt.exec(t, "run", "--emit", "manifest", "--", "echo", "hi")
	if code != 0 {
		t.Fatalf("run with a live git expose exited %d: %s", code, stderr)
	}
	m, err := dagrun.ParseManifest([]byte(stdout))
	if err != nil {
		t.Fatalf("parse the emitted manifest: %v", err)
	}
	envGot := map[string]string{}
	for _, kv := range m.Task.Env {
		k, v, _ := strings.Cut(kv, "=")
		envGot[k] = v
	}
	for k, want := range map[string]string{
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "url.http://127.0.0.1:18080/.insteadOf",
		"GIT_CONFIG_VALUE_0": defaultGitUpstream + "/",
	} {
		if envGot[k] != want {
			t.Errorf("manifest env %s = %q, want %q", k, envGot[k], want)
		}
	}
	for _, v := range envGot {
		if strings.Contains(v, testExposeToken) {
			t.Fatalf("a run env value carries the expose token: %q", v)
		}
	}
	if strings.Contains(stdout, testExposeToken) {
		t.Fatal("the emitted manifest carries the expose token")
	}
}

// RED (S487.4): the http relay (now the pod's own) forwards the git
// smart-HTTP paths intact with
// both token headers, so the pod's git speaks to the relay and the HOST's
// credential is injected beyond it — never the pod's.
func TestRelayHTTPForwardsGitSmartPath(t *testing.T) {
	var gotPath, gotQuery, gotToken, gotAuth, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("service")
		gotToken = r.Header.Get(exposeTokenHeader)
		gotAuth = r.Header.Get("Authorization")
		gotMethod = r.Method
		fmt.Fprint(w, "001e# service=git-upload-pack\n0000")
	}))
	defer upstream.Close()

	h, err := relayHTTPHandler(upstream.URL, testExposeToken, "")
	if err != nil {
		t.Fatalf("relayHTTPHandler: %v", err)
	}
	front := httptest.NewServer(h)
	defer front.Close()

	// The clone/fetch discovery request: /owner/repo.git/info/refs.
	resp, err := http.Get(front.URL + "/octo/app.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("GET info/refs through the relay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "service=git-upload-pack") {
		t.Fatalf("relay answered %d %q", resp.StatusCode, body)
	}
	if gotPath != "/octo/app.git/info/refs" {
		t.Errorf("upstream path = %q, want /octo/app.git/info/refs", gotPath)
	}
	if gotQuery != "git-upload-pack" {
		t.Errorf("upstream query service = %q, want git-upload-pack", gotQuery)
	}
	if gotToken != testExposeToken {
		t.Errorf("upstream %s = %q, want the expose token", exposeTokenHeader, gotToken)
	}
	if gotAuth != "Bearer "+testExposeToken {
		t.Errorf("upstream Authorization = %q, want a Bearer-shaped expose token", gotAuth)
	}

	// The POST half of the protocol carries the same headers.
	resp2, err := http.Post(front.URL+"/octo/app.git/git-upload-pack",
		"application/x-git-upload-pack-request", strings.NewReader("0000"))
	if err != nil {
		t.Fatalf("POST git-upload-pack through the relay: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("POST git-upload-pack answered %d", resp2.StatusCode)
	}
	if gotMethod != "POST" || gotPath != "/octo/app.git/git-upload-pack" {
		t.Errorf("upstream saw %s %q, want POST /octo/app.git/git-upload-pack", gotMethod, gotPath)
	}
	if gotToken != testExposeToken || gotAuth != "Bearer "+testExposeToken {
		t.Error("the POST lost the token headers")
	}
}
