package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/broker/door"
)

func errMessage(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// Management (?scope=principal) crosses sessions; serving does not, and the
// refusals name the owning session and how to bind to it.
func TestStickyCrossSessionManagement(t *testing.T) {
	h := newHarness(t, nil)
	a, b := NewRootSession(), NewRootSession()
	if resp, out := h.do("POST", "/v1/sticky", StickySpec{Key: "shared-key", Model: "llama3.2:3b"}, map[string]string{SessionHeader: a}); resp.StatusCode != 200 {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}

	// Session-scoped views still hide it (isolation unchanged).
	if resp, _ := h.do("GET", "/v1/sticky/shared-key", nil, map[string]string{SessionHeader: b}); resp.StatusCode != 404 {
		t.Fatalf("session-scoped show from another session: %d", resp.StatusCode)
	}
	if _, out := h.do("GET", "/v1/sticky", nil, map[string]string{SessionHeader: b}); len(out["bindings"].([]any)) != 0 {
		t.Fatalf("session-scoped ls from another session: %v", out)
	}

	// ls / show across sessions mark the owner and whether it is directly usable.
	_, out := h.do("GET", "/v1/sticky?scope=principal", nil, map[string]string{SessionHeader: b})
	list := out["bindings"].([]any)
	if len(list) != 1 {
		t.Fatalf("principal ls: %v", out)
	}
	row := list[0].(map[string]any)
	if row["session"] != a || row["visible"] != false {
		t.Fatalf("row = %v, want session %s, visible false", row, a)
	}
	resp, out := h.do("GET", "/v1/sticky/shared-key?scope=principal", nil, map[string]string{SessionHeader: b})
	if resp.StatusCode != 200 || out["session"] != a {
		t.Fatalf("principal show: %d %v", resp.StatusCode, out)
	}
	_, out = h.do("GET", "/v1/sticky/shared-key?scope=principal", nil, map[string]string{SessionHeader: a})
	if out["visible"] != true {
		t.Fatalf("owner should see visible=true: %v", out)
	}

	// Re-creating from another session names the owner and the way to bind.
	resp, out = h.do("POST", "/v1/sticky", StickySpec{Key: "shared-key", Model: "llama3.2:3b"}, map[string]string{SessionHeader: b})
	msg := errMessage(out)
	if resp.StatusCode != 409 || !strings.Contains(msg, a) || !strings.Contains(msg, "llm env --sticky shared-key") {
		t.Fatalf("owned-by-another-session error: %d %q", resp.StatusCode, msg)
	}
	// Serving without the owner's session says the same instead of "no binding".
	resp, out = h.do("POST", "/sticky/shared-key/v1/chat/completions", chat("x"), map[string]string{SessionHeader: b})
	if msg = errMessage(out); resp.StatusCode != 404 || !strings.Contains(msg, a) {
		t.Fatalf("serve from another session: %d %q", resp.StatusCode, msg)
	}
	// The URL naming the owner's session (what env --sticky prints) serves.
	if resp, _ = h.do("POST", "/s/"+a+"/sticky/shared-key/v1/chat/completions", chat("x"), map[string]string{SessionHeader: b}); resp.StatusCode != 200 {
		t.Fatalf("serve via owner session URL: %d", resp.StatusCode)
	}

	// rm: session-scoped is refused, principal scope removes it from any session.
	if resp, _ = h.do("DELETE", "/v1/sticky/shared-key", nil, map[string]string{SessionHeader: b}); resp.StatusCode != 404 {
		t.Fatalf("session-scoped rm from another session: %d", resp.StatusCode)
	}
	if resp, _ = h.do("DELETE", "/v1/sticky/shared-key?scope=principal", nil, map[string]string{SessionHeader: b}); resp.StatusCode != 200 {
		t.Fatalf("principal rm: %d", resp.StatusCode)
	}
	if resp, _ = h.do("GET", "/v1/sticky/shared-key?scope=principal", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("still there after rm: %d", resp.StatusCode)
	}
}

func TestStickyStoreCrossSession(t *testing.T) {
	st := newStickyStore("", time.Now)
	mk := func(session, key string) *Binding {
		return &Binding{scoped: scoped{Principal: "alice", Session: session, Created: time.Now()}, Spec: StickySpec{Key: key}, Digest: "abcdef1234567890"}
	}
	if _, err := st.put(mk("s-one", "k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.put(mk("s-two", "k2")); err != nil {
		t.Fatal(err)
	}
	if got := st.list("alice", "s-one"); len(got) != 1 || got[0].Spec.Key != "k1" {
		t.Fatalf("session list = %v", got)
	}
	if got := st.listOwned("alice"); len(got) != 2 {
		t.Fatalf("principal list = %v", got)
	}
	if got := st.listOwned("bob"); len(got) != 0 {
		t.Fatalf("another principal sees %v", got)
	}
	if st.owned("bob", "k1") != nil || st.deleteOwned("bob", "k1") {
		t.Fatal("another principal reached alice's key")
	}
	_, err := st.put(mk("s-two", "k1"))
	var se *StickyError
	if !errors.As(err, &se) || se.Status != 409 || !strings.Contains(se.Msg, "s-one") || !strings.Contains(se.Msg, "llm env --sticky k1") {
		t.Fatalf("put from another session: %v", err)
	}
	if st.delete("alice", "s-two", "k1") {
		t.Fatal("session-scoped delete crossed sessions")
	}
	if !st.deleteOwned("alice", "k1") || st.owned("alice", "k1") != nil {
		t.Fatal("principal delete failed")
	}
}

// runLLM runs the sticky/env CLI against a harness door, as another shell would:
// its own session in the environment, the shared owner token.
func runLLM(t *testing.T, h *harness, session string, cmd *cobra.Command, args ...string) string {
	t.Helper()
	_, port, _ := net.SplitHostPort(h.server.Listener.Addr().String())
	t.Setenv(door.PortEnv, port)
	t.Setenv(SessionEnv, session)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%s %v: %v\n%s", cmd.Name(), args, err, out.String())
	}
	return out.String()
}

func TestEnvStickyBindsAcrossShells(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	token, err := door.Token()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Token = token })
	a, b, c := NewRootSession(), NewRootSession(), NewRootSession()

	// Shell A creates the sticky; shell B (a different session) mints the URL.
	runLLM(t, h, a, newStickyCreateCmd(), "bench", "--model", "llama3.2:3b")
	var env struct {
		Session string            `json:"session"`
		Env     map[string]string `json:"env"`
	}
	raw := runLLM(t, h, b, newEnvCmd(), "--json", "--sticky", "bench")
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if env.Session != a || !strings.Contains(env.Env["OLLAMA_HOST"], "/k/"+token+"/s/"+a+"/sticky/bench") {
		t.Fatalf("env did not name the owner session: %+v", env)
	}
	// The printed URL really serves the binding, from a request with no session of its own.
	path := strings.TrimPrefix(env.Env["OPENAI_BASE_URL"], h.server.URL) + "/chat/completions"
	if resp, _ := h.do("POST", path, chat("x"), map[string]string{SessionHeader: "", "Authorization": "Bearer " + token}); resp.StatusCode != 200 {
		t.Fatalf("serve via minted URL %s: %d", path, resp.StatusCode)
	}

	// ls from a third shell shows it with its owner; show works; rm works.
	ls := runLLM(t, h, c, newStickyListCmd())
	if !strings.Contains(ls, "bench") || !strings.Contains(ls, a) {
		t.Fatalf("ls from another session:\n%s", ls)
	}
	if show := runLLM(t, h, c, newStickyShowCmd(), "bench"); !strings.Contains(show, a) {
		t.Fatalf("show from another session:\n%s", show)
	}
	if rm := runLLM(t, h, c, newStickyRmCmd(), "bench"); !strings.Contains(rm, "deleted bench") {
		t.Fatalf("rm from another session: %s", rm)
	}
	if h.b.sticky.owned(h.b.opts.Principal, "bench") != nil {
		t.Fatal("binding survived rm")
	}
}

// env --sticky KEY --model M freezes the sticky and prints its bound URL in one step.
func TestEnvStickyModelCreatesInOneStep(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	token, err := door.Token()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Token = token })
	sess := NewRootSession()
	var env struct {
		Session string            `json:"session"`
		Env     map[string]string `json:"env"`
	}
	raw := runLLM(t, h, sess, newEnvCmd(), "--json", "--sticky", "once", "--model", "llama3.2:3b", "--uses", "5")
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	b := h.b.sticky.owned(h.b.opts.Principal, "once")
	if b == nil || b.Spec.Uses != 5 || env.Session != sess {
		t.Fatalf("binding %+v, env %+v", b, env)
	}
	// Running it again from another shell binds to the same sticky, not an error.
	raw = runLLM(t, h, NewRootSession(), newEnvCmd(), "--json", "--sticky", "once", "--model", "llama3.2:3b")
	if err := json.Unmarshal([]byte(raw), &env); err != nil || env.Session != sess {
		t.Fatalf("second shell: %v %+v", err, env)
	}
}
