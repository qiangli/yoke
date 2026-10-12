package broker

// Sprint: #9999 — genie's per-model door stickies after a CLI upgrade.
//
// A sticky freezes the CLI version that serves it; when the CLI self-updates
// the key is refused with 409 forever and, until now, the only way out was
// rm + create — which lands the key in the caller's SESSION, where a nested
// door session (genie's turn under dag/ycode) cannot see it (404 "owned by
// session s-…"). The supported recovery is: `check` to see the stale keys,
// `repin` to re-resolve a key in place keeping its scope and seat, and
// `create --scope principal` to mint a principal-owned key from any shell.
// Exact-version guarantees are unchanged: nothing here serves a stale key.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
)

func stickyHeaderShort(h map[string][]string) string {
	v := strings.Join(h[StickyHeader], "")
	for _, part := range strings.Split(v, ";") {
		if k, val, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k == "identity" {
			return val
		}
	}
	return ""
}

// create --scope principal from a nested session mints a binding no session
// owns: every session of the principal, including one that did not exist when
// the key was made, serves it directly.
func TestStickyCreateAtPrincipalScope(t *testing.T) {
	h := newHarness(t, nil)
	nested := ShellSession(ExportedView(NewRootSession()))
	resp, out := h.do("POST", "/v1/sticky?scope=principal", StickySpec{Key: "genie-muse", Model: "L5", TTL: "0"}, map[string]string{SessionHeader: nested})
	if resp.StatusCode != 200 || out["session"] != "" {
		t.Fatalf("principal-scope create: %d %v", resp.StatusCode, out)
	}
	if b := h.b.sticky.owned(h.b.opts.Principal, "genie-muse"); b == nil || b.Session != "" {
		t.Fatalf("stored binding = %+v, want principal scope", b)
	}
	// A brand-new nested session (genie's next turn) and a session-less request both serve it.
	for _, s := range []string{ShellSession(ExportedView(NewRootSession())), ""} {
		if resp, out := h.do("POST", "/sticky/genie-muse/v1/chat/completions", chat("x"), map[string]string{SessionHeader: s}); resp.StatusCode != 200 {
			t.Fatalf("serve from session %q: %d %v", s, resp.StatusCode, out)
		}
	}
	// Creating the same key again at principal scope is idempotent; from a session it binds to the existing one.
	if resp, _ := h.do("POST", "/v1/sticky?scope=principal", StickySpec{Key: "genie-muse", Model: "L5", TTL: "0"}, nil); resp.StatusCode != 200 {
		t.Fatalf("idempotent principal create: %d", resp.StatusCode)
	}
	if resp, out := h.do("POST", "/v1/sticky", StickySpec{Key: "genie-muse", Model: "L5", TTL: "0"}, map[string]string{SessionHeader: NewRootSession()}); resp.StatusCode != 200 || out["session"] != "" {
		t.Fatalf("session create over a principal key: %d %v", resp.StatusCode, out)
	}

	// A session-owned key blocks a principal-scope create of the same name:
	// the refusal names the owner, nothing is replaced silently.
	owner := NewRootSession()
	h.do("POST", "/v1/sticky", StickySpec{Key: "genie-gem", Model: "L5"}, map[string]string{SessionHeader: owner})
	resp, out = h.do("POST", "/v1/sticky?scope=principal", StickySpec{Key: "genie-gem", Model: "L5"}, nil)
	if msg := errMessage(out); resp.StatusCode != 409 || !strings.Contains(msg, owner) {
		t.Fatalf("principal create over a session key: %d %q", resp.StatusCode, msg)
	}
}

// After the CLI upgrades, the key is refused (unchanged), the management view
// says so, and PUT re-pins it in place: same key, still principal-owned, new
// identity, served again — by the new version, never the old digest.
func TestStickyRepinAfterCLIUpgradeKeepsPrincipalScope(t *testing.T) {
	h := newHarness(t, nil)
	resp, out := h.do("POST", "/v1/sticky?scope=principal", StickySpec{Key: "genie-muse", Model: "muse-spark1.3", TTL: "0"}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %v", resp.StatusCode, out)
	}
	before := out["digest"].(string)
	if resp, _ := h.do("POST", "/sticky/genie-muse/v1/chat/completions", chat("x"), nil); resp.StatusCode != 200 {
		t.Fatalf("serve before upgrade: %d", resp.StatusCode)
	}

	h.tool.Store("2.2.0")
	resp, out = h.do("POST", "/sticky/genie-muse/v1/chat/completions", chat("x"), nil)
	if msg := errMessage(out); resp.StatusCode != 409 || !strings.Contains(msg, `is now "2.2.0"`) {
		t.Fatalf("serve after upgrade: %d %q, want 409 is now", resp.StatusCode, msg)
	}
	_, out = h.do("GET", "/v1/sticky/genie-muse?scope=principal", nil, nil)
	if out["serviceable"] != false || !strings.Contains(str(out["stale"]), "is now") {
		t.Fatalf("management view after upgrade: %v", out)
	}
	_, out = h.do("GET", "/v1/sticky?scope=principal", nil, nil)
	row := out["bindings"].([]any)[0].(map[string]any)
	if row["serviceable"] != false {
		t.Fatalf("ls row after upgrade: %v", row)
	}

	// The re-pin comes from a nested session (where genie's recovery shell
	// runs), through the principal scope — ownership is the principal's.
	nested := ShellSession(ExportedView(NewRootSession()))
	resp, out = h.do("PUT", "/v1/sticky/genie-muse?scope=principal", nil, map[string]string{SessionHeader: nested})
	if resp.StatusCode != 200 || out["repinned"] != true {
		t.Fatalf("repin: %d %v", resp.StatusCode, out)
	}
	after := out["digest"].(string)
	if after == before || out["previous_digest"] != before || out["session"] != "" || out["serviceable"] != true {
		t.Fatalf("repin view: %v", out)
	}
	if id, _ := out["identity"].(map[string]any); id["tool_version"] != "2.2.0" {
		t.Fatalf("re-pinned identity = %v", id)
	}
	if prev, _ := out["previous_identity"].(map[string]any); prev["tool_version"] != "2.1.0" {
		t.Fatalf("previous identity = %v", prev)
	}
	if spec, _ := out["spec"].(map[string]any); spec["ttl"] != "0" || spec["model"] != "muse-spark1.3" {
		t.Fatalf("spec not kept: %v", spec)
	}
	if out["used"] != float64(0) {
		t.Fatalf("used not reset: %v", out["used"])
	}

	resp, _ = h.do("POST", "/sticky/genie-muse/v1/chat/completions", chat("x"), map[string]string{SessionHeader: nested})
	if resp.StatusCode != 200 || resp.Header.Get(StickyHeader) == "" || stickyHeaderShort(resp.Header) != ShortDigest(after) {
		t.Fatalf("serve after repin: %d %q", resp.StatusCode, resp.Header.Get(StickyHeader))
	}
	// Idempotent: an up-to-date key is left alone.
	resp, out = h.do("PUT", "/v1/sticky/genie-muse?scope=principal", nil, nil)
	if resp.StatusCode != 200 || out["repinned"] != false || out["digest"] != after {
		t.Fatalf("second repin: %d %v", resp.StatusCode, out)
	}
}

// Re-pin ownership follows management: session-scoped from a stranger is a
// 404 naming the owner; principal scope reaches it and keeps the owner.
func TestStickyRepinOwnership(t *testing.T) {
	h := newHarness(t, nil)
	a, b := NewRootSession(), NewRootSession()
	h.do("POST", "/v1/sticky", StickySpec{Key: "bench", Model: "L5"}, map[string]string{SessionHeader: a})
	h.tool.Store("2.2.0")

	resp, out := h.do("PUT", "/v1/sticky/bench", nil, map[string]string{SessionHeader: b})
	if msg := errMessage(out); resp.StatusCode != 404 || !strings.Contains(msg, a) {
		t.Fatalf("session-scoped repin from a stranger: %d %q", resp.StatusCode, msg)
	}
	if h.b.sticky.owned(h.b.opts.Principal, "bench").Identity.ToolVersion != "2.1.0" {
		t.Fatal("a refused repin changed the binding")
	}
	resp, out = h.do("PUT", "/v1/sticky/bench?scope=principal", nil, map[string]string{SessionHeader: b})
	if resp.StatusCode != 200 || out["repinned"] != true || out["session"] != a || out["visible"] != false {
		t.Fatalf("principal repin: %d %v", resp.StatusCode, out)
	}
	// The owner serves the re-pinned key; the stranger still cannot.
	if resp, _ := h.do("POST", "/sticky/bench/v1/chat/completions", chat("x"), map[string]string{SessionHeader: a}); resp.StatusCode != 200 {
		t.Fatalf("owner after repin: %d", resp.StatusCode)
	}
	if resp, _ := h.do("POST", "/sticky/bench/v1/chat/completions", chat("x"), map[string]string{SessionHeader: b}); resp.StatusCode != 404 {
		t.Fatalf("stranger after repin: %d", resp.StatusCode)
	}
	// The owner's own session-scoped repin works too.
	h.tool.Store("2.3.0")
	if resp, out := h.do("PUT", "/v1/sticky/bench", nil, map[string]string{SessionHeader: a}); resp.StatusCode != 200 || out["repinned"] != true {
		t.Fatalf("owner repin: %d %v", resp.StatusCode, out)
	}
	// A key nobody has is a plain 404 that says how to create it.
	resp, out = h.do("PUT", "/v1/sticky/absent?scope=principal", nil, nil)
	if msg := errMessage(out); resp.StatusCode != 404 || !strings.Contains(msg, "sticky create absent") {
		t.Fatalf("repin absent: %d %q", resp.StatusCode, msg)
	}
}

// seatCLI resolves to a seat the test can move: the same agent name, but a
// different vendor model (what a fleet edit to the agent's model would do).
type seatCLI struct {
	*fakeCLI
	mu     sync.Mutex
	vendor string
}

func (c *seatCLI) ResolveAgent(ctx context.Context, model, filter string) (AgentInfo, error) {
	info, err := c.fakeCLI.ResolveAgent(ctx, model, filter)
	if err != nil {
		return info, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.vendor != "" {
		info.VendorModel = c.vendor
	}
	return info, nil
}

// A re-pin never moves the seat: when the spec now resolves to a different
// vendor model the re-pin is refused and the stale binding stays as it was.
func TestStickyRepinRefusesSeatMove(t *testing.T) {
	var cli *seatCLI
	h := newHarness(t, func(o *Options) {
		cli = &seatCLI{fakeCLI: o.CLI.(*fakeCLI)}
		o.CLI = cli
	})
	_, out := h.do("POST", "/v1/sticky?scope=principal", StickySpec{Key: "genie-opus", Model: "L5"}, nil)
	before := out["digest"].(string)

	h.tool.Store("2.2.0")
	cli.mu.Lock()
	cli.vendor = "claude-opus-6"
	cli.mu.Unlock()
	resp, out := h.do("PUT", "/v1/sticky/genie-opus?scope=principal", nil, nil)
	msg := errMessage(out)
	if resp.StatusCode != 409 || !strings.Contains(msg, "not re-pinned") || !strings.Contains(msg, `vendor model "claude-opus-6" (was "claude-opus-5")`) {
		t.Fatalf("seat move: %d %q", resp.StatusCode, msg)
	}
	cur := h.b.sticky.owned(h.b.opts.Principal, "genie-opus")
	if cur.Digest != before || cur.Identity.ToolVersion != "2.1.0" {
		t.Fatalf("refused repin changed the binding: %+v", cur.Identity)
	}
	// Still refused for serving — no metered fallback, no silent switch.
	if resp, _ := h.do("POST", "/sticky/genie-opus/v1/chat/completions", chat("x"), nil); resp.StatusCode != 409 {
		t.Fatalf("serve after refused repin: %d", resp.StatusCode)
	}
}

// A re-pin of a bind=worker/reset=none key retires the held CLI: the old
// version's warm process must not answer under the new identity, and the
// conversation starts over on a fresh worker.
func TestStickyRepinRetiresTheWorker(t *testing.T) {
	h := newHarness(t, nil)
	sess := h.cli.enableSticky()
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "L5", Bind: BindWorker, Reset: ResetNone}, nil)
	if status, _ := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}); status != 200 {
		t.Fatalf("turn1: %d", status)
	}
	h.tool.Store("2.2.0")
	if status, out := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}, [2]string{"assistant", "x"}, [2]string{"user", "B"}); status != 409 {
		t.Fatalf("turn2 after upgrade: %d %v", status, out)
	}
	resp, out := h.do("PUT", "/v1/sticky/conv", nil, nil)
	if resp.StatusCode != 200 || out["repinned"] != true {
		t.Fatalf("repin: %d %v", resp.StatusCode, out)
	}
	sess.mu.Lock()
	closed := sess.closed
	sess.mu.Unlock()
	if !closed {
		t.Fatal("repin left the old worker running")
	}
	if b := h.b.sticky.owned(h.b.opts.Principal, "conv"); len(b.Transcript) != 0 || b.Used != 0 {
		t.Fatalf("transcript/uses not reset: %+v", b)
	}
	// A fresh conversation on a fresh worker.
	h.cli.sticky = &fakeStickySession{}
	if status, _ := stickyTurn(t, h, "conv", "L5", [2]string{"user", "A"}); status != 200 {
		t.Fatalf("turn after repin: %d", status)
	}
}

// The store swap is compare-and-swap on the digest and the scope: a binding
// that changed (or vanished) while the re-pin resolved is not overwritten.
func TestStickyStoreReplaceIsCompareAndSwap(t *testing.T) {
	st := newStickyStore("", time.Now)
	old := &Binding{scoped: scoped{Principal: "alice", Created: time.Now()}, Spec: StickySpec{Key: "k", TTL: "0"}, Digest: "sha256:aaaa", Used: 7}
	if _, err := st.put(old); err != nil {
		t.Fatal(err)
	}
	stale := *old
	stale.Digest = "sha256:0000"
	if _, err := st.replace(&stale, &Binding{Spec: old.Spec, Digest: "sha256:bbbb"}); err == nil || !strings.Contains(err.Error(), "changed while re-pinning") {
		t.Fatalf("replace over a changed binding: %v", err)
	}
	next, err := st.replace(old, &Binding{Spec: old.Spec, Digest: "sha256:bbbb"})
	if err != nil || next.Digest != "sha256:bbbb" || next.Used != 0 || next.Principal != "alice" || next.Session != "" {
		t.Fatalf("replace: %+v %v", next, err)
	}
	st.deleteOwned("alice", "k")
	if _, err := st.replace(next, &Binding{Spec: old.Spec, Digest: "sha256:cccc"}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("replace over a deleted binding: %v", err)
	}
}

// The CLI end to end: check gates on stale keys, repin recovers one in place,
// create --scope principal mints a principal-owned key from a shell with a
// session of its own.
func TestStickyCheckAndRepinCommands(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	token, err := door.Token()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Token = token })
	shell, nested := NewRootSession(), ShellSession(ExportedView(NewRootSession()))

	out := runLLM(t, h, shell, newStickyCreateCmd(), "genie-muse", "--model", "muse-spark1.3", "--ttl", "0", "--scope", "principal")
	if !strings.Contains(out, "session   (principal)") {
		t.Fatalf("create --scope principal:\n%s", out)
	}
	if out := runLLM(t, h, nested, newStickyCheckCmd()); !strings.Contains(out, "genie-muse") || !strings.Contains(out, "\tok") && !strings.Contains(out, "  ok") {
		t.Fatalf("check while fresh:\n%s", out)
	}

	h.tool.Store("2.2.0")
	// check: non-zero with the stale key named and the recovery spelled out.
	check := newStickyCheckCmd()
	var buf strings.Builder
	check.SetOut(&buf)
	check.SetErr(&buf)
	check.SetArgs(nil)
	t.Setenv(SessionEnv, nested)
	err = check.Execute()
	if err == nil || !strings.Contains(err.Error(), "1 stale") || !strings.Contains(err.Error(), "sticky repin") || !strings.Contains(buf.String(), "stale: identity") {
		t.Fatalf("check after upgrade: err=%v\n%s", err, buf.String())
	}
	// A key that is not there is reported missing, not silently ok.
	check = newStickyCheckCmd()
	buf.Reset()
	check.SetOut(&buf)
	check.SetErr(&buf)
	check.SetArgs([]string{"genie-gone"})
	if err := check.Execute(); err == nil || !strings.Contains(err.Error(), "1 missing") || !strings.Contains(buf.String(), "missing: no binding") {
		t.Fatalf("check missing: err=%v\n%s", err, buf.String())
	}

	out = runLLM(t, h, nested, newStickyRepinCmd(), "genie-muse")
	if !strings.Contains(out, "re-pinned genie-muse") || !strings.Contains(out, `tool_version  "2.1.0" -> "2.2.0"`) || !strings.Contains(out, "session   (principal)") || !strings.Contains(out, "status    ok") {
		t.Fatalf("repin:\n%s", out)
	}
	if out := runLLM(t, h, nested, newStickyRepinCmd(), "genie-muse"); !strings.Contains(out, "nothing to re-pin") {
		t.Fatalf("second repin:\n%s", out)
	}
	if out := runLLM(t, h, nested, newStickyCheckCmd(), "genie-muse"); !strings.Contains(out, "ok") {
		t.Fatalf("check after repin:\n%s", out)
	}
	if ls := runLLM(t, h, shell, newStickyListCmd()); !strings.Contains(ls, "STATUS") || !strings.Contains(ls, "(principal)") {
		t.Fatalf("ls:\n%s", ls)
	}
	if b := h.b.sticky.owned(h.b.opts.Principal, "genie-muse"); b == nil || b.Session != "" || b.Identity.ToolVersion != "2.2.0" {
		t.Fatalf("binding after CLI repin: %+v", b)
	}
}

// raceCLI runs the armed hook once, inside the next resolve — for a re-pin,
// the window between reading the old binding and swapping it — so a test can
// delete and recreate the key there.
type raceCLI struct {
	*fakeCLI
	mu   sync.Mutex
	hook func()
}

func (c *raceCLI) arm(hook func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hook = hook
}

func (c *raceCLI) ResolveAgent(ctx context.Context, model, filter string) (AgentInfo, error) {
	c.mu.Lock()
	hook := c.hook
	c.hook = nil
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return c.fakeCLI.ResolveAgent(ctx, model, filter)
}

// The digest alone does not identify a binding: a key deleted and created
// again by ANOTHER session with the same spec resolves to the same identity.
// A store swap keyed on the digest would let the first session's re-pin land
// on the stranger's binding — a key it may not touch — resetting its uses and
// retiring its worker. The swap compares the scope too and refuses.
func TestStickyStoreReplaceRefusesScopeRace(t *testing.T) {
	clock := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	st := newStickyStore("", func() time.Time { return clock })
	evicted := 0
	st.onEvict = func(string, string) { evicted++ }
	spec := StickySpec{Key: "bench", Model: "L5", TTL: "0"}

	// Session a's binding, as the re-pin read it.
	old, err := st.put(&Binding{scoped: scoped{Principal: "alice", Session: "s-a", Created: clock}, Spec: spec, Digest: "sha256:aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	// Meanwhile: rm --scope principal, then create from session b — same spec,
	// same digest, a different binding.
	st.deleteOwned("alice", "bench")
	clock = clock.Add(time.Second)
	if _, err := st.put(&Binding{scoped: scoped{Principal: "alice", Session: "s-b", Created: clock}, Spec: spec, Digest: "sha256:aaaa", Used: 3}); err != nil {
		t.Fatal(err)
	}
	evicted = 0
	_, err = st.replace(old, &Binding{Spec: spec, Digest: "sha256:bbbb"})
	if err == nil || !strings.Contains(err.Error(), "deleted and created again") || !strings.Contains(err.Error(), "owned by session s-b") || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("replace across an owner change: %v", err)
	}
	if cur := st.owned("alice", "bench"); cur.Session != "s-b" || cur.Digest != "sha256:aaaa" || cur.Used != 3 {
		t.Fatalf("refused replace touched the new owner's binding: %+v", cur)
	}
	if evicted != 0 {
		t.Fatalf("refused replace retired the new owner's worker (%d evictions)", evicted)
	}

	// The same key recreated at the SAME scope (principal, no session) is
	// still a different binding: the creation instant tells them apart.
	st.deleteOwned("alice", "bench")
	old, err = st.put(&Binding{scoped: scoped{Principal: "alice", Created: clock}, Spec: spec, Digest: "sha256:aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	st.deleteOwned("alice", "bench")
	clock = clock.Add(time.Second)
	if _, err := st.put(&Binding{scoped: scoped{Principal: "alice", Created: clock}, Spec: spec, Digest: "sha256:aaaa", Used: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.replace(old, &Binding{Spec: spec, Digest: "sha256:bbbb"}); err == nil || !strings.Contains(err.Error(), "deleted and created again") || !strings.Contains(err.Error(), "principal-scoped") {
		t.Fatalf("replace across a recreate at principal scope: %v", err)
	}
	if cur := st.owned("alice", "bench"); cur.Used != 1 || cur.Digest != "sha256:aaaa" {
		t.Fatalf("refused replace touched the recreated binding: %+v", cur)
	}
	// An exported binding and its unexported twin are different bindings too.
	st.deleteOwned("alice", "bench")
	old, _ = st.put(&Binding{scoped: scoped{Principal: "alice", Session: "s-a", Created: clock}, Spec: spec, Digest: "sha256:aaaa"})
	st.deleteOwned("alice", "bench")
	st.put(&Binding{scoped: scoped{Principal: "alice", Session: "s-a", Exported: true, Created: clock}, Spec: spec, Digest: "sha256:aaaa"})
	if _, err := st.replace(old, &Binding{Spec: spec, Digest: "sha256:bbbb"}); err == nil || !strings.Contains(err.Error(), "deleted and created again") {
		t.Fatalf("replace across an export change: %v", err)
	}
	// And the unraced swap still goes through, inheriting the stored scope.
	cur := st.owned("alice", "bench")
	next, err := st.replace(cur, &Binding{Spec: spec, Digest: "sha256:bbbb"})
	if err != nil || next.Digest != "sha256:bbbb" || next.Session != "s-a" || !next.Exported {
		t.Fatalf("unraced replace: %+v %v", next, err)
	}
}

// Through the door: session a re-pins its own key; while the re-pin resolves,
// the key is removed (rm --scope principal) and created again by session b at
// the same CLI version — same spec, same digest. The re-pin must be refused
// with 409 and b's binding left exactly as b made it: a would otherwise have
// re-pinned a key it cannot even see.
func TestStickyRepinRefusesScopeRace(t *testing.T) {
	var cli *raceCLI
	h := newHarness(t, func(o *Options) {
		cli = &raceCLI{fakeCLI: o.CLI.(*fakeCLI)}
		o.CLI = cli
	})
	a, b := NewRootSession(), NewRootSession()
	h.do("POST", "/v1/sticky", StickySpec{Key: "bench", Model: "L5", TTL: "0"}, map[string]string{SessionHeader: a})
	old := h.b.sticky.owned(h.b.opts.Principal, "bench")
	if old == nil || old.Session != a {
		t.Fatalf("setup: %+v", old)
	}

	cli.arm(func() {
		// What `bashy llm sticky rm bench --scope principal` from any shell and
		// `bashy llm sticky create bench --model L5` from shell b do, at the
		// version a's binding was made with: b's binding carries a's digest.
		h.b.sticky.deleteOwned(h.b.opts.Principal, "bench")
		if _, err := h.b.sticky.put(&Binding{scoped: scoped{Principal: h.b.opts.Principal, Session: b, Created: time.Now()}, Spec: old.Spec, Identity: old.Identity, Digest: old.Digest, Used: 2}); err != nil {
			t.Error(err)
		}
	})
	h.tool.Store("2.2.0")
	resp, out := h.do("PUT", "/v1/sticky/bench", nil, map[string]string{SessionHeader: a})
	if msg := errMessage(out); resp.StatusCode != 409 || !strings.Contains(msg, "deleted and created again") || !strings.Contains(msg, "owned by session "+b) {
		t.Fatalf("repin across an owner change: %d %q", resp.StatusCode, msg)
	}
	cur := h.b.sticky.owned(h.b.opts.Principal, "bench")
	if cur == nil || cur.Session != b || cur.Digest != old.Digest || cur.Used != 2 || cur.Identity.ToolVersion != "2.1.0" {
		t.Fatalf("refused repin changed b's binding: %+v", cur)
	}
	// a's retry now gets the ownership refusal, naming b; a never reaches b's key.
	resp, out = h.do("PUT", "/v1/sticky/bench", nil, map[string]string{SessionHeader: a})
	if msg := errMessage(out); resp.StatusCode != 404 || !strings.Contains(msg, b) {
		t.Fatalf("retry from the former owner: %d %q", resp.StatusCode, msg)
	}
	if resp, _ := h.do("POST", "/sticky/bench/v1/chat/completions", chat("x"), map[string]string{SessionHeader: a}); resp.StatusCode != 404 {
		t.Fatalf("former owner serves b's key: %d", resp.StatusCode)
	}
}

// `check --json` is a gate like the table form: the envelope is written AND
// the exit status is non-zero when any key is stale or missing. Returning the
// encoder's nil used to make a stale or missing key exit 0 under --json.
func TestStickyCheckJSONExitStatus(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	token, err := door.Token()
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(o *Options) { o.Token = token })
	shell := NewRootSession()
	runLLM(t, h, shell, newStickyCreateCmd(), "genie-muse", "--model", "muse-spark1.3", "--ttl", "0", "--scope", "principal")

	type envelope struct {
		Schema   string           `json:"schema_version"`
		Bindings []map[string]any `json:"bindings"`
		Stale    []string         `json:"stale"`
		Missing  []string         `json:"missing"`
		OK       bool             `json:"ok"`
	}
	check := func(args ...string) (envelope, error) {
		t.Helper()
		cmd := newStickyCheckCmd()
		var buf strings.Builder
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(append([]string{"--json"}, args...))
		err := cmd.Execute()
		var env envelope
		dec := json.NewDecoder(strings.NewReader(buf.String()))
		if derr := dec.Decode(&env); derr != nil || dec.More() {
			t.Fatalf("check --json %v: not a single JSON value (%v):\n%s", args, derr, buf.String())
		}
		if env.Schema != "bashy-sticky-check-v1" {
			t.Fatalf("check --json %v: schema %q", args, env.Schema)
		}
		return env, err
	}

	// fresh: ok, exit 0.
	if env, err := check(); err != nil || !env.OK || len(env.Stale) != 0 || len(env.Missing) != 0 || len(env.Bindings) != 1 || env.Bindings[0]["serviceable"] != true {
		t.Fatalf("fresh: err=%v %+v", err, env)
	}
	// missing: the envelope names the key AND the command fails.
	env, err := check("genie-gone")
	if err == nil || !strings.Contains(err.Error(), "1 missing") {
		t.Fatalf("missing: err=%v", err)
	}
	if env.OK || len(env.Missing) != 1 || env.Missing[0] != "genie-gone" || len(env.Bindings) != 0 {
		t.Fatalf("missing envelope: %+v", env)
	}
	// stale (the CLI self-updated): the envelope names the key AND the command fails.
	h.tool.Store("2.2.0")
	env, err = check()
	if err == nil || !strings.Contains(err.Error(), "1 stale") || !strings.Contains(err.Error(), "sticky repin") {
		t.Fatalf("stale: err=%v", err)
	}
	if env.OK || len(env.Stale) != 1 || env.Stale[0] != "genie-muse" || env.Bindings[0]["serviceable"] != false || !strings.Contains(str(env.Bindings[0]["stale"]), "2.2.0") {
		t.Fatalf("stale envelope: %+v", env)
	}
	// stale and missing together are both counted.
	if _, err := check("genie-muse", "genie-gone"); err == nil || !strings.Contains(err.Error(), "1 stale, 1 missing") {
		t.Fatalf("stale+missing: err=%v", err)
	}
	// repin recovers it; check --json is ok and exits 0 again.
	runLLM(t, h, shell, newStickyRepinCmd(), "genie-muse")
	if env, err := check("genie-muse"); err != nil || !env.OK || len(env.Stale) != 0 {
		t.Fatalf("after repin: err=%v %+v", err, env)
	}
}
