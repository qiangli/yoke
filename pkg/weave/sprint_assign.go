package weave

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/lens"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

const sprintAssignDefaultIdleTimeout = 10 * time.Minute

type sprintAssignEvent struct {
	Story       string  `json:"story"`
	Agent       string  `json:"agent"`
	Seat        string  `json:"seat,omitempty"`
	WantedBand  int     `json:"wanted_band,omitempty"`
	Band        int     `json:"band,omitempty"`
	ChosenAgent string  `json:"chosen_agent,omitempty"`
	Expected    float64 `json:"expected"`
	Reason      string  `json:"reason"`
	Run         int64   `json:"run"`
	Manual      bool    `json:"manual,omitempty"`
}

type sprintAssignLaunch struct {
	Repo, Agent string
	Run         int64
	Env         map[string]string
	Argv        []string
	PTY         string
	MaxRuntime  time.Duration
	IdleTimeout time.Duration
}
type sprintAssignDeps struct {
	seed   func() (int64, error)
	launch func(*cobra.Command, sprintAssignLaunch) error
	record func(string, sprintAssignEvent) error
}

func sprintAssignDispatch(cmd *cobra.Command, task ladder.StoryTask, pool []ladder.Entrant, lines ladder.Lines, manual string, dry bool, repo string, deps sprintAssignDeps, idleTimeout ...time.Duration) error {
	pick := ladder.ScheduleStory(task, pool, lines)
	if manual != "" {
		found := false
		for _, e := range pool {
			if e.Agent == manual {
				found = true
				if !e.Free {
					return fmt.Errorf("agent %s is busy; refusing to double-book", manual)
				}
				standing, ok := e.Standings[task.Duty]
				if !ok {
					return fmt.Errorf("agent %s has no duty standing", manual)
				}
				pick = ladder.Pick{Agent: manual, Reason: "manual override", Expected: ladder.Expected(ladder.Rating{R: standing.R, RD: standing.RD}, ladder.Rating{R: task.Rating, RD: 50})}
				break
			}
		}
		if !found {
			return fmt.Errorf("agent %q is not an eligible fleet binding", manual)
		}
	}
	remaining := make([]ladder.Entrant, 0, len(pool))
	for _, e := range pool {
		if e.Agent != pick.Agent {
			remaining = append(remaining, e)
		}
	}
	runner := ladder.ScheduleStory(task, remaining, lines)
	fmt.Fprintf(cmd.OutOrStdout(), "pick=%s expected=%.3f reason=%s runner-up=%s (%.3f, %s)\n", pick.Agent, pick.Expected, pick.Reason, runner.Agent, runner.Expected, runner.Reason)
	if pick.Reason == "wait" {
		fmt.Fprintln(cmd.OutOrStdout(), "capacity wait: no agent available")
		return nil
	}
	if dry {
		return nil
	}
	run, err := deps.seed()
	if err != nil {
		return err
	}
	// Persist attribution before the worker can produce a commit. A failed launch
	// leaves a linked, attributed queued run for explicit recovery.
	if err = deps.record("assign", sprintAssignEvent{Story: task.ID, Agent: pick.Agent, Expected: pick.Expected, Reason: pick.Reason, Run: run, Manual: manual != ""}); err != nil {
		return err
	}
	for _, e := range pool {
		if e.Agent == pick.Agent && e.Band < task.Band {
			if err = deps.record("fallback", sprintAssignEvent{Story: task.ID, Agent: pick.Agent, ChosenAgent: pick.Agent, Seat: "worker", WantedBand: task.Band, Band: e.Band}); err != nil {
				return err
			}
			break
		}
	}
	launchIdle := sprintAssignDefaultIdleTimeout
	if len(idleTimeout) > 0 && idleTimeout[0] > 0 {
		launchIdle = idleTimeout[0]
	}
	return deps.launch(cmd, sprintAssignLaunch{Repo: repo, Agent: pick.Agent, Run: run, Env: map[string]string{"BASHY_AGENT": pick.Agent}, IdleTimeout: launchIdle})
}

func sprintReviewDispatch(cmd *cobra.Command, story string, author ladder.DutyStanding, band int, vendor string, pool []ladder.Entrant, dry bool, record func(string, sprintAssignEvent) error) error {
	_ = author
	lower := make([]ladder.Entrant, 0, len(pool))
	for _, e := range pool {
		if e.Band <= band {
			lower = append(lower, e)
		}
	}
	choices := seatPanel(lower, 1, vendor)
	if len(choices) == 0 {
		choices = seatPanel(pool, 1, vendor)
	}
	pick := ladder.Pick{Reason: "wait"}
	if len(choices) > 0 {
		pick = ladder.Pick{Agent: choices[0].Agent, Reason: fmt.Sprintf("cascade:L%d", choices[0].Band)}
	}
	if pick.Agent == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "capacity wait: no agent available")
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "reviewer=%s reason=%s\n", pick.Agent, pick.Reason)
	if dry {
		return nil
	}
	for _, e := range pool {
		if e.Agent == pick.Agent && e.Band < band {
			if err := record("fallback", sprintAssignEvent{Story: story, Agent: pick.Agent, ChosenAgent: pick.Agent, Seat: "review", WantedBand: band, Band: e.Band}); err != nil {
				return err
			}
			break
		}
	}
	return record("review-assign", sprintAssignEvent{Story: story, Agent: pick.Agent, Expected: pick.Expected, Reason: pick.Reason})
}

func sprintAssignBusy(queues []*weaveQueue, names []string) bool {
	for _, q := range queues {
		for _, name := range names {
			if weaveAgentWorkingOn(q, name, 0) != nil {
				return true
			}
		}
	}
	return false
}

// Sprint cards do not expose dispatch exclusions. Environment defaults are
// additive with command flags, so a manual pick cannot bypass host policy.
type sprintAssignExclusions struct {
	tools, agents []string
	report        func(agent, reason string)
}

func sprintAssignExcludeOptions(tools, agents []string) (sprintAssignExclusions, error) {
	x := sprintAssignExclusions{tools: append([]string(nil), tools...), agents: append([]string(nil), agents...)}
	for _, entry := range strings.Split(os.Getenv("BASHY_SPRINT_DISPATCH_EXCLUDE"), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		kind, name, ok := strings.Cut(entry, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return x, fmt.Errorf("invalid dispatch exclusion %q: want tool:TOOL or agent:NAME", entry)
		}
		switch strings.TrimSpace(kind) {
		case "tool":
			x.tools = append(x.tools, name)
		case "agent":
			x.agents = append(x.agents, name)
		default:
			return x, fmt.Errorf("invalid dispatch exclusion %q: want tool:TOOL or agent:NAME", entry)
		}
	}
	return x, nil
}

// Only cached probe evidence is consulted: dispatch never invokes a probe.
func sprintAssignPool(root string, events []ladder.Event, now time.Time, exclusions ...sprintAssignExclusions) ([]ladder.Entrant, ladder.Lines, error) {
	return seatPool(root, events, now, exclusions...)
}

func sprintAssignReadEvents() ([]ladder.Event, error) {
	path := ladder.DefaultStorePath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	store, err := ladder.OpenStore(path)
	if err != nil {
		return nil, err
	}
	return store.Read()
}

func sprintAssignRecord(cmd *cobra.Command, id int64, kind string, event sprintAssignEvent) error {
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		if err := authorizeSprintLeaseToken(cmd, s, kind); err != nil {
			return err
		}
		b, err := json.Marshal(event)
		if err != nil {
			return err
		}
		weaveStoryAppend(s, weaveConductorName(""), kind, string(b))
		return nil
	})
}

// sprintAssignCollaborationPreamble is appended to every fleet worker brief.
// Workers share the host with other lanes; before this, only managers and
// conductors ever saw the collaboration rules, so a worker that met a diff it
// did not make could only guess. The full procedure lives in the collaborate
// skill; this is the pointer plus the rules that must hold without reading it.
const sprintAssignCollaborationPreamble = `
Collaboration (other agents share this host; ` + "`bashy skill show collaborate`" + ` is the procedure):
- Watch your mail: ` + "`bashy inbox`" + ` every few minutes; answer blocking requests first.
- A change you did not make is someone's work: never stash, revert, reset, ` + "`git add -A`" + ` or commit it.
  Find the owner with ` + "`bashy claim who [PATH]`" + ` and ask with ` + "`bashy mb send <holder> \"...\"`" + `.
- Claim before exclusive use (host, ollama, podman, GPU, account): ` + "`bashy claim NAME --intent TEXT`" + `;
  on refusal ` + "`bashy claim request NAME -m TEXT`" + ` or wait — never --force without contact.
- Announce a freeze, a build cut or a pin bump: ` + "`bashy mb post --topic announce \"...\"`" + `.
`

func sprintAssignSeed(cmd *cobra.Command, sprint int64, root string, story *issue.Issue, points, band int, scriptBody ...*weaveScriptAssignment) (int64, error) {
	if todopkg.IsClosed(story.Status) || story.Weave != 0 {
		return 0, fmt.Errorf("story %s is closed or already linked to run #%d", story.ID, story.Weave)
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return 0, err
	}
	body := story.Body
	if len(story.Refs) > 0 {
		body += fmt.Sprintf("\n\nThis issue also touches: %v\n", story.Refs)
	}
	body += fmt.Sprintf("\n\nDelivery commit trailers:\nSprint: #%d\nStory: #%d\nStory-ID: %s\n", sprint, story.Seq, story.ID)
	body += sprintAssignCollaborationPreamble
	if len(scriptBody) > 0 {
		body = scriptBody[0].Body
	}
	var run *weaveItem
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		for _, it := range q.Items {
			if it.Register == story.ID && !isTerminalState(it.State) {
				return fmt.Errorf("story already has run #%d", it.ID)
			}
		}
		run = &weaveItem{ID: q.NextID, Title: story.Title, Body: body, Priority: story.Priority, Stage: story.Stage, Register: story.ID, State: "todo", Created: time.Now().UTC(), Points: points, Band: band, Judge: weaveJudgeNone}
		if len(scriptBody) > 0 {
			run.ScriptAssignment = scriptBody[0]
		}
		q.NextID++
		q.Root = root
		q.Items = append(q.Items, run)
		return nil
	})
	if err != nil {
		return 0, err
	}
	story.Weave = run.ID
	story.Status = todopkg.StatusAssigned
	if _, err = todopkg.RepoStore(root).Save(story); err != nil {
		return 0, err
	}
	board, err := sprintStoreDir()
	if err != nil {
		return 0, err
	}
	err = withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, sprint)
		if s == nil {
			return fmt.Errorf("sprint not found")
		}
		s.Runs = append(s.Runs, sprintRun{Repo: filepath.Base(root), Queue: filepath.Base(dir), ID: run.ID, Born: run.Created})
		return nil
	})
	return run.ID, err
}

// The legacy in-process start API reads cwd and environment. Serialize and
// restore that adapter's process state; the scheduling layer passes explicit
// requests, allowing tests to replace launch without executing any binary.
var sprintAssignLaunchMu sync.Mutex
var sprintAssignLaunchWorker = func(cmd *cobra.Command, r sprintAssignLaunch) error {
	sprintAssignLaunchMu.Lock()
	defer sprintAssignLaunchMu.Unlock()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err = os.Chdir(r.Repo); err != nil {
		return err
	}
	defer os.Chdir(cwd)
	for key, value := range r.Env {
		old, had := os.LookupEnv(key)
		if err = os.Setenv(key, value); err != nil {
			return err
		}
		defer func(k, v string, exists bool) {
			if exists {
				_ = os.Setenv(k, v)
			} else {
				_ = os.Unsetenv(k)
			}
		}(key, old, had)
	}
	argv, pty := r.Argv, r.PTY
	if len(argv) == 0 {
		argv = []string{r.Agent}
	}
	if pty == "" {
		pty = "never"
	}
	idle := r.IdleTimeout
	if idle <= 0 {
		idle = sprintAssignDefaultIdleTimeout
	}
	return runWeaveStart(cmd, r.Run, "", argv, weaveStartOptions{pty: pty, maxRuntime: r.MaxRuntime, idleTimeout: idle}, &weaveOutputFlags{})
}

func newSprintAssignCmd() *cobra.Command { return sprintAssignCommand(false) }
func newSprintReviewCmd() *cobra.Command { return sprintAssignCommand(true) }

func sprintAssignCommand(review bool) *cobra.Command {
	var repo, manual, author string
	var scriptFile, scriptRunner string
	var excludeTools, excludeAgents []string
	var points, band, authorBand int
	var dry bool
	var idleTimeout time.Duration
	name := "assign"
	if review {
		name = "review"
	}
	cmd := &cobra.Command{Use: name + " <sprint> <story>", Short: "Schedule a fleet worker through the band ladder", Args: cobra.ExactArgs(2)}
	cmd.Flags().StringVar(&repo, "repo", "", "repository holding the story")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print selection without mutation or launch")
	cmd.Flags().StringArrayVar(&excludeTools, "exclude-tool", nil, "exclude tool (repeatable; adds to BASHY_SPRINT_DISPATCH_EXCLUDE tool:TOOL,agent:NAME)")
	cmd.Flags().StringArrayVar(&excludeAgents, "exclude-agent", nil, "exclude agent binding (repeatable; adds to environment exclusions)")
	if review {
		cmd.Flags().StringVar(&author, "author", "", "author tool:model")
		cmd.Flags().IntVar(&authorBand, "author-band", 0, "author band (3, 4 or 5)")
	} else {
		cmd.Flags().StringVar(&scriptFile, "script-task", "", "validated JSON task file; exact body, local execution, no fleet/model selection")
		cmd.Flags().StringVar(&scriptRunner, "script-runner", "", "absolute trusted Yoke binary for --script-task")
		cmd.Flags().IntVar(&points, "points", 3, "story points (1, 2, 3, 5 or 8; defaults to 3)")
		cmd.Flags().IntVar(&band, "band", 3, "owning band (3, 4 or 5)")
		cmd.Flags().StringVar(&manual, "agent", "", "manual assignment override, recorded in the thread")
		cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 0, "output-idle watchdog threshold; 0 keeps default (10m)")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		exclusions, err := sprintAssignExcludeOptions(excludeTools, excludeAgents)
		if err != nil {
			return err
		}
		exclusions.report = func(agent, reason string) {
			fmt.Fprintf(cmd.OutOrStdout(), "excluded=%s reason=%s\n", agent, reason)
		}
		if !review && (strings.TrimSpace(repo) == "" || band < 3 || band > 5) {
			return fmt.Errorf("--repo PATH and --band 3|4|5 required")
		}
		if review && (authorBand < 3 || authorBand > 5 || !strings.Contains(author, ":")) {
			return fmt.Errorf("--author tool:model and --author-band 3|4|5 required")
		}
		flags := weaveOutputFlags{}
		id, err := sprintArg(cmd, flags.mode(), "sprint "+name, args[0])
		if err != nil {
			return err
		}
		dir, err := sprintStoreDir()
		if err != nil {
			return err
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return err
		}
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		root, story, err := resolveSprintStoryFor(s, repo, args[1])
		if err != nil {
			return err
		}
		effIdleTimeout := sprintAssignDefaultIdleTimeout
		if idleTimeout > 0 {
			effIdleTimeout = idleTimeout
		}
		if scriptFile != "" || scriptRunner != "" {
			if manual != "" || len(excludeTools) > 0 || len(excludeAgents) > 0 {
				return fmt.Errorf("script assignment cannot combine fleet selection flags")
			}
			return sprintAssignScript(cmd, id, root, story, points, band, scriptFile, scriptRunner, dry, effIdleTimeout)
		}
		events, err := sprintAssignReadEvents()
		if err != nil {
			return err
		}
		pool, lines, err := sprintAssignPool(root, events, time.Now(), exclusions)
		if err != nil {
			return err
		}
		record := func(kind string, e sprintAssignEvent) error { return sprintAssignRecord(cmd, id, kind, e) }
		if !dry {
			if err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
				current := findWeaveStory(q, id)
				if current == nil {
					return fmt.Errorf("sprint #%d not found", id)
				}
				return authorizeSprintLeaseToken(cmd, current, name)
			}); err != nil {
				return err
			}
		}
		if review {
			authorKey := strings.TrimSpace(author)
			vendor := strings.SplitN(authorKey, ":", 2)[0]
			if a, tool, _, bindErr := fleetCatalog().Binding(authorKey); bindErr == nil {
				authorKey, vendor = a.MatrixKey(), tool.Name
			}
			rep := ladder.Replay(events, ladder.SeasonOf(time.Now()))
			rec := rep.Agents[authorKey]
			standing := ladder.DutyStanding{}
			if rec != nil {
				standing = rec.Standings[ladder.DutyCode]
			}
			filtered := pool[:0]
			for _, e := range pool {
				b, _, _, bindErr := fleetCatalog().Binding(e.Agent)
				if bindErr == nil && b.MatrixKey() != authorKey {
					filtered = append(filtered, e)
				}
			}
			return sprintReviewDispatch(cmd, story.ID, standing, authorBand, vendor, filtered, dry, record)
		}
		rating, err := ladder.StoryInitialRating(ladder.Points(points))
		if err != nil {
			return err
		}
		if manual != "" {
			a, _, _, err := fleetCatalog().Binding(manual)
			if err != nil {
				return err
			}
			for _, e := range pool {
				b, _, _, _ := fleetCatalog().Binding(e.Agent)
				if b.MatrixKey() == a.MatrixKey() {
					manual = e.Agent
					break
				}
			}
		}
		task := ladder.StoryTask{ID: story.ID, Duty: ladder.DutyCode, Band: band, Points: ladder.Points(points), Rating: rating}
		return sprintAssignDispatch(cmd, task, pool, lines, manual, dry, root, sprintAssignDeps{seed: func() (int64, error) { return sprintAssignSeed(cmd, id, root, story, points, band) }, record: record, launch: func(cmd *cobra.Command, r sprintAssignLaunch) error {
			// Re-read availability after seeding: do not use clones to bypass a busy base.
			fresh, _, err := sprintAssignPool(root, events, time.Now(), exclusions)
			if err != nil {
				return err
			}
			free := false
			for _, e := range fresh {
				if e.Agent == r.Agent {
					free = e.Free
				}
			}
			if !free {
				return fmt.Errorf("agent %s became busy or ineligible; run #%d remains queued", r.Agent, r.Run)
			}
			story.Assignee = r.Agent
			if _, err = todopkg.RepoStore(root).Save(story); err != nil {
				return err
			}
			return sprintAssignLaunchWorker(cmd, r)
		}}, effIdleTimeout)
	}
	return cmd
}

// sprintAssignScript uses the normal seed/link and owned process lifecycle. It
// does not impersonate a model agent or rewrite a queue after dispatch.
func sprintAssignScript(cmd *cobra.Command, id int64, root string, story *issue.Issue, points, band int, file, runner string, dry bool, idleTimeout ...time.Duration) error {
	if file == "" || !filepath.IsAbs(runner) {
		return fmt.Errorf("--script-task FILE and --script-runner ABSOLUTE_YOKE required")
	}
	info, err := os.Stat(runner)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("script runner must be executable")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return err
	}
	task, err := lens.DecodeScriptTask(body)
	if err != nil {
		return err
	}
	if _, err = ladder.StoryInitialRating(ladder.Points(points)); err != nil {
		return err
	}
	if todopkg.IsClosed(story.Status) || story.Weave != 0 {
		return fmt.Errorf("story is closed or already linked")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "script=%s help=script no_input=true runner=%s\n", task.Artifact.Digest, runner)
	if dry {
		return nil
	}
	// Authorize before seeding; recording below rechecks the lease.
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	if err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint not found")
		}
		return authorizeSprintLeaseToken(cmd, s, "assign")
	}); err != nil {
		return err
	}
	run, err := sprintAssignSeed(cmd, id, root, story, points, band, &weaveScriptAssignment{Body: string(body), Runner: runner, Digest: task.Artifact.Digest})
	if err != nil {
		return err
	}
	if err = sprintAssignRecord(cmd, id, "assign-script", sprintAssignEvent{Story: story.ID, Seat: "script", Run: run, Reason: task.Artifact.Digest}); err != nil {
		return err
	}
	launchIdle := sprintAssignDefaultIdleTimeout
	if len(idleTimeout) > 0 && idleTimeout[0] > 0 {
		launchIdle = idleTimeout[0]
	}
	return sprintAssignLaunchWorker(cmd, sprintAssignLaunch{Repo: root, Run: run, Argv: append([]string{runner}, task.WorkerArgs()...), PTY: "always", MaxRuntime: time.Minute, IdleTimeout: launchIdle})
}

// weaveScriptAssignment freezes the trusted local process contract separately
// from story ownership. Such a process supplies evidence, never claims/submits.
type weaveScriptAssignment struct {
	Body   string `json:"body"`
	Runner string `json:"runner"`
	Digest string `json:"digest"`
}
