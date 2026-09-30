package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	yokegit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

type sprintGradeEvent struct {
	AllowTestChange []string      `json:"allow_test_change,omitempty"`
	Run             string        `json:"run"`
	Generation      string        `json:"generation"`
	Commit          string        `json:"commit"`
	Base            string        `json:"base"`
	Gate            string        `json:"gate"`
	GateExit        int           `json:"gate_exit"`
	Tamper          bool          `json:"tamper"`
	Verdict         string        `json:"verdict"`
	Duration        time.Duration `json:"duration"`
	Checkout        string        `json:"checkout"`
	Output          string        `json:"output,omitempty"`
	Reviewer        string        `json:"reviewer,omitempty"`
	Agent           string        `json:"agent,omitempty"`
	MergeCommit     string        `json:"merge_commit,omitempty"`
}

// Credentials stay in memory and are never placed in the checkout or event.
func sprintGradeSource(it *weaveItem, queue string) (string, transport.AuthMethod, error) {
	if sprintGradeBranch(it) == "" {
		return "", nil, fmt.Errorf("run has no branch")
	}
	if err := plumbing.NewBranchReferenceName(sprintGradeBranch(it)).Validate(); err != nil {
		return "", nil, err
	}
	if it.BoothForkURL == "" {
		if it.ArenaSprint != 0 {
			return "", nil, fmt.Errorf("booth has no fork")
		}
		if it.Workspace == "" {
			return "", nil, fmt.Errorf("run has no workspace")
		}
		return it.Workspace, nil, nil
	}
	u, err := url.Parse(it.BoothForkURL)
	if err != nil {
		return "", nil, fmt.Errorf("invalid fork URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return it.BoothForkURL, nil, nil
	}
	data, err := os.ReadFile(boothCredentialPath(queue, it.ID))
	if err != nil {
		return "", nil, fmt.Errorf("read booth credentials: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		raw := strings.TrimSpace(line)
		// credential-store may percent-encode the port separator in the
		// authority. Decode only the host, never the user/password or path.
		if scheme, rest, ok := strings.Cut(raw, "://"); ok {
			end := strings.IndexAny(rest, "/?#")
			if end < 0 {
				end = len(rest)
			}
			start := strings.LastIndex(rest[:end], "@") + 1
			host, e := url.PathUnescape(rest[start:end])
			if e != nil || strings.ContainsAny(host, "/?#@") {
				continue
			}
			raw = scheme + "://" + rest[:start] + host + rest[end:]
		}
		c, e := url.Parse(raw)
		if e != nil || c.User == nil || c.Scheme != u.Scheme || c.Host != u.Host {
			continue
		}
		// Git credential-store defaults to host scope and may discard the
		// path after clone. Only accept that form for the recorded booth's
		// own fork; never derive a username from the run number.
		if c.Path != u.Path && !(c.Path == "" && strings.HasPrefix(u.Path, "/"+it.BoothUser+"/")) {
			continue
		}
		password, ok := c.User.Password()
		if ok && c.User.Username() == it.BoothUser {
			u.User = nil
			return u.String(), &http.BasicAuth{Username: c.User.Username(), Password: password}, nil
		}
	}
	return "", nil, fmt.Errorf("matching booth credentials not found")
}

func sprintGradeFetch(ctx context.Context, r *gogit.Repository, source, branch string, auth transport.AuthMethod) (plumbing.Hash, error) {
	ref := plumbing.ReferenceName("refs/sprint-grade/attempt")
	remote := gogit.NewRemote(r.Storer, &config.RemoteConfig{Name: "grade", URLs: []string{source}})
	err := remote.FetchContext(ctx, &gogit.FetchOptions{Auth: auth, Tags: gogit.NoTags, RefSpecs: []config.RefSpec{config.RefSpec("+" + plumbing.NewBranchReferenceName(branch).String() + ":" + ref.String())}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return plumbing.ZeroHash, fmt.Errorf("fetch attempt failed: %w", err)
	}
	h, err := r.Reference(ref, true)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return h.Hash(), nil
}

// Patterns without slashes match basenames at every depth. ** matches any
// number of path components, including zero (tests/** includes nested tests).
func sprintGradeMatch(pattern, name string) (bool, error) {
	if !strings.Contains(pattern, "/") {
		return path.Match(pattern, path.Base(name))
	}
	p, n := strings.Split(pattern, "/"), strings.Split(name, "/")
	var match func(int, int) (bool, error)
	match = func(i, j int) (bool, error) {
		if i == len(p) {
			return j == len(n), nil
		}
		if p[i] == "**" {
			for k := j; k <= len(n); k++ {
				ok, e := match(i+1, k)
				if e != nil || ok {
					return ok, e
				}
			}
			return false, nil
		}
		_, e := path.Match(p[i], "")
		if e != nil {
			return false, e
		}
		if j == len(n) {
			return false, nil
		}
		ok, e := path.Match(p[i], n[j])
		if e != nil || !ok {
			return false, e
		}
		return match(i+1, j+1)
	}
	return match(0, 0)
}

func sprintGradeTamper(base, attempt *object.Commit, globs []string, allow ...string) (bool, error) {
	if len(globs) == 0 {
		globs = []string{"*_test.go", "test_*.py", "*_test.py", "tests/**", "grader/**"}
	}
	for _, g := range append(append([]string{}, globs...), allow...) {
		for _, part := range strings.Split(g, "/") {
			if part != "**" {
				if _, err := path.Match(part, ""); err != nil {
					return false, err
				}
			}
		}
	}
	bt, err := base.Tree()
	if err != nil {
		return false, err
	}
	at, err := attempt.Tree()
	if err != nil {
		return false, err
	}
	tamper := false
	err = bt.Files().ForEach(func(f *object.File) error {
		for _, g := range allow {
			matched, e := sprintGradeMatch(g, f.Name)
			if e != nil {
				return e
			}
			if matched {
				return nil
			}
		}
		for _, g := range globs {
			matched, e := sprintGradeMatch(g, f.Name)
			if e != nil {
				return e
			}
			if !matched {
				continue
			}
			other, e := at.FindEntry(f.Name)
			if e != nil {
				if errors.Is(e, object.ErrEntryNotFound) || errors.Is(e, object.ErrDirectoryNotFound) {
					tamper = true
					return nil
				}
				return e
			}
			if other.Hash != f.Hash || other.Mode != f.Mode {
				tamper = true
			}
			break
		}
		return nil
	})
	return tamper, err
}

func sprintGradeAttempt(ctx context.Context, sprint int64, run string, it *weaveItem, queue, base, gate string, globs []string, timeout time.Duration, allow ...string) (sprintGradeEvent, error) {
	ev := sprintGradeEvent{AllowTestChange: append([]string(nil), allow...), Run: run, Base: base, Gate: gate, GateExit: -1, Verdict: "fail"}
	if strings.TrimSpace(gate) == "" || timeout <= 0 {
		return ev, fmt.Errorf("nonempty --gate and positive --timeout required")
	}
	start := time.Now()
	source, auth, err := sprintGradeSource(it, queue)
	if err != nil {
		return ev, err
	}
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return ev, err
		}
		home = filepath.Join(home, ".bashy")
	}
	// Hash the run label so repository names cannot escape the grading root.
	parent := filepath.Join(home, "sprint", "grade", strconv.FormatInt(sprint, 10), sprintLeaseTokenHash(run)[:16])
	if err = os.MkdirAll(parent, 0700); err != nil {
		return ev, err
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return ev, err
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		return ev, err
	}
	if it.Workspace != "" {
		ws, e := filepath.EvalSymlinks(it.Workspace)
		if e != nil {
			return ev, e
		}
		ws, e = filepath.Abs(ws)
		if e != nil {
			return ev, e
		}
		rel, e := filepath.Rel(ws, parent)
		if e != nil {
			return ev, e
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ev, fmt.Errorf("grade directory must be outside the workspace")
		}
	}
	ev.Checkout, err = os.MkdirTemp(parent, "attempt-")
	if err != nil {
		return ev, err
	}
	r, err := gogit.PlainInit(ev.Checkout, false)
	if err != nil {
		return ev, err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hash, err := sprintGradeFetch(fetchCtx, r, source, sprintGradeBranch(it), auth)
	if err != nil {
		return ev, err
	}
	ev.Commit = hash.String()
	bc, err := r.CommitObject(plumbing.NewHash(base))
	if err != nil {
		return ev, fmt.Errorf("arena base unavailable: %w", err)
	}
	ac, err := r.CommitObject(hash)
	if err != nil {
		return ev, err
	}
	ancestor, err := bc.IsAncestor(ac)
	if err != nil {
		return ev, err
	}
	if !ancestor {
		return ev, fmt.Errorf("attempt does not descend from the arena base")
	}
	ev.Tamper, err = sprintGradeTamper(bc, ac, globs, allow...)
	if err != nil {
		return ev, err
	}
	w, err := r.Worktree()
	if err != nil {
		return ev, err
	}
	if err = w.Checkout(&gogit.CheckoutOptions{Hash: hash}); err != nil {
		return ev, err
	}
	gateCtx, stop := context.WithTimeout(ctx, timeout)
	defer stop()
	command := exec.CommandContext(gateCtx, "bash", "-c", gate)
	command.Dir = ev.Checkout
	command.Env = boothScrubEnv(os.Environ(), []string{"BASH_ENV", "ENV"})
	command.WaitDelay = time.Second
	out, gateErr := command.CombinedOutput()
	ev.Output = string(out)
	if gateErr == nil {
		ev.GateExit = 0
		ev.Verdict = "pass"
	} else {
		var exit *exec.ExitError
		if errors.As(gateErr, &exit) {
			ev.GateExit = exit.ExitCode()
		}
	}
	if gateCtx.Err() != nil {
		ev.GateExit = -1
		ev.Verdict = "fail"
	}
	if ev.Tamper {
		ev.Verdict = "tamper"
	}
	ev.Duration = time.Since(start)
	return ev, nil
}

func sprintGradeLatest(s *weaveStory, run, generation string) (sprintGradeEvent, error) {
	for i := len(s.Thread) - 1; i >= 0; i-- {
		c := s.Thread[i]
		if c.Kind != "grade" {
			continue
		}
		var ev sprintGradeEvent
		if err := json.Unmarshal([]byte(c.Body), &ev); err != nil {
			return ev, fmt.Errorf("invalid grade event: %w", err)
		}
		if ev.Run != run || ev.Generation != generation {
			continue
		}
		if ev.Verdict != "pass" || ev.Tamper || ev.GateExit != 0 || len(ev.Commit) != 40 {
			return ev, fmt.Errorf("latest grade did not pass without tampering")
		}
		return ev, nil
	}
	return sprintGradeEvent{}, fmt.Errorf("run requires a passing grade before merge")
}

func sprintGradeDominance(reviewer, author string, events []ladder.Event) error {
	for _, p := range []*string{&reviewer, &author} {
		if a, _, _, err := fleetCatalog().Binding(*p); err == nil {
			*p = a.MatrixKey()
		}
	}
	rep := ladder.Replay(events, ladder.SeasonOf(time.Now()))
	rr, ar := rep.Agents[reviewer], rep.Agents[author]
	if rr != nil && ar != nil {
		rs, rok := rr.Standings[ladder.DutyCode]
		as, aok := ar.Standings[ladder.DutyCode]
		if rok && aok && ladder.DominanceOK(rs, as) {
			return nil
		}
	}
	return fmt.Errorf("escalate: reviewer does not dominate author's code rating")
}

func sprintGradeMerge(ctx context.Context, into, source, branch string, auth transport.AuthMethod, ev sprintGradeEvent, agent string) (string, error) {
	if ev.Verdict != "pass" || ev.Tamper || ev.GateExit != 0 {
		return "", fmt.Errorf("merge requires a passing grade without tampering")
	}
	if agent == "" || strings.ContainsAny(agent, "\r\n") {
		return "", fmt.Errorf("attempt agent is missing or invalid")
	}
	r, err := gogit.PlainOpen(into)
	if err != nil {
		return "", err
	}
	w, err := r.Worktree()
	if err != nil {
		return "", err
	}
	status, err := w.Status()
	if err != nil {
		return "", err
	}
	if !status.IsClean() {
		return "", fmt.Errorf("merge target is not clean")
	}
	hash, err := sprintGradeFetch(ctx, r, source, branch, auth)
	if err != nil {
		return "", err
	}
	if hash.String() != ev.Commit {
		return "", fmt.Errorf("attempt changed since grade; grade the new commit first")
	}
	c, err := r.CommitObject(hash)
	if err != nil {
		return "", err
	}
	// Copy the delivery trailers verbatim; retaining the attempt as a parent
	// preserves every original commit, signature, author and trailer.
	trailers := map[string]string{}
	for _, line := range strings.Split(c.Message, "\n") {
		key, _, ok := strings.Cut(line, ":")
		if ok && (key == "Sprint" || key == "Story" || key == "Story-ID") {
			if trailers[key] != "" {
				return "", fmt.Errorf("duplicate %s trailer", key)
			}
			trailers[key] = line
		}
	}
	message := "Merge graded attempt " + ev.Run + "\n\n"
	for _, key := range []string{"Sprint", "Story", "Story-ID"} {
		if trailers[key] == "" {
			return "", fmt.Errorf("attempt lacks %s trailer", key)
		}
		message += trailers[key] + "\n"
	}
	message += "Agent: " + agent + "\n"
	if _, err = yokegit.Merge(yokegit.MergeOptions{RepoPath: into, Ref: hash.String(), NoFF: true, Message: message}); err != nil {
		return "", err
	}
	head, err := r.Head()
	if err != nil {
		return "", err
	}
	return head.Hash().String(), nil
}

func newSprintGradeCommands() []*cobra.Command {
	var commands []*cobra.Command
	for _, verb := range []string{"grade", "merge"} {
		var run, gate, into, reviewer string
		var globs, allow []string
		var jsonOut bool
		var timeout time.Duration
		cmd := &cobra.Command{Use: verb + " N --run REPO#ID", Short: "Grade outside the booth or merge a graded attempt", Args: cobra.ExactArgs(1)}
		cmd.Flags().StringVar(&run, "run", "", "linked run REPO#ID")
		cmd.Flags().BoolVar(&jsonOut, "json", false, "print the event as JSON")
		cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "gate and fetch timeout")
		if verb == "grade" {
			cmd.Flags().StringVar(&gate, "gate", "", "gate command (bash -c)")
			cmd.Flags().StringArrayVar(&globs, "tests-glob", nil, "protected test glob (repeatable)")
			cmd.Flags().StringArrayVar(&allow, "allow-test-change", nil, "accepted test change glob (repeatable; recorded in grade)")
		} else {
			cmd.Flags().StringVar(&into, "into", "", "real repository checkout")
			cmd.Flags().StringVar(&reviewer, "reviewer", "", "reviewer subject to code dominance")
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			id, err := sprintArg(cmd, (&weaveOutputFlags{}).mode(), "sprint "+verb, args[0])
			if err != nil {
				return err
			}
			dir, err := sprintStoreDir()
			if err != nil {
				return err
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be positive")
			}
			if verb == "merge" && into == "" {
				return fmt.Errorf("--into required")
			}
			// Hold the card lock across the operation: grade ordering and the lease
			// cannot change between authorization, selection, mutation and recording.
			var ev sprintGradeEvent
			err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
				s := findWeaveStory(q, id)
				if s == nil {
					return fmt.Errorf("sprint not found")
				}
				if e := authorizeSprintLeaseToken(cmd, s, verb); e != nil {
					return e
				}
				if verb == "merge" {
					if e := sprintGradeManager(s); e != nil {
						return e
					}
				}
				var link *sprintRun
				for i := range s.Runs {
					r := &s.Runs[i]
					if fmt.Sprintf("%s#%d", r.Repo, r.ID) == run {
						if link != nil {
							return fmt.Errorf("ambiguous run")
						}
						link = r
					}
				}
				if link == nil {
					return fmt.Errorf("--run must name a linked REPO#ID")
				}
				queue, e := weaveQueueDirForSprintRun(*link)
				if e != nil {
					return e
				}
				runs, e := loadWeaveQueue(queue)
				if e != nil {
					return e
				}
				it := findWeaveItem(runs, link.ID)
				if it == nil || (!link.Born.IsZero() && !link.Born.Equal(it.Created)) {
					return fmt.Errorf("linked run generation is missing")
				}
				generation := filepath.Base(queue) + ":" + it.Created.UTC().Format(time.RFC3339Nano)
				base := it.BaseSHA
				if s.Arena != nil {
					base = ""
					for _, r := range s.Arena.Repos {
						if r.Repo == link.Repo {
							base = r.Base
						}
					}
				}
				if len(base) != 40 {
					return fmt.Errorf("run has no pinned arena base")
				}
				if verb == "grade" {
					ev, e = sprintGradeAttempt(cmd.Context(), id, run, it, queue, base, gate, globs, timeout, allow...)
					if e != nil {
						return e
					}
					ev.Generation = generation
				} else {
					ev, e = sprintGradeLatest(s, run, generation)
					if e != nil {
						return e
					}
					if ev.Base != base {
						return fmt.Errorf("arena base changed since grade")
					}
					agent, ok := weaveCapabilityAgent(it)
					if !ok {
						agent = it.Owner
					}
					if agent == "" {
						return fmt.Errorf("attempt agent is unknown")
					}
					if reviewer != "" {
						events, e := sprintAssignReadEvents()
						if e != nil {
							return e
						}
						if e = sprintGradeDominance(reviewer, agent, events); e != nil {
							return e
						}
					}
					source, auth, e := sprintGradeSource(it, queue)
					if e != nil {
						return e
					}
					ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
					defer cancel()
					ev.MergeCommit, e = sprintGradeMerge(ctx, into, source, sprintGradeBranch(it), auth, ev, agent)
					if e != nil {
						return e
					}
					ev.Reviewer = reviewer
					ev.Agent = agent
				}
				b, e := json.Marshal(ev)
				if e != nil {
					return e
				}
				weaveStoryAppend(s, weaveConductorName(""), verb, string(b))
				return nil
			})
			if err != nil {
				return err
			}
			if jsonOut {
				err = json.NewEncoder(cmd.OutOrStdout()).Encode(ev)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s run=%s commit=%s verdict=%s gate_exit=%d tamper=%t duration=%s checkout=%s merge=%s\n", verb, ev.Run, ev.Commit, ev.Verdict, ev.GateExit, ev.Tamper, ev.Duration, ev.Checkout, ev.MergeCommit)
			}
			if err != nil {
				return err
			}
			if ev.Verdict != "pass" {
				return fmt.Errorf("grade %s", ev.Verdict)
			}
			return nil
		}
		commands = append(commands, cmd)
	}
	return commands
}

// Merge is a hard manager-only boundary even during the rollout's advisory
// phase. No missing lease or advisory bypass can authorize a repository write.
func sprintGradeManager(s *weaveStory) error {
	if s.Lease == nil || s.Lease.TokenHash == "" {
		return fmt.Errorf("merge requires a manager lease")
	}
	token := os.Getenv(sprintLeaseTokenEnv)
	if token == "" {
		if actor, ok := weaveConductorIdentity(""); ok && actor == s.Lease.Holder {
			token = readSprintLeaseToken(s.ID, s.Lease.Holder)
		}
	}
	if token == "" || sprintLeaseTokenHash(token) != s.Lease.TokenHash {
		return fmt.Errorf("merge requires the manager lease token")
	}
	return nil
}

func sprintGradeBranch(it *weaveItem) string {
	if it.BoothForkURL != "" || it.ArenaSprint != 0 {
		return "attempt"
	}
	return it.Branch
}

// Restore only provisioned skill directories to the pinned base. Build objects
// directly: the worker's index, checkout and HEAD remain untouched.
func sprintGradeRestoreTree(r *gogit.Repository, attempt, base plumbing.Hash, parts []string) (plumbing.Hash, error) {
	read := func(hash plumbing.Hash) ([]object.TreeEntry, error) {
		if hash.IsZero() {
			return nil, nil
		}
		tree, err := r.TreeObject(hash)
		if err != nil {
			return nil, err
		}
		return append([]object.TreeEntry(nil), tree.Entries...), nil
	}
	entries, err := read(attempt)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	original, err := read(base)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	var old, current object.TreeEntry
	for _, e := range original {
		if e.Name == parts[0] {
			old = e
		}
	}
	var kept []object.TreeEntry
	for _, e := range entries {
		if e.Name == parts[0] {
			current = e
		} else {
			kept = append(kept, e)
		}
	}
	replacement := old
	if len(parts) > 1 {
		a, b := plumbing.ZeroHash, plumbing.ZeroHash
		if current.Mode == filemode.Dir {
			a = current.Hash
		}
		if old.Mode == filemode.Dir {
			b = old.Hash
		}
		// No protected subtree at either side: preserve the existing entry.
		if a.IsZero() && b.IsZero() {
			return attempt, nil
		}
		hash, e := sprintGradeRestoreTree(r, a, b, parts[1:])
		if e != nil {
			return plumbing.ZeroHash, e
		}
		replacement = object.TreeEntry{Name: parts[0], Mode: filemode.Dir, Hash: hash}
		if hash.IsZero() {
			replacement = object.TreeEntry{}
		}
	}
	if replacement.Name != "" {
		kept = append(kept, replacement)
	}
	if len(kept) == 0 {
		return plumbing.ZeroHash, nil
	}
	sort.Sort(object.TreeEntrySorter(kept))
	tree := &object.Tree{Entries: kept}
	encoded := r.Storer.NewEncodedObject()
	if err := tree.Encode(encoded); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.Storer.SetEncodedObject(encoded)
}

func sprintGradePushAttempt(ctx context.Context, it *weaveItem, queue string) error {
	if it.ArenaSprint == 0 && it.BoothForkURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	source, auth, err := sprintGradeSource(it, queue)
	if err != nil {
		return err
	}
	r, err := gogit.PlainOpen(it.Workspace)
	if err != nil {
		return err
	}
	head, err := r.Head()
	if err != nil {
		return err
	}
	base, err := r.CommitObject(plumbing.NewHash(it.BaseSHA))
	if err != nil {
		return err
	}
	tip, err := r.CommitObject(head.Hash())
	if err != nil {
		return err
	}
	ancestor, err := base.IsAncestor(tip)
	if err != nil {
		return err
	}
	if !ancestor {
		return fmt.Errorf("booth attempt does not descend from pinned base")
	}
	// Rewrite only commits whose tree or parent changes. Keep author, committer,
	// message and delivery trailers. Contaminated objects are never pushed as
	// ancestors; original commits remain available in the booth workspace.
	memo := map[plumbing.Hash]plumbing.Hash{base.Hash: base.Hash}
	var clean func(plumbing.Hash) (plumbing.Hash, error)
	clean = func(hash plumbing.Hash) (plumbing.Hash, error) {
		if err := ctx.Err(); err != nil {
			return plumbing.ZeroHash, err
		}
		if found, ok := memo[hash]; ok {
			return found, nil
		}
		c, err := r.CommitObject(hash)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		before, err := c.IsAncestor(base)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if before {
			memo[hash] = hash
			return hash, nil
		}
		originalTree := c.TreeHash
		changed := false
		for i, p := range c.ParentHashes {
			parent, err := clean(p)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			changed = changed || parent != p
			c.ParentHashes[i] = parent
		}
		for _, name := range []string{".agents/skills", ".claude/skills"} {
			tree, err := sprintGradeRestoreTree(r, c.TreeHash, base.TreeHash, strings.Split(name, "/"))
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if tree.IsZero() {
				encoded := r.Storer.NewEncodedObject()
				if err := (&object.Tree{}).Encode(encoded); err != nil {
					return plumbing.ZeroHash, err
				}
				tree, err = r.Storer.SetEncodedObject(encoded)
				if err != nil {
					return plumbing.ZeroHash, err
				}
			}
			changed = changed || tree != c.TreeHash
			c.TreeHash = tree
		}
		// An auto-commit containing only provisioned files becomes empty.
		// Drop it so the delivery commit (and its trailers) remains the tip.
		if c.TreeHash != originalTree && len(c.ParentHashes) == 1 {
			parent, err := r.CommitObject(c.ParentHashes[0])
			if err != nil {
				return plumbing.ZeroHash, err
			}
			if parent.TreeHash == c.TreeHash {
				memo[hash] = parent.Hash
				return parent.Hash, nil
			}
		}
		result := hash
		if changed {
			c.PGPSignature = "" // A rewritten object cannot retain its old signature.
			encoded := r.Storer.NewEncodedObject()
			if err := c.Encode(encoded); err != nil {
				return plumbing.ZeroHash, err
			}
			result, err = r.Storer.SetEncodedObject(encoded)
			if err != nil {
				return plumbing.ZeroHash, err
			}
		}
		memo[hash] = result
		return result, nil
	}
	hash, err := clean(head.Hash())
	if err != nil {
		return err
	}
	remote := gogit.NewRemote(r.Storer, &config.RemoteConfig{Name: "booth-attempt", URLs: []string{source}})
	err = remote.PushContext(ctx, &gogit.PushOptions{RemoteName: "booth-attempt", Auth: auth, RefSpecs: []config.RefSpec{config.RefSpec("+" + hash.String() + ":refs/heads/attempt")}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		// Remote diagnostics are untrusted and can contain credential material.
		message := err.Error()
		if basic, ok := auth.(*http.BasicAuth); ok && basic.Password != "" {
			message = strings.ReplaceAll(message, basic.Password, "[redacted]")
			message = strings.ReplaceAll(message, url.QueryEscape(basic.Password), "[redacted]")
		}
		return fmt.Errorf("push booth attempt failed: %s", message)
	}
	return nil
}
