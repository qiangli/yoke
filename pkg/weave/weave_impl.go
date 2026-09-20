package weave

// Minimum-viable weave subverb bodies. Backs add/list/next/start/pull/
// abandon on a per-repo JSON queue + git worktrees, with no Gitea or
// merger dependency. The shape matches the surface in weave_subverbs.go;
// the full v2 design (Gitea-backed queue, loom merger auto-merge, MCP
// collab verbs) supersedes this once N+1 group A/B lands. See
// docs/loom-v2-implementation.md for the broader plan.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/gate"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/telemetry"
	"github.com/qiangli/yoke/pkg/weave/memory"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
)

type weaveQueue struct {
	NextID int64        `json:"next_id"`
	Items  []*weaveItem `json:"items"`
	// OwnerNotices is the durable lifecycle outbox and shares queue.json's
	// atomic rename with the transition that created each event.
	NextOwnerNoticeID int64              `json:"next_owner_notice_id,omitempty"`
	OwnerNotices      []weaveOwnerNotice `json:"owner_notices,omitempty"`
	// Root is the repo the queue serves, stamped on writes. Queues
	// are keyed by a path-mangled tag that can't be reversed; Root
	// lets `weave list` name nearby queues in its empty-queue hint.
	Root string `json:"root,omitempty"`
	// PausedOrchestratorLease preserves the autopilot lease across a
	// campaign pause so resume can restore the same holder/tool lease.
	PausedOrchestratorLease *weaveOrchestratorLease `json:"paused_orchestrator_lease,omitempty"`
	// NextStoryID / Stories hold the STORY board — the epic/story
	// kanban above the task queue. A story is the durable unit a
	// conductor owns and hands off (lease + continuity record); the
	// task Items are its ephemeral workers. See weave_story.go.
	NextStoryID int64         `json:"next_story_id,omitempty"`
	Stories     []*weaveStory `json:"stories,omitempty"`
}

// weaveOtherActiveQueues scans sibling queue dirs for queues with
// non-terminal items — fuel for the "ran from the wrong directory"
// hint, the most common weave confusion in dogfooding.
func weaveOtherActiveQueues(currentDir string) []map[string]any {
	base := filepath.Dir(currentDir)
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, e := range entries {
		if !e.IsDir() || filepath.Join(base, e.Name()) == currentDir {
			continue
		}
		q, err := loadWeaveQueue(filepath.Join(base, e.Name()))
		if err != nil {
			continue
		}
		if !weaveQueueRootAvailable(q) {
			continue
		}
		active := 0
		for _, it := range q.Items {
			if !isTerminalState(it.State) {
				active++
			}
		}
		if active == 0 {
			continue
		}
		name := q.Root
		if name == "" {
			name = e.Name()
		}
		out = append(out, map[string]any{"root": name, "active": active})
	}
	return out
}

func weaveOtherActiveQueuesHintSuffix(currentDir string) string {
	others := weaveOtherActiveQueues(currentDir)
	if len(others) == 0 {
		return ""
	}
	sort.Slice(others, func(i, j int) bool {
		return fmt.Sprint(others[i]["root"]) < fmt.Sprint(others[j]["root"])
	})
	parts := make([]string, 0, len(others))
	for _, o := range others {
		parts = append(parts, fmt.Sprintf("%s (%d)", o["root"], o["active"]))
	}
	return " (queues are per-repo; active weaves exist for: " + strings.Join(parts, ", ") + ")"
}

type weaveItem struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	Body     string `json:"body,omitempty"`
	Priority string `json:"priority,omitempty"`
	// Points is the story-point estimate (Fibonacci 1,2,3,5,8) from
	// the optional sprint-planning phase; 8 maps to the ~30-minute
	// runtime cap — bigger work must be split before assignment.
	Points int    `json:"points,omitempty"`
	State  string `json:"state"`
	// Stage is the SDLC stage this work belongs to: plan | code | test | deploy
	// (the atlas vocabulary, minus "cross" — a VERB may serve every stage, a unit
	// of WORK cannot BE every stage). Empty reads as "code", which needs no
	// migration: every queue that existed before this field was, by construction, a
	// queue of coding issues. Without it the board could only ever describe the
	// middle of the lifecycle — "review the design" and "promote to staging" had
	// nowhere to live.
	Stage string `json:"stage,omitempty"`
	// Parent is the issue this one was split out of (0 = not a child). An item with
	// children is a CONTAINER: never claimed by an agent, its state derived from
	// theirs. `weave add --points` has always said "8 = ~30m cap, split bigger work"
	// while offering no split; the link to the parent used to be lost the moment you
	// hand-added the children.
	Parent int64 `json:"parent,omitempty"`
	// Register is the id of the `bashy issue` register entry this work implements
	// (empty for ad-hoc work). The register is the DURABLE, COMMITTED truth — it
	// travels with the repo; this queue is per-machine execution state. The back-ref
	// is what keeps them one system instead of two: when this item merges, the
	// register entry can be closed, and `issue show` can say what is in flight.
	Register string `json:"register,omitempty"`
	// DependsOn lists issues that must be DONE (merged) before this one may start.
	// This is the conductor's "schedule by parallel safety" rule, moved out of the
	// conductor's head and into the data — where a resumed conductor, or a second
	// agent, can actually see it.
	DependsOn []int64 `json:"depends_on,omitempty"`
	// Tool is the short (argv[0] basename) name of the CLI working
	// the issue — codex, claude, gemini, opencode, bash — recorded
	// at claim time, updated on resume.
	Tool string `json:"tool,omitempty"`
	// Owner is the named agent INSTANCE working the issue, distinct
	// from Tool (the CLI). Two agents on the same tool get distinct
	// owners (e.g. "codex-a", "claude-b") so the comment thread and
	// commit attribution stay legible and the conductor can address a
	// specific "who". Assigned at claim time (weaveAgentName); handed to
	// the subagent as $WEAVE_AGENT so it signs its own comments.
	Owner     string `json:"owner,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// legacyWorkspace reads the pre-rename queue.json key. The on-disk
	// isolation dir + this field were called "sandbox" before the
	// userland/workspace/sandbox/cluster taxonomy reserved "sandbox" for
	// podman/OCI containers. loadWeaveQueue migrates it into Workspace on
	// load (then it stops being written — omitempty), so existing queues
	// keep their stored clone paths without a migration step.
	LegacyWorkspace string `json:"sandbox,omitempty"`
	Branch          string `json:"branch,omitempty"`
	// BaseSHA is the base-branch commit captured in the WORKSPACE at
	// clone time. weaveMeasureBranch counts commits ahead against this
	// immutable sha rather than the base branch NAME — the workspace's
	// local "main" ref drifts (origin is removed; the root branch may
	// advance after clone), which is what made `weave status` miscount
	// "0 commits ahead" when the branch actually had a commit.
	BaseSHA string `json:"base_sha,omitempty"`
	// LaunchPhase is durable, operator-visible progress while a workspace is
	// being provisioned.  In particular, hydration can legitimately take a
	// while; leaving an item as todo until it finishes makes an active launch
	// indistinguishable from a forgotten one.
	LaunchPhase string    `json:"launch_phase,omitempty"`
	Created     time.Time `json:"created"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	// CommitsAhead and Head are the wrapper's own git measurement of
	// the workspace branch at terminal time (rev-list count over base,
	// plus HEAD). They are the EVIDENCE behind a submitted state for
	// a run that did not exit 0: state never says submitted unless
	// the substrate itself verified shippable commits, and the exit
	// code / kill reason are preserved untouched so nothing about how
	// the run ended is hidden.
	CommitsAhead int    `json:"commits_ahead,omitempty"`
	Head         string `json:"head,omitempty"`
	// Salvageable and UnmergedCommits are read-time projections. They are
	// recomputed from the workspace and the repository's current base branch by
	// list/status; a terminal label must never hide commits that have not landed.
	Salvageable     bool `json:"salvageable,omitempty"`
	UnmergedCommits int  `json:"unmerged_commits,omitempty"`
	// VerifyCommand is the substrate-verified outcome hook, supplied
	// at `weave add --verify "<cmd>"`. The WRAPPER runs it via
	// `bash -c` inside the workspace at terminal time — when the tool
	// exited 0 or left commits ahead — and records VerifyExit and
	// VerifyOutput (last 2000 bytes) as evidence. Verify never changes
	// the terminal state itself; `weave pull` refuses to merge
	// submitted items whose VerifyExit is set and non-zero.
	VerifyCommand  string    `json:"verify_command,omitempty"`
	VerifyExit     *int      `json:"verify_exit,omitempty"`
	VerifyOutput   string    `json:"verify_output,omitempty"`
	VerifyTree     string    `json:"verify_tree,omitempty"`
	ReviewVerdict  string    `json:"review_verdict,omitempty"`
	ReviewBlocking bool      `json:"review_blocking,omitempty"`
	ReviewNotes    string    `json:"review_notes,omitempty"`
	ReviewExit     int       `json:"review_exit,omitempty"`
	ReviewBy       string    `json:"review_by,omitempty"`
	ReviewAt       time.Time `json:"review_at,omitempty"`
	// CodingAgent and ReviewAgent make separation of duties auditable. The
	// reviewer is an acting pair: it may add evidence, while PairVerdict records
	// the harness/gate outcome (never a model approval).
	CodingAgent     string `json:"coding_agent,omitempty"`
	ReviewAgent     string `json:"review_agent,omitempty"`
	ReviewAddedTest bool   `json:"review_added_test,omitempty"`
	PairVerdict     string `json:"pair_verdict,omitempty"`
	PairReason      string `json:"pair_reason,omitempty"`
	PairExit        int    `json:"pair_exit,omitempty"`
	// Judge is the item's VERIFIABILITY TIER — how much evidence a merge needs
	// beyond the deterministic gate. "none" means the probe (build+test / suite /
	// clean-room verify) is sufficient on its own: work whose correctness a machine
	// can settle needs no LLM arbiter. This is the DEFAULT: an empty value reads as
	// none. "required" is retained as compatibility metadata for callers that
	// explicitly classify work; model review runs only when --review-agent is
	// supplied. It never relaxes a configured deterministic probe.
	Judge string `json:"judge,omitempty"`
	// Band is the issue's DIFFICULTY band (L1-L4; 0 = unpegged). It raises the
	// judge floor: a verdict-required merge needs a judge at max(L3, Band). When
	// unset, the difficulty is inferred from the coding agent's band (weaveIssueBand).
	Band            int    `json:"band,omitempty"`
	SuiteGate       string `json:"suite_gate,omitempty"`
	SuiteGateExit   *int   `json:"suite_gate_exit,omitempty"`
	SuiteGateOutput string `json:"suite_gate_output,omitempty"`
	Dirty           bool   `json:"dirty"`
	DirtyFiles      int    `json:"dirty_files,omitempty"`
	UntrackedFiles  int    `json:"untracked_files,omitempty"`
	AutoCommitted   bool   `json:"auto_committed,omitempty"`
	AutoCommitError string `json:"auto_commit_error,omitempty"`
	// CleanupError records the last failed release of a run-owned artifact
	// (the managed build cache today). It is set on the row rather than only
	// printed, because the transition that failed is over by the time anyone
	// reads stderr; hygiene and `sprint end` refuse on it.
	CleanupError string `json:"cleanup_error,omitempty"`
	// Disposition is the durable one-word outcome of the run's work
	// (merged | superseded | rejected | empty), set by the verb that decided
	// it; DispositionReason is the operator's short why; SalvageRef names the
	// ref that preserves a rejected/superseded tip. Together they are what
	// survives teardown — see weaveItemSettled.
	Disposition       string `json:"disposition,omitempty"`
	DispositionReason string `json:"disposition_reason,omitempty"`
	SalvageRef        string `json:"salvage_ref,omitempty"`
	ExitCode          *int   `json:"exit_code,omitempty"`
	KilledBy          string `json:"killed_by,omitempty"`
	// Completion records an explicit terminalization that did not come from the
	// agent process exiting. It is deliberately distinct from ExitCode: a
	// conductor may observe an interactive TUI return idle, but weave never
	// infers success from that prose or prompt alone.
	Completion     string    `json:"completion,omitempty"`
	FinalizerPID   int       `json:"finalizer_pid,omitempty"`
	FinalizingAt   time.Time `json:"finalizing_at,omitempty"`
	LogPath        string    `json:"log_path,omitempty"`
	Throttled      bool      `json:"throttled,omitempty"`
	ThrottleSignal string    `json:"throttle_signal,omitempty"`
	// WrapperPid is the PID of the `bashy weave start` process
	// supervising this item (NOT the subagent's PID — the wrapper
	// is the session leader after auto-setsid and signals propagate
	// from there to the whole subagent process group). Set when
	// state flips to working; cleared on terminal state. Used by
	// `weave abandon` for precise SIGTERM instead of pkill-by-name.
	WrapperPid               int    `json:"wrapper_pid,omitempty"`
	WrapperStartID           string `json:"wrapper_start_id,omitempty"` // OS birth identity, recorded at launch; absent on legacy runs
	PauseRequestedBy         string `json:"pause_requested_by,omitempty"`
	PauseReason              string `json:"pause_reason,omitempty"`
	ResourceTerminated       bool   `json:"resource_terminated,omitempty"`
	ResourceReservationID    string `json:"resource_reservation_id,omitempty"`
	ResourceReservationOwner string `json:"resource_reservation_owner,omitempty"`
	// Stale is computed at read time by `weave list` (never
	// persisted): state is "working" but the recorded wrapper PID
	// is no longer alive — the wrapper crashed or was killed
	// outside weave's control. Resume or abandon the item.
	Stale bool `json:"stale,omitempty"`
	// AgeSeconds is another read-time projection used by machine-wide boards.
	// It keeps those boards from spawning the mutating doctor command merely to
	// calculate an item's age.
	AgeSeconds int64 `json:"age_seconds,omitempty"`
	// CtlSock is the wrapper's per-issue control socket while the
	// subagent runs (PTY mode only). `weave say` connects here to
	// inject a line into the subagent's stdin. Set at claim time,
	// cleared on terminal state.
	CtlSock string `json:"ctl_sock,omitempty"`
	// LaunchSpec is the durable command/watchdog recipe needed to
	// relaunch the same worker in the same workspace after `weave pause`.
	LaunchSpec *weaveLaunchSpec `json:"launch_spec,omitempty"`
	// Comments is the append-only history thread on the issue: handoff
	// references, agent progress/decisions/blockers, conductor review
	// notes, and auto-recorded lifecycle events. It is what lets the
	// conductor reference a handoff doc in the issue and have the agent
	// append feedback to the thread instead of a human relaying notes.
	Comments []weaveComment `json:"comments,omitempty"`
	// Blocked is computed at read time (never persisted): the newest
	// comment is kind "blocker" and the item is still active — surfaces
	// "agent is waiting on input/decision" so it isn't mistaken for slow
	// progress. Cleared when a later non-blocker comment is appended.
	Blocked bool `json:"blocked,omitempty"`
	// LiveRoot / LiveTreeSHA / LiveTreeLines / LiveTreeTruncated are the
	// isolation baseline: the state of the LIVE checkout this workspace was
	// cloned FROM, fingerprinted at claim time. Workspace isolation is soft
	// (own clone, own cwd — but no fence against `cd ..` or an absolute
	// path), and an agent that escapes corrupts the human's tree and leaves
	// a branch that no longer describes the run. Re-checked at submit /
	// status / list / pull; see weave_isolation.go.
	LiveRoot          string   `json:"live_root,omitempty"`
	LiveTreeSHA       string   `json:"live_tree_sha,omitempty"`
	LiveTreeLines     []string `json:"live_tree_lines,omitempty"`
	LiveTreeTruncated bool     `json:"live_tree_truncated,omitempty"`
	// IsolationViolated records that the live checkout moved while this run
	// held its workspace, with EscapedPaths naming what differs. Sticky
	// once set (a run that tidied up after itself still escaped) and, at
	// read time, recomputed for display like Stale/Blocked. `weave pull`
	// refuses these without --force.
	IsolationViolated bool     `json:"isolation_violated,omitempty"`
	EscapedPaths      []string `json:"escaped_paths,omitempty"`
	// OutsideWorkspacePaths is an advisory extracted from the PTY log when a
	// worker exits without commit evidence. It does not prove a write occurred
	// and never blocks a merge; it points operators at likely wrong-directory work.
	OutsideWorkspacePaths []string `json:"outside_workspace_paths,omitempty"`
	// Salvageable and NeedsSteward are the REAPER's durable flags — the two
	// places the lifecycle ends in a decision a machine may not make for you
	// (see weave_reaper.go). Unlike Stale/Blocked they ARE persisted: the
	// whole failure they fix is that "somebody must decide this" existed only
	// for as long as one command's output was on screen.
	//
	// NeedsSteward: submitted, past the steward threshold, with no merge;
	// StewardReason names the decision that is owed. (Salvageable — this
	// stopped run is sitting on committed work — is declared above with
	// UnmergedCommits, the projection it is measured alongside.)
	NeedsSteward  bool   `json:"needs_steward,omitempty"`
	StewardReason string `json:"steward_reason,omitempty"`

	CoachTotalCalls    int     `json:"coach_total_calls,omitempty"`
	CoachDistinctCalls int     `json:"coach_distinct_calls,omitempty"`
	CoachRepeatRatio   float64 `json:"coach_repeat_ratio,omitempty"`
	CoachSteers        int     `json:"coach_steers,omitempty"`
	CoachRecovered     bool    `json:"coach_recovered,omitempty"`
	CoachMode          string  `json:"coach_mode,omitempty"`
}

// weaveComment is one entry in an issue's history thread. Append-only;
// Author is the agent Owner name, "conductor", or "human"; Kind is one
// of note|progress|blocker|decision|review|system.
type weaveComment struct {
	At     time.Time `json:"at"`
	Author string    `json:"author,omitempty"`
	Kind   string    `json:"kind,omitempty"`
	Body   string    `json:"body"`
}

type weaveLaunchSpec struct {
	Tool        string        `json:"tool"`
	Argv        []string      `json:"argv,omitempty"`
	Agent       string        `json:"agent,omitempty"` // nickname, when started by agent name
	Model       string        `json:"model,omitempty"` // provider-side id actually selected
	MaxRuntime  time.Duration `json:"max_runtime,omitempty"`
	MemLimit    string        `json:"mem_limit,omitempty"`
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"`
	PTY         string        `json:"pty,omitempty"`
}

// weaveAppendComment appends one entry to an issue's history thread.
// Empty body is a no-op. Kind defaults to "note", author to "conductor".
func weaveAppendComment(it *weaveItem, author, kind, body string) {
	body = strings.TrimSpace(body)
	if it == nil || body == "" {
		return
	}
	if kind == "" {
		kind = "note"
	}
	if author == "" {
		author = "conductor"
	}
	it.Comments = append(it.Comments, weaveComment{
		At:     time.Now().UTC(),
		Author: author,
		Kind:   kind,
		Body:   body,
	})
}

// weaveComputeBlocked sets it.Blocked when the newest comment is a
// "blocker" and the item is still active (not terminal). Read-time only.
func weaveComputeBlocked(it *weaveItem) {
	it.Blocked = false
	if isTerminalState(it.State) || len(it.Comments) == 0 {
		return
	}
	if it.Comments[len(it.Comments)-1].Kind == "blocker" {
		it.Blocked = true
	}
}

// weaveAgentName derives the named agent INSTANCE handle for an issue:
// the tool's display name plus a short per-issue suffix (a..z by id),
// e.g. "codex-a", "claude-c". Distinguishes multiple agents on the same
// tool in the thread + commit attribution; handed over as $WEAVE_AGENT.
func weaveAgentName(tool string, id int64) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		tool = "agent"
	}
	suffix := string(rune('a' + int((id-1+26)%26)))
	return tool + "-" + suffix
}

// weaveCtlSockPath returns the per-issue control socket path,
// falling back to the temp dir when the queue-dir path would
// exceed the unix socket path limit (104 bytes on darwin).
func weaveCtlSockPath(dir string, id int64) string {
	p := filepath.Join(dir, "ctl", fmt.Sprintf("issue-%d.sock", id))
	if len(p) <= 100 {
		return p
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(dir))
	return filepath.Join(os.TempDir(), fmt.Sprintf("ycode-weave-%x-issue-%d.sock", h.Sum32(), id))
}

// Terminal states for queue items — used by `weave wait` and similar
// orchestrator-side polling. "submitted" means the subagent exited
// cleanly and weave measured committed work ready to be merged by `weave pull`.
// "no-op" means the subagent exited cleanly but produced no diff evidence.
// "failed" means the subagent exited non-zero (or left an uncommitted tree).
func isTerminalState(s string) bool {
	switch s {
	case "submitted", "no-op", "failed", "killed", "done", "abandoned":
		return true
	}
	return false
}

// isPrunableState reports whether `weave prune` will sweep an item in
// this state: the terminal states that leave a reclaimable workspace
// behind. "submitted" is excluded — it's awaiting `weave pull` — unless
// reconciliation has already flipped it to "done" (see
// weaveReconcileMerged).
func isPrunableState(s string) bool {
	switch s {
	case "done", "abandoned", "no-op", "failed", "killed":
		return true
	}
	return false
}

// weaveItemVisibleInList reports whether an item belongs in the default
// (active) list. Failed and killed runs remain actionable only while their
// workspace still exists: that clone is what can be inspected, resumed, or
// salvaged. Once it is gone, keeping the terminal record in the active table
// creates a ghost row with no possible next action. --history still shows the
// durable record.
func weaveItemVisibleInList(it *weaveItem, includeHistory bool) bool {
	if it == nil || includeHistory {
		return it != nil
	}
	switch it.State {
	case "done", "abandoned", "no-op":
		return false
	case "failed", "killed":
		return weaveWorkspacePresent(it)
	default:
		return true
	}
}

// weaveRunConsumesCapacity reports whether a queue item still occupies an
// active execution slot. Finalizing items remain live until the conductor
// records the measured terminal state, so they still count here.
func weaveRunConsumesCapacity(s string) bool {
	switch s {
	case "todo", "allocated", "working", "finalizing":
		return true
	}
	return false
}

func weaveRepoRoot(cwd string) (string, error) {
	out, err := exec.Command(gitBin(), "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repo (run from a clone): %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func weaveBaseBranch(root string) string {
	for _, b := range []string{"main", "master"} {
		if err := exec.Command(gitBin(), "-C", root, "rev-parse", "--verify", "refs/heads/"+b).Run(); err == nil {
			return b
		}
	}
	return "HEAD"
}

// weaveStateRoot is the AgentOS weave state directory (~/.bashy/weave).
// weave was re-homed out of ycode into the hub and is driven by the AgentOS
// shell (`bashy weave`), so it gets its own top-level dotdir rather than
// living under ycode's. weaveLegacyStateRoots are pre-re-home locations,
// consulted only as one-time migration sources (newest-rename first).
func weaveStateRoot(home string) string { return filepath.Join(home, ".bashy", "weave") }

// StateRoot is the weave state directory for the current user (~/.bashy/weave),
// exported so a resource map can name it without recomputing it. Empty when no
// home directory can be determined.
func StateRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return weaveStateRoot(home)
}
func weaveLegacyStateRoots(home string) []string {
	return []string{
		filepath.Join(home, ".agents", "weave"),          // interim host-agnostic root
		filepath.Join(home, ".agents", "ycode", "weave"), // original ycode-hosted root
	}
}

// weaveWorkspaceOwner reports the weave root that OWNS repoRoot, when repoRoot
// is itself a weave workspace clone.
//
// The queue dir is keyed on the repo PATH (see weaveQueueDir), so a weave
// command run from INSIDE a workspace used to hash that clone's path and mint a
// brand-new root — one per clone, none of which ever held a queue, because the
// real queue stayed in the root that dispatched the run. That is how
// ~/.bashy/weave accumulated 237 empty directories named issue-N-<hash> and
// <repo>-<hash>: every agent that ran `bashy weave ...` inside its own
// workspace forked a root instead of finding the one it belonged to.
//
// A workspace always lives at <stateRoot>/<tag>/{workspaces,sandboxes}/<name>,
// so the owner is recoverable from the path alone — no origin lookup, and no
// need to breach the containment rule that keeps the origin path out of the tag.
// Legacy roots are included: a workspace under one still belongs to its queue.
func weaveWorkspaceOwner(home, repoRoot string) (string, bool) {
	roots := append([]string{weaveStateRoot(home)}, weaveLegacyStateRoots(home)...)
	clean := filepath.Clean(repoRoot)
	for _, root := range roots {
		rel, err := filepath.Rel(filepath.Clean(root), clean)
		if err != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(rel, string(filepath.Separator))
		// <tag>/<workspaces|sandboxes>/<name>[/...]
		if len(parts) < 3 {
			continue
		}
		if parts[1] != "workspaces" && parts[1] != "sandboxes" {
			continue
		}
		owner := filepath.Join(root, parts[0])
		if st, err := os.Stat(owner); err == nil && st.IsDir() {
			return owner, true
		}
	}
	return "", false
}

// weaveQueueNames derives the current and pre-hash queue directory names for a
// repository. It is deliberately pure: read paths use the same identity as
// weaveQueueDir without inheriting its migration and mkdir side effects.
func weaveQueueNames(repoRoot string) (tag, legacy string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repoRoot))
	tag = fmt.Sprintf("%s-%08x", filepath.Base(repoRoot), h.Sum32())

	r := strings.NewReplacer(string(filepath.Separator), "_", ":", "_")
	legacy = r.Replace(strings.TrimPrefix(repoRoot, string(filepath.Separator)))
	if len(legacy) > 120 {
		legacy = legacy[len(legacy)-120:]
	}
	return tag, legacy
}

func weaveCanonicalRepoRoot(repoRoot string) string {
	if root, err := weaveRepoRoot(repoRoot); err == nil {
		return root
	}
	return filepath.Clean(repoRoot)
}

func weaveExistingLegacyQueue(home, repoRoot string) (string, error) {
	tag, legacy := weaveQueueNames(repoRoot)
	for _, root := range weaveLegacyStateRoots(home) {
		for _, name := range []string{tag, legacy} {
			candidate := filepath.Join(root, name)
			if st, err := os.Stat(candidate); err == nil && st.IsDir() {
				return candidate, nil
			} else if err != nil && !os.IsNotExist(err) {
				return "", err
			}
		}
	}
	return "", nil
}

// weaveQueueDir resolves a queue path without creating or migrating anything.
// This is the default because board, inbox, fleet, and list all resolve queue
// paths while rendering read-only snapshots. A read must not turn a repository
// with no weave state into one merely by looking. Creation belongs beside the
// write; see ensureWeaveQueueDir, weaveWriteFile, and weaveAppendFile.
func weaveQueueDir(repoRoot string) (string, error) {
	repoRoot = weaveCanonicalRepoRoot(repoRoot)
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if owner, ok := weaveWorkspaceOwner(home, repoRoot); ok {
		return owner, nil
	}

	tag, _ := weaveQueueNames(repoRoot)
	dir := filepath.Join(weaveStateRoot(home), tag)
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	legacyDir, err := weaveExistingLegacyQueue(home, repoRoot)
	if err != nil {
		return "", err
	}
	if legacyDir != "" {
		return legacyDir, nil
	}
	return dir, nil
}

// ensureWeaveQueueDir is the explicit write-side resolver. Existing legacy
// queues remain in place: renaming one can strand absolute workspace/log/socket
// paths and let a cached writer recreate the old root, splitting the queue.
// A fresh writer creates only the canonical root selected by weaveQueueDir.
func ensureWeaveQueueDir(repoRoot string) (string, error) {
	dir, err := weaveQueueDir(repoRoot)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func ensureWeaveQueueDirPath(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// weaveEnsureQueueDir creates a queue dir on behalf of a caller that is about to
// write into an already-resolved path. Read paths must never call this.
func weaveEnsureQueueDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func weaveWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func weaveAppendFile(path string, perm os.FileMode) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, perm)
}

func loadWeaveQueue(dir string) (*weaveQueue, error) {
	b, err := os.ReadFile(filepath.Join(dir, "queue.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &weaveQueue{NextID: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var q weaveQueue
	if err := json.Unmarshal(b, &q); err != nil {
		return nil, fmt.Errorf("queue parse: %w", err)
	}
	if q.NextID == 0 {
		q.NextID = 1
	}
	// Back-compat: migrate the pre-rename "sandbox" key into Workspace for
	// queues written before the sandbox→workspace taxonomy rename. The
	// stored clone path is absolute, so old items keep working; clearing
	// the legacy field means it stops being written on the next save.
	for i := range q.Items {
		if q.Items[i].Workspace == "" && q.Items[i].LegacyWorkspace != "" {
			q.Items[i].Workspace = q.Items[i].LegacyWorkspace
		}
		q.Items[i].LegacyWorkspace = ""
	}
	// Priority-first is the sprint execution invariant, including for cards
	// written before the explicit execution-policy field existed. Story priority
	// remains the only ordering source; this flag records that the policy applies.
	for i := range q.Stories {
		// Sprint-level review was an unenforced duplicate of story submission:
		// no command entered it automatically and it behaved exactly like doing.
		// Keep existing cards active when reading queues written before the
		// column was removed; the next ordinary write persists the canonical
		// value without making a read-only command mutate disk.
		if q.Stories[i].Column == "review" {
			q.Stories[i].Column = "doing"
		}
		q.Stories[i].Execution.PriorityFirst = true
	}
	return &q, nil
}

func saveWeaveQueue(dir string, q *weaveQueue) error {
	if err := ensureWeaveQueueDirPath(dir); err != nil {
		return err
	}
	if q.Root == "" {
		// Best-effort back-stamp for queues created before Root
		// existed; saveWeaveQueue callers all run from the repo.
		if cwd, err := os.Getwd(); err == nil {
			if root, err := weaveRepoRoot(cwd); err == nil {
				q.Root = root
			}
		}
	}
	path := filepath.Join(dir, "queue.json")
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(q, "", "  ")
	if err != nil {
		return err
	}
	if err := weaveWriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// weaveStartedCol renders the subagent's start time for the list
// table: clock time for today, date for older, "-" when the item
// never started (or predates the started_at field).
// weaveTildePath abbreviates the user's home prefix to ~ for table
// display; JSON output keeps absolute paths.
func weaveTildePath(p string) string {
	if p == "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

func weaveStartedCol(it *weaveItem) string {
	if it.StartedAt.IsZero() {
		return "-"
	}
	t := it.StartedAt.Local()
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04:05")
	}
	return t.Format("Jan02")
}

// weaveDurationCol renders elapsed run time: live (now-started) for
// working items, started→finished for terminal ones, "-" otherwise.
func weaveDurationCol(it *weaveItem) string {
	if it.StartedAt.IsZero() {
		return "-"
	}
	var d time.Duration
	switch {
	case it.State == "working":
		d = time.Since(it.StartedAt)
	case !it.FinishedAt.IsZero():
		d = it.FinishedAt.Sub(it.StartedAt)
	default:
		return "-"
	}
	if d < 0 {
		return "-"
	}
	d = d.Round(time.Second)
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// weaveCountRef picks the ref to count commits-ahead against: the
// item's immutable BaseSHA (captured at clone) when available, else the
// base branch name. Using the sha is what fixes the "0 commits ahead"
// miscount — the workspace's local base branch ref can drift, but the
// clone-point sha is fixed and the agent's commits always descend from it.
func weaveCountRef(it *weaveItem, base string) string {
	if it != nil && it.BaseSHA != "" {
		return it.BaseSHA
	}
	return base
}

// weaveMeasureBranch is the wrapper's first-hand git interrogation of
// a workspace at terminal time: commits ahead of base and the HEAD sha.
// This — not the tool's exit code, and never an agent's claim — is
// what qualifies a non-zero exit for the submitted state. `base` may be
// a branch name OR a commit sha (see weaveCountRef).
func weaveMeasureBranch(workspace, base string) (ahead int, head string) {
	if workspace == "" {
		return 0, ""
	}
	if out, err := exec.Command(gitBin(), "-C", workspace, "rev-list", "--count", base+"..HEAD").Output(); err == nil {
		ahead, _ = strconv.Atoi(strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(gitBin(), "-C", workspace, "rev-parse", "HEAD").Output(); err == nil {
		head = strings.TrimSpace(string(out))
	}
	return ahead, head
}

// weaveUnmergedAhead is THE measurement of "work this run holds that is not on
// the base branch": the number of commits in the run's workspace that the user
// repo cannot reach from base, plus the workspace HEAD sha.
//
// ONE FUNCTION, ON PURPOSE. `weave list` used to compute this and print
// "SALVAGEABLE: #169 ... hold committed work not merged to the base branch"
// while `weave pull`, one line later in the same session, printed "nothing to
// merge" for that same run — because pull never asked. Two code paths that
// answer the same question separately WILL drift, and when the question is
// "does this run still hold work", the drift reads as "the work is gone" and
// the reasonable next action is `weave abandon`. So every verb that reports on,
// refuses, or destroys held work measures through here: list (via
// weaveClassifySalvageable), pull's refusal path, prune's and abandon's guards.
//
// It deliberately interrogates Git instead of trusting CommitsAhead: the
// wrapper that records terminal evidence may be the process that was killed
// after the agent commit.
//
// Counting each workspace commit against the root is slightly more work than a
// single rev-list count, but it is exact when main has advanced or absorbed only
// part of a branch. The root clone knows every commit that has actually landed;
// an unknown object and a known-but-unmerged object both correctly count as
// unmerged — the conservative direction for a guard that stands in front of
// `rm -rf`.
func weaveUnmergedAhead(root, base string, it *weaveItem) (ahead int, head string) {
	if it == nil || it.Workspace == "" {
		return 0, ""
	}
	if st, err := os.Stat(it.Workspace); err != nil || !st.IsDir() {
		return 0, ""
	}
	if out, err := exec.Command(gitBin(), "-C", it.Workspace, "rev-parse", "HEAD").Output(); err == nil {
		head = strings.TrimSpace(string(out))
	}
	ref := weaveCountRef(it, base)
	out, err := exec.Command(gitBin(), "-C", it.Workspace, "rev-list", ref+"..HEAD").Output()
	if err != nil {
		return 0, head
	}
	for _, sha := range strings.Fields(string(out)) {
		if exec.Command(gitBin(), "-C", root, "merge-base", "--is-ancestor", sha, base).Run() != nil {
			ahead++
		}
	}
	return ahead, head
}

// weaveClassifySalvageable reports committed work held by a TERMINAL run —
// weaveUnmergedAhead plus the state gate that decides whether `weave salvage`
// is the verb that applies.
func weaveClassifySalvageable(root, base string, it *weaveItem) (bool, int) {
	if it == nil || (it.State != "killed" && it.State != "failed" && it.State != "submitted") {
		return false, 0
	}
	unmerged, _ := weaveUnmergedAhead(root, base, it)
	return unmerged > 0, unmerged
}

// weaveSalvageableState reports whether `weave salvage <id>` accepts a run in
// this state (see runWeaveSalvage's switch). It is what lets a refusal name the
// verb that WOULD work instead of leaving the operator to guess — and guessing,
// from a message shaped like absence, means `weave abandon`.
func weaveSalvageableState(state string) bool {
	switch state {
	case "killed", "failed", "submitted", "working":
		return true
	}
	return false
}

func weaveAnnotateSalvageable(root, base string, it *weaveItem) {
	it.Salvageable, it.UnmergedCommits = weaveClassifySalvageable(root, base, it)
}

// weaveItemMerged reports whether an item's work is already contained
// in the base branch — its recorded terminal HEAD sha (or the workspace's
// live HEAD, as a fallback) is an ancestor of base in the USER repo.
// This is git's own truth, independent of the recorded queue State, and
// is what lets the lifecycle verbs detect a "submitted" item that landed
// in main by some route other than `weave pull` (a manual merge, a peer
// weave that absorbed it, the same commits fetched and merged earlier).
//
// It is deliberately conservative: it answers yes ONLY when that exact
// commit object is reachable from base in the user repo. A branch that
// was never fetched (its commits absent from the user repo) reads as
// not-merged — the safe answer, since nothing has actually landed. We
// check the workspace HEAD, never `git branch -d` against the user repo:
// agent branches live only in the workspace clone and are never fetched
// unless `weave pull` ran, so a branch-name check is a near-permanent
// no-op (the bug this replaces).
func weaveItemMerged(root, base string, it *weaveItem) bool {
	if root == "" || base == "" {
		return false
	}
	// "Merged" means work that LANDED, not an empty run. An item with no
	// commits ahead of base has a HEAD equal to the base commit, which
	// is trivially its own ancestor — without this guard every clean
	// zero-commit run would read as merged (and a "submitted" one would
	// get reconciled to done and vanish from the list). CommitsAhead is
	// the wrapper's terminal-time measurement; 0 means nothing to merge,
	// which is "empty", not "merged".
	//
	// MEASURED, not merely recorded. Both of the inputs below used to be read
	// straight off the queue — and both are written by the WRAPPER at terminal
	// time, so a tool that crashes on its way out (or commits a moment after the
	// wrapper measured) leaves them describing a run that no longer exists:
	// CommitsAhead 0 while the branch carries the feature, and Head still pointing
	// at the base commit the workspace was forked from.
	//
	// The consequence was quiet and annoying rather than dangerous: work that HAD
	// landed in base went on reading as `submitted` forever, because the guard
	// short-circuited on a stale zero and never got as far as asking git.
	//
	// This is the fifth place in this file that had to learn the same thing, so it
	// is written down once more: a record written by a process that did not
	// survive is not evidence of absence. If the artifact is on disk, ask the
	// artifact.
	ahead, liveHead := it.CommitsAhead, ""
	if it.Workspace != "" {
		if st, err := os.Stat(it.Workspace); err == nil && st.IsDir() {
			ahead, liveHead = weaveMeasureBranch(it.Workspace, weaveCountRef(it, base))
		}
	}
	if ahead <= 0 {
		return false
	}
	sha := liveHead
	if sha == "" {
		sha = it.Head
	}
	if sha == "" {
		return false
	}
	// `merge-base --is-ancestor A B` exits 0 iff A is an ancestor of B.
	// A missing object exits 128; a known-but-unmerged sha exits 1 —
	// both mean "not merged" for our purposes.
	return exec.Command(gitBin(), "-C", root, "merge-base", "--is-ancestor", sha, base).Run() == nil
}

// weaveReconcileMerged flips any "submitted" item whose work is already
// merged into base (per weaveItemMerged) to "done", so the recorded
// State stops contradicting git reality. Returns the count of items
// changed. Callers holding the queue lock (pull, prune) persist the
// change; the read-only list path uses it for display only.
func weaveReconcileMerged(root, base string, q *weaveQueue) int {
	n := 0
	for _, it := range q.Items {
		if it.State == "submitted" && weaveItemMerged(root, base, it) {
			it.State = "done"
			it.Disposition = weaveDispositionMerged
			n++
		}
	}
	return n
}

// weaveMeasureDirtiness records tracked working-tree changes
// separately from untracked litter. The pull safety gate cares about
// tracked changes because they can make verify attest bytes that HEAD
// will not merge; untracked files are preserved as context only.
func weaveMeasureDirtiness(workspace string) (dirty bool, dirtyFiles, untrackedFiles int) {
	if workspace == "" {
		return false, 0, 0
	}
	out, err := exec.Command(gitBin(), "-C", workspace, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		return false, 0, 0
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "?? ") {
			untrackedFiles++
			continue
		}
		dirtyFiles++
	}
	return dirtyFiles > 0, dirtyFiles, untrackedFiles
}

// weaveRunVerify executes an item's verify command via `bash -c` in
// the workspace with a 10-minute ceiling, returning the exit code and
// the last 2000 bytes of combined output. Like weaveMeasureBranch,
// this is the wrapper's own measurement — claims are measured by
// weave, not asserted by agents. A timeout or signal death surfaces
// as a non-zero exit; the decision about what to do with a failing
// verify belongs to `weave pull`, never to this function.
// weaveSiblingReplaceDirs scans a repo's go.mod for relative replace targets
// (`replace … => ../NAME/…`) and returns the distinct SIBLING REPOS they live
// in — i.e. `../NAME`, never the nested path beneath it. Empty when there is no
// go.mod or no relative replaces (the generic, non-Go case is a no-op). Handles
// both the single-line and `replace ( … )` block forms.
//
// Collapsing to the top-level sibling is the whole point, and getting it wrong
// was a real bug. A replace may point INSIDE a sibling:
//
//	replace github.com/qiangli/yoke/external/otel => ../coreutils/external/otel
//	replace github.com/qiangli/yoke/pkg/oci      => ../coreutils/pkg/oci
//	replace go.podman.io/podman/v6                    => ../coreutils/external/podman/src
//
// These are NOT siblings — they are subdirectories of one sibling (`coreutils`),
// and cloning `coreutils` satisfies all of them, because `../coreutils/pkg/oci`
// resolves through the clone. The old code took filepath.Base(rel), so it tried
// to provision "otel", "oci" and "src" as top-level repos. That produced a junk
// top-level `src` clone (and two different replaces collided on that one name),
// and — worse — it FAILED on `oci` (a plain directory, not a git repo) and told
// the agent:
//
//	WARNING could not provision sibling dep "oci" — the build may fail
//
// The build was never going to fail. A false "your build is broken" is the most
// expensive kind of wrong thing to say to an agent: it sends it chasing a ghost.
func weaveSiblingReplaceDirs(root string) []string {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil
	}
	var dirs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		idx := strings.Index(line, "=>")
		if idx < 0 {
			continue
		}
		rhs := strings.TrimSpace(line[idx+2:])
		// rhs is "../path" or "../path version"; take the first field.
		if sp := strings.IndexAny(rhs, " \t"); sp >= 0 {
			rhs = rhs[:sp]
		}
		if !strings.HasPrefix(rhs, "../") {
			continue
		}
		// Collapse to the top-level sibling: "../coreutils/external/otel" → "../coreutils".
		parts := strings.Split(filepath.ToSlash(rhs), "/")
		if len(parts) < 2 || parts[1] == "" || parts[1] == ".." {
			continue // "../" alone, or a grandparent hop we don't model
		}
		sib := "../" + parts[1]
		if !seen[sib] {
			seen[sib] = true
			dirs = append(dirs, sib)
		}
	}
	return dirs
}

// weaveSyncSiblingDeps provisions the target repo's `replace … => ../X` sibling
// dependencies for the workspace: each is a SHARED clone placed at
// <workspaces>/<name> (the path `../<name>` resolves to from every issue
// workspace), re-synced to the original sibling's current HEAD. No symlinks
// (Windows-safe); the user's real repo is never edited (it's a clone). Returns
// the names successfully synced and any that failed (so the caller can warn).
func weaveSyncSiblingDeps(root, workspace string) (synced, failed []string) {
	rels := weaveSiblingReplaceDirs(root)
	if len(rels) == 0 {
		return nil, nil
	}
	workspaceParent := filepath.Dir(workspace) // <queueDir>/workspaces
	for _, rel := range rels {
		name := filepath.Base(rel)
		orig := filepath.Clean(filepath.Join(root, rel))
		if fi, err := os.Stat(orig); err != nil || !fi.IsDir() {
			continue // sibling not present next to the source; skip quietly
		}
		dst := filepath.Join(workspaceParent, name)
		if err := weaveEnsureSyncedClone(orig, dst); err != nil {
			failed = append(failed, name)
			continue
		}
		synced = append(synced, name)
	}
	return synced, failed
}

// weaveEnsureSyncedClone makes dst a local clone of the git repo at orig, reset
// to orig's current HEAD (and cleaned of any stray edits a prior worker left,
// so the shared clone is always a faithful copy of the source). Clones on first
// use; otherwise fetches + hard-resets — cheap for a `--local` clone.
func weaveEnsureSyncedClone(orig, dst string) error {
	head, err := exec.Command(gitBin(), "-C", orig, "rev-parse", "HEAD").Output()
	if err != nil {
		return fmt.Errorf("resolve HEAD of %s: %w", orig, err)
	}
	sha := strings.TrimSpace(string(head))
	gitDir := filepath.Join(dst, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		_ = os.RemoveAll(dst)
		if out, err := exec.Command(gitBin(), "clone", "--local", "--no-hardlinks", orig, dst).CombinedOutput(); err != nil {
			return fmt.Errorf("clone sibling %s: %w: %s", orig, err, out)
		}
	}
	// Bring orig's latest objects over and pin dst to orig's HEAD.
	_ = exec.Command(gitBin(), "-C", dst, "fetch", "--quiet", orig).Run()
	if out, err := exec.Command(gitBin(), "-C", dst, "reset", "--hard", "--quiet", sha).CombinedOutput(); err != nil {
		return fmt.Errorf("sync sibling %s to %.12s: %w: %s", dst, sha, err, out)
	}
	_ = exec.Command(gitBin(), "-C", dst, "clean", "-qfdx").Run()
	return nil
}

// weaveHydrateSubmodules populates the workspace's git submodules from the LOCAL
// origin — offline, fast, and local-first — instead of their network URLs.
//
// `git clone --local` (how a workspace is made) does NOT recurse submodules, so a
// repo whose go.mod `replace`s point into a submodule can't build until the
// submodule working trees exist. coreutils is exactly this case
// (external/ollama/src, external/podman/src), and an empty submodule was the
// blocker that stopped it from self-repairing: the full gate died reading a
// missing external/ollama/src/go.mod.
//
// For each declared submodule whose tree the origin already has, we point the
// submodule URL at the origin's populated working tree and update from there;
// protocol.file.allow=always is required because git blocks the file:// transport
// for submodules by default (CVE-2022-39253). A submodule the origin lacks is
// left to its .gitmodules URL (network fallback). Afterwards `submodule sync`
// resets the URLs to their canonical .gitmodules values, so the local-origin path
// is not left as a breadcrumb an escaping agent could follow back (mirroring the
// `git remote remove origin` isolation above).
const weaveProvisioningTimeout = 2 * time.Minute

func weaveHydrateSubmodules(root, workspace string, out, errw io.Writer) error {
	if _, err := os.Stat(filepath.Join(workspace, ".gitmodules")); err != nil {
		return nil // no submodules — nothing to hydrate
	}
	ctx, cancel := context.WithTimeout(context.Background(), weaveProvisioningTimeout)
	defer cancel()

	anyLocal := false
	// Configure local origin URL overrides for submodules at all levels.
	_ = filepath.WalkDir(workspace, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.Name() != ".gitmodules" {
			return nil
		}
		subDir := filepath.Dir(p)
		listed, err := exec.Command(gitBin(), "-C", subDir, "config", "-f", ".gitmodules",
			"--get-regexp", `^submodule\..*\.path$`).Output()
		if err != nil {
			return nil
		}
		relSubDir, err := filepath.Rel(workspace, subDir)
		if err != nil || relSubDir == "." {
			relSubDir = ""
		}
		for _, line := range strings.Split(strings.TrimSpace(string(listed)), "\n") {
			key, path, ok := strings.Cut(strings.TrimSpace(line), " ")
			if !ok {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
			relPath := filepath.Join(relSubDir, path)
			localSrc := filepath.Join(root, relPath)
			if _, err := os.Stat(filepath.Join(localSrc, ".git")); err == nil {
				_ = exec.Command(gitBin(), "-C", subDir, "config", "submodule."+name+".url", localSrc).Run()
				anyLocal = true
			}
		}
		return nil
	})

	// A local checkout must never wait forever on a broken submodule transport.
	// The queue has already recorded the hydration phase before reaching here,
	// so timeout failure is both bounded and diagnosable by the conductor.
	up := exec.CommandContext(ctx, gitBin(), "-C", workspace, "-c", "protocol.file.allow=always",
		"submodule", "update", "--init", "--recursive")
	up.Stdout, up.Stderr = out, errw
	if err := up.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git submodule update --init timed out after 2m: %w", ctx.Err())
		}
		return fmt.Errorf("git submodule update --init: %w", err)
	}
	if anyLocal {
		// Restore canonical .gitmodules URLs recursively so local-origin paths are not
		// left as breadcrumbs in any submodule config.
		_ = exec.CommandContext(ctx, gitBin(), "-C", workspace, "submodule", "sync", "--recursive").Run()
	}
	// Hydration runs before an agent exists, so it must leave the freshly
	// allocated workspace clean. In particular, nested submodule checkout
	// helpers can leave generated or untracked artifacts behind; those make the
	// wrapper's later auto-commit fail even when the agent changed no source.
	// This is safe here because the workspace has not been handed to an agent.
	clean := exec.CommandContext(ctx, gitBin(), "-C", workspace, "submodule", "foreach", "--recursive",
		"git reset --hard --quiet && git clean -ffdx -q")
	clean.Stdout, clean.Stderr = out, errw
	if err := clean.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("clean hydrated submodules timed out after 2m: %w", ctx.Err())
		}
		return fmt.Errorf("clean hydrated submodules: %w", err)
	}
	// A nested clean can change parent submodules' gitlink markers and status caches.
	// Reset all submodules recursively after cleaning so parent submodules update
	// their gitlink markers after child submodules are cleaned.
	resetSubs := exec.CommandContext(ctx, gitBin(), "-C", workspace, "submodule", "foreach", "--recursive",
		"git reset --hard --quiet")
	resetSubs.Stdout, resetSubs.Stderr = out, errw
	if err := resetSubs.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("reset hydrated submodules timed out after 2m: %w", ctx.Err())
		}
		return fmt.Errorf("reset hydrated submodules: %w", err)
	}
	// Reset the superproject too, which only restores its recorded gitlinks and never
	// touches user work because this clone is still pre-agent.
	reset := exec.CommandContext(ctx, gitBin(), "-C", workspace, "reset", "--hard", "--quiet")
	reset.Stdout, reset.Stderr = out, errw
	if err := reset.Run(); err != nil {
		return fmt.Errorf("reset hydrated workspace: %w", err)
	}
	return nil
}

// weaveMarkLaunchFailed makes provisioning failures durable.  A timed out
// clone/hydration used to return while the item still looked todo, leaving a
// conductor with no observable worker or actionable terminal evidence.
func weaveMarkLaunchFailed(dir string, issueID int64, cause error) {
	_ = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		if it := findWeaveItem(q, issueID); it != nil {
			it.State = "failed"
			it.LaunchPhase = "failed: " + cause.Error()
			it.FinishedAt = time.Now().UTC()
			it.WrapperPid = 0
			it.CtlSock = ""
		}
		return nil
	})
}

// weaveRecoverOrphanedAllocations and weaveRecoverAbandonedFinalizations are
// the locked, single-rule entry points kept for callers that want exactly one
// recovery. The rules themselves now live in weave_reaper.go, where they are
// two of the passes weaveReapQueue applies together — see the lifecycle
// invariant at the top of weave_lifecycle.go.
func weaveRecoverOrphanedAllocations(dir string) error {
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		weaveReapOrphanedAllocations(q, time.Now().UTC())
		return nil
	})
}

func weaveRecoverAbandonedFinalizations(dir string) error {
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		weaveReapAbandonedFinalizations(q, time.Now().UTC())
		return nil
	})
}

// weaveFinalizationLease exceeds the 10-minute verify ceiling plus process
// shutdown grace. An expired claim is recovered even if its numeric PID now
// happens to exist, because PIDs can be reused after a conductor crash.
const weaveFinalizationLease = 15 * time.Minute

func weaveAllocatedLaunchOrphaned(it *weaveItem, now time.Time) bool {
	if it == nil || it.State != "allocated" {
		return false
	}
	if it.WrapperPid > 0 {
		return !pidAlive(it.WrapperPid)
	}
	if it.StartedAt.IsZero() || now.Sub(it.StartedAt) < weaveProvisioningTimeout {
		return false
	}
	switch it.LaunchPhase {
	case "provisioning workspace", "hydrating submodules", "syncing sibling dependencies":
		return true
	default:
		return false
	}
}

func weaveRunVerify(workspace, queueDir, command string, it *weaveItem) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// Hermetic shell: --noprofile --norc keeps the measurement
	// immune to user dotfiles (a broken ~/.bash_profile once failed
	// an honest gate in production), and PWD is pinned like the
	// subagent's own environment.
	vc := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-c", command)
	vc.Dir = workspace
	vc.Env = weaveVerifyEnv(os.Environ(), workspace, queueDir, it)
	out, err := vc.CombinedOutput()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = 1
		}
		if exit < 0 {
			// Signal death (incl. the 10m timeout kill) has no wait
			// status; normalize so verify_exit is always meaningful.
			exit = 1
		}
	}
	s := string(out)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		s += "\n[weave: verify command timed out after 10m]"
	}
	return exit, weaveTrimVerifyOutput(s, 2000)
}

// weaveTrimVerifyOutput shortens verify output to about max bytes WITHOUT
// discarding the lines that say what went wrong.
//
// It used to be `s = s[len(s)-2000:]` — keep the tail. For `go test ./...`
// over 200 packages that is catastrophic: the run ends with the alphabetically
// last packages passing, so the tail is all `ok` lines and a bare `FAIL`, while
// every `FAIL <pkg>` and every `# <pkg>` build error sits in the discarded
// head. The stored verdict then reads like a failure with no cause, and the
// summary line quotes a fragment of whatever `ok` line the cut landed in
// ("suite-gate-failed — dge 2.817s").
//
// That is the same defect this repo keeps finding in its own checks: a result
// that survives while the evidence for it is thrown away. It cost two separate
// multi-hour diagnoses in one session — once chasing a sibling-replace break,
// once chasing this.
//
// So: salient lines first, in order, then as much of the tail as still fits.
// A reader gets the failures even when the run is enormous.
func weaveTrimVerifyOutput(s string, max int) string {
	if len(s) <= max {
		return s
	}
	var salient []string
	for _, ln := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(ln, "FAIL"),
			strings.HasPrefix(ln, "--- FAIL"),
			strings.HasPrefix(ln, "# "),
			strings.HasPrefix(ln, "panic:"),
			strings.Contains(ln, "[weave: verify command timed out"):
			salient = append(salient, ln)
		}
	}
	head := strings.Join(salient, "\n")
	if len(head) > max {
		// More failures than budget: keep the FIRST ones. The first failure is
		// usually the cause; later ones are often its consequences.
		head = head[:max] + "\n[weave: +more failures truncated]"
		return head
	}
	room := max - len(head)
	tail := s
	if len(tail) > room {
		tail = tail[len(tail)-room:]
	}
	if head == "" {
		return tail
	}
	return head + "\n[weave: ...output trimmed, failures above...]\n" + tail
}

func weaveCollectVerifyEvidence(workspace, queueDir, command string, it *weaveItem, dirty bool, dirtyFiles int) (verifyExit *int, verifyOutput, verifyTree string) {
	if command == "" {
		return nil, "", ""
	}
	ve, vo := weaveRunVerify(workspace, queueDir, command, it)
	if dirty {
		verifyTree = "working-tree-dirty"
		vo += fmt.Sprintf("\n[weave: VERIFY ATTESTED A DIRTY WORKING TREE: working tree had tracked uncommitted changes in %d file(s); HEAD alone is not the verified tree]", dirtyFiles)
	} else {
		verifyTree = "head"
	}
	if len(vo) > 2000 {
		// weaveRunVerify already preserves salient failure lines when it
		// limits output. Adding the dirty-tree attestation can take that
		// result over the budget, so use the same reducer here rather than
		// reverting to a tail cut and discarding the evidence we just kept.
		vo = weaveTrimVerifyOutput(vo, 2000)
	}
	return &ve, vo, verifyTree
}

type weaveTerminalEvidence struct {
	CommitsAhead   int
	Head           string
	FilesTouched   []string
	Dirty          bool
	DirtyFiles     int
	UntrackedFiles int
	VerifyExit     *int
	VerifyOutput   string
	VerifyTree     string
}

func weaveCollectTerminalEvidence(workspace, base, queueDir, verifyCommand string, it *weaveItem, runVerify bool) weaveTerminalEvidence {
	ahead, head := weaveMeasureBranch(workspace, base)
	dirty, dirtyFiles, untrackedFiles := weaveMeasureDirtiness(workspace)
	ev := weaveTerminalEvidence{
		CommitsAhead:   ahead,
		Head:           head,
		FilesTouched:   weaveCollectFilesTouched(workspace, base),
		Dirty:          dirty,
		DirtyFiles:     dirtyFiles,
		UntrackedFiles: untrackedFiles,
	}
	if runVerify && verifyCommand != "" {
		ev.VerifyExit, ev.VerifyOutput, ev.VerifyTree = weaveCollectVerifyEvidence(workspace, queueDir, verifyCommand, it, dirty, dirtyFiles)
	}
	return ev
}

func weaveCollectFilesTouched(workspace, base string) []string {
	seen := map[string]bool{}
	addLines := func(out []byte) {
		for _, ln := range strings.Split(string(out), "\n") {
			ln = strings.TrimSpace(ln)
			if ln != "" {
				seen[ln] = true
			}
		}
	}
	if out, err := exec.Command(gitBin(), "-C", workspace, "diff", "--name-only", base+"...HEAD").Output(); err == nil {
		addLines(out)
	}
	if out, err := exec.Command(gitBin(), "-C", workspace, "diff", "--name-only").Output(); err == nil {
		addLines(out)
	}
	if out, err := exec.Command(gitBin(), "-C", workspace, "ls-files", "--others", "--exclude-standard").Output(); err == nil {
		addLines(out)
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

func weaveApplyTerminalEvidence(it *weaveItem, ev weaveTerminalEvidence) {
	it.CommitsAhead = ev.CommitsAhead
	it.Head = ev.Head
	it.Dirty = ev.Dirty
	it.DirtyFiles = ev.DirtyFiles
	it.UntrackedFiles = ev.UntrackedFiles
	if ev.VerifyExit != nil {
		it.VerifyExit = ev.VerifyExit
		it.VerifyOutput = ev.VerifyOutput
		it.VerifyTree = ev.VerifyTree
	}
}

func weaveClearCurrentRunTerminalEvidence(it *weaveItem) {
	it.ExitCode = nil
	it.KilledBy = ""
	it.Completion = ""
	it.FinalizerPID = 0
	it.FinalizingAt = time.Time{}
	it.FinishedAt = time.Time{}
	it.CommitsAhead = 0
	it.Head = ""
	it.VerifyExit = nil
	it.VerifyOutput = ""
	it.VerifyTree = ""
	it.CodingAgent = ""
	it.ReviewAgent = ""
	it.ReviewAddedTest = false
	it.PairVerdict = ""
	it.PairReason = ""
	it.PairExit = 0
	if strings.HasPrefix(it.StewardReason, "PAIR ") {
		it.NeedsSteward = false
		it.StewardReason = ""
	}
	it.Dirty = false
	it.DirtyFiles = 0
	it.UntrackedFiles = 0
	it.AutoCommitted = false
	it.AutoCommitError = ""
	it.Throttled = false
	it.ThrottleSignal = ""
	// The isolation verdict describes THIS run, like the exit code beside
	// it. The caller re-baselines against the live checkout as it stands
	// now, so drift the previous run left behind is baselined IN rather
	// than blamed on its successor — otherwise one escape would wedge the
	// issue permanently, refusing every retry for a tree nobody touched.
	it.IsolationViolated = false
	it.EscapedPaths = nil
	it.OutsideWorkspacePaths = nil
}

func weaveTerminalState(exitCode int, runErr error, killedBy string, ev weaveTerminalEvidence) string {
	if killedBy != "" || exitCode >= 129 {
		return "killed"
	}
	if exitCode == 0 && runErr == nil && ev.CommitsAhead > 0 {
		return "submitted"
	}
	if exitCode == 0 && runErr == nil && ev.CommitsAhead == 0 && !ev.Dirty && ev.UntrackedFiles == 0 {
		return "no-op"
	}
	return "failed"
}

func weaveIssueMemoryFiles(it *weaveItem) []string {
	if it == nil {
		return nil
	}
	text := it.Title + "\n" + it.Body
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\n', '\t', '\r', ',', ';', ':', '"', '\'', '`', '(', ')', '[', ']', '{', '}':
			return true
		}
		return false
	}) {
		tok := strings.Trim(raw, "./")
		if tok == "" || strings.Contains(tok, "://") {
			continue
		}
		if strings.Contains(tok, "/") || strings.Contains(filepath.Base(tok), ".") {
			seen[tok] = true
		}
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

func weaveRememberObservation(dir string, it *weaveItem, ev weaveTerminalEvidence) error {
	if it == nil {
		return nil
	}
	outcome := it.State
	if outcome == "done" {
		outcome = "merged"
	}
	if outcome != "submitted" && outcome != "no-op" && outcome != "failed" && outcome != "killed" && outcome != "merged" {
		return nil
	}
	verifyExit := 0
	if it.VerifyExit != nil {
		verifyExit = *it.VerifyExit
	}
	gateExit := 0
	if it.SuiteGateExit != nil {
		gateExit = *it.SuiteGateExit
	}
	tags := []string{}
	if it.Throttled {
		tags = append(tags, "throttled")
		if it.ThrottleSignal != "" {
			tags = append(tags, it.ThrottleSignal)
		}
	}
	st, _, err := memory.Open(dir, memory.Prefs{})
	if err != nil {
		return err
	}
	return st.Remember(context.Background(), memory.Observation{
		IssueID:      it.ID,
		Title:        it.Title,
		Tool:         it.Tool,
		Outcome:      outcome,
		FilesTouched: ev.FilesTouched,
		Commits:      it.CommitsAhead,
		VerifyExit:   verifyExit,
		GateExit:     gateExit,
		KilledBy:     it.KilledBy,
		Summary:      strings.TrimSpace(fmt.Sprintf("%s: %s", it.State, it.Title)),
		Tags:         tags,
		CreatedAt:    time.Now().UTC(),
	})
}

func weaveInjectMemoryFile(dir, workspace string, it *weaveItem) error {
	return weaveInjectMemoryFileWithPrefix(dir, workspace, it, "")
}

func weaveInjectMemoryFileWithPrefix(dir, workspace string, it *weaveItem, prefix string) error {
	if it == nil || workspace == "" {
		return nil
	}
	st, _, err := memory.Open(dir, memory.Prefs{})
	if err != nil {
		return err
	}
	obs, err := st.Recall(context.Background(), memory.Query{
		Files:   weaveIssueMemoryFiles(it),
		Title:   it.Title,
		IssueID: it.ID,
		Limit:   10,
	})
	if err != nil {
		return err
	}
	prefix = strings.TrimSpace(prefix)
	if len(obs) == 0 && prefix == "" {
		_ = os.Remove(filepath.Join(workspace, "WEAVE_MEMORY.md"))
		return nil
	}
	var b strings.Builder
	b.WriteString("# WEAVE_MEMORY\n\n")
	if prefix != "" {
		b.WriteString(prefix)
		b.WriteString("\n\n")
	}
	if len(obs) > 0 {
		b.WriteString("## Prior runs on related work\n\n")
		for _, o := range obs {
			files := "-"
			if len(o.FilesTouched) > 0 {
				files = strings.Join(o.FilesTouched, ", ")
				if len(files) > 160 {
					files = files[:157] + "..."
				}
			}
			summary := strings.TrimSpace(o.Summary)
			if summary == "" {
				summary = o.Title
			}
			fmt.Fprintf(&b, "- run #%d: %s; files: %s; %s\n", o.IssueID, o.Outcome, files, weaveTruncate(summary, 160))
		}
		b.WriteString("\nRun `weave recall <q>` for detail.\n")
	}
	if err := os.WriteFile(filepath.Join(workspace, "WEAVE_MEMORY.md"), []byte(b.String()), 0o644); err != nil {
		return err
	}
	return weaveExcludeWorkspaceFile(workspace, "WEAVE_MEMORY.md")
}

// weaveExcludeWorkspaceFile marks a workspace drop (WEAVE_MEMORY.md, KB.md)
// as git-excluded so it can never ride a merge.
func weaveExcludeWorkspaceFile(workspace, name string) error {
	exclude := filepath.Join(workspace, ".git", "info", "exclude")
	b, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(b), "\n"+name+"\n") || strings.HasSuffix(string(b), "\n"+name) || strings.HasPrefix(string(b), name+"\n") {
		return nil
	}
	f, err := os.OpenFile(exclude, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(name + "\n")
	return err
}

func weaveAutoCommitMessage(it *weaveItem) string {
	return fmt.Sprintf("weave(auto): issue %d — %s", it.ID, it.Title)
}

func weaveAutoCommitMessageWithContext(it *weaveItem, ev weaveTerminalEvidence) string {
	trailer := weaveContextTrailer(it, ev)
	if trailer == "" {
		return weaveAutoCommitMessage(it)
	}
	return weaveAutoCommitMessage(it) + "\n\n" + trailer
}

func weaveContextTrailer(it *weaveItem, ev weaveTerminalEvidence) string {
	if it == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("[weave-context]\n")
	fmt.Fprintf(&b, "issue: #%d %s\n", it.ID, strings.TrimSpace(it.Title))
	files := ev.FilesTouched
	if len(files) > 10 {
		files = files[:10]
	}
	if len(files) == 0 {
		b.WriteString("files: -\n")
	} else {
		fmt.Fprintf(&b, "files: %s\n", strings.Join(files, ", "))
	}
	fmt.Fprintf(&b, "commits-ahead: %d\n", ev.CommitsAhead)
	if ev.VerifyExit == nil {
		b.WriteString("verify: n/a\n")
	} else {
		fmt.Fprintf(&b, "verify: exit=%d\n", *ev.VerifyExit)
	}
	if it.Throttled && strings.TrimSpace(it.ThrottleSignal) != "" {
		fmt.Fprintf(&b, "throttled: %s\n", strings.TrimSpace(it.ThrottleSignal))
	}
	return strings.TrimRight(b.String(), "\n")
}

func parseWeaveContextTrailer(commitMsg string) (string, bool) {
	commitMsg = strings.ReplaceAll(commitMsg, "\r\n", "\n")
	lines := strings.Split(commitMsg, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "[weave-context]" {
			start = i
		}
	}
	if start < 0 {
		return "", false
	}
	out := []string{strings.TrimSpace(lines[start])}
	for _, line := range lines[start+1:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		if !strings.Contains(line, ":") {
			break
		}
		out = append(out, strings.TrimRight(line, " \t"))
	}
	if len(out) == 1 {
		return "", false
	}
	return strings.Join(out, "\n"), true
}

func weaveResumeMemoryPrefix(workspace string) string {
	out, err := exec.Command(gitBin(), "-C", workspace, "log", "-1", "--format=%B").Output()
	if err != nil {
		return ""
	}
	trailer, ok := parseWeaveContextTrailer(string(out))
	if !ok {
		return ""
	}
	return "## Resuming — last context\n\n```text\n" + trailer + "\n```"
}

// weaveEnsureBashyExclude adds bashy's own skill-export markers to the clone's
// .git/info/exclude so the wrapper's auto-commit never captures them. The
// `.bashy-export.json` ownership marker is written into a worker's workspace
// (under .agents/skills/…, .claude/skills/…) as agent scaffolding at launch —
// not worker code — and it carries workspace-specific content, so committing it
// per-branch makes `weave pull` add/add-CONFLICT across parallel workers on an
// identical artifact path, silently blocking every merge. Excluding the basename
// (gitignore matches it at any depth) keeps it untracked so `git add -A` skips it.
func weaveEnsureBashyExclude(workspace string) {
	excl := filepath.Join(workspace, ".git", "info", "exclude")
	existing, _ := os.ReadFile(excl)
	if strings.Contains(string(existing), ".bashy-export.json") {
		return
	}
	f, err := os.OpenFile(excl, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return // best-effort; a missing exclude only reinstates the old behavior
	}
	defer f.Close()
	_, _ = f.WriteString("\n# bashy skill-export marker — never commit (weave: causes add/add merge conflicts)\n.bashy-export.json\n")
}

func maybeAutoCommit(workspace, msg string) (bool, error) {
	weaveEnsureBashyExclude(workspace)
	dirty, _, untrackedFiles := weaveMeasureDirtiness(workspace)
	if !dirty && untrackedFiles == 0 {
		return false, nil
	}
	add := exec.Command(gitBin(), "-C", workspace, "add", "-A")
	if out, err := add.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git add -A: %w: %s", err, strings.TrimSpace(string(out)))
	}
	commit := exec.Command(gitBin(), "-C", workspace, "commit", "-m", msg)
	if out, err := commit.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

// weaveSuiteGateCommand resolves the gate a merge must clear.
//
// Precedence: the issue's own override, then the PROJECT'S gate (`bashy gate`),
// then weave's historical `.agents/weave/suite-gate`.
//
// The project gate is consulted second so that a repo defines "does this pass?"
// ONCE and weave, sdlc, dag, CI and a human at a terminal all read the same
// answer. Before `bashy gate`, that question was spelled four incompatible ways
// in four packages — they never disagreed about semantics (run a command, let its
// exit status be the verdict), only about where the command lived. weave keeps
// reading its own file last, so every project already using it works unchanged:
// the point of unifying is to stop breaking people, not to start.
func weaveSuiteGateCommand(root string, it *weaveItem) string {
	if it.SuiteGate != "" {
		return it.SuiteGate
	}
	if def, err := gate.Resolve(root, ""); err == nil && len(def.Commands) > 0 {
		return strings.Join(def.Commands, " && ")
	}
	b, err := os.ReadFile(filepath.Join(root, ".agents", "weave", "suite-gate"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func weaveRunSuiteGate(root, command string) (int, string) {
	if command == "" {
		return 0, ""
	}
	return weaveRunVerify(root, "", command, nil)
}

// weaveRunCandidateSuiteGate proves the candidate against one exact base SHA
// without touching the shared live checkout. A private clone gives the gate
// the same merged tree it historically saw after the live merge; the caller
// later revalidates that SHA under pull.lock before committing.
func weaveRunCandidateSuiteGate(root, workspace, branch, baseSHA, command string) (exit int, output string, conflict bool, err error) {
	// THE CLONE GOES INSIDE A TEMP PARENT, under the repo's own directory name,
	// so a sibling-path replace still resolves.
	//
	// `replace mvdan.cc/sh/v3 => ../sh` is resolved relative to the module
	// directory. Cloning straight into /tmp/weave-pull-gate-XXXX makes ../sh
	// mean /tmp/sh, which does not exist, and EVERY package fails to load —
	// measured at 40 copies of "replacement directory ../sh does not exist".
	// The gate then reported suite-gate-failed for candidates that are green in
	// their workspace, which is exactly where a human goes to reproduce it and
	// where it passes. It failed closed, so nothing bad merged; it also said
	// nothing true about any candidate.
	tmpParent, err := os.MkdirTemp("", "weave-pull-gate-*")
	if err != nil {
		return 0, "", false, fmt.Errorf("create isolated suite-gate checkout: %w", err)
	}
	defer os.RemoveAll(tmpParent)
	tmp := filepath.Join(tmpParent, filepath.Base(root))
	if err := weaveLinkSiblingReplaces(root, tmpParent); err != nil {
		return 0, "", false, err
	}
	if out, err := exec.Command(gitBin(), "clone", "--quiet", "--no-local", root, tmp).CombinedOutput(); err != nil {
		return 0, "", false, fmt.Errorf("clone isolated suite-gate checkout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(gitBin(), "-C", tmp, "checkout", "--quiet", "--detach", baseSHA).CombinedOutput(); err != nil {
		return 0, "", false, fmt.Errorf("checkout suite-gate base %s: %w: %s", baseSHA, err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(gitBin(), "-C", tmp, "fetch", "--quiet", "--no-tags", workspace, branch).CombinedOutput(); err != nil {
		return 0, "", false, fmt.Errorf("fetch candidate for suite gate: %w: %s", err, strings.TrimSpace(string(out)))
	}
	merge := exec.Command(gitBin(), "-C", tmp,
		"-c", "user.name=weave", "-c", "user.email=weave@localhost",
		"merge", "--no-ff", "--no-edit", "FETCH_HEAD")
	if out, err := merge.CombinedOutput(); err != nil {
		return 0, strings.TrimSpace(string(out)), true, nil
	}
	// Hydrate submodules with the SAME helper a workspace uses. `git clone`
	// leaves a submodule directory EMPTY, so an internal `=> ./external/...`
	// replace cannot resolve — weaveHydrateSubmodules' own doc names this exact
	// failure in this exact repo. Only visible once the sibling break above was
	// fixed: it was masking this behind a louder one.
	//
	// It must be a real hydration, never a symlink. git refuses ("expected
	// submodule path ... not to be a symbolic link") and that breaks go's VCS
	// stamping for the WHOLE module, taking `go list ./...` from 255 packages
	// to 0 — strictly worse than the empty submodule. Measured, not reasoned.
	//
	// After the merge, because the candidate may move the pin. Best-effort: a
	// gate that refuses to run is worse than one reporting what the compiler says.
	_ = weaveHydrateSubmodules(root, tmp, io.Discard, io.Discard)
	exit, output = weaveRunSuiteGate(tmp, command)
	return exit, output, false, nil
}

// weaveLinkSiblingReplaces symlinks every `=> ../x` replace target named by
// root's go.mod into dst, so a module cloned to dst/<name> resolves them.
//
// It reads the DECLARED replaces rather than linking every sibling it can see:
// the umbrella has a dozen projects next to each other, and a gate that quietly
// exposed all of them would pass on a dependency the module never declared.
//
// A missing target is not an error. A repo may point at a sibling that is not
// checked out here, and the gate's job is to report what the compiler says
// about that, not to refuse to run.
func weaveLinkSiblingReplaces(root, dst string) error {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil // no go.mod: nothing to resolve
	}
	parent := filepath.Dir(root)
	linked := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, "=> ../")
		if i < 0 || strings.HasPrefix(line, "//") {
			continue
		}
		fields := strings.Fields(line[i+len("=> "):])
		if len(fields) == 0 {
			continue
		}
		name := filepath.Base(filepath.Clean(fields[0]))
		if name == "" || name == "." || name == ".." || linked[name] {
			continue
		}
		src := filepath.Join(parent, name)
		if _, err := os.Stat(src); err != nil {
			continue // not checked out here; let the compiler say so
		}
		if err := os.Symlink(src, filepath.Join(dst, name)); err != nil && !os.IsExist(err) {
			return fmt.Errorf("link sibling %q for suite gate: %w", name, err)
		}
		linked[name] = true
	}
	return nil
}

// weaveValidatePullEvidence runs only while pull.lock is held. It refuses to
// apply a verdict to any target other than the base/live tree and queue item
// that produced that verdict.
func weaveValidatePullEvidence(root, dir string, issueID int64, wantHead string, wantLive weaveLiveSnapshot, wantItem string) error {
	headOut, err := gitOut(root, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("run #%d: revalidate merge target: %w", issueID, err)
	}
	gotHead := strings.TrimSpace(headOut)
	if gotHead != wantHead {
		return fmt.Errorf("%w: run #%d base HEAD %s -> %s; verdict is stale, retry pull",
			errWeavePullStale, issueID, wantHead, gotHead)
	}
	gotLive, err := weaveSnapshotLiveTree(root)
	if err != nil {
		return fmt.Errorf("run #%d: revalidate live checkout: %w", issueID, err)
	}
	if gotLive.SHA != wantLive.SHA {
		return fmt.Errorf("%w: run #%d live working-tree fingerprint changed; verdict is stale, retry pull",
			errWeavePullStale, issueID)
	}
	fresh, err := readWeaveQueue(dir)
	if err != nil {
		return fmt.Errorf("run #%d: revalidate queue item: %w", issueID, err)
	}
	gotItem, ok := weaveItemFingerprints(fresh)[issueID]
	if !ok {
		return fmt.Errorf("%w: run #%d queue item was removed; verdict is stale, retry pull",
			errWeavePullStale, issueID)
	}
	if gotItem != wantItem {
		return fmt.Errorf("%w: run #%d queue record fingerprint changed; verdict is stale, retry pull",
			errWeavePullStale, issueID)
	}
	return nil
}

func findWeaveItem(q *weaveQueue, id int64) *weaveItem {
	for _, it := range q.Items {
		if it.ID == id {
			return it
		}
	}
	return nil
}

// nextTodo picks the issue an agent should claim next.
//
// It is the single choke point for BOTH the read-only peek (`weave next`) and the
// real claim inside `weave start`/`autopilot` — so dependency-blocking and epic
// containers are honored everywhere by construction, not by remembering to check
// twice.
//
// weaveClaimable is what "todo" used to mean, plus the two facts a flat queue could
// not express: an item split into children is worked through the children, and an
// item that needs another issue's merged code cannot start before it lands.
func nextTodo(q *weaveQueue) *weaveItem {
	var todos []*weaveItem
	for _, it := range q.Items {
		if weaveClaimable(q, it) {
			todos = append(todos, it)
		}
	}
	sort.SliceStable(todos, func(i, j int) bool {
		pi := prioRank(todos[i].Priority)
		pj := prioRank(todos[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return todos[i].ID < todos[j].ID
	})
	if len(todos) == 0 {
		return nil
	}
	return todos[0]
}

func prioRank(p string) int {
	switch p {
	case "p0":
		return 0
	case "p1":
		return 1
	case "p3":
		return 3
	default:
		return 2
	}
}

func ec(code int) error {
	if code == 0 {
		return nil
	}
	return &exitCodeError{code: code}
}

// runWeaveAddPointed validates optional story points and files the
// issue in one queue transaction. Points used to be stamped in a
// second read-modify-write after add returned; that made add --points
// sensitive to concurrent queue writers and to "last item" races.
func runWeaveAddStaged(cmd *cobra.Command, title, body, priority, verify, suiteGate, stage, judge string, points, band int, flags *weaveOutputFlags) error {
	st, err := normalizeStage(stage)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave add",
			weavecli.ExitInvalidArg, err))
	}
	return runWeaveAddFullTiered(cmd, title, body, priority, verify, suiteGate, st, judge, points, band, flags)
}

func runWeaveAddPointed(cmd *cobra.Command, title, body, priority, verify, suiteGate string, points int, flags *weaveOutputFlags) error {
	if points != 0 && !weaveValidPoints(points) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave add",
			weavecli.ExitInvalidArg, fmt.Errorf("points must be one of 1,2,3,5,8")))
	}
	return runWeaveAddWithPoints(cmd, title, body, priority, verify, suiteGate, points, flags)
}

func runWeaveAdd(cmd *cobra.Command, title, body, priority, verify string, flags *weaveOutputFlags) error {
	return runWeaveAddWithPoints(cmd, title, body, priority, verify, "", 0, flags)
}

func runWeaveAddWithPoints(cmd *cobra.Command, title, body, priority, verify, suiteGate string, points int, flags *weaveOutputFlags) error {
	return runWeaveAddFull(cmd, title, body, priority, verify, suiteGate, "", points, flags)
}

func runWeaveAddFull(cmd *cobra.Command, title, body, priority, verify, suiteGate, stage string, points int, flags *weaveOutputFlags) error {
	return runWeaveAddFullTiered(cmd, title, body, priority, verify, suiteGate, stage, "", points, 0, flags)
}

func runWeaveAddFullTiered(cmd *cobra.Command, title, body, priority, verify, suiteGate, stage, judge string, points, band int, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if title == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitInvalidArg, fmt.Errorf("title required")))
	}
	if !weaveValidJudgeTier(judge) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitInvalidArg, fmt.Errorf("--judge must be %q or %q (empty defaults to %q)", weaveJudgeNone, weaveJudgeRequired, weaveJudgeNone)))
	}
	if band < 0 || band > fleet.MaxBand {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitInvalidArg, fmt.Errorf("--band must be 1-%d (0 = unpegged)", fleet.MaxBand)))
	}
	judge = strings.ToLower(strings.TrimSpace(judge))
	if judge == "" {
		judge = weaveJudgeNone
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitGenericFail, err))
	}
	prio := priority
	if prio == "" {
		prio = "p2"
	}
	var it *weaveItem
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it = &weaveItem{
			ID:            q.NextID,
			Title:         title,
			Body:          body,
			Priority:      prio,
			Stage:         stage,
			State:         "todo",
			VerifyCommand: verify,
			SuiteGate:     suiteGate,
			Points:        points,
			Judge:         judge,
			Band:          band,
			Created:       time.Now().UTC(),
		}
		q.NextID++
		q.Items = append(q.Items, it)
		weaveTestPauseInsideAddLock()
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitGenericFail, lockErr))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave add", map[string]any{
			"issue":    it.ID,
			"title":    it.Title,
			"priority": it.Priority,
			"stage":    itemStage(it),
			"state":    it.State,
			"points":   it.Points,
			"judge":    weaveJudgeMode(it),
			"band":     it.Band,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave add: run #%d created (%s/%s, todo) — %q\n",
		it.ID, it.Priority, itemStage(it), it.Title)
	return nil
}

func weaveTestPauseInsideAddLock() {
	pause := os.Getenv("WEAVE_TEST_ADD_INSIDE_LOCK_FILE")
	if pause == "" {
		return
	}
	_ = os.WriteFile(pause+".ready", []byte("ready"), 0o644)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(pause); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// weaveRenderItemRows prints the standard list table rows (no header).
// weaveRenderItemRows prints the list table.
//
// It takes the whole queue, not just the rows, because two of the columns are FACTS
// ABOUT THE GRAPH and cannot be read off a lone item: whether an issue is an epic
// (derived from its children) and whether it is blocked (derived from its
// dependencies). A field nobody can see in the primary view may as well not exist —
// so stage, epics and blockers all surface here.
func weaveRenderItemRows(w io.Writer, q *weaveQueue, items []*weaveItem) {
	for _, it := range items {
		title := weaveTruncate(it.Title, 40)
		if it.Parent != 0 {
			title = weaveTruncate("↳ "+it.Title, 40)
		}
		state := it.State
		if q != nil {
			if kids := weaveChildren(q, it.ID); len(kids) > 0 {
				done := 0
				for _, k := range kids {
					if k.State == "done" {
						done++
					}
				}
				state = fmt.Sprintf("epic %d/%d", done, len(kids))
			} else if it.State == "todo" {
				if b := weaveBlockers(q, it); len(b) > 0 {
					// Blocked work looks like available work in a flat list; that is
					// how a conductor picks up an issue whose prerequisite is not in
					// main yet.
					ids := make([]string, 0, len(b))
					for _, d := range b {
						ids = append(ids, "#"+strconv.FormatInt(d.ID, 10))
					}
					state = "blocked→" + strings.Join(ids, ",")
				}
			}
		}
		if it.Stale {
			state = it.State + "*"
		}
		// "!" outranks "*": a stale wrapper is a run to re-attach, an
		// escaped run is a tree to inspect before anything else happens.
		// Gated on the workspace still existing, for the same reason the
		// footer is: there is no tree left to inspect once it is pruned, and
		// a marker whose footer no longer prints is an unexplained one.
		if it.IsolationViolated && weaveWorkspacePresent(it) {
			state = it.State + "!"
		}
		toolCol := it.Tool
		if toolCol == "" {
			toolCol = "-"
		}
		if len(toolCol) > 9 {
			toolCol = toolCol[:9]
		}
		pts := "-"
		if it.Points > 0 {
			pts = strconv.Itoa(it.Points)
		}
		work := "-"
		if it.Salvageable {
			work = fmt.Sprintf("%d commits", it.UnmergedCommits)
		}
		// REF is the run's canonical address, run:<repo-basename>-<n> — the
		// spelling a citation uses ([[run:coreutils-21]]) and `bashy define`
		// resolves; the current checkout's queue is authoritative for it.
		runRef := fmt.Sprintf("run:%s-%d", filepath.Base(filepath.Clean(q.Root)), it.ID)
		fmt.Fprintf(w, "%-4d %-4s %-6s %-3s %-14s %-9s %-10s %-8s %-8s %-40s %-24s %s\n",
			it.ID, it.Priority, itemStage(it), pts, state, toolCol,
			work, weaveStartedCol(it), weaveDurationCol(it), title, runRef, weaveTildePath(it.Workspace))
	}
}

// weaveTruncate shortens s to at most maxRunes runes, cutting on a rune
// boundary and appending an ellipsis. Byte-slicing a title (the old
// `title[:37]`) split mid-rune on multibyte titles and rendered as the
// `recurrence �...` mojibake in the list table; counting runes fixes it.
func weaveTruncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-3]) + "..."
}

// weaveAllQueueDirs returns every queue dir under the weave base.
func weaveAllQueueDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	// Enumerate the current root and the legacy ones, so queues that haven't
	// been touched (hence not yet migrated) still show up in listings.
	var dirs []string
	for _, base := range append([]string{weaveStateRoot(home)}, weaveLegacyStateRoots(home)...) {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(base, e.Name()))
			}
		}
	}
	return dirs
}

// weaveQueueRootAvailable reports whether a discovered queue still belongs to
// a repository on disk. Test and other short-lived repositories leave queue
// registrations behind after their temporary root is removed; those records
// must not make `weave list --all` advertise a repository that cannot be
// visited.
//
// Discovery deliberately does not delete the queue directory. It may contain
// the only copy of an isolated workspace branch, and queue discovery has
// neither enough evidence nor authority to decide that work is disposable.
// Queues predating the Root field remain visible for backward compatibility.
func weaveQueueRootAvailable(q *weaveQueue) bool {
	if q.Root == "" {
		return true
	}
	fi, err := os.Stat(q.Root)
	return err == nil && fi.IsDir()
}

// weaveQueueSummaries prints one compact line per queue on the
// machine: basename as the queue's name, state counts, and what is
// actively running. Used when the current repo has no queue.
//
// activeOnly skips queues with no non-terminal items — the "where's the
// action" hint shown when the current repo's queue is empty should not
// surface a sibling queue whose every item is done/abandoned (there is
// nothing to cd to). It is left false for the not-a-repo machine
// overview, where seeing finished queues is the point. This mirrors the
// filter weaveOtherActiveQueues already applies to the JSON / hint-
// suffix paths, so the human and machine views agree on "active".
func weaveQueueSummaries(w io.Writer, skipDir string, activeOnly bool) int {
	printed := 0
	for _, dir := range weaveAllQueueDirs() {
		if dir == skipDir {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil || len(q.Items) == 0 {
			continue
		}
		if !weaveQueueRootAvailable(q) {
			continue
		}
		root := q.Root
		if root == "" {
			root = filepath.Base(dir)
		}
		name := filepath.Base(root)
		counts := map[string]int{}
		active := 0
		var live []string
		for _, it := range q.Items {
			counts[it.State]++
			if !isTerminalState(it.State) {
				active++
			}
			if it.State == "working" {
				tool := it.Tool
				if tool == "" {
					tool = "?"
				}
				live = append(live, fmt.Sprintf("#%d %s %s", it.ID, tool, weaveDurationCol(it)))
			}
		}
		if activeOnly && active == 0 {
			continue
		}
		summary := fmt.Sprintf("  %-12s %d total", name, len(q.Items))
		order := []string{"working", "paused", "allocated", "todo", "submitted", "no-op", "killed", "failed", "done", "abandoned"}
		for _, st := range order {
			if counts[st] > 0 {
				summary += fmt.Sprintf(", %d %s", counts[st], st)
			}
		}
		if len(live) > 0 {
			summary += " — " + strings.Join(live, "; ")
		}
		fmt.Fprintln(w, summary)
		fmt.Fprintf(w, "  %-12s %s\n", "", weaveTildePath(root))
		printed++
	}
	return printed
}

// runWeaveListAll renders every weave queue on the machine — the
// global view. activeOnly limits rows to non-terminal items (the
// shape used when the current repo's queue is empty: show where the
// action is instead of a bare hint).
func runWeaveListAll(cmd *cobra.Command, includeHistory bool, activeOnly bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	type queueView struct {
		Root  string       `json:"root"`
		Dir   string       `json:"dir"`
		Items []*weaveItem `json:"items"`
	}
	var views []queueView
	now := time.Now().UTC()
	totalUnattended := 0
	for _, dir := range weaveAllQueueDirs() {
		q, err := loadWeaveQueue(dir)
		if err != nil || len(q.Items) == 0 {
			continue
		}
		if !weaveQueueRootAvailable(q) {
			continue
		}
		root := q.Root
		if root == "" {
			root = filepath.Base(dir)
		}
		base := weaveBaseBranch(root)
		weaveReconcileMerged(root, base, q)
		var items []*weaveItem
		for _, it := range q.Items {
			weaveAnnotateSalvageable(root, base, it)
			if activeOnly && isTerminalState(it.State) {
				continue
			}
			if !activeOnly && !weaveItemVisibleInList(it, includeHistory) {
				continue
			}
			if it.State == "working" && it.WrapperPid > 0 && !pidAlive(it.WrapperPid) {
				it.Stale = true
			}
			items = append(items, it)
		}
		if len(items) == 0 {
			continue
		}
		unattended := weaveDoctorStaleCount(q, defaultWeaveDoctorThresholds(), now)
		totalUnattended += unattended
		views = append(views, queueView{Root: root, Dir: dir, Items: items})
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave list", map[string]any{
			"queues": views,
		}))
	}
	if len(views) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "weave list: no weaves on this machine")
		return nil
	}
	w := cmd.OutOrStdout()
	if totalUnattended > 0 {
		fmt.Fprintf(w, "ATTENTION: %d unattended item(s) — see `weave doctor`\n", totalUnattended)
	}
	for i, v := range views {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s\n", weaveTildePath(v.Root))
		fmt.Fprintf(w, "%-4s %-4s %-6s %-3s %-14s %-9s %-10s %-8s %-8s %-40s %-24s %s\n", "ID", "PRIO", "STAGE", "PTS", "STATE", "TOOL", "UNMERGED", "STARTED", "DUR", "TITLE", "REF", "WORKSPACE")
		// The graph columns (epic, blocked) are facts about the WHOLE queue, so the
		// renderer needs one — this view carries the same items under another name.
		weaveRenderItemRows(w, &weaveQueue{Root: v.Root, Items: v.Items}, v.Items)
	}
	return nil
}

func runWeaveList(cmd *cobra.Command, includeHistory bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		// Not a repo: there is no "this queue" — show the machine
		// summary instead of a dead end (JSON callers get the same
		// via the --all shape).
		if mode == weavecli.OutputJSON {
			return runWeaveListAll(cmd, includeHistory, false, flags)
		}
		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "weave list: %s is not a git repo; weaves on this machine:\n", cwd)
		if weaveQueueSummaries(w, "", false) == 0 {
			fmt.Fprintln(w, "  (none)")
		}
		return nil
	}
	dir, _ := weaveQueueDir(root)
	// Lock-free read (weave_lock_common.go): a board read must return even
	// while a multi-minute merge is running.
	q, err := readWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave list",
			weavecli.ExitGenericFail, err))
	}
	now := time.Now().UTC()
	unattended := weaveDoctorStaleCount(q, defaultWeaveDoctorThresholds(), now)
	// Reconcile for display (not persisted here — prune/pull hold the
	// lock and persist): a "submitted" item whose work already landed
	// in base shows as "done" instead of lying about pending work.
	base := weaveBaseBranch(root)
	weaveReconcileMerged(root, base, q)
	// Same display-only contract as the reconcile above: one git status over
	// the shared live root, judged against every item's baseline.
	weaveComputeIsolation(root, q)
	var items []*weaveItem
	anyStale := false
	reclaimable := 0
	var salvageable []int64
	var violated []*weaveItem
	var outsideRefs []*weaveItem
	var needsSteward []*weaveItem
	for _, it := range q.Items {
		age := weaveDoctorItemAge(it, now)
		it.AgeSeconds = int64(age / time.Second)
		it.Stale = weaveDoctorItemStale(it, age, defaultWeaveDoctorThresholds())
		// A run that DIED WITH GOOD WORK INSIDE IT.
		//
		// weaveTerminalState is correctly conservative: `submitted` requires a
		// zero exit AND commits, so a wrapper that crashed can never claim
		// success. But the inverse costs real work. A tool that commits its
		// changes and THEN crashes on the way out (opencode does this — its
		// storage layer blows the filename limit on exit) leaves a run marked
		// `failed` with a perfect, building, tested diff sitting on its branch —
		// and nothing anywhere says so. It reads exactly like a run that
		// achieved nothing.
		//
		// Observed: a run marked `failed` had committed the whole feature, built
		// clean, and passed its tests. The only way to know was to go looking.
		// The next conductor would have redone eight minutes of work for free.
		//
		// So: do not change the state (a crash is a crash), SURFACE THE EVIDENCE.
		// MEASURE IT, DO NOT TRUST THE REPORT. CommitsAhead is written by the
		// WRAPPER — which, in exactly this case, is the process that died. A tool
		// that commits its work and then crashes on the way out never reaches the
		// evidence-recording step, so the queue records commits_ahead=0 while the
		// branch carries a complete feature.
		//
		// That is the fleet-evidence rule inverted: we would be concluding "no
		// work" from the ABSENCE OF A REPORT BY A PROCESS THAT DID NOT SURVIVE TO
		// REPORT. The artifact is right there on disk. Go and look at it.
		//
		// The reaper persists it.Salvageable so the decision outlives this one
		// screenful; this re-measures for a queue read before any reap. Both
		// sides go through weaveClassifySalvageable, so the displayed answer
		// and the persisted one can never disagree.
		weaveAnnotateSalvageable(root, base, it)
		if it.Salvageable {
			salvageable = append(salvageable, it.ID)
		}
		// Terminal items whose workspace clone still occupies disk are
		// reclaimable via `weave prune` — surfaced in a footer so the
		// clutter isn't invisible until something trips over it.
		if isPrunableState(it.State) && it.Workspace != "" {
			if st, statErr := os.Stat(it.Workspace); statErr == nil && st.IsDir() {
				reclaimable++
			}
		}
		if !weaveItemVisibleInList(it, includeHistory) {
			continue
		}
		// Computed, never persisted: a "working" item whose wrapper
		// died without reaching the terminal-state write (crash,
		// SIGKILL, machine OOM) would otherwise claim to be working
		// forever and silently block `weave wait --all`.
		if it.State == "working" && it.WrapperPid > 0 && !pidAlive(it.WrapperPid) {
			it.Stale = true
			anyStale = true
		}
		// Both advisories describe a WORKSPACE — an escaped run's branch is
		// "not the whole diff", and a worker touched paths outside its clone.
		// Once that workspace is pruned the branch is gone with it, so the
		// footers name work that cannot be pulled, inspected or diffed and
		// their own advice (`weave pull`) cannot run. The fact stays on the
		// item and `weave status N` still reports it; what stops is presenting
		// a closed run as pending work.
		if it.IsolationViolated && weaveWorkspacePresent(it) {
			violated = append(violated, it)
		}
		if len(it.OutsideWorkspacePaths) > 0 && weaveWorkspacePresent(it) {
			outsideRefs = append(outsideRefs, it)
		}
		// The reaper's other determinate outcome: a submission nobody
		// pulled. Without this it is indistinguishable from work in flight.
		if it.NeedsSteward && it.State == "submitted" {
			needsSteward = append(needsSteward, it)
		}
		items = append(items, it)
	}
	var others []map[string]any
	if len(items) == 0 {
		others = weaveOtherActiveQueues(dir)
	}
	if mode == weavecli.OutputJSON {
		res := map[string]any{"items": items}
		if len(others) > 0 {
			res["other_active_queues"] = others
		}
		if len(salvageable) > 0 {
			res["salvageable"] = salvageable
		}
		if reclaimable > 0 {
			res["reclaimable"] = reclaimable
		}
		if len(needsSteward) > 0 {
			rows := make([]map[string]any, 0, len(needsSteward))
			for _, it := range needsSteward {
				rows = append(rows, map[string]any{"issue": it.ID, "reason": it.StewardReason})
			}
			res["needs_steward"] = rows
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave list", res))
	}
	if len(items) == 0 {
		w := cmd.OutOrStdout()
		// "Empty" here means no ACTIVE weaves; terminal items (done/
		// abandoned) are hidden without --history. Say so, with the
		// count, so an all-terminal queue doesn't read as "nothing ever
		// happened here".
		history := 0
		for _, it := range q.Items {
			if it.State == "done" || it.State == "abandoned" {
				history++
			}
		}
		if history > 0 {
			fmt.Fprintf(w, "weave list: no active weaves for %s (this repo); %d in history (--history to view)\n", filepath.Base(root), history)
		} else {
			fmt.Fprintf(w, "weave list: no weaves for %s (this repo)\n", filepath.Base(root))
		}
		// Only surface OTHER queues that have active work — an all-
		// terminal sibling queue is not somewhere to cd to.
		if weaveQueueSummaries(w, dir, true) > 0 {
			fmt.Fprintln(w, "  (queues are per-repo; cd there, or `weave list --all` for full tables)")
		}
		weavePrintSalvageableFooter(w, salvageable)
		weavePrintReclaimableFooter(w, reclaimable)
		return nil
	}
	if unattended > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "ATTENTION: %d unattended item(s) — see `weave doctor`\n", unattended)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%-4s %-4s %-6s %-3s %-14s %-9s %-10s %-8s %-8s %-40s %-24s %s\n", "ID", "PRIO", "STAGE", "PTS", "STATE", "TOOL", "UNMERGED", "STARTED", "DUR", "TITLE", "REF", "WORKSPACE")
	weaveRenderItemRows(cmd.OutOrStdout(), q, items)
	if anyStale {
		fmt.Fprintln(cmd.OutOrStdout(), "* wrapper process is dead — re-attach with `weave start --resume --issue N` or `weave abandon N`")
	}
	weavePrintIsolationFooter(cmd.OutOrStdout(), violated)
	weavePrintOutsideWorkspaceRefs(cmd.OutOrStdout(), outsideRefs)
	weavePrintSalvageableFooter(cmd.OutOrStdout(), salvageable)
	weavePrintStewardFooter(cmd.OutOrStdout(), needsSteward)
	weavePrintReclaimableFooter(cmd.OutOrStdout(), reclaimable)
	return nil
}

// weavePrintStewardFooter names submissions that have been sitting unmerged
// past the steward threshold. A `submitted` item has no process behind it and
// no timeout: without this line it is indistinguishable from work in flight,
// which is exactly how a finished branch waits forever.
func weavePrintStewardFooter(w io.Writer, items []*weaveItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "needs steward decision (%d):\n", len(items))
	for _, it := range items {
		fmt.Fprintf(w, "  #%d %s\n", it.ID, it.StewardReason)
	}
}

type weavePauseResult struct {
	Issue       int64  `json:"issue"`
	Tool        string `json:"tool,omitempty"`
	Head        string `json:"head,omitempty"`
	WrapperPid  int    `json:"wrapper_pid,omitempty"`
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	AlreadyDead bool   `json:"already_dead,omitempty"`
}

type weaveResumeResult struct {
	Issue      int64  `json:"issue"`
	Tool       string `json:"tool,omitempty"`
	WrapperPid int    `json:"wrapper_pid,omitempty"`
	State      string `json:"state"`
	Detail     string `json:"detail,omitempty"`
}

func runWeavePause(cmd *cobra.Command, reason string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave pause",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)

	type target struct {
		id      int64
		pid     int
		startID string
	}
	var targets []target
	targetIDs := map[int64]bool{}
	if q, err := loadWeaveQueue(dir); err == nil {
		for _, it := range q.Items {
			if it.State == "working" && weaveControlOwned(dir, q, it) {
				if err := weaveVerifiedWrapper(cmd.Context(), it); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "weave pause: run #%d skipped: %v\n", it.ID, err)
					continue
				}
				targets = append(targets, target{id: it.ID, pid: it.WrapperPid, startID: it.WrapperStartID})
				targetIDs[it.ID] = true
			}
		}
	}
	// Persist explicit owner intent before signalling. The wrapper checkpoints
	// and acknowledges paused only after its owned child exits.
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		for _, t := range targets {
			it := findWeaveItem(q, t.id)
			if it == nil || it.WrapperPid != t.pid || it.WrapperStartID != t.startID || !weaveControlOwned(dir, q, it) {
				delete(targetIDs, t.id)
				continue
			}
			it.PauseRequestedBy, _ = weaveConductorIdentity("")
			it.PauseReason = reason
		}
		return nil
	}); err != nil {
		return err
	}
	for _, t := range targets {
		if !targetIDs[t.id] {
			continue
		}
		if t.pid > 0 && pidAlive(t.pid) {
			if err := weaveStopVerifiedWrapper(cmd.Context(), t.pid, t.startID, func() bool {
				q, e := loadWeaveQueue(dir)
				if e != nil {
					return false
				}
				it := findWeaveItem(q, t.id)
				return it != nil && it.State == "paused" && it.WrapperStartID == t.startID && it.PauseRequestedBy != ""
			}); err != nil {
				delete(targetIDs, t.id)
				fmt.Fprintf(cmd.ErrOrStderr(), "weave pause: run #%d retained: %v\n", t.id, err)
			}
		}
	}

	var results []weavePauseResult
	var releasedLease *weaveOrchestratorLease
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		if l, ok, err := loadWeaveAutopilotLease(dir); err != nil {
			return err
		} else if actor, known := weaveConductorIdentity(""); ok && known && actor == l.Holder {
			releasedLease = &l
			q.PausedOrchestratorLease = &l
			if err := os.Remove(weaveAutopilotLeasePath(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		for _, it := range q.Items {
			if !targetIDs[it.ID] || !weaveControlOwned(dir, q, it) {
				continue
			}
			if it.State == "paused" && it.WrapperPid == 0 && it.PauseRequestedBy != "" {
				results = append(results, weavePauseResult{Issue: it.ID, Tool: it.Tool, Head: it.Head, State: "paused", Reason: it.PauseReason})
				continue
			}
			matched := false
			for _, t := range targets {
				if t.id == it.ID && t.pid == it.WrapperPid && t.startID == it.WrapperStartID {
					matched = true
				}
			}
			if !matched || pidAlive(it.WrapperPid) {
				continue
			}
			alreadyDead := it.WrapperPid > 0 && !pidAlive(it.WrapperPid)
			head := ""
			if it.Workspace != "" {
				if out, err := exec.Command(gitBin(), "-C", it.Workspace, "rev-parse", "HEAD").Output(); err == nil {
					head = strings.TrimSpace(string(out))
				}
			}
			it.State = "paused"
			it.Head = head
			it.WrapperPid = 0
			it.WrapperStartID = ""
			it.CtlSock = ""
			results = append(results, weavePauseResult{
				Issue:       it.ID,
				Tool:        it.Tool,
				Head:        head,
				State:       it.State,
				Reason:      reason,
				AlreadyDead: alreadyDead,
			})
		}
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave pause",
			weavecli.ExitGenericFail, lockErr))
	}
	if mode == weavecli.OutputJSON {
		res := map[string]any{"paused": results}
		if releasedLease != nil {
			res["released_orchestrator_lease"] = releasedLease
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave pause", res))
	}
	if len(results) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "weave pause: no working items")
		return nil
	}
	for _, r := range results {
		fmt.Fprintf(cmd.OutOrStdout(), "weave pause: run #%d paused tool=%s head=%s\n", r.Issue, r.Tool, r.Head)
	}
	if releasedLease != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "weave pause: released orchestrator lease holder=%s tool=%s\n", releasedLease.Holder, releasedLease.Tool)
	}
	return nil
}

func runWeaveResume(cmd *cobra.Command, issueID int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave resume",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)

	var paused []*weaveItem
	var restoredLease *weaveOrchestratorLease
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		if issueID > 0 && findWeaveItem(q, issueID) == nil {
			return fmt.Errorf("run #%d not found%s", issueID, weaveOtherActiveQueuesHintSuffix(dir))
		}
		if actor, known := weaveConductorIdentity(""); q.PausedOrchestratorLease != nil && known && actor == q.PausedOrchestratorLease.Holder {
			l := *q.PausedOrchestratorLease
			now := time.Now().UTC()
			ttl := l.ExpiresAt.Sub(l.HeartbeatAt)
			if ttl <= 0 {
				ttl = 30 * time.Second
			}
			l.PID = os.Getpid()
			l.HeartbeatAt = now
			l.ExpiresAt = now.Add(ttl)
			if err := saveWeaveAutopilotLease(dir, l); err != nil {
				return err
			}
			restoredLease = &l
			q.PausedOrchestratorLease = nil
		}
		for _, it := range q.Items {
			if issueID > 0 && it.ID != issueID {
				continue
			}
			if it.State == "paused" && weaveControlOwned(dir, q, it) && (it.ResourceReservationID == "" || it.ResourceTerminated) {
				cp := *it
				paused = append(paused, &cp)
			}
		}
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") {
			code = weavecli.ExitInvalidArg
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave resume", code, lockErr))
	}

	var results []weaveResumeResult
	for _, it := range paused {
		if it.LaunchSpec == nil || it.LaunchSpec.Tool == "" {
			results = append(results, weaveResumeResult{Issue: it.ID, State: it.State, Detail: "missing launch_spec"})
			continue
		}
		pid, err := weaveSpawnResumeWrapper(dir, it.ID, it.LaunchSpec)
		if err != nil {
			results = append(results, weaveResumeResult{Issue: it.ID, Tool: it.Tool, State: it.State, Detail: err.Error()})
			continue
		}
		results = append(results, weaveResumeResult{Issue: it.ID, Tool: it.LaunchSpec.Tool, WrapperPid: pid, State: "working"})
	}
	if mode == weavecli.OutputJSON {
		res := map[string]any{"resumed": results}
		if restoredLease != nil {
			res["restored_orchestrator_lease"] = restoredLease
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave resume", res))
	}
	if len(results) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "weave resume: no paused items")
		return nil
	}
	for _, r := range results {
		if r.Detail != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "weave resume: run #%d skipped: %s\n", r.Issue, r.Detail)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "weave resume: run #%d relaunched tool=%s wrapper_pid=%d\n", r.Issue, r.Tool, r.WrapperPid)
	}
	if restoredLease != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "weave resume: restored orchestrator lease holder=%s tool=%s\n", restoredLease.Holder, restoredLease.Tool)
	}
	return nil
}

func weaveSpawnResumeWrapper(dir string, issueID int64, spec *weaveLaunchSpec) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	args := []string{"weave", "start", "--resume", "--issue", strconv.FormatInt(issueID, 10)}
	if spec.IdleTimeout > 0 {
		args = append(args, "--idle-timeout", spec.IdleTimeout.String())
	}
	if spec.MaxRuntime > 0 {
		args = append(args, "--max-runtime", spec.MaxRuntime.String())
	}
	if spec.MemLimit != "" {
		args = append(args, "--mem-limit", spec.MemLimit)
	}
	if spec.PTY != "" {
		args = append(args, "--pty", spec.PTY)
	}
	args = append(args, "--")
	args = append(args, spec.Tool)
	args = append(args, spec.Argv...)

	c := exec.Command(exe, args...)
	c.Stdin = nil
	var logFile *os.File
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err == nil {
		if f, err := os.OpenFile(filepath.Join(logsDir, fmt.Sprintf("issue-%d-wrapper.log", issueID)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			logFile = f
			c.Stdout = f
			c.Stderr = f
		}
	}
	if err := c.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return 0, err
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	pid := c.Process.Pid
	_ = c.Process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		q, err := loadWeaveQueue(dir)
		if err == nil {
			if it := findWeaveItem(q, issueID); it != nil && it.State == "working" && it.WrapperPid > 0 {
				return it.WrapperPid, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return pid, nil
}

// weavePrintReclaimableFooter emits the one-line clutter hint when
// terminal items still hold workspace clones on disk.
func weavePrintReclaimableFooter(w io.Writer, reclaimable int) {
	if reclaimable <= 0 {
		return
	}
	noun := "workspace"
	if reclaimable != 1 {
		noun = "workspaces"
	}
	fmt.Fprintf(w, "+%d terminal item(s) holding %s on disk — run `weave prune` to reclaim\n", reclaimable, noun)
}

func runWeaveNext(cmd *cobra.Command, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave next",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, _ := loadWeaveQueue(dir)
	it := nextTodo(q)
	if it == nil {
		// A BLOCKED queue must never look like an EMPTY one. "queue empty" means
		// "we are finished"; a queue full of work that is all waiting on a dead
		// dependency means "we are stuck" — and if both print the same line, a
		// conductor (human or agent) reads a deadlock as success and walks away.
		blocked := weaveBlockedTodos(q)
		if mode == weavecli.OutputJSON {
			out := map[string]any{"empty": len(blocked) == 0}
			if len(blocked) > 0 {
				out["blocked"] = weaveBlockedJSON(q, blocked)
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave next", out))
		}
		if len(blocked) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "weave next: queue empty")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "weave next: nothing claimable — %d issue(s) blocked\n", len(blocked))
		for _, b := range blocked {
			var parts []string
			dead := false
			for _, dep := range weaveBlockers(q, b) {
				parts = append(parts, fmt.Sprintf("#%d (%s)", dep.ID, dep.State))
				if weaveDepDead(dep) {
					dead = true
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  #%-4d %-40s waits on %s\n", b.ID, truncate(b.Title, 40), strings.Join(parts, ", "))
			if dead {
				fmt.Fprintf(cmd.OutOrStdout(), "        a dependency is dead — `weave link %d --depends-on <id> --unlink` to unstick it\n", b.ID)
			}
		}
		return nil
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave next", map[string]any{
			"issue":    it.ID,
			"title":    it.Title,
			"priority": it.Priority,
			"stage":    itemStage(it),
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave next: #%d (%s/%s) %q\n", it.ID, it.Priority, itemStage(it), it.Title)
	return nil
}

// weaveBlockedTodos are the todo items held back ONLY by an unmet dependency —
// i.e. the work that would run if its blockers landed. Containers are excluded:
// an epic is not "blocked", it is worked through its children.
func weaveBlockedTodos(q *weaveQueue) []*weaveItem {
	var out []*weaveItem
	for _, it := range q.Items {
		if it.State == "todo" && !weaveIsContainer(q, it) && len(weaveBlockers(q, it)) > 0 {
			out = append(out, it)
		}
	}
	return out
}

func weaveBlockedJSON(q *weaveQueue, blocked []*weaveItem) []map[string]any {
	out := make([]map[string]any, 0, len(blocked))
	for _, b := range blocked {
		var deps []map[string]any
		for _, dep := range weaveBlockers(q, b) {
			deps = append(deps, map[string]any{
				"issue": dep.ID,
				"state": dep.State,
				"dead":  weaveDepDead(dep),
			})
		}
		out = append(out, map[string]any{
			"issue":    b.ID,
			"title":    b.Title,
			"waits_on": deps,
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// weaveStartOptions controls runWeaveStart behavior. noSpawn does
// every step up to and including state mutation but skips the tool
// exec. resume reattaches to an existing "working" workspace without
// rebuilding the worktree — useful when an agent crashed and the
// user wants to re-launch it inside the same workspace without losing
// in-progress changes. pty controls TTY allocation for the subagent
// (auto = on, always = on, never = inherit FDs). idleTimeout, when
// > 0, sends SIGTERM to the subagent if no PTY output appears for
// that long — the dogfood found that some TUI agents (claude TUI,
// when launched without -p) never exit on their own and need a
// heuristic kill on idle.
type weaveStartOptions struct {
	noSpawn bool
	resume  bool
	// clone runs this issue under a per-issue EPHEMERAL clone of the named agent
	// instead of the agent itself, so several issues can run in parallel without
	// sharing one identity's cursor, kb attribution and ledger. Without it, an
	// agent already working another issue is left to finish it — see
	// weave_agent_singleton.go.
	clone       bool
	pty         string // "auto" (default), "always", "never"
	idleTimeout time.Duration
	maxRuntime  time.Duration
	memLimit    string // e.g. "16g"; "0" disables
}

// weaveGuards bundles the subagent watchdog limits threaded into
// runWeaveToolPTY. Three independent tripwires, each SIGTERMing the
// subagent's process tree:
//   - idleTimeout: no PTY output for this long (stuck-TUI heuristic;
//     useless against a runaway TUI, whose spinner keeps emitting).
//   - maxRuntime: hard wall-clock ceiling, immune to spinner output.
//   - memLimitBytes: total RSS of the subagent's process tree. This
//     is the OOM backstop — whatever the leak mechanism, the agent
//     dies at the budget instead of taking the machine down.
type weaveGuards struct {
	idleTimeout   time.Duration
	maxRuntime    time.Duration
	memLimitBytes int64
	// ctlSock, when non-empty, is the unix socket runWeaveToolPTY
	// serves for `weave say`: each line received is written to the
	// PTY master with a trailing \r — keystrokes, as far as the
	// subagent can tell.
	ctlSock string
	// coachee is the binding of the agent running this item, used by the reflex
	// coach's P2b escalation to pick an agent-coach one band above it.
	coachee string
	// eventsPath is a tool-declared structured progress stream. Its follower
	// writes concise events to the worker log and feeds the idle watchdog.
	eventsPath string
}

// errWeaveWrapperLive is returned from inside the queue-lock callback
// when the issue already has a live wrapper process; runWeaveStart
// translates it into an ExitStateConflict envelope instead of the
// generic "queue write failed (continuing)" path.
var errWeaveWrapperLive = errors.New("wrapper already running")

// parseWeaveMemLimit parses a human byte size ("16g", "512m",
// "1024k", plain bytes). Empty or "0" disables the limit.
func parseWeaveMemLimit(s string) (int64, error) {
	orig := s
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "b")
	mult := int64(1)
	if len(s) > 0 {
		switch s[len(s)-1] {
		case 'k':
			mult, s = 1<<10, s[:len(s)-1]
		case 'm':
			mult, s = 1<<20, s[:len(s)-1]
		case 'g':
			mult, s = 1<<30, s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid --mem-limit %q (want e.g. 16g, 512m, 0 to disable)", orig)
	}
	return n * mult, nil
}

// weavePTYMode returns the normalized PTY mode for runWeaveStart.
func (o weaveStartOptions) ptyMode() string {
	switch o.pty {
	case "always", "never", "auto":
		return o.pty
	case "":
		return "auto"
	default:
		return "auto"
	}
}

func weaveLaunchSpecFromArgs(toolArgs []string, opts weaveStartOptions) *weaveLaunchSpec {
	if len(toolArgs) == 0 {
		return nil
	}
	return &weaveLaunchSpec{
		Tool:        toolArgs[0],
		Argv:        append([]string(nil), toolArgs[1:]...),
		MaxRuntime:  opts.maxRuntime,
		MemLimit:    opts.memLimit,
		IdleTimeout: opts.idleTimeout,
		PTY:         opts.ptyMode(),
	}
}

func weaveResultTurns(s string) int64 {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			NumTurns int64  `json:"num_turns"`
		}
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "result" {
			return ev.NumTurns
		}
	}
	return 0
}

func runWeaveStart(cmd *cobra.Command, issueID int64, toolFlag string, toolArgs []string, opts weaveStartOptions, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if len(toolArgs) == 0 && toolFlag != "" {
		toolArgs = []string{toolFlag}
	}
	if !opts.noSpawn && len(toolArgs) == 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitInvalidArg, fmt.Errorf("provide trailing '-- <tool> [args...]' or --tool <name> (or pass --no-spawn to allocate only)")))
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitGenericFail, err))
	}
	var it *weaveItem
	if issueID > 0 {
		it = findWeaveItem(q, issueID)
		if it == nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", issueID, weaveOtherActiveQueuesHintSuffix(dir))))
		}
	} else {
		if opts.resume {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitInvalidArg, fmt.Errorf("--resume requires --issue <id>")))
		}
		it = nextTodo(q)
		if it == nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitPrecondFail, fmt.Errorf("queue empty")))
		}
	}
	if it.State == "done" || it.State == "abandoned" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d state is %q", it.ID, it.State)))
	}
	if it.ResourceReservationID != "" && !it.ResourceTerminated {
		return fmt.Errorf("run #%d prior child termination is unverified; reconcile its retained reservation before restart", it.ID)
	}
	boundedRuntime, budgetErr := weaveBoundRuntime(it.Points, opts.maxRuntime)
	if budgetErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d: %w", it.ID, budgetErr)))
	}
	opts.maxRuntime = boundedRuntime
	launchSpec := weaveLaunchSpecFromArgs(toolArgs, opts)
	// `-- 007` / `-- claude:opus` name an AGENT, so the launch argv comes from
	// the registry with the issue body as the prompt. This is resolved here,
	// after the issue is known, because the body IS the prompt.
	agentLaunch, agentArgv, aerr := weaveExpandAgent(toolArgs, it.Body, it.Title)
	if aerr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitInvalidArg, aerr))
	}

	// ONE AGENT, ONE LIVE ISSUE. Two issues under one identity mix context, and
	// a worker answering about this issue using what it learned on another is
	// wrong in the way that looks right. See weave_agent_singleton.go.
	//
	// Ephemeral workers of FINISHED issues are reclaimed here rather than by a
	// sweeper — reading is the reconciliation, as everywhere else in this queue.
	weaveReapIssueClones(q)
	if agentLaunch != nil && agentLaunch.Named() {
		if busy := weaveAgentWorkingOn(q, agentLaunch.Nick, it.ID); busy != nil {
			if !opts.clone {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitStateConflict, weaveAgentBusyErr(agentLaunch.Nick, busy, it)))
			}
			cloneName, cerr := weaveCloneAgentForIssue(agentLaunch.Nick, it.ID)
			if cerr != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitGenericFail, cerr))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "weave: %s is on #%d; running #%d as %s (its own context)\n",
				agentLaunch.Nick, busy.ID, it.ID, cloneName)
			toolArgs = []string{cloneName}
			launchSpec = weaveLaunchSpecFromArgs(toolArgs, opts)
			agentLaunch, agentArgv, aerr = weaveExpandAgent(toolArgs, it.Body, it.Title)
			if aerr != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitInvalidArg, aerr))
			}
		}
	}
	// Record WHICH AGENT is working this issue. The field was declared for this
	// and never filled, so the queue could say which tool was running but not
	// which identity — which is exactly what the check above needs to read.
	if agentLaunch != nil && launchSpec != nil {
		launchSpec.Agent = agentLaunch.Nick
	}
	// Bare-tool launch guard. A single tool token that did not resolve to a fleet
	// agent (agentLaunch==nil, len==1) is launched VERBATIM under a PTY — deliberately
	// interactive, which is correct at a real terminal. But in a headless run (a
	// conductor driving `weave start` over a pipe, no controlling TTY) that interactive
	// TUI has no one to answer it and hangs until the watchdog kills it — burning the
	// conductor's whole budget on a silent stall (observed live). Fail loud instead,
	// and say exactly how to fix it: pin a fleet AGENT, which expands to the tool's
	// headless argv.
	if agentLaunch == nil && len(toolArgs) == 1 && !opts.noSpawn && opts.ptyMode() != "never" &&
		!term.IsTerminal(int(os.Stdin.Fd())) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitInvalidArg, fmt.Errorf(
				"pinned worker %q is a bare tool, not a headless agent — with no controlling terminal it launches an interactive TUI that hangs. Pin a fleet AGENT instead (a nickname, or tool:model like %q); `bashy agent list` shows the choices",
				toolArgs[0], toolArgs[0]+":<model>")))
	}
	// displayTool is what the queue records; for an agent launch it is the
	// tool's registry name, not the resolved executable path.
	displayTool := weaveToolDisplayName(toolArgs)
	ownerBase := displayTool
	if agentLaunch != nil {
		toolArgs = agentArgv
		launchSpec = weaveLaunchSpecFromArgs(toolArgs, opts)
		launchSpec.Agent, launchSpec.Model = agentLaunch.Nick, agentLaunch.Model
		displayTool, ownerBase = agentLaunch.ToolName, agentLaunch.Nick
	}
	trustLaunch := weaveTrustLaunchFor(displayTool)
	if opts.resume {
		// Any state with a preserved workspace is resumable: "working"
		// (wrapper died), "failed" (weave kill / watchdog kill — the
		// retry path the kill docs promise), "submitted" (tool exited
		// but the branch was kicked back, e.g. merge conflict). done
		// and abandoned were rejected above; their workspaces are gone.
		if it.Workspace == "" {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitStateConflict, fmt.Errorf("--resume: run #%d has no workspace to reattach (state=%q)", it.ID, it.State)))
		}
		if _, err := os.Stat(it.Workspace); err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitStateConflict, fmt.Errorf("--resume: workspace missing on disk: %s", it.Workspace)))
		}
	}
	// The wrapper owns the lifecycle lock and reservation before provisioning.
	// A detached launcher merely starts this process; it owns no capacity.
	lifecycle, admissionErr := weaveRunLifecycleLock(dir, it.ID)
	if admissionErr != nil {
		return fmt.Errorf("run #%d is active or being reclaimed: %w", it.ID, admissionErr)
	}
	defer lifecycle.Release()
	memoryDemand, admissionErr := parseWeaveMemLimit(opts.memLimit)
	if admissionErr != nil {
		return admissionErr
	}
	admissionModel, admissionAgent := "", ""
	if launchSpec != nil {
		admissionModel, admissionAgent = launchSpec.Model, launchSpec.Agent
	}
	admission, admissionErr := beginWeaveAdmission(cmd.Context(), weaveResourceHooks(cmd.Context()), WeaveResourceDemand{
		Run: filepath.Join(dir, strconv.FormatInt(it.ID, 10)), Queue: dir, Model: admissionModel, Agent: admissionAgent, Workspace: it.Workspace, MemoryBytes: uint64(memoryDemand),
	})
	if admissionErr != nil {
		return admissionErr
	}
	if opts.ptyMode() == "never" {
		stopSignals := weaveWatchPlainTermination(admission.cancel)
		defer stopSignals()
	}
	childLaunched, childTerminated := false, false
	defer func() {
		if !childLaunched || childTerminated {
			_ = withWeaveQueueLock(dir, func(q *weaveQueue) error {
				if current := findWeaveItem(q, it.ID); current != nil && current.ResourceReservationID == admission.request.ID {
					current.ResourceTerminated = true
				}
				return nil
			})
		}
		if err := admission.finish(childTerminated, childLaunched); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "weave: reservation retained: %v\n", err)
		}
	}()
	base := weaveBaseBranch(root)
	// Snapshot the actual source commit before provisioning. `--branch main`
	// resolves through the clone's shared refs and can silently select a newer
	// main than a detached conductor checkout. A run's base is a commit, not a
	// moving branch name.
	baseOut, baseErr := exec.CommandContext(admission.ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if baseErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitPrecondFail, fmt.Errorf("resolve source HEAD: %w", baseErr)))
	}
	baseSHA := strings.TrimSpace(string(baseOut))
	workspace := filepath.Join(dir, "workspaces", fmt.Sprintf("issue-%d", it.ID))
	branch := fmt.Sprintf("agent/weave-issue-%d", it.ID)
	if opts.resume {
		workspace = it.Workspace
		branch = it.Branch
	}
	agentEventsPath := ""
	if agentLaunch != nil {
		toolArgs, err = weaveBindAgentWorkspace(agentLaunch, toolArgs, workspace)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitPrecondFail, err))
		}
		if len(agentlaunch.EventFileArgsWithCatalog(*agentLaunch, "events", fleetCatalog)) > 0 {
			agentEventsPath = filepath.Join(workspace, ".git", "weave-agent-events.jsonl")
			toolArgs = weaveAgentEventsFileArgv(agentLaunch, toolArgs, agentEventsPath)
		}
		launchSpec = weaveLaunchSpecFromArgs(toolArgs, opts)
		launchSpec.Agent, launchSpec.Model = agentLaunch.Nick, agentLaunch.Model
	}
	// Control socket for `weave say` — only meaningful when the
	// subagent gets a PTY and we're actually spawning it.
	ctlSock := ""
	if opts.ptyMode() != "never" && !opts.noSpawn {
		ctlSock = weaveCtlSockPath(dir, it.ID)
	}
	if opts.resume {
		// Re-claim the issue under the queue lock: refuse when a
		// previous wrapper is still alive (two wrappers in one
		// workspace = two agents fighting over the same checkout —
		// the dogfood OOM had a stale wrapper_pid precisely
		// because resume skipped this bookkeeping), and record
		// OUR pid so `weave kill` / `weave abandon` signal the
		// process that is actually running, not a long-dead one.
		lockErr := withWeaveQueueLock(dir, func(freshQ *weaveQueue) error {
			freshIt := findWeaveItem(freshQ, it.ID)
			if freshIt == nil {
				return fmt.Errorf("queue lock: run #%d disappeared", it.ID)
			}
			if freshIt.WrapperPid > 0 && freshIt.WrapperPid != os.Getpid() && pidAlive(freshIt.WrapperPid) {
				return fmt.Errorf("run #%d already has a live wrapper (pid %d); run `bashy weave kill --issue %d` first: %w",
					it.ID, freshIt.WrapperPid, it.ID, errWeaveWrapperLive)
			}
			prevOwner := freshIt.Owner
			freshIt.WrapperPid = os.Getpid()
			weaveRecordAdmission(freshIt, admission)
			// Flip back to working and clear the stale terminal
			// record — otherwise `weave list` shows failed while an
			// agent is actively running and `weave wait` returns
			// immediately on the old terminal state. The append-only
			// body/comments keep the old killed/failed facts; the item
			// fields describe only the current run.
			freshIt.State = "working"
			freshIt.LaunchPhase = "launching agent"
			weaveClearCurrentRunTerminalEvidence(freshIt)
			// Re-baseline: this is a NEW run in the same workspace, so it
			// is answerable for what the live checkout does from HERE, not
			// for drift left by the run that died.
			weaveRecordLiveBaseline(freshIt, root)
			freshIt.StartedAt = time.Now().UTC()
			freshIt.CtlSock = ctlSock
			if len(toolArgs) > 0 {
				freshIt.Tool = displayTool
				// A resume may deliberately reassign the preserved workspace to a
				// different agent. The live owner and launch spec must change
				// atomically or list/status attribute the new process to the dead
				// worker's seat.
				freshIt.Owner = weaveAgentName(ownerBase, freshIt.ID)
				freshIt.LaunchSpec = launchSpec
			}
			if prevOwner == "" && freshIt.Owner != "" {
				weaveAppendComment(freshIt, "conductor", "system",
					fmt.Sprintf("assigned to %s", freshIt.Owner))
			} else if prevOwner != "" && prevOwner != freshIt.Owner {
				weaveAppendComment(freshIt, "conductor", "system",
					fmt.Sprintf("reassigned %s → %s", prevOwner, freshIt.Owner))
			}
			weaveQueueOwnerNotice(dir, freshQ, freshIt, "assignment-started")
			it = freshIt
			return nil
		})
		if lockErr != nil {
			code := weavecli.ExitGenericFail
			if errors.Is(lockErr, errWeaveWrapperLive) {
				code = weavecli.ExitStateConflict
			}
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start", code, lockErr))
		}
		weaveDeliverOwnerNotices(dir)
	}
	if !opts.resume {
		// Persist before clone/hydration. This is deliberately allocated rather
		// than working: no child has launched yet, but list/status can now name
		// the workspace, immutable base, and the operation holding progress.
		if err := withWeaveQueueLock(dir, func(freshQ *weaveQueue) error {
			freshIt := findWeaveItem(freshQ, it.ID)
			if freshIt == nil {
				return fmt.Errorf("queue lock: run #%d disappeared", it.ID)
			}
			// CONFIRM THE CLAIM UNDER THE LOCK.
			//
			// The item was chosen by nextTodo well above this, outside any lock,
			// so two `weave start` calls racing for top-of-queue can both have
			// selected it. Without this check the loser wrote its own WrapperPid
			// over the winner's and both proceeded into the same workspace — two
			// agents on one checkout, and the pid that `weave kill` would signal
			// belonging to only one of them. The same guard the --resume path has
			// always had; it was simply never applied to a fresh claim.
			if freshIt.WrapperPid > 0 && freshIt.WrapperPid != os.Getpid() && pidAlive(freshIt.WrapperPid) {
				return fmt.Errorf("run #%d was claimed by another start (pid %d) while this one was preparing; retry to take the next run: %w",
					it.ID, freshIt.WrapperPid, errWeaveWrapperLive)
			}
			freshIt.State = "allocated"
			freshIt.Workspace = workspace
			freshIt.Branch = branch
			freshIt.BaseSHA = baseSHA
			freshIt.LaunchPhase = "provisioning workspace"
			freshIt.StartedAt = time.Now().UTC()
			// Record the provisioning launcher before clone/hydration. If it
			// disappears before the working transition, list/status can turn the
			// allocation into durable failure instead of advertising a phantom
			// worker forever.
			freshIt.WrapperPid = os.Getpid()
			weaveRecordAdmission(freshIt, admission)
			it = freshIt
			return nil
		}); err != nil {
			code := weavecli.ExitGenericFail
			if errors.Is(err, errWeaveWrapperLive) {
				// A lost race is a STATE CONFLICT, not a generic failure: the
				// caller's correct response is to retry for the next run, and
				// the exit code is what tells a scripted fan-out that.
				code = weavecli.ExitStateConflict
			}
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start", code, err))
		}
	}
	if !opts.resume {
		if _, err := os.Stat(workspace); err != nil {
			if err := os.MkdirAll(filepath.Dir(workspace), 0o755); err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitGenericFail, err))
			}
			// Workspace isolation: a full local clone, NOT a worktree.
			// git worktree shares `.git/objects` and `.git/refs` with
			// the source repo — an agent that wandered out of its
			// workspace cwd (cd to the source checkout, or `git
			// update-ref`) could mutate the source's branches. With a
			// clone, the workspace has its own `.git`; refs and HEAD
			// can't cross the boundary, and a wandering agent hits a
			// different git repo entirely.
			gw := exec.CommandContext(admission.ctx, "git", "clone", "--local", "--no-hardlinks", "--no-checkout", root, workspace)
			gw.Stdout = cmd.OutOrStdout()
			gw.Stderr = cmd.ErrOrStderr()
			if err := gw.Run(); err != nil {
				weaveMarkLaunchFailed(dir, it.ID, fmt.Errorf("clone workspace: %w", err))
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitGenericFail, fmt.Errorf("git clone --local --no-hardlinks: %w", err)))
			}
			// Check out the per-issue agent branch in the clone.
			ck := exec.CommandContext(admission.ctx, "git", "-C", workspace, "checkout", "-b", branch, baseSHA)
			ck.Stdout = cmd.OutOrStdout()
			ck.Stderr = cmd.ErrOrStderr()
			if err := ck.Run(); err != nil {
				weaveMarkLaunchFailed(dir, it.ID, fmt.Errorf("checkout immutable base %s: %w", baseSHA, err))
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitGenericFail, fmt.Errorf("git checkout -b %s: %w", branch, err)))
			}
			// Remove `origin` from the clone: it points at the user's
			// real checkout, and in dogfooding a subagent followed it
			// (`git remote -v`) to escape the workspace and commit to the
			// origin repo's master directly. Nothing in the weave flow
			// needs the remote — `weave pull` fetches FROM the workspace
			// path into the user's repo, never the other way around.
			_ = exec.CommandContext(admission.ctx, "git", "-C", workspace, "remote", "remove", "origin").Run()
			// Scrub reflogs: `git clone` records "clone: from <abs
			// origin path>" in .git/logs/HEAD — the breadcrumb the
			// second workspace escape had available after the remote
			// was gone. git recreates reflogs as the agent works;
			// only the clone-time entries carry the origin path.
			_ = os.RemoveAll(filepath.Join(workspace, ".git", "logs"))
			// Hydrate git submodules from the LOCAL origin. `git clone --local`
			// does NOT recurse submodules, so a repo whose go.mod `replace`s point
			// into a submodule (coreutils -> external/{ollama,podman}/src) can't
			// build until the submodule working trees exist. This is what blocked
			// coreutils self-repair: the full gate died on a missing
			// external/ollama/src/go.mod.
			_ = withWeaveQueueLock(dir, func(freshQ *weaveQueue) error {
				if freshIt := findWeaveItem(freshQ, it.ID); freshIt != nil {
					freshIt.LaunchPhase = "hydrating submodules"
				}
				return nil
			})
			if err := admission.ctx.Err(); err != nil {
				return err
			}
			if err := weaveHydrateSubmodules(root, workspace, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
				weaveMarkLaunchFailed(dir, it.ID, fmt.Errorf("hydrate submodules: %w", err))
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitGenericFail, fmt.Errorf("hydrate submodules: %w", err)))
			}
		}
		// Faithful, fast, Windows-safe sibling-dep view. A clone of the target
		// alone can't build when its go.mod has `replace … => ../X` directives
		// (sh/coreutils/readline in this umbrella). We provide each such sibling
		// as a SHARED clone placed directly at <workspaces>/<name> — exactly
		// where `../<name>` resolves from every issue workspace, so NO symlinks
		// are needed (symlinks require admin/Developer Mode on Windows). It is a
		// clone (the user's real repo is never edited), shared across the
		// queue's workspaces (cloned once), and re-synced to the source's
		// current HEAD on every start — fixing the prior staleness where a
		// workspace built against an old sibling SHA (the #1 weave friction).
		// Nested replaces resolve too: <workspaces>/coreutils's own `../sh` lands
		// on <workspaces>/sh, which we also provision.
		_ = withWeaveQueueLock(dir, func(freshQ *weaveQueue) error {
			if freshIt := findWeaveItem(freshQ, it.ID); freshIt != nil {
				freshIt.LaunchPhase = "syncing sibling dependencies"
			}
			return nil
		})
		if synced, failed := weaveSyncSiblingDeps(root, workspace); len(synced) > 0 || len(failed) > 0 {
			if len(synced) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "weave: sibling deps synced to source HEAD: %s\n", strings.Join(synced, ", "))
			}
			for _, f := range failed {
				fmt.Fprintf(cmd.ErrOrStderr(), "weave: WARNING could not provision sibling dep %q — the build may fail; ask the orchestrator for help\n", f)
			}
		}
		// Host-provided workspace provisioning (e.g. agent skills) — the
		// workspace is weave-owned, so the host may stock it freely.
		if ProvisionWorkspace != nil {
			ProvisionWorkspace(workspace, cmd.ErrOrStderr())
		}
		for _, kv := range [][2]string{
			{"user.name", fmt.Sprintf("agent-weave-issue-%d", it.ID)},
			{"user.email", fmt.Sprintf("agent-weave-issue-%d@ycode.local", it.ID)},
		} {
			_ = exec.CommandContext(admission.ctx, "git", "-C", workspace, "config", kv[0], kv[1]).Run()
		}
		// Lock around the state=working transition so concurrent
		// `weave start --issue N` invocations targeting different
		// issues don't race on the queue.json write (last-write-
		// wins would silently strand one of the items).
		lockErr := withWeaveQueueLock(dir, func(freshQ *weaveQueue) error {
			freshIt := findWeaveItem(freshQ, it.ID)
			if freshIt == nil {
				return fmt.Errorf("queue lock: run #%d disappeared", it.ID)
			}
			if freshIt.WrapperPid > 0 && freshIt.WrapperPid != os.Getpid() && pidAlive(freshIt.WrapperPid) {
				return fmt.Errorf("run #%d already has a live wrapper (pid %d); run `bashy weave kill --issue %d` first: %w",
					it.ID, freshIt.WrapperPid, it.ID, errWeaveWrapperLive)
			}
			prevOwner := freshIt.Owner
			freshIt.State = "working"
			freshIt.LaunchPhase = "launching agent"
			freshIt.Workspace = workspace
			freshIt.Branch = branch
			freshIt.WrapperPid = os.Getpid()
			weaveRecordAdmission(freshIt, admission)
			freshIt.CtlSock = ctlSock
			freshIt.StartedAt = time.Now().UTC()
			// Isolation baseline: fingerprint the live checkout NOW, so a
			// later re-check can tell whether this run reached back out of
			// its workspace and touched it. See weave_isolation.go.
			weaveRecordLiveBaseline(freshIt, root)
			// BaseSHA was captured from the source HEAD before clone/hydration;
			// never resolve a mutable branch ref here.
			if len(toolArgs) > 0 {
				freshIt.Tool = displayTool
				// The OWNER is the per-issue seat (`007-a`), not the agent.
				freshIt.Owner = weaveAgentName(ownerBase, freshIt.ID)
				freshIt.LaunchSpec = launchSpec
			}
			// Record assignment / formal reassignment in the task thread.
			// A reassignment (prevOwner set and different) is how a task
			// moves from a failed/killed/incapable agent to a new one.
			switch {
			case prevOwner == "" && freshIt.Owner != "":
				weaveAppendComment(freshIt, "conductor", "system",
					fmt.Sprintf("assigned to %s", freshIt.Owner))
			case prevOwner != "" && prevOwner != freshIt.Owner:
				weaveAppendComment(freshIt, "conductor", "system",
					fmt.Sprintf("reassigned %s → %s", prevOwner, freshIt.Owner))
			}
			weaveQueueOwnerNotice(dir, freshQ, freshIt, "assignment-started")
			it = freshIt
			return nil
		})
		if lockErr != nil {
			if errors.Is(lockErr, errWeaveWrapperLive) {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
					weavecli.ExitStateConflict, lockErr))
			}
			return fmt.Errorf("weave start: queue write failed: %w", lockErr)
		}
		weaveDeliverOwnerNotices(dir)
	}
	memoryPrefix := ""
	if opts.resume {
		memoryPrefix = weaveResumeMemoryPrefix(workspace)
	}
	if err := weaveInjectMemoryFileWithPrefix(dir, workspace, it, memoryPrefix); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "weave start: memory inject failed (continuing): %v\n", err)
	}
	if err := weaveInjectKBFile(dir, workspace, it); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "weave start: kb inject failed (continuing): %v\n", err)
	}
	if mode != weavecli.OutputJSON {
		fmt.Fprintf(cmd.OutOrStdout(), "weave start: run #%d workspace=%s branch=%s\n", it.ID, workspace, branch)
		if opts.noSpawn {
			fmt.Fprintf(cmd.OutOrStdout(), "weave start: --no-spawn (skipping tool exec)\n")
		} else {
			if agentLaunch != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "weave start: launching %s (%s) as %s ...\n",
					agentLaunch.Nick, agentLaunch.Binding(), it.Owner)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "weave start: launching %s ...\n", strings.Join(toolArgs, " "))
			}
		}
	}
	if opts.noSpawn {
		// Allocated, not working: nothing is running, so don't record
		// a wrapper pid that will immediately read as a dead/stale
		// worker in `weave list`. The workspace waits for a later
		// `start --resume --issue N -- <tool>` to assign an agent.
		_ = withWeaveQueueLock(dir, func(freshQ *weaveQueue) error {
			if freshIt := findWeaveItem(freshQ, it.ID); freshIt != nil {
				freshIt.State = "allocated"
				freshIt.WrapperPid = 0
				freshIt.CtlSock = ""
				freshIt.StartedAt = time.Time{}
				freshIt.Tool = ""
			}
			return nil
		})
		if mode == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave start", map[string]any{
				"issue":     it.ID,
				"workspace": workspace,
				"branch":    branch,
				"state":     "allocated",
				"no_spawn":  true,
			}))
		}
		return nil
	}
	if err := weaveApplyTrustPreseed(workspace, trustLaunch.Preseed); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "weave start: trust preseed failed (continuing): %v\n", err)
	}
	ctx, span := telemetry.Tracer().Start(admission.ctx, "weave.run")
	defer span.End()

	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)

	// The child's environment — containment, credential firewall, and own-auth
	// preservation all live in weaveChildEnv so the launch assertion can test
	// the same code path this spawn runs. dir is the queue (state) dir, passed
	// so each ycode run gets its own data store under it.
	launcherEnv := os.Environ()
	env := weaveChildEnv(launcherEnv, workspace, branch, base, dir, it, agentLaunch)
	for k, v := range carrier {
		env = append(env, fmt.Sprintf("%s=%s", strings.ToUpper(k), v))
	}
	if err := weaveRunWorkspacePreflight(agentLaunch, workspace, env); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitPrecondFail, err))
	}
	tool := exec.CommandContext(admission.ctx, toolArgs[0], toolArgs[1:]...)
	weaveConfigureOwnedCancellation(tool)
	if opts.ptyMode() == "never" {
		weavePrepareOwnedChild(tool)
	}
	tool.Dir = workspace
	tool.Env = env

	// PTY allocation policy:
	//   - never:        inherit FDs (legacy, breaks TUI subagents).
	//   - always|auto:  allocate a PTY. When parent stdin is a TTY,
	//                   pass-through interactively (raw mode). When
	//                   parent stdin is NOT a TTY (orchestrator pipe
	//                   or backgrounded by shell &), route subagent
	//                   PTY output to a per-issue log file under the
	//                   queue dir so the subagent renders correctly
	//                   AND we don't pump its TUI output back into
	//                   the orchestrator's pipe (the OOM footgun the
	//                   original incident exposed).
	memLimitBytes, err := parseWeaveMemLimit(opts.memLimit)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitInvalidArg, err))
	}
	guards := weaveGuards{
		idleTimeout:   opts.idleTimeout,
		maxRuntime:    opts.maxRuntime,
		memLimitBytes: memLimitBytes,
		ctlSock:       ctlSock,
		coachee:       it.Tool,
		eventsPath:    agentEventsPath,
	}
	if ctlSock != "" {
		if err := os.MkdirAll(filepath.Dir(ctlSock), 0o755); err != nil {
			// Non-fatal: `weave say` degrades to state_conflict.
			guards.ctlSock = ""
		}
	}
	ptyMode := opts.ptyMode()
	parentStdinTTY := weaveStdinIsTTY()
	useLogFile := ptyMode != "never" && !parentStdinTTY
	var logFile *os.File
	var logPath string
	if useLogFile {
		logsDir := filepath.Join(dir, "logs")
		if err := os.MkdirAll(logsDir, 0o755); err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitGenericFail, fmt.Errorf("create log dir: %w", err)))
		}
		logPath = filepath.Join(logsDir, fmt.Sprintf("issue-%d.log", it.ID))
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
				weavecli.ExitGenericFail, fmt.Errorf("open log: %w", err)))
		}
		logFile = f
		// A live worker must be distinguishable from a launch that never began.
		// Agent event streams provide ongoing progress below, while this marker
		// makes the log observable immediately, before the first provider event.
		agentName := ""
		if agentLaunch != nil {
			agentName = agentLaunch.Nick
		}
		fmt.Fprintf(logFile, "[weave] launch started at=%s agent=%q tool=%q\n",
			time.Now().UTC().Format(time.RFC3339), agentName, displayTool)
		if agentEventsPath != "" {
			fmt.Fprintf(logFile, "[weave] structured events path=%q\n", agentEventsPath)
		}
	}
	var captureRedaction weaveCaptureRedaction
	if useLogFile || ptyMode == "never" {
		captureRedaction = newWeaveCaptureRedaction(launcherEnv, cmd.ErrOrStderr())
	}
	if mode != weavecli.OutputJSON && useLogFile {
		fmt.Fprintf(cmd.OutOrStdout(), "weave start: PTY → %s\n", logPath)
	}

	// Auto-detach from the parent shell's session when invoked
	// non-interactively. Without this, a backgrounded ycode (e.g.
	// `bashy weave start ... &` from a script) receives SIGHUP when
	// the launching shell exits, killing the subagent partway
	// through its work. Setsid puts us in a new session so we
	// outlive the launcher. Only safe when stdin is non-TTY — a
	// user at a terminal expects to be able to ^C their own
	// invocation, which Setsid would break.
	weaveMaybeSetsid(parentStdinTTY)

	// Join the host room so a weave worker is discoverable + steerable like every
	// other agent instance — the same board `bashy chat sessions` shows and the
	// same ctlsock `weave attach`/`chat steer` reach (docs/agent-room-mesh-design.md).
	wtool, wmodel, _ := strings.Cut(guards.coachee, ":")
	weaveCard := room.Card{
		ID:        fmt.Sprintf("weave-%d-%d", it.ID, os.Getpid()),
		Principal: "conductor",
		Tool:      wtool,
		Model:     wmodel,
		Binding:   guards.coachee,
		Mode:      "weave",
		Task:      fmt.Sprintf("#%d %s", it.ID, it.Title),
		CtlSock:   guards.ctlSock,
		LogPath:   logPath,
		PID:       os.Getpid(),
		Cwd:       tool.Dir,
	}
	_ = room.Join(weaveCard)
	defer room.Leave(weaveCard.ID)

	var (
		exitCode   int
		killReason string
		runErr     error
		toolStdout bytes.Buffer
		coachRep   chat.CoachReport
		coachMode  string
	)
	if ptyMode == "never" {
		var outputMu sync.Mutex
		stdout := weaveSynchronizedWriter{mu: &outputMu, dst: cmd.OutOrStdout()}
		stderr := weaveSynchronizedWriter{mu: &outputMu, dst: cmd.ErrOrStderr()}
		stdoutCapture := captureRedaction.Writer(io.MultiWriter(stdout, &toolStdout))
		stderrCapture := captureRedaction.Writer(stderr)
		tool.Stdin = os.Stdin
		tool.Stdout = stdoutCapture
		tool.Stderr = stderrCapture
		childLaunched = true
		runErr = tool.Run()
		if err := stdoutCapture.Close(); runErr == nil && err != nil {
			runErr = fmt.Errorf("flush redacted tool stdout: %w", err)
		}
		if err := stderrCapture.Close(); runErr == nil && err != nil {
			runErr = fmt.Errorf("flush redacted tool stderr: %w", err)
		}
		if runErr != nil {
			if ee, ok := runErr.(*exec.ExitError); ok {
				exitCode = ee.ExitCode()
				runErr = nil
			} else {
				exitCode = 1
			}
		}
	} else {
		if useLogFile {
			// Subagent stdio: PTY ↔ log file. stdin is the PTY slave
			// (no user input source); stdout/stderr go to the PTY
			// master which we copy to logFile.
			logCapture := captureRedaction.Writer(logFile)
			childLaunched = true
			exitCode, killReason, coachRep, coachMode, runErr = runWeaveToolPTY(tool, logCapture, guards)
			if err := logCapture.Close(); runErr == nil && err != nil {
				runErr = fmt.Errorf("flush redacted PTY log: %w", err)
			}
			if err := logFile.Close(); runErr == nil && err != nil {
				runErr = fmt.Errorf("close PTY log: %w", err)
			}
		} else {
			// Interactive TTY pass-through.
			childLaunched = true
			exitCode, killReason, coachRep, coachMode, runErr = runWeaveToolPTY(tool, nil, guards)
		}
	}

	// Persist the outcome regardless of envelope mode — `weave wait`
	// and `weave pull` read the queue, not stdout. Take the queue
	// lock for the final read-modify-write so concurrent
	// `weave start` calls (the orchestrator's parallel-agent
	// pattern) don't clobber each other's terminal-state updates.
	// We re-load inside the lock to pick up any updates that
	// landed while the tool was running.
	childLaunched = tool.Process != nil
	childTerminated = weaveWaitOwnedChildTerminated(tool)
	finishedAt := time.Now().UTC()
	// Measure the branch outside the lock: this is the substrate
	// evidence for the terminal state. A non-zero exit (crash,
	// watchdog, kill) with verified commits ahead is still shippable
	// work — record it as submitted WITH the exit code and measurement
	// preserved, so `weave pull` picks it up and the audit trail shows
	// exactly how the run ended. No commits, no submitted: nothing is
	// taken on faith.
	// Verify command (from `weave add --verify`): run it now, outside
	// the lock — it can take up to 10 minutes. Only when there is
	// something to verify (clean exit or commits ahead). The result is
	// EVIDENCE recorded alongside the terminal state; it never changes
	// the submitted/killed/failed decision below — `weave pull` is the
	// consumer that acts on a non-zero verify_exit.
	ev := weaveCollectTerminalEvidence(workspace, weaveCountRef(it, base), dir, "", it, false)
	if it.VerifyCommand != "" && (exitCode == 0 || ev.CommitsAhead > 0) {
		ev = weaveCollectTerminalEvidence(workspace, weaveCountRef(it, base), dir, it.VerifyCommand, it, true)
	}
	var outsideWorkspacePaths []string
	if ev.CommitsAhead == 0 && logPath != "" {
		outsideWorkspacePaths = weaveOutsideWorkspacePaths(weaveReadThrottleLogTail(logPath), workspace)
	}
	autoCommitted := false
	autoCommitErr := ""
	// throttleLogTail carries the matched throttle log chunk out of the
	// queue lock so the cooldown reset can be parsed + recorded after the
	// state write (best-effort, mirroring memory/reporter below).
	throttleLogTail := ""
	// PRESERVE THE ARTIFACT. DO NOT ASSERT SUCCESS ON IT.
	//
	// This gate used to require exitCode == 0, which quietly stranded the work of
	// every tool that crashes on its way OUT. Observed across a whole fleet run:
	// three opencode workers each did their job — tests passing, by their own logs
	// — and then died in their storage layer on exit. Non-zero exit, so no
	// auto-commit, so no commits, so no evidence, so `failed`. The finished work
	// sat uncommitted in a workspace that `weave prune` would have deleted.
	//
	// A crash on the way out is not evidence that the work is bad. It is evidence
	// of nothing at all about the work.
	//
	// So the tree is committed either way — onto an ISOLATED BRANCH that can only
	// reach base through the gate, so committing costs nothing and risks nothing.
	// What does NOT change is the terminal STATE: weaveTerminalState still refuses
	// `submitted` for a non-zero exit, so this never turns a crash into a success.
	// It turns work-that-is-silently-at-risk into work-that-is-recorded-and-must-
	// still-prove-itself.
	//
	// The commit message says plainly which of the two happened.
	autoCommitEligible := (ev.VerifyExit == nil || *ev.VerifyExit == 0) &&
		(ev.Dirty || ev.UntrackedFiles > 0)
	if autoCommitEligible {
		msg := weaveAutoCommitMessageWithContext(it, ev)
		if exitCode != 0 || runErr != nil || killReason != "" {
			msg = weaveCrashedAutoCommitMessage(it, exitCode, killReason)
		}
		committed, err := maybeAutoCommit(workspace, msg)
		autoCommitted = committed
		if err != nil {
			autoCommitErr = err.Error()
			ev = weaveCollectTerminalEvidence(workspace, weaveCountRef(it, base), dir, it.VerifyCommand, it, it.VerifyCommand != "")
		} else if committed {
			ev = weaveCollectTerminalEvidence(workspace, weaveCountRef(it, base), dir, it.VerifyCommand, it, true)
		}
	}
	finalizationClaimed := false
	lockErr := withWeaveQueueLock(dir, func(freshQ *weaveQueue) error {
		freshIt := findWeaveItem(freshQ, it.ID)
		if freshIt == nil {
			return fmt.Errorf("queue lock: run #%d disappeared", it.ID)
		}
		if weaveWrapperTerminalClaimed(freshIt) {
			// A conductor explicitly claimed this interactive run before
			// stopping our process tree. Do not race its measured finalization
			// with an inferred exit outcome from the forced stop.
			finalizationClaimed = true
			it = freshIt
			return nil
		}
		freshIt.ResourceTerminated = childTerminated
		freshIt.FinishedAt = finishedAt
		freshIt.ExitCode = &exitCode
		freshIt.KilledBy = killReason
		freshIt.WrapperPid = 0
		freshIt.CtlSock = ""
		freshIt.Throttled = false
		freshIt.ThrottleSignal = ""
		if logPath != "" {
			logTail := weaveReadThrottleLogTail(logPath)
			throttled, signal := weaveClassifyThrottle(it.Tool, exitCode, logTail)
			if throttled {
				freshIt.Throttled = true
				freshIt.ThrottleSignal = signal
				throttleLogTail = logTail
			}
		}
		weaveApplyTerminalEvidence(freshIt, ev)
		freshIt.OutsideWorkspacePaths = outsideWorkspacePaths
		// Isolation verdict, recorded with the rest of the terminal
		// evidence: did the live checkout move while this run held its
		// workspace? Measured here, under the lock, so the durable record
		// exists even if nobody ever runs `weave status`.
		weaveApplyIsolationCheck(freshIt)
		freshIt.AutoCommitted = autoCommitted
		freshIt.AutoCommitError = autoCommitErr
		if logPath != "" {
			freshIt.LogPath = logPath
		}
		freshIt.State = weaveTerminalState(exitCode, runErr, killReason, ev)
		if freshIt.State == "no-op" {
			freshIt.Disposition = weaveDispositionEmpty
		}
		if freshIt.PauseRequestedBy != "" && childTerminated {
			freshIt.State = "paused"
			weaveAppendComment(freshIt, freshIt.PauseRequestedBy, "system", "paused with progress preserved: "+freshIt.PauseReason)
		}
		weaveQueueOwnerNotice(dir, freshQ, freshIt, "run-terminal")
		if freshIt.State == "killed" {
			// Signal death (watchdog, weave kill escalation, external
			// SIGTERM): killed stays killed — never silently promoted.
			// The wrapper-measured evidence travels with the item so
			// the orchestrator can verify and decide.
			freshIt.State = "killed"
			if ev.CommitsAhead > 0 {
				freshIt.Body = fmt.Sprintf("[killed exit %d with %d wrapper-verified commit(s) ahead at %.12s — inspect, then resume or merge deliberately]\n\n",
					exitCode, ev.CommitsAhead, ev.Head) + freshIt.Body
			}
		}
		// Coach evidence from the reflex loop detector.
		freshIt.CoachTotalCalls = coachRep.Total
		freshIt.CoachDistinctCalls = coachRep.Distinct
		freshIt.CoachRepeatRatio = coachRep.Repeat
		freshIt.CoachSteers = len(coachRep.Steers)
		freshIt.CoachMode = coachMode
		steered := len(coachRep.Steers) >= 1
		converged := weaveStateAssertsSuccess(freshIt.State)
		if steered && converged {
			freshIt.CoachRecovered = true
		}
		it = freshIt
		return nil
	})
	if finalizationClaimed {
		return nil
	}
	if lockErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "weave start: queue write failed after tool exit: %v\n", lockErr)
	} else {
		weaveDeliverOwnerNotices(dir)
		weaveReleaseManagedGOCache(cmd.ErrOrStderr(), "weave start", dir, it)
		// Say it out loud at the moment it is found. A flag only `weave
		// status` would show is a flag nobody reads until after the merge.
		if w := weaveIsolationWarning(it); w != "" {
			fmt.Fprint(cmd.ErrOrStderr(), w)
			fmt.Fprintf(cmd.ErrOrStderr(), "weave: `weave pull` will REFUSE run #%d without --force\n", it.ID)
		}
		if w := weaveOutsideWorkspaceWarning(it); w != "" {
			fmt.Fprint(cmd.ErrOrStderr(), w)
		}
		if err := weaveRememberObservation(dir, it, ev); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "weave start: memory capture failed (continuing): %v\n", err)
		}
		if err := weaveReportTerminal(context.Background(), root, it, ev); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "weave start: reporter: terminal report failed (continuing): %v\n", err)
		}
		// Fold the terminal gate evidence into the capability matrix.
		// Best-effort, like the memory/reporter calls above.
		weaveRecordCapability(it)
		// Throttle-aware cooldown: when the run terminated as a throttle and
		// a reset time is parseable, record the underlying tool on cooldown
		// so the orchestrator (`weave fleet`) fails over now and re-engages
		// it after the reset. Best-effort — a cooldown error never fails the
		// run, matching the memory/reporter calls above.
		if it.Throttled && throttleLogTail != "" {
			msg := throttleLogTail
			if it.ThrottleSignal != "" {
				msg = it.ThrottleSignal + "\n" + msg
			}
			if reset, ok := parseThrottleReset(msg, time.Now()); ok {
				coolTool := weaveThrottleToolFromSignal(it.Tool, throttleLogTail)
				if err := recordToolCooldownCause(dir, coolTool, reset, weaveThrottleCause(msg)); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "weave start: cooldown record failed (continuing): %v\n", err)
				}
			}
		}
	}

	var turns int64
	if logPath != "" {
		if b, err := os.ReadFile(logPath); err == nil {
			turns = weaveResultTurns(string(b))
		}
	} else if toolStdout.Len() > 0 {
		turns = weaveResultTurns(toolStdout.String())
	}

	modelArg := ""
	nick := it.Owner
	if it.LaunchSpec != nil {
		modelArg = it.LaunchSpec.Model
		if it.LaunchSpec.Agent != "" {
			nick = it.LaunchSpec.Agent
		}
	}
	band, canonicalModel := fleet.ResolveLaunchModel(it.Tool, modelArg)

	gateRes := -1
	if ev.VerifyExit != nil {
		gateRes = *ev.VerifyExit
	} else if it.VerifyExit != nil {
		gateRes = *it.VerifyExit
	}

	span.SetAttributes(
		attribute.String("agent", it.Owner),
		attribute.String("nick", nick),
		attribute.Int("band", band),
		attribute.String("tool:model", it.Tool+":"+canonicalModel),
		attribute.Int64("issue", it.ID),
		attribute.String("outcome", it.State),
		attribute.Bool("converged", it.State == "submitted" || it.State == "verified" || it.State == "done" || it.State == "merged"),
		attribute.Int("gate_result", gateRes),
		attribute.Int64("turns", turns),
		attribute.Float64("duration", finishedAt.Sub(it.StartedAt).Seconds()),
	)
	if exitCode != 0 || runErr != nil || killReason != "" {
		span.SetStatus(codes.Error, "failed")
	}

	if turns > 0 {
		m, _ := otel.Meter("github.com/qiangli/yoke/pkg/weave").Int64Histogram("fleet.run.turns")
		if m != nil {
			m.Record(context.Background(), turns, metric.WithAttributes(
				attribute.String("agent", it.Owner),
				attribute.Int("band", band),
			))
		}
	}

	if runErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitGenericFail, runErr))
	}
	if exitCode != 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave start",
			weavecli.ExitGenericFail, fmt.Errorf("tool exited with %d", exitCode)))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave start", map[string]any{
			"issue":     it.ID,
			"workspace": workspace,
			"branch":    branch,
			"state":     it.State,
			"exit_code": exitCode,
			"log_path":  logPath,
		}))
	}
	return nil
}

// runWeaveSay injects one line into a running subagent's PTY via
// the wrapper's per-issue control socket. The wrapper appends \r,
// so the TUI treats it as a submitted message.
//
// Flags:
//   - tab: prepend a literal Tab keystroke
//   - enter: send only a bare Enter (text becomes optional)
//   - raw: send C-style decoded bytes verbatim (\t \r \n \x1b etc.)
func runWeaveSay(cmd *cobra.Command, id int64, text string, tab, enter bool, raw string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave say",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave say",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave say",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if it.State != "working" || it.WrapperPid == 0 || !pidAlive(it.WrapperPid) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave say",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no live subagent (state=%q)", it.ID, it.State)))
	}
	if it.CtlSock == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave say",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no control socket — its wrapper predates `weave say` or ran with --pty=never", it.ID)))
	}

	// Build the byte sequence according to flags.
	var payload []byte
	switch {
	case raw != "":
		// C-style escape decoding.
		payload = decodeCescape(raw)
	case enter:
		// Send only a bare Enter (carriage return).
		payload = []byte{'\r'}
	default:
		// Plain text mode: strip newlines and add trailing \r.
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r", " "), "\n", " ")
		if tab {
			payload = append([]byte{'\t'}, text...)
		} else {
			payload = []byte(text)
		}
		payload = append(payload, '\r')
	}

	// The wire is agentpty's, not weave's. Two encodings of one protocol is one
	// protocol that will drift — and this one had: weave built its own base64
	// frames here while `meet say` went through BrokerSay, so the two commands
	// meant subtly different things by "send this to the agent".
	//
	// A special mode is a KEYSTROKE (raw bytes, Enter, Tab), which only the
	// verbatim frame can carry. Plain text is a SENTENCE, which the line protocol
	// delivers as typing.
	var frame string
	if raw != "" || enter || tab {
		frame = agentpty.VerbatimFrame(payload)
	} else {
		frame = string(payload) + "\n"
	}
	if err := weaveWriteControlFrame(it.CtlSock, frame); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave say",
			weavecli.ExitDepUnhealthy, err))
	}

	sentDesc := string(payload)
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave say", map[string]any{
			"issue": it.ID,
			"sent":  sentDesc,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave say: sent to run #%d — watch `weave log %d -f`\n", it.ID, it.ID)
	return nil
}

// weaveWriteControlFrame writes a frame to a run's control channel. The frame is
// built by agentpty (TextFrame / VerbatimFrame); weave no longer encodes its own.
func weaveWriteControlFrame(path, frame string) error {
	return agentpty.SendFrame(path, frame)
}

// decodeCescape decodes C-style escape sequences: \t, \r, \n, \xNN, \\.
func decodeCescape(s string) []byte {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out = append(out, s[i])
			continue
		}
		switch s[i+1] {
		case 't':
			out = append(out, '\t')
			i++
		case 'r':
			out = append(out, '\r')
			i++
		case 'n':
			out = append(out, '\n')
			i++
		case '\\':
			out = append(out, '\\')
			i++
		case 'x':
			if i+3 < len(s) {
				b, err := strconv.ParseUint(s[i+2:i+4], 16, 8)
				if err == nil {
					out = append(out, byte(b))
					i += 3
					continue
				}
			}
			out = append(out, s[i])
		default:
			out = append(out, s[i])
		}
	}
	return out
}

// runWeaveLog prints the captured PTY log for an issue, optionally
// following appended output until the issue reaches a terminal
// state. The capture file only exists when the subagent ran with a
// captured PTY (non-TTY parent); interactive passthrough sessions
// have nothing recorded.
// weaveLogSummary prints a compact one-glance outcome for an issue
// instead of the raw PTY capture: terminal state, the substrate's own
// evidence (exit code, verify result, commits ahead), and whether the
// work has landed in base. The default `weave log` dumps the full PTY
// stream, which lands mid-diff under `tail` and buries the bottom line.
func weaveLogSummary(cmd *cobra.Command, mode weavecli.OutputMode, root string, it *weaveItem) error {
	base := weaveBaseBranch(root)
	merged := weaveItemMerged(root, base, it)
	if mode == weavecli.OutputJSON {
		weaveComputeBlocked(it)
		res := map[string]any{
			"issue":         it.ID,
			"state":         it.State,
			"tool":          it.Tool,
			"owner":         it.Owner,
			"blocked":       it.Blocked,
			"comments":      len(it.Comments),
			"duration":      weaveDurationCol(it),
			"commits_ahead": it.CommitsAhead,
			"merged":        merged,
		}
		if it.Head != "" {
			res["head"] = it.Head
		}
		if it.ExitCode != nil {
			res["exit_code"] = *it.ExitCode
		}
		if it.VerifyExit != nil {
			res["verify_exit"] = *it.VerifyExit
		}
		res["auto_committed"] = it.AutoCommitted
		if it.AutoCommitError != "" {
			res["auto_commit_error"] = it.AutoCommitError
		}
		if it.CleanupError != "" {
			res["cleanup_error"] = it.CleanupError
		}
		if it.KilledBy != "" {
			res["killed_by"] = it.KilledBy
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave log", res))
	}
	w := cmd.OutOrStdout()
	tool := it.Tool
	if tool == "" {
		tool = "-"
	}
	owner := it.Owner
	if owner == "" {
		owner = "-"
	}
	weaveComputeBlocked(it)
	fmt.Fprintf(w, "run #%d — %s\n", it.ID, weaveTruncate(it.Title, 72))
	fmt.Fprintf(w, "  state:    %s   tool: %s   owner: %s   dur: %s\n", it.State, tool, owner, weaveDurationCol(it))
	if it.Blocked {
		fmt.Fprintf(w, "  blocked:  awaiting input — newest comment is a blocker (weave comments %d)\n", it.ID)
	}
	if n := len(it.Comments); n > 0 {
		fmt.Fprintf(w, "  comments: %d (weave comments %d)\n", n, it.ID)
	}
	if it.ExitCode != nil {
		fmt.Fprintf(w, "  exit:     %d\n", *it.ExitCode)
	}
	if it.KilledBy != "" {
		fmt.Fprintf(w, "  killed:   %s\n", it.KilledBy)
	}
	if it.VerifyExit != nil {
		verdict := "passed"
		if *it.VerifyExit != 0 {
			verdict = fmt.Sprintf("FAILED (exit %d)", *it.VerifyExit)
		}
		fmt.Fprintf(w, "  verify:   %s\n", verdict)
	}
	if it.AutoCommitted {
		fmt.Fprintf(w, "  auto:     committed dirty workspace changes\n")
	} else if it.AutoCommitError != "" {
		fmt.Fprintf(w, "  auto:     commit failed: %s\n", it.AutoCommitError)
	}
	if it.CleanupError != "" {
		fmt.Fprintf(w, "  cleanup:  FAILED: %s\n", it.CleanupError)
	}
	branchInfo := fmt.Sprintf("%d commit(s) ahead of %s", it.CommitsAhead, base)
	if len(it.Head) >= 12 {
		branchInfo += " @ " + it.Head[:12]
	}
	fmt.Fprintf(w, "  branch:   %s\n", branchInfo)
	if it.Dirty {
		fmt.Fprintf(w, "  dirty:    %d tracked uncommitted file(s)\n", it.DirtyFiles)
	}
	mergedStr := "no — `weave pull` to merge"
	if merged {
		mergedStr = "yes — already in " + base
	}
	fmt.Fprintf(w, "  merged:   %s\n", mergedStr)
	return nil
}

func runWeaveLog(cmd *cobra.Command, id int64, follow bool, tailN int, summary bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if summary {
		return weaveLogSummary(cmd, mode, root, it)
	}
	logPath := it.LogPath
	if logPath == "" {
		// The queue persists log_path on exit; while the subagent is
		// still running, fall back to the conventional capture path —
		// watching a LIVE issue is this subverb's main use case.
		conventional := filepath.Join(dir, "logs", fmt.Sprintf("issue-%d.log", it.ID))
		if _, err := os.Stat(conventional); err == nil {
			logPath = conventional
		}
	}
	if logPath == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no PTY capture (state=%q) — it either hasn't started or ran interactively (PTY passthrough)", it.ID, it.State)))
	}
	f, err := os.Open(logPath)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
			weavecli.ExitStateConflict, fmt.Errorf("log missing on disk: %s", logPath)))
	}
	defer f.Close()
	if mode == weavecli.OutputJSON {
		// Agent mode: the raw PTY stream isn't envelope-safe; return
		// the metadata and let the caller read/tail the file itself.
		st, statErr := f.Stat()
		var size int64
		if statErr == nil {
			size = st.Size()
		}
		res := map[string]any{
			"issue":      it.ID,
			"state":      it.State,
			"log_path":   logPath,
			"size_bytes": size,
		}
		if it.ExitCode != nil {
			res["exit_code"] = *it.ExitCode
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave log", res))
	}
	if tailN >= 0 {
		off, err := tailOffset(f, tailN)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
				weavecli.ExitGenericFail, err))
		}
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
				weavecli.ExitGenericFail, err))
		}
	}
	out := cmd.OutOrStdout()
	if _, err := io.Copy(out, f); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
			weavecli.ExitGenericFail, err))
	}
	if !follow {
		return nil
	}
	if err := weaveFollowLog(context.Background(), out, f, dir, id); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave log",
			weavecli.ExitGenericFail, err))
	}
	return nil
}

func weaveFollowLog(ctx context.Context, out io.Writer, f *os.File, dir string, id int64) error {
	// Follow: poll for appended bytes (regular files don't support
	// blocking reads past EOF). Stop once the issue is terminal AND
	// the file is drained — terminal-then-drain, not drain-then-
	// terminal, so the final flush after exit is never truncated.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		n, err := io.Copy(out, f)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		q2, err := loadWeaveQueue(dir)
		if err != nil {
			continue // transient queue read race; keep following
		}
		it2 := findWeaveItem(q2, id)
		if it2 == nil || isTerminalState(it2.State) {
			return nil
		}
	}
}

// tailOffset returns the byte offset where the last n lines of f
// begin, with tail(1) semantics: a trailing newline terminates the
// final line rather than starting an empty one. n<=0 returns the
// end offset (print nothing; with -f that means "new output only").
func tailOffset(f *os.File, n int) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if n <= 0 || size == 0 {
		return size, nil
	}
	end := size
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, size-1); err == nil && one[0] == '\n' {
		end = size - 1
	}
	const chunk = 32 * 1024
	buf := make([]byte, chunk)
	count := 0
	pos := end
	for pos > 0 {
		readSize := int64(chunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		m, err := f.ReadAt(buf[:readSize], pos)
		if err != nil && m <= 0 {
			return 0, err
		}
		for i := m - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				count++
				if count == n {
					return pos + int64(i) + 1, nil
				}
			}
		}
	}
	return 0, nil
}

func weaveRequireReviewGate(it *weaveItem) error {
	if it == nil {
		return fmt.Errorf("issue has no passing review; run `weave review`")
	}
	if it.ReviewVerdict != "pass" || it.ReviewBlocking {
		return fmt.Errorf("run #%d has no passing review; run `weave review %d`", it.ID, it.ID)
	}
	return nil
}

func runWeavePull(cmd *cobra.Command, flags *weaveOutputFlags, issueID int64, issueSpecified bool, requireReview, force, waiveRecordedPair bool, reviewAgents ...string) error {
	reviewAgent := ""
	if len(reviewAgents) > 0 {
		reviewAgent = reviewAgents[0]
	}
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave pull",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	base := weaveBaseBranch(root)
	type result struct {
		Issue           int64  `json:"issue"`
		Branch          string `json:"branch"`
		Status          string `json:"status"`
		Detail          string `json:"detail,omitempty"`
		SuiteGateExit   *int   `json:"suite_gate_exit,omitempty"`
		SuiteGateOutput string `json:"suite_gate_output,omitempty"`
		ReviewAgent     string `json:"review_agent,omitempty"`
		ReviewAddedTest bool   `json:"review_added_test,omitempty"`
		PairVerdict     string `json:"pair_verdict,omitempty"`
		PairReason      string `json:"pair_reason,omitempty"`
		PairExit        int    `json:"pair_exit,omitempty"`
	}
	var results []result
	var mergedReports []*weaveItem
	// gateDecisions snapshots items whose pull produced FRESH gate evidence
	// (a pair verdict, a verify re-run on pair evidence, or a suite-gate
	// exit) so the capability matrix can fold them in after the lock —
	// best-effort, without double-counting the finalize-time record.
	var gateDecisions []weaveItem
	pairExit := weavePairPassExit
	// Snapshot the live checkout ONCE for the isolation guard. Pull mutates
	// this very tree, so re-snapshotting per item would let item #1's merge
	// read as item #2's escape. One instant is judged against every item's
	// claim-time baseline.
	liveNow, liveNowErr := weaveSnapshotLiveTree(root)
	// LOCK DISCIPLINE (see weave_lock_common.go). Pull is the long operation:
	// with --review-agent it runs an adversarial-review AGENT SUBPROCESS and a
	// suite gate, minutes apiece. It used to do all of that holding the queue
	// lock, which froze `weave list`, `weave add` and every other command in
	// the repo for the whole cycle — a steward could not even read the board
	// while the autopilot ran.
	//
	// So: pull does review and suite-gate work on a PRIVATE COPY without
	// pull.lock. Only the seconds-long live fetch/merge commit takes that
	// exclusive lock. Immediately after acquiring it we revalidate both the
	// live checkout and the queue item against the evidence used by the gate;
	// a moved target invalidates the verdict and is refused.
	lockErr := func() error {
		var q *weaveQueue
		if err := withWeaveQueueLock(dir, func(fresh *weaveQueue) error {
			if issueSpecified && findWeaveItem(fresh, issueID) == nil {
				return fmt.Errorf("run #%d not found%s", issueID, weaveOtherActiveQueuesHintSuffix(dir))
			}
			q = fresh
			return nil
		}); err != nil {
			return err
		}
		// Test hook: simulate a long review/gate. Deliberately outside both
		// queue.lock and pull.lock.
		weaveTestPauseAfterPullLoad()
		// What the items looked like when we took our copy. Anything unchanged
		// is not written back, so a concurrent writer's edit to an untouched
		// item is never clobbered.
		before := weaveItemFingerprints(q)
		// When review is explicitly requested, refuse to BEGIN the merge loop unless
		// every submitted item in scope has an eligible,
		// band-matched, different-family judge. Running this before any merge means
		// the loop never proceeds per-run: a required item that cannot be judged
		// HALTS the whole pull (non-zero exit) instead of silently merging its
		// peers around it. With no reviewer, deterministic gates remain authoritative.
		if err := weaveRequireEligibleJudge(q.Items, reviewAgent, issueID, issueSpecified); err != nil {
			return err
		}
		for _, it := range q.Items {
			if issueSpecified && it.ID != issueID {
				continue
			}
			// killed_by is durable forensic evidence, not a permanent merge
			// veto. A killed item still needs an explicit `weave salvage` to
			// reach submitted; once it does, pull must honor that deliberate
			// state transition and run the ordinary verification/merge gates.
			// This also repairs items stranded by older salvage versions, which
			// promoted State but retained KilledBy.
			if it.KilledBy != "" && it.State != "submitted" {
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "killed",
					Detail: fmt.Sprintf("run recorded killed_by=%q; inspect or salvage committed work explicitly", it.KilledBy)})
				continue
			}
			// Already landed in base by some other route (manual merge,
			// a peer weave). Reconcile the stale "submitted" to "done"
			// and report it rather than re-fetching a no-op branch.
			if it.State == "submitted" && weaveItemMerged(root, base, it) {
				it.State = "done"
				it.Disposition = weaveDispositionMerged
				weaveCloseRegisterOnMerge(root, base, it)
				if it.Workspace != "" {
					_ = safeRemoveWorkspace(dir, it.Workspace)
					it.Workspace = ""
				}
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "already-merged",
					Detail: "work already in " + base + "; marked done"})
				continue
			}
			// Merge any branch belonging to an item that's either still
			// running (working — predates state transitions) or that
			// finished with committed evidence (submitted). Items in "failed", "done",
			// or "abandoned" are skipped: failed shouldn't auto-merge,
			// done is already merged, abandoned was torn down.
			//
			// A SKIP IS NOT AN EMPTINESS. This `continue` used to be silent, and a
			// silent skip on the only in-scope item left `results` empty — so pull
			// finished by printing:
			//
			//	weave pull: nothing to merge
			//
			// for run #169, which held three commits of gate-passing work that
			// existed nowhere but its workspace. The same weave printed
			// "SALVAGEABLE: #169 ... hold committed work not merged to the base
			// branch" one line later, from weaveClassifySalvageable. Pull was
			// reporting "this run is not in a state I accept" in the words of "there
			// is nothing here" — and the reasonable next action for someone who
			// reads that is `weave abandon`, which destroys the work.
			//
			// So: before skipping, ASK — through the same weaveUnmergedAhead that
			// feeds the SALVAGEABLE line, never a second count that can drift from
			// it. Zero commits ahead still skips silently; that run really is empty.
			if it.State != "working" && it.State != "submitted" {
				if ahead, _ := weaveUnmergedAhead(root, base, it); ahead > 0 {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "not-pullable",
						Detail: weaveNotPullableDetail(it, base, ahead)})
				}
				continue
			}
			if requireReview {
				if err := weaveRequireReviewGate(it); err != nil {
					return err
				}
			}
			if it.State == "working" && it.WrapperPid > 0 && pidAlive(it.WrapperPid) {
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "running",
					Detail: fmt.Sprintf("wrapper pid %d alive; wait or kill before pull", it.WrapperPid)})
				continue
			}
			// MEASURE, DO NOT TRUST THE REPORT. Third time in this file.
			//
			// CommitsAhead is written by the WRAPPER at terminal time. A tool that
			// crashes on its way out — or whose commit lands a moment after the
			// wrapper measured — leaves the record saying 0 while the branch carries
			// the whole feature. `weave list` and `weave prune` were both taught to
			// go and look; pull was not, so it went on refusing to merge work that
			// was sitting right there:
			//
			//   run #3: empty — terminal evidence recorded 0 commits ahead
			//
			// while `git rev-list --count main..HEAD` in that very workspace said 1.
			//
			// A record written by a process that did not survive is not evidence of
			// absence. Ask git.
			if it.State == "submitted" {
				ahead := it.CommitsAhead
				if ahead <= 0 && it.Workspace != "" {
					if st, err := os.Stat(it.Workspace); err == nil && st.IsDir() {
						ahead, _ = weaveMeasureBranch(it.Workspace, weaveCountRef(it, base))
					}
				}
				if ahead <= 0 {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "empty",
						Detail: "0 commits ahead of base — MEASURED in the workspace, not merely recorded; nothing mergeable"})
					continue
				}
			}
			// Same rule as the commit count above: it.Dirty is a RECORD, written by
			// the wrapper at terminal time. A tree that has been committed since —
			// by the salvage path, or by hand — is clean now, and refusing to merge
			// it because a dead process once saw it dirty is refusing on the strength
			// of stale hearsay. Look at the tree.
			dirtyNow, dirtyFilesNow := it.Dirty, it.DirtyFiles
			if it.Workspace != "" {
				if st, err := os.Stat(it.Workspace); err == nil && st.IsDir() {
					dirtyNow, dirtyFilesNow, _ = weaveMeasureDirtiness(it.Workspace)
				}
			}
			if dirtyNow {
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "dirty",
					Detail: fmt.Sprintf("working tree has %d uncommitted tracked file(s) RIGHT NOW (measured, not recorded); commit them in the workspace (`weave shell %d`) and re-run", dirtyFilesNow, it.ID)})
				continue
			}
			// Bind every slow verdict below to the exact target it was computed
			// against. The working-tree snapshot and queue fingerprint use the
			// same evidence machinery as other long queue operations; HEAD names
			// the base commit whose integration result the suite gate observes.
			gateLive, err := weaveSnapshotLiveTree(root)
			if err != nil {
				return fmt.Errorf("run #%d: snapshot merge target before review: %w", it.ID, err)
			}
			gateHeadOut, err := gitOut(root, "rev-parse", "HEAD")
			if err != nil {
				return fmt.Errorf("run #%d: identify merge target before review: %w", it.ID, err)
			}
			gateHead := strings.TrimSpace(gateHeadOut)
			gateItemFingerprint := before[it.ID]
			// An acting pair augments the RUN WORKSPACE before the terminal
			// evidence is re-collected. It has no approve/reject path: a test it
			// adds is committed as evidence, then the existing verify/suite gate
			// below is the only arbiter. Default-off preserves legacy pull.
			//
			// Model review is strictly opt-in through --review-agent. In its absence,
			// the deterministic gates below are sufficient regardless of stored tier
			// or stale pair evidence. Eligibility was vetted by the pre-pass above.
			if reviewAgent != "" && it.State == "submitted" {
				if it.Workspace == "" {
					pr := weaveNormalizePairReview(weavePairReviewResult{}, errors.New("no workspace recorded for adversarial review"))
					it.PairVerdict, it.PairReason, it.PairExit = string(pr.Verdict), pr.Reason, pr.ExitCode
					it.NeedsSteward, it.StewardReason = true, pr.verdictLine()
					pairExit = pr.ExitCode
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "harness-error", Detail: pr.Reason,
						PairVerdict: string(pr.Verdict), PairReason: pr.Reason, PairExit: pr.ExitCode})
					continue
				}
				if _, err := os.Stat(it.Workspace); err != nil {
					pr := weaveNormalizePairReview(weavePairReviewResult{}, fmt.Errorf("workspace missing: %v", err))
					it.PairVerdict, it.PairReason, it.PairExit = string(pr.Verdict), pr.Reason, pr.ExitCode
					it.NeedsSteward, it.StewardReason = true, pr.verdictLine()
					pairExit = pr.ExitCode
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "harness-error", Detail: pr.Reason,
						PairVerdict: string(pr.Verdict), PairReason: pr.Reason, PairExit: pr.ExitCode})
					continue
				}
				gateCommand := it.VerifyCommand
				if gateCommand == "" {
					gateCommand = weaveSuiteGateCommand(root, it)
				}
				pr, reviewErr := weavePairReviewRunner(it.Workspace, weaveCountRef(it, base), gateCommand, reviewAgent, it)
				pr = weaveNormalizePairReview(pr, reviewErr)
				// A reviewer that died on its provider quota must land on
				// cooldown NOW, or `weave fleet` keeps reporting it available
				// and the orchestrator dispatches review after review into the
				// same wall (runs #140/#146).
				weaveRecordPairThrottle(dir, pr, time.Now())
				it.CodingAgent = pr.CodingAgent
				it.ReviewAgent = pr.ReviewAgent
				it.ReviewAddedTest = pr.AddedTest
				it.PairVerdict = string(pr.Verdict)
				it.PairReason = pr.Reason
				it.PairExit = pr.ExitCode
				if pr.ExitCode != weavePairPassExit && pairExit == weavePairPassExit {
					pairExit = pr.ExitCode
				}
				pairResult := result{
					Issue: it.ID, Branch: it.Branch, Detail: pr.Reason,
					ReviewAgent: pr.ReviewAgent, ReviewAddedTest: pr.AddedTest,
					PairVerdict: string(pr.Verdict), PairReason: pr.Reason, PairExit: pr.ExitCode,
				}
				switch pr.Verdict {
				case weavePairHarnessError:
					// A broken harness has neither approved nor blocked the work. Keep
					// the submission intact and name the decision still owed.
					it.State = "submitted"
					it.NeedsSteward = true
					it.StewardReason = pr.verdictLine()
					pairResult.Status = "harness-error"
					results = append(results, pairResult)
					continue
				case weavePairBrokenBefore:
					it.State = "submitted"
					it.NeedsSteward = true
					it.StewardReason = pr.verdictLine()
					pairResult.Status = "broken-before"
					results = append(results, pairResult)
					continue
				case weavePairUnmeasured:
					it.State = "submitted"
					it.NeedsSteward = true
					it.StewardReason = pr.verdictLine()
					pairResult.Status = "unmeasured"
					results = append(results, pairResult)
					continue
				case weavePairRefuted:
					it.State = "failed"
					pairResult.Status = "review-block"
					results = append(results, pairResult)
					gateDecisions = append(gateDecisions, *it)
					continue
				case weavePairPass:
					// Only a named pass reaches the existing substrate gate below.
				default:
					pr = weaveNormalizePairReview(pr, fmt.Errorf("unknown pair verdict %q", pr.Verdict))
					it.State = "submitted"
					it.NeedsSteward = true
					it.StewardReason = pr.verdictLine()
					pairExit = pr.ExitCode
					pairResult.Status, pairResult.Detail = "harness-error", pr.Reason
					pairResult.PairVerdict, pairResult.PairReason, pairResult.PairExit = string(pr.Verdict), pr.Reason, pr.ExitCode
					results = append(results, pairResult)
					continue
				}
				ev := weaveCollectTerminalEvidence(it.Workspace, weaveCountRef(it, base), dir, it.VerifyCommand, it, it.VerifyCommand != "")
				weaveApplyTerminalEvidence(it, ev)
				if ev.VerifyExit != nil && *ev.VerifyExit != 0 {
					it.State = "failed"
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "verify-failed",
						Detail:      fmt.Sprintf("adversarial review by %s added_test=%v; verify command exited %d — pair evidence remains committed in the workspace", pr.ReviewAgent, pr.AddedTest, *ev.VerifyExit),
						ReviewAgent: pr.ReviewAgent, ReviewAddedTest: pr.AddedTest,
						PairVerdict: string(pr.Verdict), PairReason: pr.Reason, PairExit: pr.ExitCode})
					gateDecisions = append(gateDecisions, *it)
					continue
				}
			}
			// Substrate-verified outcome gate: the wrapper ran the item's
			// verify command at terminal time; a recorded non-zero exit
			// means the work failed its own acceptance check. Refuse to
			// merge — before fetching, so the branch never even lands in
			// the user's repo. (A future --force may override.)
			if it.VerifyExit != nil && *it.VerifyExit != 0 {
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "verify-failed",
					Detail: fmt.Sprintf("verify command exited %d — inspect with `weave shell %d`, fix or abandon", *it.VerifyExit, it.ID)})
				continue
			}
			// Isolation gate. A run whose live checkout moved underneath it
			// is one whose branch is NOT the whole diff: some of its effect
			// is already sitting, uncommitted and unreviewed, in the user's
			// tree. Merging that silently is how a half-applied change gets
			// blessed as reviewed work. Refuse, name the paths, and make the
			// operator say --force — which stays available precisely because
			// the common cause is innocent (the human edited their own repo
			// while the agent ran), and the guard cannot tell the two apart.
			if liveNowErr == nil {
				if violated, escaped := weaveIsolationStatus(it, liveNow); violated {
					it.IsolationViolated = true
					if len(escaped) > 0 {
						it.EscapedPaths = escaped
					}
				}
			}
			if it.IsolationViolated && !force {
				fmt.Fprint(cmd.ErrOrStderr(), weaveIsolationWarning(it))
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "isolation-violated",
					Detail: weaveIsolationDetail(it)})
				continue
			}
			if it.IsolationViolated && force {
				fmt.Fprintf(cmd.ErrOrStderr(), "weave: --force: merging isolation-violated run #%d anyway — %s\n",
					it.ID, weaveIsolationDetail(it))
			}
			// The agent's branch lives in the workspace clone, not the
			// user's repo. Fetch it across (idempotent — already-present
			// commits are skipped). If the workspace is gone (abandoned
			// mid-pull, disk wiped) we record a skip with the reason.
			if it.Workspace == "" {
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "skipped", Detail: "no workspace recorded"})
				continue
			}
			if _, err := os.Stat(it.Workspace); err != nil {
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "skipped", Detail: fmt.Sprintf("workspace missing: %v", err)})
				continue
			}
			liveDirty, liveDirtyFiles, liveUntrackedFiles := weaveMeasureDirtiness(it.Workspace)
			if liveDirty {
				it.Dirty = true
				it.DirtyFiles = liveDirtyFiles
				it.UntrackedFiles = liveUntrackedFiles
				results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "dirty",
					Detail: fmt.Sprintf("workspace has %d tracked uncommitted file(s); resume the agent to commit the work (or commit manually in the workspace) and re-verify", liveDirtyFiles)})
				continue
			}
			suiteGate := weaveSuiteGateCommand(root, it)
			var suiteGateExit *int
			var suiteGateOutput string
			if suiteGate != "" {
				sgExit, sgOutput, conflict, gateErr := weaveRunCandidateSuiteGate(root, it.Workspace, it.Branch, gateHead, suiteGate)
				if gateErr != nil {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "skipped", Detail: gateErr.Error()})
					continue
				}
				if conflict {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "conflict", Detail: sgOutput})
					continue
				}
				it.SuiteGateExit = &sgExit
				it.SuiteGateOutput = sgOutput
				suiteGateExit = &sgExit
				suiteGateOutput = sgOutput
				if sgExit != 0 {
					if reviewAgent != "" && it.ReviewAgent != "" {
						it.State = "failed"
					}
					suiteResult := result{
						Issue:           it.ID,
						Branch:          it.Branch,
						Status:          "suite-gate-failed",
						Detail:          sgOutput,
						SuiteGateExit:   &sgExit,
						SuiteGateOutput: sgOutput,
					}
					if !waiveRecordedPair && reviewAgent != "" {
						suiteResult.ReviewAgent = it.ReviewAgent
						suiteResult.ReviewAddedTest = it.ReviewAddedTest
						suiteResult.PairVerdict = it.PairVerdict
						suiteResult.PairReason = it.PairReason
						suiteResult.PairExit = it.PairExit
					}
					results = append(results, suiteResult)
					gateDecisions = append(gateDecisions, *it)
					continue
				}
			}

			// COMMIT is the serialization point. The expensive review and suite
			// gate above ran without pull.lock in an isolated checkout. Once the
			// kernel grants the lock, refuse a verdict whose base, live tree, or
			// queue item moved before touching the shared checkout.
			merged := false
			mergeErr := withWeaveNamedPullLock(dir, fmt.Sprintf("run #%d", it.ID), func() error {
				if err := weaveValidatePullEvidence(root, dir, it.ID, gateHead, gateLive, gateItemFingerprint); err != nil {
					return err
				}
				fetchSpec := fmt.Sprintf("%s:%s", it.Branch, it.Branch)
				if _, err := gitOut(root, "fetch", "--no-tags", it.Workspace, fetchSpec); err != nil {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "skipped", Detail: fmt.Sprintf("fetch from workspace: %v", err)})
					return nil
				}
				cnt, err := gitOut(root, "rev-list", "--count", fmt.Sprintf("HEAD..%s", it.Branch))
				if err != nil {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "skipped", Detail: err.Error()})
					return nil
				}
				ahead, _ := strconv.Atoi(strings.TrimSpace(cnt))
				if ahead == 0 {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "empty",
						Detail: "branch has 0 commits ahead of HEAD; nothing mergeable"})
					return nil
				}
				mergeSubject := fmt.Sprintf("weave: merge run #%d — %s", it.ID, it.Title)
				mergeMsg := weaveMergeCommitMessage(root, it.Branch, mergeSubject)
				mc := exec.Command(gitBin(), "-C", root, "merge", "--no-ff", "-m", mergeMsg, it.Branch)
				out, err := mc.CombinedOutput()
				if err != nil {
					_ = exec.Command(gitBin(), "-C", root, "merge", "--abort").Run()
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "conflict", Detail: strings.TrimSpace(string(out))})
					return nil
				}
				// Delete the fetched branch from user repo if fully merged (-d,
				// never -D), while the same live-checkout lock is still held.
				_ = exec.Command(gitBin(), "-C", root, "branch", "-d", it.Branch).Run()
				weaveCloseRegisterOnMerge(root, base, it)
				merged = true
				return nil
			})
			if mergeErr != nil {
				return mergeErr
			}
			if !merged {
				continue
			}
			// Workspace is a full clone (not a worktree) — use safeRemoveAll with
			// containment check to prevent accidental deletion outside the queue dir.
			if it.Workspace != "" {
				if err := safeRemoveWorkspace(dir, it.Workspace); err != nil {
					results = append(results, result{Issue: it.ID, Branch: it.Branch, Status: "cleanup-failed", Detail: err.Error()})
					continue
				}
			}
			reportIt := *it
			it.State = "done"
			it.Disposition = weaveDispositionMerged
			it.Workspace = ""
			reportIt.State = "done"
			// The work landed, so the register entry it implements is settled. A
			// register that stays open after its fix merges is worse than none —
			// people trust it, and it lies.
			mergedReports = append(mergedReports, &reportIt)
			mergedResult := result{
				Issue:           it.ID,
				Branch:          it.Branch,
				Status:          "merged",
				SuiteGateExit:   suiteGateExit,
				SuiteGateOutput: suiteGateOutput,
			}
			// An explicit no-review salvage may retain old forensic evidence on
			// the queue item, but that evidence did not authorize this merge and
			// must not be rendered as though the pair ran in this invocation.
			if !waiveRecordedPair && reviewAgent != "" {
				mergedResult.ReviewAgent = it.ReviewAgent
				mergedResult.ReviewAddedTest = it.ReviewAddedTest
				mergedResult.PairVerdict = it.PairVerdict
				mergedResult.PairReason = it.PairReason
				mergedResult.PairExit = it.PairExit
			}
			results = append(results, mergedResult)
			// A merge with no suite gate and no pair verdict adds no evidence
			// beyond what the finalize path already recorded; skip those.
			if suiteGateExit != nil || (reviewAgent != "" && it.PairVerdict != "") {
				gateDecisions = append(gateDecisions, *it)
			}
		}
		// Re-acquire briefly and record the outcomes onto the CURRENT queue.
		return weaveWriteBackChangedItems(dir, q, before)
	}()
	if lockErr != nil {
		if weaveIsBusy(lockErr) {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave pull",
				weavecli.ExitStateConflict, lockErr))
		}
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") {
			code = weavecli.ExitInvalidArg
		} else if strings.Contains(lockErr.Error(), "has no passing review") || errors.Is(lockErr, errWeavePullStale) {
			code = weavecli.ExitStateConflict
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave pull",
			code, lockErr))
	}
	// The merge settled the run; the one guarded teardown reclaims what the
	// merge itself did not touch (log, socket, cache, agent data, lock).
	for _, it := range mergedReports {
		for _, a := range weavePruneOwnedRun(dir, it.ID, filepath.Base(root)) {
			if a.Err != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "weave pull: run #%d %s %s: %s\n", it.ID, a.Kind, a.Target, a.Err)
			}
		}
	}
	for _, it := range mergedReports {
		ev := weaveTerminalEvidence{
			CommitsAhead: it.CommitsAhead,
			Head:         it.Head,
			VerifyExit:   it.VerifyExit,
			VerifyOutput: it.VerifyOutput,
			VerifyTree:   it.VerifyTree,
		}
		if it.SuiteGateOutput != "" {
			ev.VerifyOutput = it.SuiteGateOutput
		}
		if err := weaveReportTerminal(context.Background(), root, it, ev); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "weave pull: reporter: merge report failed (continuing): %v\n", err)
		}
	}
	// Fold pull's fresh gate evidence (pair verdicts, suite-gate exits) into
	// the capability matrix — best-effort, like the reporter loop above.
	for i := range gateDecisions {
		weaveRecordCapability(&gateDecisions[i])
	}
	if mode == weavecli.OutputJSON {
		_ = emitOK(cmd.OutOrStdout(), mode, "weave pull", map[string]any{
			"results": results,
		})
		if pairExit != weavePairPassExit {
			return ec(pairExit)
		}
		return nil
	}
	if len(results) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "weave pull: nothing to merge")
		return nil
	}
	for _, r := range results {
		detail := ""
		if r.Detail != "" {
			detail = " — " + r.Detail
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  run #%d (%s): %s%s\n", r.Issue, r.Branch, r.Status, detail)
		if r.PairVerdict != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "    PAIR %s — %s\n", strings.ToUpper(r.PairVerdict), r.PairReason)
		}
	}
	if pairExit != weavePairPassExit {
		return ec(pairExit)
	}
	return nil
}

func weaveTestPauseAfterPullLoad() {
	pause := os.Getenv("WEAVE_TEST_PULL_AFTER_LOAD_FILE")
	if pause == "" {
		return
	}
	_ = os.WriteFile(pause+".ready", []byte("ready"), 0o644)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(pause); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func runWeaveAbandon(cmd *cobra.Command, id int64, reason, disposition string, yes, force bool, flags *weaveOutputFlags) error {
	if disposition != "" {
		if !weaveValidDisposition(disposition) || disposition == weaveDispositionMerged {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "weave abandon",
				weavecli.ExitInvalidArg, fmt.Errorf("--disposition must be superseded, rejected or empty (merged is what `weave pull` records)")))
		}
		// A disposition is an explicit decision about the work; the guard that
		// --force lifts exists for the operator who has not made one.
		force = true
	}
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave abandon",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	if err := weaveRecoverAbandonedFinalizations(dir); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave abandon",
			weavecli.ExitGenericFail, err))
	}
	if err := weaveConfirmTargeted(cmd, mode,
		fmt.Sprintf("weave abandon: tears down run #%d's workspace + branch; any unmerged work is lost.", id), yes); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave abandon",
			weavecli.ExitInvalidArg, err))
	}
	base := weaveBaseBranch(root)
	var it *weaveItem
	var preservedRef string
	notFoundHint := weaveOtherActiveQueuesHintSuffix(dir)
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it = findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found%s", id, notFoundHint)
		}
		// REFUSE TO DESTROY WORK THAT HAS NOWHERE ELSE TO LIVE.
		//
		// `weave prune` already refuses to sweep a workspace holding unmerged
		// commits or an uncommitted tree — that guard is load-bearing because an
		// agent branch lives ONLY inside its workspace clone until `weave pull`
		// fetches it. `weave abandon` tears down the very same workspace and
		// branch but had no such guard: `abandon --yes` on a run with a
		// finished, committed feature destroyed it outright, with only a
		// manually-preserved ref (not enforced by anything) standing between
		// that work and gone for good.
		//
		// Mirror prune's check here. Without --force, refuse and name the
		// commits. With --force, fetch the branch tip into the user's repo as
		// refs/salvage/abandoned-<id> BEFORE anything is destroyed, so --force
		// means "I accept the risk" rather than "make it disappear."
		if it.Workspace != "" {
			if st, statErr := os.Stat(it.Workspace); statErr == nil && st.IsDir() {
				if !weaveItemMerged(root, base, it) {
					// Same single measurement as list / pull / prune.
					ahead, head := weaveUnmergedAhead(root, base, it)
					dirty, dirtyFiles, untracked := weaveMeasureDirtiness(it.Workspace)
					if ahead > 0 || dirty {
						if !force {
							why := strings.ReplaceAll(weavePruneHoldReason(ahead, dirtyFiles, untracked), "<id>", fmt.Sprint(id))
							return fmt.Errorf("run #%d holds unmerged work — refusing to abandon: %s", id, why)
						}
						// Commit a dirty tree BEFORE preserving, so --force means
						// the same thing for uncommitted work as for commits.
						// Only a commit is reachable by a ref, so without this
						// step the preserve below saves the branch tip and drops
						// the working tree on the floor — silently, because the
						// success line only ever named the commits.
						if dirty {
							committed, cerr := maybeAutoCommit(it.Workspace, weaveForcedSalvageCommitMessage(it))
							if cerr != nil {
								return fmt.Errorf("run #%d: --force could not commit %d uncommitted file(s) for preservation, refusing to destroy them: %w", id, dirtyFiles+untracked, cerr)
							}
							if committed {
								ahead, head = weaveUnmergedAhead(root, base, it)
							}
						}
						if ahead > 0 && head != "" {
							ref, ferr := weavePreserveAbandonedTip(root, it.Workspace, id, head)
							if ferr != nil {
								return fmt.Errorf("run #%d: --force could not preserve %d unmerged commit(s) as %s, refusing to destroy them: %w", id, ahead, ref, ferr)
							}
							preservedRef = ref
						}
					}
				}
			}
		}
		// If a wrapper PID is recorded and the item is still working,
		// signal precisely — SIGTERM the recorded PID, wait briefly,
		// escalate to SIGKILL. The wrapper is its own session leader
		// (auto-setsid on non-TTY), so SIGTERM reaches the subagent's
		// process group cleanly. This is the supported way to stop a
		// running weave; the dogfood found that `pkill -f` would also
		// catch peer ycode/claude sessions belonging to other agents,
		// which is dangerous in a shared agentic environment.
		if (it.State == "working" || weaveWrapperTerminalClaimed(it)) && it.WrapperPid > 0 {
			weaveStopWrapper(it.WrapperPid)
		}
		// Workspace is a real git clone now (not a worktree); delete the
		// directory tree. The agent's branch lives inside that clone —
		// no separate `git branch -D` against the user's repo because
		// the branch doesn't exist there unless `weave pull` fetched it.
		// The workspace, branch and every other run-owned artifact come down
		// through the ONE guarded teardown below, after this lock is released;
		// the row keeps its paths until that teardown proves and removes them.
		it.State = "abandoned"
		it.SalvageRef = preservedRef
		it.Disposition = disposition
		if it.Disposition == "" {
			it.Disposition = weaveDispositionEmpty
			if preservedRef != "" {
				it.Disposition = weaveDispositionRejected
			}
		}
		it.DispositionReason = reason
		it.WrapperPid = 0
		it.Completion = ""
		it.FinalizerPID = 0
		it.FinalizingAt = time.Time{}
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") || strings.Contains(lockErr.Error(), "refusing to") {
			code = weavecli.ExitInvalidArg
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave abandon",
			code, lockErr))
	}
	// Auto-status: dropping the run returns its linked todo to the backlog
	// (assigned -> todo, link cleared), so the list stops showing a stale "assigned".
	weaveReleaseRegister(root, it)
	acts := weavePruneOwnedRun(dir, it.ID, filepath.Base(root))
	var leftovers []string
	for _, a := range acts {
		if a.Err != "" {
			leftovers = append(leftovers, fmt.Sprintf("%s %s: %s", a.Kind, a.Target, a.Err))
		}
	}
	if len(acts) == 0 && it.Workspace != "" {
		if _, err := os.Stat(it.Workspace); err == nil {
			leftovers = append(leftovers, "workspace "+it.Workspace+": teardown deferred (wrapper still winding down, or settlement unproven) — `weave prune` reclaims it")
		}
	}
	if mode == weavecli.OutputJSON {
		res := map[string]any{
			"issue":       it.ID,
			"state":       it.State,
			"reason":      reason,
			"disposition": it.Disposition,
			"cleanup":     acts,
		}
		if preservedRef != "" {
			res["preserved_ref"] = preservedRef
		}
		if len(leftovers) > 0 {
			res["leftovers"] = leftovers
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave abandon", res))
	}
	if preservedRef != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "weave abandon: run #%d abandoned as %s (unmerged commits preserved at %s)\n", it.ID, it.Disposition, preservedRef)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "weave abandon: run #%d abandoned as %s\n", it.ID, it.Disposition)
	}
	for _, l := range leftovers {
		fmt.Fprintf(cmd.ErrOrStderr(), "weave abandon: left alone: %s\n", l)
	}
	return nil
}

// weavePreserveAbandonedTip imports a workspace HEAD before removing that
// workspace. The original abandoned-N name remains the pleasant common case,
// but it is not a namespace that belongs to a single lifetime of an issue:
// resuming a killed run can amend its WIP, and queues/history can reuse IDs.
// Never move an existing salvage ref; a distinct tip receives a deterministic
// SHA-qualified name instead.
func weavePreserveAbandonedTip(root, workspace string, id int64, head string) (string, error) {
	base := fmt.Sprintf("refs/salvage/abandoned-%d", id)
	if _, err := gitOut(root, "fetch", "--no-tags", workspace, "HEAD"); err != nil {
		return base, err
	}

	for n := 0; ; n++ {
		ref := base
		if n == 0 {
			// Keep the established ref for the first preserved tip.
		} else if n == 1 {
			ref += "-" + head
		} else {
			// A manually-created ref can occupy even the SHA-qualified name.
			// Preserve it too and continue deterministically rather than replacing it.
			ref += fmt.Sprintf("-%s-%d", head, n)
		}
		out, err := gitOut(root, "rev-parse", "--verify", "--quiet", ref)
		if err == nil {
			if strings.TrimSpace(out) == head {
				return ref, nil
			}
			continue
		}
		// The empty old value makes this an atomic create: a concurrent abandon
		// cannot turn this operation into an overwrite.
		if _, err := gitOut(root, "update-ref", ref, head, ""); err == nil {
			return ref, nil
		} else {
			// If it was not a competing create, report the actual failure instead
			// of retrying forever (for example, a malformed or missing object).
			out, checkErr := gitOut(root, "rev-parse", "--verify", "--quiet", ref)
			if checkErr != nil {
				return ref, err
			}
			if strings.TrimSpace(out) == head {
				return ref, nil
			}
		}
	}
}

// runWeaveStatus answers the single most common operator question about
// an item — "is this already in main?" — directly, instead of forcing a
// manual `queue.json` → workspace `git log` → `merge-base --is-ancestor`
// investigation. Reports the recorded state reconciled against git
// reality, the branch + workspace HEAD, merged-into-base, commits ahead,
// and the last verify result.
func runWeaveStatus(cmd *cobra.Command, id int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave status",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := readWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave status",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave status",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	// Status is an observation. Lifecycle repair belongs to doctor and the
	// heartbeat; a status poll must not rewrite queue.json or create a lock.
	healthNow := time.Now().UTC()
	rawHealth := weaveClassifyHealth(weaveHealthSnapshotFor(it, defaultWeaveHealthProbe(healthNow)), it, healthNow)
	base := weaveBaseBranch(root)
	merged := weaveItemMerged(root, base, it)
	weaveAnnotateSalvageable(root, base, it)
	// Reconcile for display: a submitted item already in base reads as
	// done (and reconciledFrom records the drift so the operator sees
	// why prune would now sweep it).
	displayState := it.State
	reconciledFrom := ""
	if it.State == "submitted" && merged {
		displayState = "done"
		reconciledFrom = "submitted"
	}
	stale := it.State == "working" && it.WrapperPid > 0 && !pidAlive(it.WrapperPid)
	healthSnapshot := weaveHealthSnapshotFor(it, defaultWeaveHealthProbe(healthNow))
	health := weaveClassifyHealth(healthSnapshot, it, healthNow)
	health = rawHealth
	// Read-time isolation check, like stale/blocked: nothing is persisted
	// here (status loads without the lock), but a run that escaped WHILE
	// still running should say so now, not only once it terminates.
	weaveComputeIsolation(root, q)
	workspaceExists := false
	if it.Workspace != "" {
		if st, statErr := os.Stat(it.Workspace); statErr == nil && st.IsDir() {
			workspaceExists = true
		}
	}

	if mode == weavecli.OutputJSON {
		res := map[string]any{
			"issue":              it.ID,
			"title":              it.Title,
			"state":              displayState,
			"recorded_state":     it.State,
			"merged":             merged,
			"base":               base,
			"base_sha":           it.BaseSHA,
			"launch_phase":       it.LaunchPhase,
			"completion":         it.Completion,
			"commits_ahead":      it.CommitsAhead,
			"salvageable":        it.Salvageable,
			"unmerged_commits":   it.UnmergedCommits,
			"branch":             it.Branch,
			"workspace":          it.Workspace,
			"workspace_exists":   workspaceExists,
			"stale":              stale,
			"dirty":              it.Dirty,
			"isolation_violated": it.IsolationViolated,
			"health":             health.Health,
			"health_reason":      health.Reason,
			"health_next_action": health.Next,
			"health_snapshot":    health.Snapshot,
		}
		if len(it.OutsideWorkspacePaths) > 0 {
			res["outside_workspace_paths"] = it.OutsideWorkspacePaths
		}
		if it.IsolationViolated {
			res["escaped_paths"] = it.EscapedPaths
			res["isolation_detail"] = weaveIsolationDetail(it)
			res["mergeable"] = false
		}
		if it.Register != "" {
			res["register"] = it.Register
		}
		if reconciledFrom != "" {
			res["reconciled_from"] = reconciledFrom
		}
		if it.Head != "" {
			res["head"] = it.Head
		}
		if it.ExitCode != nil {
			res["exit_code"] = *it.ExitCode
		}
		if it.VerifyExit != nil {
			res["verify_exit"] = *it.VerifyExit
		}
		if it.Salvageable {
			res["salvageable"] = true
		}
		if it.NeedsSteward {
			res["needs_steward"] = true
			res["steward_reason"] = it.StewardReason
		}
		// The lifecycle invariant, per-item: an open run always answers
		// "what closes this?".
		if !weaveIsClosedState(it.State) {
			res["next_steps"] = weaveNextSteps(it)
		}
		res["auto_committed"] = it.AutoCommitted
		if it.AutoCommitError != "" {
			res["auto_commit_error"] = it.AutoCommitError
		}
		if it.CleanupError != "" {
			res["cleanup_error"] = it.CleanupError
		}
		if it.KilledBy != "" {
			res["killed_by"] = it.KilledBy
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave status", res))
	}

	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "run #%d — %s\n", it.ID, weaveTruncate(it.Title, 72))
	stateLine := displayState
	if reconciledFrom != "" {
		stateLine += fmt.Sprintf(" (recorded %q; merged outside `weave pull`)", reconciledFrom)
	} else if stale {
		stateLine += " (stale — wrapper pid dead)"
	}
	fmt.Fprintf(w, "  state:    %s\n", stateLine)
	fmt.Fprintf(w, "  health:   %s — %s\n", health.Health, health.Reason)
	fmt.Fprintf(w, "  action:   %s\n", health.Next)
	if !weaveIsClosedState(it.State) {
		fmt.Fprintf(w, "  next:     %s\n", weaveNextSteps(it))
	}
	if it.Register != "" {
		regStr := it.Register
		if len(regStr) > 8 {
			regStr = regStr[:8]
		}
		fmt.Fprintf(w, "  register: %s\n", regStr)
	}
	if it.Tool != "" {
		fmt.Fprintf(w, "  tool:     %s   dur: %s\n", it.Tool, weaveDurationCol(it))
	}
	if it.Branch != "" {
		fmt.Fprintf(w, "  branch:   %s\n", it.Branch)
	}
	branchInfo := fmt.Sprintf("%d commit(s) ahead of %s", it.CommitsAhead, base)
	if len(it.Head) >= 12 {
		branchInfo += " @ " + it.Head[:12]
	}
	fmt.Fprintf(w, "  commits:  %s\n", branchInfo)
	if it.Salvageable {
		fmt.Fprintf(w, "  salvage:  SALVAGEABLE — has %d unmerged commit(s); inspect with `weave shell %d`, then `weave salvage %d` to run configured deterministic gates and merge\n", it.UnmergedCommits, it.ID, it.ID)
	}
	if it.ExitCode != nil {
		fmt.Fprintf(w, "  exit:     %d\n", *it.ExitCode)
	}
	if it.KilledBy != "" {
		fmt.Fprintf(w, "  killed:   %s\n", it.KilledBy)
	}
	if it.VerifyExit != nil {
		verdict := "passed"
		if *it.VerifyExit != 0 {
			verdict = fmt.Sprintf("FAILED (exit %d)", *it.VerifyExit)
		}
		fmt.Fprintf(w, "  verify:   %s\n", verdict)
	}
	if it.AutoCommitted {
		fmt.Fprintf(w, "  auto:     committed dirty workspace changes\n")
	} else if it.AutoCommitError != "" {
		fmt.Fprintf(w, "  auto:     commit failed: %s\n", it.AutoCommitError)
	}
	if it.CleanupError != "" {
		fmt.Fprintf(w, "  cleanup:  FAILED: %s\n", it.CleanupError)
	}
	if it.Dirty {
		fmt.Fprintf(w, "  dirty:    %d tracked uncommitted file(s)\n", it.DirtyFiles)
	}
	if it.IsolationViolated {
		fmt.Fprintf(w, "  ISOLATION: VIOLATED — not mergeable without --force\n")
		fmt.Fprintf(w, "             %s\n", weaveIsolationDetail(it))
	}
	if len(it.OutsideWorkspacePaths) > 0 {
		fmt.Fprintf(w, "  advisory: worker referenced paths outside its workspace: %s\n",
			strings.Join(it.OutsideWorkspacePaths, ", "))
	}
	if it.Workspace != "" {
		state := "present"
		if !workspaceExists {
			state = "gone on disk"
		}
		fmt.Fprintf(w, "  workspace:  %s (%s)\n", weaveTildePath(it.Workspace), state)
	}
	mergedStr := "no"
	switch {
	case merged:
		mergedStr = "yes — already in " + base
	case it.State == "submitted":
		mergedStr = "no — `weave pull` to merge"
	}
	fmt.Fprintf(w, "  merged:   %s\n", mergedStr)
	return nil
}

func runWeaveReverify(cmd *cobra.Command, id int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reverify",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reverify",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reverify",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if it.Workspace == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reverify",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no workspace recorded", id)))
	}
	if st, err := os.Stat(it.Workspace); err != nil || !st.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reverify",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d workspace unavailable: %s: %w", id, it.Workspace, err)))
	}

	base := weaveBaseBranch(root)
	verifyCommand := it.VerifyCommand
	ev := weaveCollectTerminalEvidence(it.Workspace, weaveCountRef(it, base), dir, verifyCommand, it, verifyCommand != "")
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		freshIt := findWeaveItem(q, id)
		if freshIt == nil {
			return fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))
		}
		weaveApplyTerminalEvidence(freshIt, ev)
		if verifyCommand == "" {
			freshIt.VerifyExit = nil
			freshIt.VerifyOutput = ""
			freshIt.VerifyTree = ""
		}
		freshIt.AutoCommitError = ""
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reverify",
			weavecli.ExitGenericFail, lockErr))
	}
	if isTerminalState(it.State) {
		weaveReleaseManagedGOCache(cmd.ErrOrStderr(), "weave reverify", dir, it)
	}
	if mode == weavecli.OutputJSON {
		res := map[string]any{
			"issue":           id,
			"commits_ahead":   ev.CommitsAhead,
			"head":            ev.Head,
			"dirty":           ev.Dirty,
			"dirty_files":     ev.DirtyFiles,
			"untracked_files": ev.UntrackedFiles,
			"verify_rerun":    verifyCommand != "",
		}
		if ev.VerifyExit != nil {
			res["verify_exit"] = *ev.VerifyExit
			res["verify_tree"] = ev.VerifyTree
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave reverify", res))
	}
	if verifyCommand == "" {
		fmt.Fprintf(cmd.OutOrStdout(), "weave reverify: run #%d remeasured (no verify command recorded): %d commit(s) ahead, dirty=%v\n", id, ev.CommitsAhead, ev.Dirty)
		return nil
	}
	verifyExit := 0
	if ev.VerifyExit != nil {
		verifyExit = *ev.VerifyExit
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave reverify: run #%d verify_exit=%d commits_ahead=%d dirty=%v\n", id, verifyExit, ev.CommitsAhead, ev.Dirty)
	return nil
}

func gitOut(root string, args ...string) (string, error) {
	a := append([]string{"-C", root}, args...)
	out, err := exec.Command(gitBin(), a...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// safeRemoveWorkspace removes a workspace directory safely by verifying
// containment: the path must be non-empty and live under the queue
// directory's workspaces/ subdirectory (filepath.Rel containment check).
// Returns nil on success or if path is already gone.
func safeRemoveWorkspace(queueDir, workspacePath string) error {
	if workspacePath == "" {
		return nil
	}
	// Resolve to absolute paths for reliable comparison
	absQueue, err := filepath.Abs(queueDir)
	if err != nil {
		return err
	}
	absWorkspace, err := filepath.Abs(workspacePath)
	if err != nil {
		return err
	}
	// Containment check: workspace must be under queueDir/workspaces/ — or
	// the legacy queueDir/sandboxes/ for clones created before the
	// sandbox→workspace rename (their absolute path is stored in queue.json
	// and still points at the old dir). Either parent is a safe container.
	contained := false
	for _, parent := range []string{
		filepath.Join(absQueue, "workspaces"),
		filepath.Join(absQueue, "sandboxes"),
	} {
		rel, err := filepath.Rel(parent, absWorkspace)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(rel, "..") && rel != "." {
			contained = true
			break
		}
	}
	if !contained {
		return fmt.Errorf("workspace path %q is not contained in %q/{workspaces,sandboxes}", absWorkspace, absQueue)
	}
	if q, err := loadWeaveQueue(absQueue); err == nil {
		for _, it := range q.Items {
			if it.Workspace == "" || it.State != "working" || it.WrapperPid == 0 || !pidAlive(it.WrapperPid) {
				continue
			}
			itemWorkspace, err := filepath.Abs(it.Workspace)
			if err != nil {
				continue
			}
			if itemWorkspace == absWorkspace {
				return fmt.Errorf("refusing to remove workspace %q: run #%d has live wrapper pid %d", absWorkspace, it.ID, it.WrapperPid)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace live-wrapper check failed: %w", err)
	}
	// Additional safety: verify it's a directory before removal
	if st, err := os.Stat(absWorkspace); err != nil {
		if os.IsNotExist(err) {
			return nil // Already gone
		}
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("workspace path %q is not a directory", absWorkspace)
	}
	if dirty, dirtyFiles, _ := weaveMeasureDirtiness(absWorkspace); dirty {
		return fmt.Errorf("refusing to remove workspace %q: %d tracked file(s) have uncommitted changes", absWorkspace, dirtyFiles)
	}
	return os.RemoveAll(absWorkspace)
}

// runWeavePrio sets an issue's priority tier on the local queue.
// --auto (LLM-rank the whole queue) requires an LLM provider and is
// not available in the local backend; we emit a precondition_failed
// envelope so agent callers see a stable shape.
func runWeavePrio(cmd *cobra.Command, id int64, tier string, auto bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if auto {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prio",
			weavecli.ExitDepUnhealthy, fmt.Errorf("--auto requires an LLM provider; none is configured")))
	}
	if !isValidPriority(tier) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prio",
			weavecli.ExitInvalidArg, fmt.Errorf("priority must be one of p0|p1|p2|p3 (got %q)", tier)))
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prio",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	var it *weaveItem
	var prev string
	notFoundHint := weaveOtherActiveQueuesHintSuffix(dir)
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it = findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found%s", id, notFoundHint)
		}
		prev = it.Priority
		it.Priority = tier
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") {
			code = weavecli.ExitInvalidArg
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prio",
			code, lockErr))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave prio", map[string]any{
			"issue":    it.ID,
			"priority": it.Priority,
			"previous": prev,
			"title":    it.Title,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave prio: run #%d %s → %s\n", it.ID, prev, it.Priority)
	return nil
}

// runWeaveShell drops the user into $SHELL with cwd set to the
// issue's workspace so they can poke at the worktree directly.
// Inherits stdio; exits with the shell's exit code.
func runWeaveShell(cmd *cobra.Command, id int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave shell",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave shell",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave shell",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if it.Workspace == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave shell",
			weavecli.ExitStateConflict, fmt.Errorf("run #%d has no workspace (state=%q) — run `weave start --issue %d --no-spawn` first", it.ID, it.State, it.ID)))
	}
	if _, err := os.Stat(it.Workspace); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave shell",
			weavecli.ExitStateConflict, fmt.Errorf("workspace missing on disk: %s", it.Workspace)))
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	if mode == weavecli.OutputJSON {
		// Agent mode: return the workspace + shell info instead of execing
		// — agents can't drive an interactive shell anyway.
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave shell", map[string]any{
			"issue":     it.ID,
			"workspace": it.Workspace,
			"branch":    it.Branch,
			"shell":     shell,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave shell: run #%d workspace=%s (exit shell to return)\n", it.ID, it.Workspace)
	sh := exec.Command(shell)
	sh.Dir = it.Workspace
	sh.Env = append(os.Environ(),
		fmt.Sprintf("WEAVE_ID=weave-issue-%d", it.ID),
		fmt.Sprintf("WEAVE_ISSUE=%d", it.ID),
		fmt.Sprintf("WEAVE_BRANCH=%s", it.Branch),
		fmt.Sprintf("WEAVE_ISSUE_TITLE=%s", it.Title),
	)
	sh.Stdin = os.Stdin
	sh.Stdout = os.Stdout
	sh.Stderr = os.Stderr
	if err := sh.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return &exitCodeError{code: exit.ExitCode()}
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave shell",
			weavecli.ExitGenericFail, err))
	}
	return nil
}

// runWeaveReset tears down every weave for the current repo:
// removes each worktree, deletes each branch, and clears the queue
// file. Refuses without --yes unless stdin is a TTY and the user
// confirms at the prompt.
func runWeaveReset(cmd *cobra.Command, yes bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reset",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	if err := weaveConfirmBatch(cmd, mode, "reset",
		"weave reset: this removes every workspace + branch for this repo and clears the queue.", yes); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reset",
			weavecli.ExitInvalidArg, err))
	}
	type tear struct {
		Issue     int64  `json:"issue"`
		Branch    string `json:"branch,omitempty"`
		Workspace string `json:"workspace,omitempty"`
	}
	var teardowns []tear
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		for _, it := range q.Items {
			if it.Workspace == "" && it.Branch == "" && it.WrapperPid == 0 {
				continue
			}
			// Stop any still-running wrapper precisely (PID + setsid
			// group). Reset is a destructive batch op — we want
			// everything torn down cleanly.
			if it.WrapperPid > 0 {
				weaveStopWrapper(it.WrapperPid)
			}
			// Workspaces are independent git clones — rm -rf is right.
			if it.Workspace != "" {
				_ = os.RemoveAll(it.Workspace)
			}
			if it.Branch != "" {
				// Best-effort: drop the branch from the user's repo if
				// `weave pull` fetched it earlier.
				_ = exec.Command(gitBin(), "-C", root, "branch", "-D", it.Branch).Run()
			}
			teardowns = append(teardowns, tear{Issue: it.ID, Branch: it.Branch, Workspace: it.Workspace})
		}
		q.Items = nil
		q.NextID = 1
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reset",
			weavecli.ExitGenericFail, lockErr))
	}
	// Reset means the project has no weave state left. Removing only the clone
	// directories and emptying queue.json left logs, locks, agent data, and the
	// queue itself behind forever; an idle machine therefore accumulated one
	// state root per repo even after an explicit reset. The queue lock has been
	// released at this point, so remove the whole project root, then remove the
	// shared parent if this was its last child. A later write recreates both.
	if err := os.RemoveAll(dir); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reset",
			weavecli.ExitGenericFail, fmt.Errorf("remove project state: %w", err)))
	}
	if err := os.Remove(filepath.Dir(dir)); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTEMPTY) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave reset",
			weavecli.ExitGenericFail, fmt.Errorf("remove empty weave root: %w", err)))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave reset", map[string]any{
			"teardowns": teardowns,
			"count":     len(teardowns),
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave reset: tore down %d weaves; queue cleared\n", len(teardowns))
	return nil
}

// runWeaveOpen surfaces an issue's workspace worktree as a file:// URL so
// you can jump straight to the files an agent produced. weave is
// local-only — there is no remote page to open — so this resolves
// entirely on the local filesystem.
func runWeaveOpen(cmd *cobra.Command, issueID int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave open",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, _ := loadWeaveQueue(dir)
	it := findWeaveItem(q, issueID)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave open",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", issueID, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if it.Workspace == "" {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave open",
			weavecli.ExitPrecondFail, fmt.Errorf("run #%d has no workspace yet (run `weave start` first)", issueID)))
	}
	fileURL := "file://" + it.Workspace
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave open", map[string]any{
			"issue":         it.ID,
			"workspace":     it.Workspace,
			"workspace_url": fileURL,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave open: run #%d workspace %s\n", it.ID, fileURL)
	return nil
}

// addFromFile parses a markdown checklist or a JSON list and bulk-
// adds each entry to the queue. Returns the IDs created.
//
// Markdown shape (each line, ignoring leading/trailing whitespace):
//
//   - [ ] title goes here
//   - [ ] another title
//
// JSON shape: an array of objects with at minimum a `title` field;
// optional `body`, `priority`, `tool` overrides:
//
//	[
//	  {"title": "fix null deref", "priority": "p0"},
//	  {"title": "refactor user service", "body": "as discussed"}
//	]
func addFromFile(path string, defaultPriority string) ([]*weaveItem, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read --from-file: %w", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		// JSON array
		var entries []struct {
			Title    string `json:"title"`
			Body     string `json:"body"`
			Priority string `json:"priority"`
		}
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, fmt.Errorf("parse --from-file as JSON: %w", err)
		}
		var out []*weaveItem
		for i, e := range entries {
			if strings.TrimSpace(e.Title) == "" {
				return nil, fmt.Errorf("--from-file entry %d: title required", i)
			}
			prio := e.Priority
			if prio == "" {
				prio = defaultPriority
			}
			out = append(out, &weaveItem{Title: e.Title, Body: e.Body, Priority: prio})
		}
		return out, nil
	}
	// Markdown checklist
	var out []*weaveItem
	for _, line := range strings.Split(string(raw), "\n") {
		l := strings.TrimSpace(line)
		// Match `- [ ] ...` or `- [x] ...` (case-insensitive).
		if len(l) < 6 || l[0] != '-' {
			continue
		}
		rest := strings.TrimSpace(l[1:])
		if len(rest) < 4 || rest[0] != '[' || rest[2] != ']' {
			continue
		}
		title := strings.TrimSpace(rest[3:])
		if title == "" {
			continue
		}
		out = append(out, &weaveItem{Title: title, Priority: defaultPriority})
	}
	return out, nil
}

// runWeaveAddFromFile bulk-adds from a markdown or JSON file. Each
// successful add increments NextID and emits one envelope (in JSON
// mode, a single result containing all added IDs).
func runWeaveAddFromFile(cmd *cobra.Command, path, defaultPriority string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if defaultPriority == "" {
		defaultPriority = "p2"
	}
	if !isValidPriority(defaultPriority) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitInvalidArg, fmt.Errorf("priority must be one of p0|p1|p2|p3 (got %q)", defaultPriority)))
	}
	entries, err := addFromFile(path, defaultPriority)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitInvalidArg, err))
	}
	if len(entries) == 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitInvalidArg, fmt.Errorf("--from-file %s contained no actionable entries", path)))
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitGenericFail, err))
	}
	now := time.Now().UTC()
	type added struct {
		ID       int64  `json:"id"`
		Title    string `json:"title"`
		Priority string `json:"priority"`
	}
	var addedAll []added
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		for _, e := range entries {
			e.ID = q.NextID
			q.NextID++
			e.State = "todo"
			e.Created = now
			q.Items = append(q.Items, e)
			addedAll = append(addedAll, added{ID: e.ID, Title: e.Title, Priority: e.Priority})
		}
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add",
			weavecli.ExitGenericFail, lockErr))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave add", map[string]any{
			"added":  addedAll,
			"count":  len(addedAll),
			"source": path,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave add: bulk-added %d issues from %s\n", len(addedAll), path)
	return nil
}

// runWeaveListWatch polls the queue file every interval and emits
// a transition event (NDJSON envelope in JSON mode, one line in
// human modes) every time an item's state changes. Terminates on
// SIGINT or when the command context is cancelled.
func runWeaveListWatch(cmd *cobra.Command, includeHistory bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave list",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	prev := map[int64]string{}
	snapshot := func() (map[int64]string, []*weaveItem, error) {
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return nil, nil, err
		}
		cur := map[int64]string{}
		var items []*weaveItem
		for _, it := range q.Items {
			if !weaveItemVisibleInList(it, includeHistory) {
				continue
			}
			cur[it.ID] = it.State
			items = append(items, it)
		}
		return cur, items, nil
	}
	// Initial snapshot — emit as a synthetic "snapshot" event so a
	// watcher gets the full picture at t=0.
	cur, items, err := snapshot()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave list",
			weavecli.ExitGenericFail, err))
	}
	emitInitial := func() {
		if mode == weavecli.OutputJSON {
			_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
				"schema_version": weavecli.SchemaVersion,
				"command":        "weave list",
				"status":         "ok",
				"event":          "snapshot",
				"items":          items,
			})
			return
		}
		fmt.Fprintf(cmd.OutOrStdout(), "weave list --watch: %d active issue(s) at t=0\n", len(items))
		for _, it := range items {
			fmt.Fprintf(cmd.OutOrStdout(), "  #%d %-10s %s — %s\n", it.ID, it.State, it.Priority, it.Title)
		}
	}
	emitInitial()
	prev = cur

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		cur, _, err := snapshot()
		if err != nil {
			// Don't kill the watch on transient read errors — queue.json
			// is rewritten via tmp+rename, so a read between writes can
			// fail with ENOENT briefly. Skip this tick.
			continue
		}
		for id, st := range cur {
			if prev[id] != st {
				if mode == weavecli.OutputJSON {
					_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
						"schema_version": weavecli.SchemaVersion,
						"command":        "weave list",
						"status":         "ok",
						"event":          "transition",
						"issue":          id,
						"from":           prev[id],
						"to":             st,
					})
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  #%d %s → %s\n", id, prev[id], st)
				}
			}
		}
		for id, st := range prev {
			if _, ok := cur[id]; !ok {
				if mode == weavecli.OutputJSON {
					_ = json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
						"schema_version": weavecli.SchemaVersion,
						"command":        "weave list",
						"status":         "ok",
						"event":          "removed",
						"issue":          id,
						"from":           st,
					})
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  #%d %s → (removed)\n", id, st)
				}
			}
		}
		prev = cur
	}
}

// isValidPriority returns true for any of the accepted priority tiers.
// weaveValidPoints is the allowed story-point scale (Fibonacci;
// 8 = the ~30-minute cap — split anything judged bigger).
func weaveValidPoints(n int) bool {
	switch n {
	case 1, 2, 3, 5, 8:
		return true
	}
	return false
}

// weavePointRuntimeCap makes the estimate an execution ceiling. The scale is
// linear and exact: one point is 3m45s, so the largest accepted item (8) gets
// 30m and every smaller Fibonacci estimate gets proportionally less. A caller
// must reject an invalid point value rather than treating it as unbounded.
func weavePointRuntimeCap(points int) (time.Duration, bool) {
	if !weaveValidPoints(points) {
		return 0, false
	}
	return time.Duration(points) * (15 * time.Minute / 4), true
}

func weaveBoundRuntime(points int, requested time.Duration) (time.Duration, error) {
	if requested < 0 {
		return 0, fmt.Errorf("--max-runtime must not be negative")
	}
	if points == 0 { // legacy standalone work remains supported
		return requested, nil
	}
	cap, ok := weavePointRuntimeCap(points)
	if !ok {
		return 0, fmt.Errorf("invalid points %d (want 1,2,3,5,8); correct it before launch", points)
	}
	if requested == 0 {
		return cap, nil
	}
	if requested > cap {
		return 0, fmt.Errorf("--max-runtime %s exceeds the %d-point cap %s; split or re-point the work instead", requested, points, cap)
	}
	return requested, nil
}

func runWeavePoint(cmd *cobra.Command, id int64, points int, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave point",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	if !weaveValidPoints(points) {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave point",
			weavecli.ExitInvalidArg, fmt.Errorf("points must be one of 1,2,3,5,8 (8 = ~30m cap; split bigger work)")))
	}
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))
		}
		// Points define the watchdog budget. Once a run is claimed, changing
		// them would change the record without changing the already-running
		// guard. Re-point only while the item is still in planning.
		if it.State != "todo" {
			return fmt.Errorf("run #%d state is %q; points may only change while state is todo", id, it.State)
		}
		it.Points = points
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") {
			code = weavecli.ExitInvalidArg
		} else if strings.Contains(lockErr.Error(), "points may only change") {
			code = weavecli.ExitStateConflict
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave point", code, lockErr))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave point", map[string]any{
			"issue": id, "points": points,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave point: run #%d = %d points\n", id, points)
	return nil
}

func isValidPriority(s string) bool {
	switch s {
	case "p0", "p1", "p2", "p3":
		return true
	}
	return false
}

// stdinIsTTY reports whether stdin is a terminal. Used by reset to
// distinguish "user at a terminal who can answer a prompt" from
// "scripted invocation that needs --yes".
func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// weaveConfirmPrompt runs the shared [y/N] prompt at a TTY and returns a
// "cancelled" error on anything but yes. Callers gate when it runs.
func weaveConfirmPrompt(cmd *cobra.Command, prompt string) error {
	if prompt != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", prompt)
	}
	fmt.Fprint(cmd.OutOrStdout(), "proceed? [y/N] ")
	var resp string
	_, _ = fmt.Fscanln(os.Stdin, &resp)
	if !strings.EqualFold(resp, "y") && !strings.EqualFold(resp, "yes") {
		return fmt.Errorf("cancelled")
	}
	return nil
}

// weaveConfirmBatch gates the BATCH destructive verbs (reset, prune)
// that act on an implicit SET the caller never enumerated — accidental
// invocation is catastrophic, so a non-interactive (or --json) call
// without --yes is refused outright. With --yes it proceeds silently; at
// a TTY it prompts. Refusal is one clean error — no hung prompt, no
// usage dump (attach() sets SilenceUsage on the leaf).
func weaveConfirmBatch(cmd *cobra.Command, mode weavecli.OutputMode, verb, prompt string, yes bool) error {
	if yes {
		return nil
	}
	if mode == weavecli.OutputJSON || !stdinIsTTY() {
		return fmt.Errorf("refusing destructive %s without --yes in non-interactive mode", verb)
	}
	return weaveConfirmPrompt(cmd, prompt)
}

// weaveConfirmTargeted gates the verbs that act on a single EXPLICITLY
// NAMED issue (abandon, kill). The blast radius is one issue the caller
// already chose, and orchestrators invoke these programmatically — so a
// non-interactive call PROCEEDS (it is not refused). The prompt fires
// only for an interactive human at a TTY; --yes skips even that.
func weaveConfirmTargeted(cmd *cobra.Command, mode weavecli.OutputMode, prompt string, yes bool) error {
	if yes || mode == weavecli.OutputJSON || !stdinIsTTY() {
		return nil
	}
	return weaveConfirmPrompt(cmd, prompt)
}

// runWeaveKill stops the running wrapper for an issue WITHOUT
// tearing down its workspace or branch. Use when a subagent has gone
// stuck (no output for a long time, runaway iteration, etc.) and
// the orchestrator wants the partial work preserved for inspection
// or for a `weave start --resume` retry.
//
// The orchestrator-safe shape: orchestrators MUST NOT shell out to
// pkill / killall / kill -9 — those match by name and will catch
// peer ycode/claude/codex sessions belonging to OTHER agents in
// the same machine. `weave kill <issue>` reads the recorded
// wrapper PID from the queue and signals only that process group,
// then flips the queue item to `failed` with a "killed by
// orchestrator" marker.
func runWeaveKill(cmd *cobra.Command, id int64, reason string, yes bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave kill",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	if err := weaveRecoverAbandonedFinalizations(dir); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave kill",
			weavecli.ExitGenericFail, err))
	}
	if err := weaveConfirmTargeted(cmd, mode,
		fmt.Sprintf("weave kill: stops the running subagent for run #%d (workspace + branch preserved).", id), yes); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave kill",
			weavecli.ExitInvalidArg, err))
	}
	base := weaveBaseBranch(root)

	// Graceful first: if the wrapper serves a control socket, ask the
	// TUI to leave on its own (`/exit`, then `/quit` for tools that
	// spell it differently) and give each a short grace window. A
	// clean self-exit means the WRAPPER records the terminal state
	// from a real exit code — the most accurate outcome possible, no
	// inference involved. Only a non-responding tool gets signals.
	if q0, err := loadWeaveQueue(dir); err == nil {
		if it0 := findWeaveItem(q0, id); it0 != nil && it0.State == "working" &&
			it0.CtlSock != "" && it0.WrapperPid > 0 && pidAlive(it0.WrapperPid) {
			// The completion signal is the QUEUE STATE, not the pid:
			// the wrapper's terminal write is the event we're waiting
			// for, and a pid check lies when the wrapper is a zombie
			// child of some still-running parent (kill(pid,0) succeeds
			// on zombies).
			gracefulState := ""
		verbs:
			for _, verb := range []string{"/exit", "/quit"} {
				_ = weaveWriteControlFrame(it0.CtlSock, agentpty.VerbatimFrame([]byte(verb+"\n")))
				deadline := time.Now().Add(6 * time.Second)
				for time.Now().Before(deadline) {
					time.Sleep(300 * time.Millisecond)
					if q1, err := loadWeaveQueue(dir); err == nil {
						if it1 := findWeaveItem(q1, id); it1 != nil && isTerminalState(it1.State) {
							gracefulState = it1.State
							break verbs
						}
					}
				}
			}
			if gracefulState != "" {
				// The wrapper recorded the truthful terminal state
				// from the tool's own exit; report what it wrote.
				if mode == weavecli.OutputJSON {
					return ec(emitOK(cmd.OutOrStdout(), mode, "weave kill", map[string]any{
						"issue": id, "state": gracefulState, "graceful": true, "reason": reason,
					}))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "weave kill: run #%d exited gracefully on /exit, state=%s\n", id, gracefulState)
				return nil
			}
		}
	}

	var killed bool
	var wrapperPid int
	var finalState string
	var workspace string
	var verifyCommand string
	var verifyItem *weaveItem
	notFoundHint := weaveOtherActiveQueuesHintSuffix(dir)
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found%s", id, notFoundHint)
		}
		if it.State != "working" && !weaveWrapperTerminalClaimed(it) {
			return fmt.Errorf("run #%d state is %q (kill requires working)", id, it.State)
		}
		wrapperPid = it.WrapperPid
		workspace = it.Workspace
		verifyCommand = it.VerifyCommand
		copy := *it
		verifyItem = &copy
		return nil
	})
	if lockErr == nil && wrapperPid > 0 {
		weaveStopWrapper(wrapperPid)
		killed = true
	}

	// killed stays killed: the forced stop is recorded as its own
	// terminal state, never silently promoted. Measure after the
	// process tree is dead so verify cannot race a still-running build.
	// Like the normal terminal path, expensive substrate evidence is
	// collected outside the queue lock and attached during the final
	// locked write.
	ahead, head := weaveMeasureBranch(workspace, base)
	dirty, dirtyFiles, untrackedFiles := weaveMeasureDirtiness(workspace)
	var verifyExit *int
	var verifyOutput string
	var verifyTree string
	if lockErr == nil && verifyCommand != "" && (ahead > 0 || dirty) {
		verifyExit, verifyOutput, verifyTree = weaveCollectVerifyEvidence(workspace, dir, verifyCommand, verifyItem, dirty, dirtyFiles)
	}

	if lockErr == nil {
		lockErr = withWeaveQueueLock(dir, func(q *weaveQueue) error {
			it := findWeaveItem(q, id)
			if it == nil {
				return fmt.Errorf("run #%d not found%s", id, notFoundHint)
			}
			if it.State != "working" && !weaveWrapperTerminalClaimed(it) && it.State != "killed" {
				return fmt.Errorf("run #%d state is %q (kill requires working)", id, it.State)
			}
			it.CommitsAhead = ahead
			it.Head = head
			it.Dirty = dirty
			it.DirtyFiles = dirtyFiles
			it.UntrackedFiles = untrackedFiles
			if verifyExit != nil {
				it.VerifyExit = verifyExit
				it.VerifyOutput = verifyOutput
				it.VerifyTree = verifyTree
			}
			it.State = "killed"
			it.Completion = ""
			it.FinalizerPID = 0
			it.FinalizingAt = time.Time{}
			finalState = it.State
			killCode := -1
			if it.ExitCode == nil {
				it.ExitCode = &killCode
			}
			it.FinishedAt = time.Now().UTC()
			it.WrapperPid = 0
			it.CtlSock = ""
			// Stash the kill reason in the Body so `weave list` shows
			// it (Body isn't load-bearing once the subagent has
			// started — the prompt's already been consumed).
			note := "[killed by orchestrator"
			if reason != "" {
				note += ": " + reason
			}
			if ahead > 0 {
				note += fmt.Sprintf(" — %d wrapper-verified commit(s) ahead at %.12s", ahead, head)
			}
			if !strings.HasPrefix(it.Body, "[killed") {
				it.Body = note + "]\n\n" + it.Body
			}
			return nil
		})
	}
	if lockErr != nil {
		if strings.Contains(lockErr.Error(), "not found") {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave kill",
				weavecli.ExitInvalidArg, lockErr))
		}
		if strings.Contains(lockErr.Error(), "kill requires working") {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave kill",
				weavecli.ExitStateConflict, lockErr))
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave kill",
			weavecli.ExitGenericFail, lockErr))
	}
	weaveReleaseManagedGOCache(cmd.ErrOrStderr(), "weave kill", dir, verifyItem)
	if mode == weavecli.OutputJSON {
		result := map[string]any{
			"issue":       id,
			"state":       finalState,
			"wrapper_pid": wrapperPid,
			"killed":      killed,
			"reason":      reason,
		}
		if verifyExit != nil {
			result["verify_exit"] = *verifyExit
			result["verify_output"] = verifyOutput
			result["verify_tree"] = verifyTree
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave kill", result))
	}
	if verifyExit != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "weave kill: run #%d wrapper_pid=%d killed=%v state=%s verify_exit=%d\n", id, wrapperPid, killed, finalState, *verifyExit)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "weave kill: run #%d wrapper_pid=%d killed=%v state=%s\n", id, wrapperPid, killed, finalState)
	}
	return nil
}

// runWeaveFinalize records a conductor-observed idle interactive session without
// guessing from PTY output. It is intentionally opt-in: the caller attests that
// the named TUI returned idle, then weave stops only that wrapper and measures
// the isolated branch to determine submitted versus failed.
func runWeaveFinalize(cmd *cobra.Command, id int64, observedIdle bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if !observedIdle {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave finalize",
			weavecli.ExitInvalidArg, fmt.Errorf("refusing to infer interactive completion; pass --observed-idle after observing the named agent return idle")))
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave finalize",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	base := weaveBaseBranch(root)
	var countRef string
	var wrapperPID int
	var workspace, verifyCommand string
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))
		}
		if it.State != "working" {
			return fmt.Errorf("run #%d state is %q (finalize requires working)", id, it.State)
		}
		wrapperPID, workspace, verifyCommand = it.WrapperPid, it.Workspace, it.VerifyCommand
		countRef = weaveCountRef(it, base)
		// Claim the terminal write BEFORE stopping the wrapper. Its normal
		// exit path then yields to this explicit finalizer instead of racing
		// us to write a killed/failed inference.
		it.State = "finalizing"
		it.Completion = "conductor-finalizing-observed-idle"
		it.FinalizerPID = os.Getpid()
		it.FinalizingAt = time.Now().UTC()
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") || strings.Contains(lockErr.Error(), "requires working") {
			code = weavecli.ExitStateConflict
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave finalize", code, lockErr))
	}
	weaveTestPauseAfterFinalizeClaim()
	if wrapperPID > 0 && pidAlive(wrapperPID) {
		weaveStopWrapper(wrapperPID)
	}
	ev := weaveCollectTerminalEvidence(workspace, countRef, dir, "", &weaveItem{ID: id}, false)
	if verifyCommand != "" && (ev.CommitsAhead > 0 || ev.Dirty || ev.UntrackedFiles > 0) {
		ev = weaveCollectTerminalEvidence(workspace, countRef, dir, verifyCommand, &weaveItem{ID: id}, true)
	}
	state := weaveTerminalState(0, nil, "", ev)
	if state == "submitted" && (ev.Dirty || ev.UntrackedFiles > 0 || (ev.VerifyExit != nil && *ev.VerifyExit != 0)) {
		state = "failed"
	}
	var finalized *weaveItem
	lockErr = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d disappeared while finalizing", id)
		}
		if !weaveWrapperTerminalClaimed(it) {
			return fmt.Errorf("run #%d transitioned to %q while finalizing", id, it.State)
		}
		weaveApplyTerminalEvidence(it, ev)
		weaveApplyIsolationCheck(it)
		it.State = state
		if state == "no-op" {
			it.Disposition = weaveDispositionEmpty
		}
		it.Completion = "conductor-finalized-observed-idle"
		it.FinalizerPID = 0
		it.FinalizingAt = time.Time{}
		it.FinishedAt = time.Now().UTC()
		it.WrapperPid = 0
		it.CtlSock = ""
		weaveAppendComment(it, "conductor", "system", "finalized after explicit observed-idle attestation; terminal state measured from workspace evidence")
		finalized = it
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave finalize", weavecli.ExitGenericFail, lockErr))
	}
	weaveReleaseManagedGOCache(cmd.ErrOrStderr(), "weave finalize", dir, finalized)
	// Fold the terminal gate evidence into the capability matrix (best-effort).
	weaveRecordCapability(finalized)
	result := map[string]any{"issue": id, "state": state, "completion": "conductor-finalized-observed-idle"}
	if ev.VerifyExit != nil {
		result["verify_exit"] = *ev.VerifyExit
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave finalize", result))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave finalize: run #%d state=%s completion=conductor-finalized-observed-idle\n", id, state)
	return nil
}

func weaveWrapperTerminalClaimed(it *weaveItem) bool {
	return it != nil && it.State == "finalizing" && it.Completion == "conductor-finalizing-observed-idle"
}

func weaveTestPauseAfterFinalizeClaim() {
	pause := os.Getenv("WEAVE_TEST_FINALIZE_AFTER_CLAIM_FILE")
	if pause == "" {
		return
	}
	_ = os.WriteFile(pause+".ready", []byte("ready"), 0o644)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(pause); os.IsNotExist(err) || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runWeavePrune removes workspace directories for terminal items (done,
// abandoned, failed, killed) and deletes their agent/weave-issue-N branches
// from the user repo if fully merged (git branch -d, never -D). Prints a
// per-item line + summary; --yes skips confirmation.
//
// Before classifying, it reconciles "submitted" items against git: one
// whose work is already an ancestor of the base branch (merged by some
// route other than `weave pull`) is flipped to "done" and swept in the
// same pass — without this, such an item is stranded forever (prune
// refuses it, leaving only the data-loss-flavored `abandon`).
// runWeaveSalvage merges the committed work of a KILLED (or failed) item that
// `weave pull` won't auto-merge, without the manual fetch+cherry-pick dance.
// It is not a blind force: it promotes the item to "submitted" only after
// confirming it has commits ahead of base and a clean tree, then delegates to
// runWeavePull — so pull's dirty / verify-exit gates still apply. This is the
// supported path for "the agent did good work but its TUI was killed, so it
// landed in `killed` state."
//
// Model review is opt-in here, as it is for pull. A bare salvage still runs the
// deterministic dirty / verify / suite / isolation gates. --no-review remains
// accepted for compatibility but is no longer needed to bypass a model gate.
//
// Salvage never pushes: merging and publishing are separate decisions, and
// nothing in weave contacts a remote. Publishing salvaged work is the operator's
// deliberate, separate act.
func runWeaveSalvage(cmd *cobra.Command, flags *weaveOutputFlags, issueID int64, reviewAgent string, noReview bool) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave salvage",
			weavecli.ExitPrecondFail, err))
	}
	reviewAgent = strings.TrimSpace(reviewAgent)
	if reviewAgent != "" && noReview {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave salvage", weavecli.ExitInvalidArg,
			fmt.Errorf("--review-agent and --no-review are mutually exclusive: pick review or name the escape, not both")))
	}
	dir, _ := weaveQueueDir(root)
	base := weaveBaseBranch(root)
	var diffStat string
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, issueID)
		if it == nil {
			return fmt.Errorf("run #%d not found%s", issueID, weaveOtherActiveQueuesHintSuffix(dir))
		}
		switch it.State {
		case "submitted", "working":
			// Already a normal pull target; let pull handle it — but the review
			// gate below still applies, because salvage is the verb that was
			// asked for and it must not be a cheaper door into the same merge.
		case "killed", "failed":
			// promotable below
		default:
			return fmt.Errorf("run #%d is %q — salvage applies to killed/failed items holding committed work (done/abandoned/allocated have nothing to merge)", issueID, it.State)
		}
		if it.State == "submitted" || it.State == "working" {
			diffStat = weaveSalvageDiffStat(it, base)
			return nil
		}
		if it.Workspace == "" {
			return fmt.Errorf("run #%d has no workspace to salvage from", issueID)
		}
		if dirty, _, _ := weaveMeasureDirtiness(it.Workspace); dirty {
			return fmt.Errorf("run #%d workspace has uncommitted changes — commit them in the workspace (`weave shell %d`) first, then salvage", issueID, issueID)
		}
		ahead, head := weaveMeasureBranch(it.Workspace, weaveCountRef(it, base))
		if ahead <= 0 {
			return fmt.Errorf("run #%d has 0 commits ahead of %s — nothing to salvage", issueID, base)
		}
		diffStat = weaveSalvageDiffStat(it, base)
		it.State = "submitted"
		it.Body = fmt.Sprintf("[salvaged: promoted from killed/failed to submitted at %.12s (%d commit(s) ahead); merging via pull's verify gate]\n\n", head, ahead) + it.Body
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave salvage",
			weavecli.ExitInvalidArg, lockErr))
	}
	// Salvage claims to let you inspect. Show what is about to be merged before
	// merging it, so the claim is true. JSON mode stays machine-shaped: pull's
	// envelope is the whole document, so the diff goes to stderr there.
	if diffStat != "" {
		w := cmd.OutOrStdout()
		if mode == weavecli.OutputJSON {
			w = cmd.ErrOrStderr()
		}
		fmt.Fprintf(w, "weave salvage: run #%d — the diff about to be merged into %s:\n%s\n", issueID, base, diffStat)
	}
	// Delegate to pull: re-acquires the lock and runs the full verify + merge
	// path on the now-"submitted" item. force=false — salvage rescues work a
	// run committed, which is no reason to skip the isolation gate; a flagged
	// run still has to be reviewed and pulled with an explicit --force.
	return runWeavePull(cmd, flags, issueID, true, false, false, noReview, reviewAgent)
}

// weaveSalvageDiffStat renders what salvage is about to merge. Best-effort: a
// missing workspace or a git hiccup must not block the merge path, only the
// courtesy of showing it.
func weaveSalvageDiffStat(it *weaveItem, base string) string {
	if it == nil || it.Workspace == "" {
		return ""
	}
	if st, err := os.Stat(it.Workspace); err != nil || !st.IsDir() {
		return ""
	}
	ref := weaveCountRef(it, base)
	out, err := gitOut(it.Workspace, "diff", "--stat", ref+"...HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimRight(out, "\n")
}

// weavePrunableForSweep reports whether `weave prune` (optionally --stale)
// should sweep an item. The base set is the terminal states (isPrunableState);
// --stale additionally sweeps orphaned "allocated" items — a workspace was
// created but the tool never launched / died before any commit, so there is no
// work to lose. Items holding commits (submitted/working) are never swept here.
func weavePrunableForSweep(state string, stale bool) bool {
	if isPrunableState(state) {
		return true
	}
	return stale && state == "allocated"
}

func runWeavePrune(cmd *cobra.Command, yes, stale, force bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prune",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	base := weaveBaseBranch(root)

	// First pass (no lock): reconcile in-memory and count what prune
	// would sweep, so the confirmation prompt names a real number.
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prune",
			weavecli.ExitGenericFail, err))
	}
	weaveReconcileMerged(root, base, q)
	pendingCount := 0
	for _, it := range q.Items {
		if weavePrunableForSweep(it.State, stale) {
			pendingCount++
		}
	}
	cacheTargets, err := weaveManagedGOCacheSweepTargets(dir, q, stale)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prune",
			weavecli.ExitGenericFail, err))
	}
	// Directories under workspaces/ that no item claims. The loop above cannot
	// see them at any flag setting, so without this pass they are unreclaimable
	// for the life of the queue.
	orphanTargets, err := weaveOrphanWorkspaceTargets(dir, q)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prune",
			weavecli.ExitGenericFail, err))
	}
	orphanSweepable := 0
	for _, o := range orphanTargets {
		if force || o.Hold == "" {
			orphanSweepable++
		}
	}
	if pendingCount == 0 && len(cacheTargets) == 0 && len(orphanTargets) == 0 {
		if mode == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave prune", map[string]any{
				"removed":       0,
				"cache_removed": 0,
				"results":       []any{},
			}))
		}
		fmt.Fprintln(cmd.OutOrStdout(), "weave prune: no terminal items or managed GOCACHE directories to clean up")
		return nil
	}

	prompt := fmt.Sprintf("weave prune: will clean up %d terminal item(s) (workspace + merged branches).", pendingCount)
	if len(cacheTargets) > 0 {
		prompt = fmt.Sprintf("weave prune: will clean up %d terminal item(s) and %d managed GOCACHE director%s.",
			pendingCount, len(cacheTargets), map[bool]string{true: "y", false: "ies"}[len(cacheTargets) == 1])
	}
	if orphanSweepable > 0 {
		prompt += fmt.Sprintf(" Also %d unclaimed workspace director%s.",
			orphanSweepable, map[bool]string{true: "y", false: "ies"}[orphanSweepable == 1])
	}
	if err := weaveConfirmBatch(cmd, mode, "prune",
		prompt, yes); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prune",
			weavecli.ExitInvalidArg, err))
	}

	type pruneResult struct {
		Issue     int64  `json:"issue"`
		State     string `json:"state"`
		Workspace string `json:"workspace,omitempty"`
		Cache     string `json:"cache,omitempty"`
		Branch    string `json:"branch,omitempty"`
		Merged    bool   `json:"merged"`
		Action    string `json:"action"` // "removed", "skipped", "branch_deleted", "failed: ..."
	}

	var results []pruneResult
	swept := 0
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		// Re-reconcile under the lock so the flip persists to disk.
		weaveReconcileMerged(root, base, q)
		for _, it := range q.Items {
			if !weavePrunableForSweep(it.State, stale) {
				continue
			}
			swept++
			// Whether the work landed in base — git's truth, checked via
			// the workspace HEAD sha (recorded at terminal time), NOT a
			// user-repo branch lookup that's a no-op for unfetched
			// agent branches. Read before the workspace is removed.
			merged := weaveItemMerged(root, base, it)

			// REFUSE TO DELETE WORK THAT HAS NOWHERE ELSE TO LIVE.
			//
			// The branch half of this function has always been careful: `git branch
			// -d`, never -D, so git itself refuses to drop an unmerged branch. The
			// WORKSPACE half had no such guard, and it is the half that matters —
			// an agent branch lives ONLY inside its workspace clone until `weave
			// pull` fetches it. Delete the workspace and the commits are not
			// "unmerged", they are GONE.
			//
			// Found with two live runs sitting in the queue: #3 had crashed on exit
			// with a complete, tested feature committed on its branch; #4 had the
			// same feature's tests passing in an uncommitted tree. Both were
			// `failed`. `weave prune` classified both by their STATE LABEL alone and
			// would have erased the lot, announcing only "will clean up 3 terminal
			// item(s)".
			//
			// A destructive sweep may not decide what is expendable from a label. It
			// has to look at the artifact.
			//
			// The measurement goes through weaveUnmergedAhead — the same one behind
			// `weave list`'s SALVAGEABLE line and pull's refusal — and NOT through a
			// local rev-list of its own. This half used to count `base..HEAD` with
			// the base BRANCH NAME as read inside the workspace clone, ignoring the
			// item's clone-point BaseSHA; a workspace whose local base ref had moved
			// on to include the agent's commits counts 0 that way, and 0 is the
			// number that lets a destructive sweep proceed.
			if !force && it.Workspace != "" && !merged {
				if _, statErr := os.Stat(it.Workspace); statErr == nil {
					ahead, _ := weaveUnmergedAhead(root, base, it)
					dirty, dirtyFiles, untracked := weaveMeasureDirtiness(it.Workspace)
					if ahead > 0 || dirty {
						why := weavePruneHoldReason(ahead, dirtyFiles, untracked)
						results = append(results, pruneResult{
							Issue: it.ID, State: it.State, Workspace: it.Workspace,
							Merged: false, Action: "skipped: " + why,
						})
						continue
					}
				}
			}

			// --force used to mean "destroy it anyway", which for a workspace
			// meant the commits and the tree both went with it — the branch
			// exists nowhere else. Preserve first, exactly as `weave abandon
			// --force` does, so --force means "I accept the risk" and not
			// "make it disappear". A tree is committed first, because only a
			// commit is reachable by a ref.
			if force && it.Workspace != "" && !merged {
				if _, statErr := os.Stat(it.Workspace); statErr == nil {
					ahead, head := weaveUnmergedAhead(root, base, it)
					if dirty, dirtyFiles, untracked := weaveMeasureDirtiness(it.Workspace); dirty || untracked > 0 {
						committed, cerr := maybeAutoCommit(it.Workspace, weaveForcedSalvageCommitMessage(it))
						if cerr != nil {
							results = append(results, pruneResult{
								Issue: it.ID, State: it.State, Workspace: it.Workspace, Merged: false,
								Action: fmt.Sprintf("failed: --force could not commit %d uncommitted file(s) for preservation, refusing to destroy them: %v", dirtyFiles+untracked, cerr),
							})
							continue
						}
						if committed {
							ahead, head = weaveUnmergedAhead(root, base, it)
						}
					}
					if ahead > 0 && head != "" {
						ref, perr := weavePreserveAbandonedTip(root, it.Workspace, it.ID, head)
						if perr != nil {
							results = append(results, pruneResult{
								Issue: it.ID, State: it.State, Workspace: it.Workspace, Merged: false,
								Action: fmt.Sprintf("failed: --force could not preserve %d unmerged commit(s) as %s, refusing to destroy them: %v", ahead, ref, perr),
							})
							continue
						}
						results = append(results, pruneResult{
							Issue: it.ID, State: it.State, Workspace: it.Workspace, Merged: false,
							Action: "preserved: " + ref,
						})
					}
				}
			}

			if it.Workspace != "" {
				if _, statErr := os.Stat(it.Workspace); statErr == nil {
					if rmErr := safeRemoveWorkspace(dir, it.Workspace); rmErr == nil {
						results = append(results, pruneResult{Issue: it.ID, State: it.State, Workspace: it.Workspace, Merged: merged, Action: "removed"})
						it.Workspace = "" // stale path must not linger in the queue
					} else {
						results = append(results, pruneResult{Issue: it.ID, State: it.State, Workspace: it.Workspace, Merged: merged, Action: "failed: " + rmErr.Error()})
					}
				} else {
					it.Workspace = "" // already gone on disk; drop the dead pointer
				}
			}
			// Best-effort: drop the branch from the user repo if `weave
			// pull` fetched it earlier. -d (never -D) refuses unmerged.
			if it.Branch != "" {
				if exec.Command(gitBin(), "-C", root, "branch", "-d", it.Branch).Run() == nil {
					results = append(results, pruneResult{Issue: it.ID, State: it.State, Branch: it.Branch, Merged: merged, Action: "branch_deleted"})
				}
			}
		}
		// Unclaimed workspace directories. Recomputed under the lock, like the
		// cache targets, so the sweep acts on the queue it just reconciled.
		orphans, oerr := weaveOrphanWorkspaceTargets(dir, q)
		if oerr != nil {
			return oerr
		}
		for _, o := range orphans {
			// --force lifts the private-work hold, never the live-run one: a
			// sibling clone a working run is building against is not litter.
			if o.Hold != "" && (!force || strings.HasPrefix(o.Hold, "sibling clone shared with live run")) {
				results = append(results, pruneResult{
					State: "orphaned-workspace", Workspace: o.Path,
					Action: "skipped: " + o.Hold,
				})
				continue
			}
			if rmErr := safeRemoveWorkspace(dir, o.Path); rmErr == nil {
				swept++
				results = append(results, pruneResult{
					State: "orphaned-workspace", Workspace: o.Path, Action: "removed",
				})
			} else {
				results = append(results, pruneResult{
					State: "orphaned-workspace", Workspace: o.Path, Action: "failed: " + rmErr.Error(),
				})
			}
		}
		cacheTargets, err := weaveManagedGOCacheSweepTargets(dir, q, stale)
		if err != nil {
			return err
		}
		for _, target := range cacheTargets {
			state := "orphaned-cache"
			if it := findWeaveItem(q, target.Issue); it != nil {
				state = it.State
			}
			if err := safeRemoveManagedGOCache(dir, target.Path); err != nil {
				results = append(results, pruneResult{Issue: target.Issue, State: state, Cache: target.Path, Action: "failed: " + err.Error()})
				continue
			}
			results = append(results, pruneResult{Issue: target.Issue, State: state, Cache: target.Path, Action: "cache_removed"})
		}
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave prune",
			weavecli.ExitGenericFail, lockErr))
	}
	// The pass above owns workspaces, orphans and caches; the ONE guarded
	// teardown reclaims what it does not name (log, socket, agent data, lock)
	// for every settled terminal run, and compacts the row.
	if q, err := loadWeaveQueue(dir); err == nil {
		for _, it := range q.Items {
			if it == nil || !weavePrunableForSweep(it.State, stale) {
				continue
			}
			for _, a := range weavePruneOwnedRun(dir, it.ID, filepath.Base(root)) {
				switch {
				case a.Err != "":
					results = append(results, pruneResult{Issue: it.ID, State: it.State, Action: "failed: " + a.Kind + " " + a.Target + ": " + a.Err})
				case a.Kind == "workspace" || a.Kind == "cache":
					// Already counted by the pass above when it got there first.
				default:
					results = append(results, pruneResult{Issue: it.ID, State: it.State, Action: a.Kind + "_removed"})
				}
			}
		}
	}

	// COUNT WHAT HAPPENED, NOT WHAT WAS CONSIDERED. A result row can also
	// describe work that was kept or a branch-only cleanup; neither is a
	// removed workspace. Keep the structured and human summaries on the same
	// definition so automation cannot overstate a destructive action.
	removed, cachesRemoved, kept := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Action == "removed":
			removed++
		case r.Action == "cache_removed":
			cachesRemoved++
		case strings.HasPrefix(r.Action, "skipped:"):
			kept++
		}
	}

	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave prune", map[string]any{
			"removed":       removed,
			"cache_removed": cachesRemoved,
			"results":       results,
		}))
	}

	// Human-readable output
	for _, r := range results {
		// An unclaimed directory has no run and no branch, so "run #0" and
		// "NOT merged" would both be inventions. Name it by what it is.
		orphan := r.State == "orphaned-workspace"
		label := fmt.Sprintf("run #%d (%s)", r.Issue, r.State)
		if orphan {
			label = "unclaimed workspace " + filepath.Base(r.Workspace)
		}
		switch {
		case r.Action == "removed":
			merged := ""
			if !r.Merged && !orphan {
				merged = " (NOT merged into " + base + ")"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: removed workspace%s\n", label, merged)
		case r.Action == "cache_removed":
			if r.Issue > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "  run #%d (%s): removed managed GOCACHE %s\n", r.Issue, r.State, r.Cache)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s: removed managed GOCACHE %s\n", r.State, r.Cache)
			}
		case r.Action == "branch_deleted":
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: deleted merged branch %s\n", label, r.Branch)
		case strings.HasPrefix(r.Action, "preserved:"):
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: work preserved at %s before removal\n", label,
				strings.TrimPrefix(r.Action, "preserved: "))
		case strings.HasPrefix(r.Action, "skipped:"):
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: KEPT — %s\n", label,
				strings.ReplaceAll(strings.TrimPrefix(r.Action, "skipped: "), "<id>", fmt.Sprint(r.Issue)))
		case strings.HasPrefix(r.Action, "failed:"):
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", label, r.Action)
		}
	}
	// COUNT WHAT HAPPENED, NOT WHAT WAS CONSIDERED.
	//
	// `swept` counts every item the sweep LOOKED at — including the ones it
	// refused to touch. Reporting that as "cleaned up N" told the operator their
	// workspaces were gone while two of them were quietly still there holding
	// unmerged work. A summary that overstates a destructive action is worse than
	// no summary: it is the number people check instead of the list.
	if home, err := os.UserHomeDir(); err == nil {
		if stray := weaveSweepEmptyStateRoots(home, dir, time.Now()); len(stray) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "weave prune: removed %d empty state root(s) holding no queue: %s\n",
				len(stray), strings.Join(stray, ", "))
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave prune: cleaned up %d item(s)", removed)
	if cachesRemoved > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "; removed %d managed GOCACHE director%s",
			cachesRemoved, map[bool]string{true: "y", false: "ies"}[cachesRemoved == 1])
	}
	if kept > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "; KEPT %d holding unmerged work (see above; --force to delete anyway)", kept)
	}
	fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

// runWeaveWait blocks until issue(s) reach a terminal state
// (submitted, no-op, failed, killed, done, abandoned). With --issue N, waits on
// one issue; with --all, waits until no working items remain.
// Times out after timeout (default 1h); on timeout, emits
// precondition_failed and returns ExitPrecondFail so the
// orchestrator can react.
func runWeaveWait(cmd *cobra.Command, issueID int64, all bool, timeout time.Duration, broker bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if !all && issueID <= 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave wait",
			weavecli.ExitInvalidArg, fmt.Errorf("provide --issue N or --all")))
	}
	if all && issueID > 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave wait",
			weavecli.ExitInvalidArg, fmt.Errorf("--issue and --all are mutually exclusive")))
	}
	if timeout <= 0 {
		timeout = time.Hour
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave wait",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)

	type readyItem struct {
		ID       int64  `json:"id"`
		State    string `json:"state"`
		ExitCode *int   `json:"exit_code,omitempty"`
		LogPath  string `json:"log_path,omitempty"`
	}
	deadline := time.Now().Add(timeout)
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	brokers := map[int64]*gateBroker{}

	for {
		q, err := loadWeaveQueue(dir)
		if err != nil {
			// queue.json may be momentarily absent if reset just ran;
			// surface as ok-empty rather than fail the wait.
			q = &weaveQueue{}
		}
		if issueID > 0 {
			it := findWeaveItem(q, issueID)
			if it == nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave wait",
					weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", issueID, weaveOtherActiveQueuesHintSuffix(dir))))
			}
			if broker && it.State == "working" {
				runGateBrokerForItem(brokers, cmd.ErrOrStderr(), dir, it, time.Now())
			}
			if isTerminalState(it.State) {
				return ec(emitOK(cmd.OutOrStdout(), mode, "weave wait", map[string]any{
					"ready": []readyItem{{ID: it.ID, State: it.State, ExitCode: it.ExitCode, LogPath: it.LogPath}},
				}))
			}
		} else {
			// --all: ready only when every non-terminal item is gone.
			// A finalizing run is still in flight until the conductor
			// records its terminal evidence, so it must keep the wait
			// blocked just like todo/allocated/working items do.
			var ready []readyItem
			pending := 0
			for _, it := range q.Items {
				if !isTerminalState(it.State) {
					pending++
					if broker && it.State == "working" {
						runGateBrokerForItem(brokers, cmd.ErrOrStderr(), dir, it, time.Now())
					}
					continue
				}
				ready = append(ready, readyItem{ID: it.ID, State: it.State, ExitCode: it.ExitCode, LogPath: it.LogPath})
			}
			if pending == 0 {
				return ec(emitOK(cmd.OutOrStdout(), mode, "weave wait", map[string]any{
					"ready": ready,
				}))
			}
		}
		if time.Now().After(deadline) {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave wait",
				weavecli.ExitPrecondFail, fmt.Errorf("timeout after %s", timeout)))
		}
		select {
		case <-ctx.Done():
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave wait",
				weavecli.ExitGenericFail, fmt.Errorf("cancelled")))
		case <-time.After(time.Second):
		}
	}
}

// weavePrintSalvageableFooter says that a terminal run left work behind.
//
// A `failed` row with a green diff on its branch looks exactly like a `failed`
// row with nothing on it. That silence is expensive: the obvious response to a
// failed run is to run it again, and re-running it throws away a completed
// feature and pays for it a second time.
func weavePrintSalvageableFooter(w io.Writer, ids []int64) {
	if len(ids) == 0 {
		return
	}
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "#%d", id)
	}
	fmt.Fprintf(w, "SALVAGEABLE: %s hold committed work not merged to the base branch — inspect with `weave status <id>`; pull submitted runs or salvage killed/failed runs (do NOT re-run: the diff is already there)\n", b.String())
}

// weaveNotPullableDetail explains a pull refusal for a run that pull does not
// merge but that IS holding committed work.
//
// Three things it must say, because their absence is what made the old silent
// skip dangerous: the run's REAL state (never anything that reads as empty),
// the COMMIT COUNT (proof the work is there), and the VERB THAT WOULD WORK. A
// refusal with no next step is how an operator ends up at `weave abandon`,
// which is the one command that turns "not merged yet" into "gone".
func weaveNotPullableDetail(it *weaveItem, base string, ahead int) string {
	next := fmt.Sprintf("inspect with `weave status %d`", it.ID)
	if weaveSalvageableState(it.State) {
		next = fmt.Sprintf("inspect the killed/failed run, then `weave salvage %d`", it.ID)
	}
	return fmt.Sprintf("run is %q and holds %d commit(s) not on %s — NOT empty; pull merges `submitted` runs, so %s. Do NOT abandon/prune: those commits exist only in this run's workspace clone",
		it.State, ahead, base, next)
}

// weavePruneHoldReason says, in one line, why a workspace was not deleted.
//
// "skipped" with no reason is how a safety check becomes a mystery, and a
// mystery is how people learn to pass --force by reflex.
// weaveEmptyRootGrace is how long a childless state root is left alone before
// prune will sweep it. The first queue writer creates the root, so a root
// created moments ago may belong to a weave STARTING in
// another repo — and other agents run weaves on this machine concurrently.
const weaveEmptyRootGrace = time.Hour

// weaveSweepEmptyStateRoots removes weave state roots that contain no files at
// any depth, and reports what it removed.
//
// Before weaveWorkspaceOwner landed, a weave command run INSIDE a workspace
// minted a fresh root keyed on that clone's path. Those forks never held a
// queue — the real one stayed in the root that dispatched the run — so they are
// precisely the roots with no files in them. One dev box had accumulated 237.
//
// Emptiness IS the test, and it is a strong one: a root with a queue, a log,
// or a single memory line has a file and is never touched. Two things are
// exempt regardless: `keep` (the root prune is operating on, which must survive
// even while momentarily empty) and any root younger than the grace window.
func weaveSweepEmptyStateRoots(home, keep string, now time.Time) []string {
	root := weaveStateRoot(home)
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	keep = filepath.Clean(keep)
	var swept []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if filepath.Clean(dir) == keep {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) < weaveEmptyRootGrace {
			continue
		}
		hasFile := false
		err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				hasFile = true
				return filepath.SkipAll
			}
			return nil
		})
		// An unreadable root is not a provably empty one. Leave it.
		if err != nil || hasFile {
			continue
		}
		if os.RemoveAll(dir) == nil {
			swept = append(swept, e.Name())
		}
	}
	return swept
}

func weavePruneHoldReason(ahead, dirtyFiles, untracked int) string {
	var parts []string
	if ahead > 0 {
		// This is the dangerous one. The commits exist ONLY here.
		parts = append(parts, fmt.Sprintf("%d unmerged commit(s) — inspect them, then `weave salvage %s` to keep them", ahead, "<id>"))
	}
	if n := dirtyFiles + untracked; n > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted file(s) — `weave abandon %s --force --yes` preserves them under a salvage ref before removal", n, "<id>"))
	}
	if len(parts) == 0 {
		return "holds unmerged work"
	}
	return strings.Join(parts, "; ") + " (--force preserves queue-backed work before removal)"
}

// weaveCrashedAutoCommitMessage labels work preserved from a run that did not
// exit cleanly.
//
// The message has to be honest in both directions. It must not read like a
// completed piece of work (the run failed; the state says so and the commit must
// not contradict it), and it must not read like garbage either — the work is
// often finished and correct, and the next person to look at this branch needs to
// know that a human decision is required rather than a rerun.
func weaveCrashedAutoCommitMessage(it *weaveItem, exitCode int, killReason string) string {
	how := fmt.Sprintf("exited %d", exitCode)
	if killReason != "" {
		how = "killed: " + killReason
	}
	return fmt.Sprintf("wip(weave #%d): work preserved from a run that %s\n\n"+
		"%s\n\n"+
		"The agent did not exit cleanly, so this run is NOT submitted and this\n"+
		"commit asserts nothing about whether the work is correct or complete.\n"+
		"It exists so the work is not lost: it was sitting uncommitted in a\n"+
		"workspace, and an uncommitted tree is one `weave prune` away from gone.\n\n"+
		"Inspect it, then `weave salvage %d` to run any configured deterministic gates and merge.",
		it.ID, how, strings.TrimSpace(it.Title), it.ID)
}
