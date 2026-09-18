package weave

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

// Launching an agent by name.
//
// `weave start -- <tool> <args...>` passes its trailing argv verbatim: the
// conductor writes the flags. That is exactly why a model was never selected —
// a model is not something you can spell in a flag list you wrote by hand for
// every issue.
//
// So a SINGLE trailing token naming an agent expands from the fleet registry
// into the tool's full headless argv, with the issue body as the prompt:
//
//	weave start --issue 3 -- 007
//	  → claude --dangerously-skip-permissions --model fable "<issue body>"
//
// A multi-token argv is the conductor speaking deliberately and is left alone,
// even when its executable happens to share a registered agent name. The one
// fail-closed exception is a registered agent followed by a flag belonging to
// `weave start`: that shape is the common flags-after-`--` mistake and would
// otherwise silently change the agent name into an executable.

// weaveAgentLaunch is the shared agent launch resolved from the fleet registry.
type weaveAgentLaunch = agentlaunch.Launch

// weaveResolveAgent resolves a name to an agent: a nickname, an alias, or a
// bare tool:model. It returns (nil, nil) when the name is not an agent —
// a bare tool name, or something the registry has never heard of.
//
// Availability is NOT decided here. This answers "what would this launch",
// which is the question both `weave start` and `weave fleet` begin with.
func weaveResolveAgent(name string) (*weaveAgentLaunch, error) {
	cat := fleetCatalog()
	if _, ok := cat.Agent(name); !ok {
		if t, m, ok := strings.Cut(name, ":"); !ok || t == "" || m == "" {
			return nil, nil
		}
	}
	l, err := agentlaunch.ResolveWithCatalog(name, agentlaunch.Options{
		DryRun: true, Workspace: fleet.WorkspaceToken,
	}, fleetCatalog)
	if err != nil {
		return nil, err
	}
	if l.ModelName == "" {
		return nil, nil
	}
	return &l, nil
}

// weaveBindAgentWorkspace replaces the generic launch placeholder after weave
// has allocated the concrete clone. No tool name is consulted here: workspace
// binding and preflight behavior come entirely from the fleet launch metadata.
func weaveBindAgentWorkspace(l *weaveAgentLaunch, argv []string, workspace string) ([]string, error) {
	if l == nil {
		return argv, nil
	}
	bound, err := agentlaunch.RenderWorkspace(argv, workspace)
	if err != nil {
		return nil, err
	}
	if len(l.WorkspacePreflight) > 0 {
		l.WorkspacePreflight, err = agentlaunch.RenderWorkspace(l.WorkspacePreflight, workspace)
		if err != nil {
			return nil, err
		}
	}
	return bound, nil
}

// weaveRunWorkspacePreflight runs a tool-declared read-only project-directory
// check before the source-writing worker is started. A declared check fails
// closed: errors, timeouts, extra output, and path mismatches all refuse launch.
func weaveRunWorkspacePreflight(l *weaveAgentLaunch, workspace string, env []string) error {
	if l == nil || len(l.WorkspacePreflight) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	argv := l.WorkspacePreflight
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workspace
	cmd.Env = env
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return fmt.Errorf("workspace preflight timed out: %w", ctx.Err())
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return fmt.Errorf("workspace preflight failed: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return fmt.Errorf("workspace preflight failed: %w", err)
	}
	return weaveVerifyReportedWorkspace(string(out), workspace)
}

func weaveVerifyReportedWorkspace(report, workspace string) error {
	report = strings.TrimSpace(report)
	if strings.Contains(report, "\n") || strings.Contains(report, "\r") {
		return fmt.Errorf("workspace preflight returned extra output: %q", report)
	}
	for _, prefix := range []string{"PWD=", "PROJECT_DIR="} {
		if strings.HasPrefix(report, prefix) {
			report = strings.TrimSpace(strings.TrimPrefix(report, prefix))
			break
		}
	}
	want, err := filepath.Abs(workspace)
	if err != nil {
		return fmt.Errorf("workspace preflight resolve allocated path: %w", err)
	}
	got, err := filepath.Abs(report)
	if err == nil && filepath.IsAbs(report) {
		// macOS commonly reports /private/var/... for a /var/... temp path.
		// Compare filesystem-resolved paths so this harmless alias does not turn
		// the fail-closed check into a false rejection.
		if resolved, resolveErr := filepath.EvalSymlinks(got); resolveErr == nil {
			got = resolved
		}
		if resolved, resolveErr := filepath.EvalSymlinks(want); resolveErr == nil {
			want = resolved
		}
	}
	if err != nil || !filepath.IsAbs(report) || filepath.Clean(got) != filepath.Clean(want) {
		return fmt.Errorf("workspace preflight mismatch: tool reported %q, allocated workspace is %q", report, want)
	}
	return nil
}

// weaveExpandAgent expands a single agent token into a full launch argv.
//
// It returns (nil, nil) when toolArgs is not an agent reference — the caller
// then uses the argv verbatim, unchanged.
//
// The issue body is the prompt, because the body IS the sandbox contract. An
// issue with no body falls back to its title rather than handing the tool an
// empty argument, which most agent CLIs read as "no task" and stall on.
func weaveExpandAgent(toolArgs []string, body, title string) (*weaveAgentLaunch, []string, error) {
	if len(toolArgs) == 0 {
		return nil, nil, nil
	}
	if len(toolArgs) != 1 {
		l, err := weaveResolveAgent(toolArgs[0])
		if err != nil {
			return nil, nil, err
		}
		if l != nil && weaveHasMisplacedStartFlag(toolArgs[1:]) {
			return nil, nil, fmt.Errorf(
				"%q is a registered agent followed by a weave flag after `--`; put weave flags before `--` (for example `weave start --json --quiet -- %s`), or use a raw executable argv that does not contain weave flags",
				toolArgs[0], toolArgs[0])
		}
		return nil, nil, nil // the conductor wrote a raw executable argv; honor it
	}
	l, err := weaveResolveAgent(toolArgs[0])
	if err != nil || l == nil {
		return nil, nil, err
	}
	prompt := strings.TrimSpace(body)
	if prompt == "" {
		prompt = strings.TrimSpace(title)
	}
	if prompt == "" {
		return nil, nil, fmt.Errorf("agent %q needs a prompt: issue has neither a body nor a title", toolArgs[0])
	}
	argv := l.Argv(prompt + weaveWorkerContract)
	return l, weaveAgentEventsStdoutArgv(l, argv), nil
}

func weaveHasMisplacedStartFlag(args []string) bool {
	for _, arg := range args {
		name := arg
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		switch name {
		case "--json", "--plain", "--quiet", "--issue", "--run", "--tool",
			"--resume", "--no-spawn", "--clone", "--auto-commit", "--pty",
			"--idle-timeout", "--max-runtime", "--mem-limit":
			return true
		}
	}
	return false
}

// weaveAgentEventsStdoutArgv connects the fleet's event-stream declaration to
// weave's stream decoder. Keep the prompt final: several agent CLIs treat the
// last argument specially, and Launch.Argv deliberately guarantees that
// position. Raw conductor argv never reaches this helper and remains verbatim.
func weaveAgentEventsStdoutArgv(l *weaveAgentLaunch, argv []string) []string {
	if l == nil || len(argv) < 2 {
		return argv
	}
	events := agentlaunch.EventStdoutArgsWithCatalog(*l, fleetCatalog)
	return agentlaunch.InsertBeforePrompt(argv, events)
}

func weaveAgentEventsFileArgv(l *weaveAgentLaunch, argv []string, path string) []string {
	if l == nil || len(argv) < 2 || strings.TrimSpace(path) == "" {
		return argv
	}
	events := agentlaunch.EventFileArgsWithCatalog(*l, path, fleetCatalog)
	return agentlaunch.InsertBeforePrompt(argv, events)
}

// weaveWorkerContract is appended to EVERY worker's prompt: the non-negotiable
// mechanics a subagent must follow for its work to be visible to the gate and
// mergeable. Stating it here (not just in the brief) means no brief can forget it —
// the single most common real failure is an agent doing the work but never
// COMMITTING it, so an in-workspace verify passes while `weave pull` sees nothing and
// records the run failed and the work looks lost (observed with a real ycode port).
const weaveWorkerContract = `

---
WEAVE WORKER CONTRACT — non-negotiable; your work is INVISIBLE until you do these:
1. COMMIT your changes to the CURRENT branch with git before you finish. Uncommitted
   or untracked files do NOT merge and the run is reported failed — a passing local
   build is not enough, the diff must be committed. Verify with 'git status' (clean)
   and 'git log --oneline -1' (your commit) before you stop.
2. Do NOT push, do NOT switch branches, do NOT merge to main — the conductor pulls.
3. If you cannot finish, COMMIT what works and state briefly what remains.
4. Run the gate yourself before declaring done; a tool exiting 0 is not proof.`

// weaveAgentEnv stamps the spawned worker with the principal it is acting as.
//
// WEAVE_AGENT stays the per-issue SEAT (`007-a`) — the slot this run occupies,
// which is what the queue displays and what distinguishes two concurrent runs.
// BASHY_PRINCIPAL is the AGENT (`007`), the thing that persists across issues
// and that `bashy whois 007` resolves. A seat is not a principal, and writing
// the seat into the principal slot would put a name in the record that resolves
// to nothing.
func weaveAgentEnv(env []string, l *weaveAgentLaunch) []string {
	if l == nil {
		return env
	}
	return agentlaunch.PrincipalEnv(env, agentlaunch.Launch(*l))
}

// weaveChildEnv assembles the exact environment `weave start` hands to a
// spawned agent CLI. It is a pure function of the launcher's environ so the
// live-launch assertion can exercise the SAME code path the spawn uses,
// rather than a re-creation of it in a test.
//
// That distinction is load-bearing. The scrub regressed once already under a
// green build: the unit tests covered weavePreserveOwnAuth in isolation, so
// nothing failed when the caller stopped supplying the key. A test that only
// checks the pieces cannot see a hole between them.
//
// Order matters: stamp, then scrub, then preserve. The scrub must run over the
// finished env (so nothing sneaks a credential in after it), and the preserve
// must run last (so it can restore what the scrub took).
//
// queueDir is the weave state directory for this repo (NOT the git workspace):
// it is where the per-run ycode data store is placed, so two concurrent ycode
// workers each get their own store without polluting the workspace or the diff.
// A resumed run re-derives the same path from the same issue id and reuses it.
func weaveChildEnv(environ []string, workspace, branch, base, queueDir string, it *weaveItem, l *weaveAgentLaunch) []string {
	// Containment: the subagent must not learn the origin repo's path from its
	// environment. The orchestrator's shell typically sits in the origin repo,
	// so the inherited PWD/OLDPWD point straight at it — drop them and pin PWD
	// to the workspace.
	env := make([]string, 0, len(environ)+8)
	for _, kv := range environ {
		if strings.HasPrefix(kv, "PWD=") || strings.HasPrefix(kv, "OLDPWD=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "PWD="+workspace)
	env = append(env,
		fmt.Sprintf("WEAVE_ID=weave-issue-%d", it.ID),
		fmt.Sprintf("WEAVE_BRANCH=%s", branch),
		fmt.Sprintf("WEAVE_BASE=%s", base),
		fmt.Sprintf("WEAVE_ISSUE=%d", it.ID),
		fmt.Sprintf("WEAVE_ISSUE_TITLE=%s", it.Title),
		fmt.Sprintf("WEAVE_ISSUE_BODY=%s", it.Body),
		// The agent's own name. The skill prelude opens with "You are
		// $WEAVE_AGENT" so it signs its thread comments and commit trailers,
		// making reassignment + attribution legible.
		fmt.Sprintf("WEAVE_AGENT=%s", it.Owner),
		fmt.Sprintf("WEAVE_OWNER=%s", it.Owner),
	)
	// WEAVE_AGENT is the seat; BASHY_PRINCIPAL is the agent that fills it.
	env = weaveAgentEnv(env, l)
	// Per-run agent store isolation. ycode locks its data dir, so two workers
	// sharing one store is a hidden concurrency limit of 1 — the second dies on
	// launch. Derive a per-issue store under the queue dir (NOT the workspace),
	// keyed by issue id so a --resume reuses it. Non-ycode tools, a bare/raw
	// (nil) launch, and any operator-set YCODE_DATA_DIR/YCODE_HOME are left
	// untouched. See agentlaunch.ApplyYcodeDataDir.
	var resolvedLaunch agentlaunch.Launch
	if l != nil {
		resolvedLaunch = agentlaunch.Launch(*l)
	}
	env = agentlaunch.ApplyYcodeDataDir(env, environ, resolvedLaunch, queueDir, strconv.FormatInt(it.ID, 10))
	env = weaveApplyManagedGOCache(env, environ, queueDir, it)
	// Credential firewall: a weave subagent is a third-party CLI processing
	// untrusted repo content with its own network egress, so it must not inherit
	// the operator's vault secrets by default (the lethal trifecta). Removes only
	// the vault-projected names; WEAVE_*/BASHY_*/YCODE_* stamped above are untouched.
	// Opt back in with secrets.AllowAgentSecretsEnv.
	env = secrets.ScrubAgentEnv(env)
	// It is NEVER weave's job to hand out API keys — every agent is preconfigured
	// with its own auth (ycode from the ambient env, opencode from its own
	// auth.json, …) and must authenticate from that, not from anything weave
	// injects. Weave used to look up the launched model's api_key_ref and inject
	// that key back from the operator's own environment (grantAgentModelKey) —
	// but that value can go stale (a stale DEEPSEEK_API_KEY once clobbered valid
	// auth on every ycode/opencode agent with 401s), and reading a provider key
	// at all is exactly the "weave hands out API keys" behavior this must not do.
	// So weave injects nothing; it only preserves credential names declared by
	// the generic resolved launch. Values remain opaque and come verbatim from
	// the launcher's own environment.
	if l == nil {
		return env
	}
	return secrets.PreserveEnvNames(env, environ, l.PreserveEnv)
}

// --- roster members ---------------------------------------------------------

// weaveMember is one roster entry: an agent (a nickname, an alias, or a bare
// tool:model) or a bare tool.
//
// Autopilot and fleet both take rosters, and both need the same three answers:
// what do I exec, which key do I record throttles and profiles under, and which
// model — if any — does this entry select.
type weaveMember struct {
	// Name is the entry as the operator wrote it.
	Name string
	// Tool is the registry tool name. It is the key for cooldowns and for the
	// persistent tool profile: two agents on one tool share both, because a
	// throttle and a launch contract belong to the binary, not the binding.
	Tool string
	// Bin is the executable. A tool's binary need not be its name.
	Bin string
	// agent is nil for a bare tool.
	agent *weaveAgentLaunch
}

// Label is what a human sees: the agent's nickname, or the tool's name.
func (m weaveMember) Label() string {
	if m.agent != nil {
		return m.agent.Nick
	}
	return m.Tool
}

// Binding is the capability-matrix key, or "" for a bare tool.
func (m weaveMember) Binding() string {
	if m.agent == nil {
		return ""
	}
	return m.agent.Binding()
}

// Model is the provider-side id this member selects, or "" for a bare tool.
func (m weaveMember) Model() string {
	if m.agent == nil {
		return ""
	}
	return m.agent.Model
}

// IsAgent reports whether this entry selects a model.
func (m weaveMember) IsAgent() bool { return m.agent != nil }

// resolveWeaveMember turns one roster entry into a member.
//
// A bare tool resolves to itself, unvalidated — exactly as every roster entry
// did before agents existed. An AGENT is validated, because naming one asserts
// a model, and a binding that cannot run is a configuration error the operator
// should hear about now rather than at failover time.
func resolveWeaveMember(name string) (weaveMember, error) {
	launch, err := weaveResolveAgent(name)
	if err != nil {
		return weaveMember{}, err
	}
	if launch == nil {
		return weaveMember{Name: name, Tool: name, Bin: name}, nil
	}
	if chk := fleetCatalog().VerifyModel(launch.ModelName, fleet.Probes(nil)); !chk.OK {
		return weaveMember{}, fmt.Errorf("agent %q: model %s: %s", name, launch.ModelName, chk.Reason)
	}
	return weaveMember{Name: name, Tool: launch.ToolName, Bin: launch.Tool, agent: launch}, nil
}

// resolveWeaveRoster resolves every entry, reporting the first that cannot run.
func resolveWeaveRoster(names []string) ([]weaveMember, error) {
	out := make([]weaveMember, 0, len(names))
	for _, n := range names {
		m, err := resolveWeaveMember(n)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// headlessArgs is the flag list this member launches with, prompt excluded.
//
// A tool's persisted profile wins when it exists: `fleet interview --live`
// repairs a drifted launch contract there, and discarding that repair to
// re-render from the registry would reintroduce the very flags the interview
// found broken. The model is then layered on by flag, which is why a tool
// declares the flag's spelling rather than only the whole template.
func (m weaveMember) headlessArgs() []string {
	var persisted []string
	if toolsDir, err := weaveToolsDir(); err == nil {
		if p, ok := loadToolProfile(toolsDir, m.Tool); ok {
			persisted = append(persisted, p.HeadlessArgs...)
		}
	}
	if m.agent == nil {
		if persisted != nil {
			return persisted
		}
		seed, _ := seededContract(m.Tool)
		return seed.HeadlessArgs
	}
	if persisted == nil {
		return append([]string(nil), m.agent.Args...)
	}
	flag := ""
	if t, ok := fleetCatalog().Tool(m.Tool); ok {
		flag = t.ModelFlag()
	}
	if flag == "" {
		// The template positions the model without a flag; it can only be
		// rendered whole, so the registry's argv wins over the profile.
		return append([]string(nil), m.agent.Args...)
	}
	return withModelFlag(persisted, flag, m.agent.Model)
}

// withModelFlag sets flag's value in args, replacing an existing occurrence
// rather than appending a second one.
func withModelFlag(args []string, flag, value string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == flag {
			out[i+1] = value
			return out
		}
	}
	return append(out, flag, value)
}

// agentNick is the member's nickname, or "" for a bare tool.
func (m weaveMember) agentNick() string {
	if m.agent == nil {
		return ""
	}
	return m.agent.Nick
}

// memberLogFields renders a member for the lease log: always the tool, plus
// the binding when one was named. Existing log readers keep finding `tool=`.
func memberLogFields(m weaveMember) string {
	s := "tool=" + m.Tool
	if b := m.Binding(); b != "" {
		s += " agent=" + m.Label() + " binding=" + b
	}
	return s
}
