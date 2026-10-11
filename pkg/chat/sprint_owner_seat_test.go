package chat

import (
	"context"
	"os"
	"strings"
	"testing"
)

// pinChildEnvIsolation keeps the credential firewall ACTIVE and hermetic for a
// child-env test: an empty vault config, no operator opt-out, no shim writes.
// The lease-token restore under test is meaningless with the firewall off.
func pinChildEnvIsolation(t *testing.T) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("BASHY_ALLOW_AGENT_SECRETS", "")
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
}

// ownerLaunch is a resolved launch the way foreman resolves a managed sprint
// owner: a NAMED fleet agent on whichever tool the binding picked.
func ownerLaunch(tool string) Launch {
	return Launch{Nick: "s421-manager", Tool: tool, ToolName: tool, Model: "m-id", ModelName: "fable"}
}

// The bug (sprint #421): `bashy foreman serve <manager>` held
// BASHY_SPRINT_LEASE_TOKEN in its own environment, but the binding child it
// launched did not — the credential firewall removes the name by SHAPE (_TOKEN
// suffix) — so every gated sprint verb from the session logged "conductor
// (bypass)". The child must carry the lease and the owner's seat for every
// tool the binding can resolve to.
func TestManagedOwnerChildEnvCarriesLeaseAndSeat(t *testing.T) {
	pinChildEnvIsolation(t)
	t.Setenv(sprintLeaseTokenEnv, "lease-secret")

	for _, tool := range []string{"claude", "codex", "ycode", "genie", "opencode", "agy"} {
		env := agentChildEnv(withLaunch(context.Background(), ownerLaunch(tool)))
		if got, n := envValue(env, sprintLeaseTokenEnv), envCount(env, sprintLeaseTokenEnv); n != 1 || got != "lease-secret" {
			t.Errorf("%s: child env lease = %q x%d, want exactly one %q", tool, got, n, "lease-secret")
		}
		if got, n := envValue(env, "BASHY_AGENT"), envCount(env, "BASHY_AGENT"); n != 1 || got != "s421-manager" {
			t.Errorf("%s: child env BASHY_AGENT = %q x%d, want exactly one %q", tool, got, n, "s421-manager")
		}
		// The identity PrincipalEnv already carried must survive beside the seat.
		if !hasEnv(env, "BASHY_PRINCIPAL=dhnt:agent/s421-manager") || !hasEnv(env, "BASHY_AGENT_ID=s421-manager") {
			t.Errorf("%s: child env lost principal identity: %v", tool, childEnvNames(env))
		}
	}
}

// Prove the wiring at the real choke point every governed spawn goes through —
// the same *exec.Cmd constructor the session, one-shot, ACP and interact paths
// use — not just the helper.
func TestManagedOwnerAgentCommandCarriesLeaseAndSeat(t *testing.T) {
	pinChildEnvIsolation(t)
	t.Setenv(sprintLeaseTokenEnv, "lease-secret")

	l := ownerLaunch("ycode")
	cmd := agentCommand(withLaunch(context.Background(), l), l.Tool, nil, ".")
	if got := envValue(cmd.Env, sprintLeaseTokenEnv); got != "lease-secret" {
		t.Fatalf("cmd.Env lease = %q, want %q", got, "lease-secret")
	}
	if got := envValue(cmd.Env, "BASHY_AGENT"); got != "s421-manager" {
		t.Fatalf("cmd.Env BASHY_AGENT = %q, want %q", got, "s421-manager")
	}
}

// The manager process may itself carry a BASHY_AGENT from whoever started it.
// The binding child must claim the OWNER's seat, not inherit that one — a
// stale inherited seat is how a manager's board sends arrive attributed to the
// operator (or to nobody) instead of the sprint's manager.
func TestManagedOwnerChildEnvReplacesInheritedSeat(t *testing.T) {
	pinChildEnvIsolation(t)
	t.Setenv(sprintLeaseTokenEnv, "lease-secret")
	t.Setenv("BASHY_AGENT", "whoever-started-the-manager")

	env := agentChildEnv(withLaunch(context.Background(), ownerLaunch("claude")))
	if got, n := envValue(env, "BASHY_AGENT"), envCount(env, "BASHY_AGENT"); n != 1 || got != "s421-manager" {
		t.Fatalf("child env BASHY_AGENT = %q x%d, want exactly one %q", got, n, "s421-manager")
	}
}

// Without the lease token this is not a managed owner session, and no seat is
// stamped: an ordinary launch keeps whatever BASHY_AGENT it inherited,
// unchanged — the restore is gated on the manager marker, not on every chat
// spawn. (The test host may itself run inside an agent session, so "inherited"
// can be a real seat here.)
func TestUnmanagedChildEnvStampsNoSeat(t *testing.T) {
	pinChildEnvIsolation(t)
	inherited := os.Getenv("BASHY_AGENT")

	env := agentChildEnv(withLaunch(context.Background(), ownerLaunch("claude")))
	if childEnvHasName(env, sprintLeaseTokenEnv) {
		t.Fatalf("unmanaged launch carried a lease: %v", childEnvNames(env))
	}
	if got := envValue(env, "BASHY_AGENT"); got != inherited {
		t.Fatalf("unmanaged launch had its seat rewritten: BASHY_AGENT = %q, want the inherited %q", got, inherited)
	}
}

// A lease-shaped name is restored from the launcher only: a DIFFERENT
// credential-shaped variable the firewall took must stay taken.
func TestManagedOwnerRestoreDoesNotReopenOtherTokens(t *testing.T) {
	pinChildEnvIsolation(t)
	t.Setenv(sprintLeaseTokenEnv, "lease-secret")
	t.Setenv("OPERATOR_API_TOKEN", "operator-secret")

	env := agentChildEnv(withLaunch(context.Background(), ownerLaunch("codex")))
	if childEnvHasName(env, "OPERATOR_API_TOKEN") {
		t.Fatalf("credential-shaped variable reopened beside the lease: %v", childEnvNames(env))
	}
	if !hasEnv(env, sprintLeaseTokenEnv+"=lease-secret") {
		t.Fatalf("lease missing while other token stayed scrubbed: %v", childEnvNames(env))
	}
}

func envCount(env []string, name string) int {
	n := 0
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			n++
		}
	}
	return n
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}
