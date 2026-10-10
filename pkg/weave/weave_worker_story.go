package weave

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

// A launch freezes explicit story identity, its store, and the actual agent
// binding. Queue slot numbers and the launcher's environment are not identity.
type weaveWorkerStory struct {
	Sprint   int64  `json:"sprint"`
	SprintID string `json:"sprint_id,omitempty"`
	ID       string `json:"id"`
	Seq      int    `json:"seq"`
	Repo     string `json:"repo"`
	Actor    string `json:"actor"`
}

// Briefs can put instructions after trailers. Only exact provenance lines
// count; mentioning a story in prose never volunteers for it.
func weaveBriefTrace(body string) (commitTrace, error) {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		key, _, ok := strings.Cut(line, ":")
		if ok && (key == "Sprint" || key == "Sprint-ID" || key == "Story" || key == "Story-ID") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return commitTrace{}, nil
	}
	return parseCommitTrace("worker brief\n\n" + strings.Join(lines, "\n"))
}

func weaveResolveWorkerStories(queueDir string, it *weaveItem) ([]weaveWorkerStory, error) {
	trace, err := weaveBriefTrace(it.Body)
	if err != nil {
		return nil, fmt.Errorf("explicit story trailers: %w", err)
	}
	refs := trace.Stories
	if len(refs) > 0 && it.Register != "" {
		matches := false
		for _, ref := range refs {
			if strings.HasPrefix(ref.ID, it.Register) {
				matches = true
			}
		}
		if !matches {
			return nil, fmt.Errorf("registered story conflicts with brief trailers")
		}
	}
	if len(refs) == 0 && it.Register == "" {
		return nil, nil
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	board, err := loadWeaveQueue(dir)
	if err != nil {
		return nil, err
	}
	// A link constrains the sprint, but cannot invent a story. Reject stale
	// generations rather than inheriting another run's sprint attribution.
	var linked int64
	for _, s := range board.Stories {
		for _, r := range s.Runs {
			if r.ID == it.ID && r.Queue == filepath.Base(queueDir) && !r.Born.IsZero() && r.Born.Equal(it.Created) {
				if linked != 0 && linked != s.ID {
					return nil, fmt.Errorf("run has conflicting sprint links")
				}
				linked = s.ID
			}
		}
	}
	if trace.Sprint != 0 && linked != 0 && linked != trace.Sprint {
		return nil, fmt.Errorf("brief sprint conflicts with run link")
	}
	if len(refs) == 0 {
		refs = []commitStoryRef{{ID: it.Register}}
	}
	var out []weaveWorkerStory
	for _, ref := range refs {
		var matches []weaveWorkerStory
		for _, s := range board.Stories {
			if trace.Sprint != 0 && s.ID != trace.Sprint || linked != 0 && s.ID != linked {
				continue
			}
			if trace.SprintID != "" && s.UUID != trace.SprintID {
				continue
			}
			for _, root := range sprintDeclaredStoryRoots(s) {
				story, e := todopkg.ResolveRef(todopkg.RepoStore(root), ref.ID)
				if e != nil || story == nil || !storyBelongsToSprint(story, s) {
					continue
				}
				if ref.Number != 0 && story.Seq != ref.Number {
					return nil, fmt.Errorf("Story-ID %s does not match Story #%d", ref.ID, ref.Number)
				}
				matches = append(matches, weaveWorkerStory{Sprint: s.ID, SprintID: s.UUID, ID: story.ID, Seq: story.Seq, Repo: root})
			}
		}
		if len(matches) != 1 {
			return nil, fmt.Errorf("explicit story %s resolves to %d stores; require an unambiguous sprint/story link", ref.ID, len(matches))
		}
		out = append(out, matches[0])
	}
	return out, nil
}

// weaveWorkerActor is the agent a run acts as: its launch spec's agent, else
// the queue owner. Story claims and the worker's BASHY_AGENT both use it.
func weaveWorkerActor(it *weaveItem) string {
	if it == nil {
		return ""
	}
	if it.LaunchSpec != nil && it.LaunchSpec.Agent != "" {
		return it.LaunchSpec.Agent
	}
	return it.Owner
}

// weaveWorkerAgent is weaveWorkerActor in its canonical fleet spelling when
// the registry knows it, so commit attribution matches the story claimant.
func weaveWorkerAgent(it *weaveItem) string {
	actor := strings.TrimSpace(weaveWorkerActor(it))
	if canonical, ok := canonicalFleetAgentName(actor); ok {
		return canonical
	}
	return actor
}

func weaveClaimWorkerStories(cmd *cobra.Command, queueDir string, it *weaveItem) error {
	stories, err := weaveResolveWorkerStories(queueDir, it)
	if err != nil {
		return err
	}
	if len(stories) == 0 {
		return nil
	}
	actor := weaveWorkerActor(it)
	if err := validateSprintClaimant(actor); err != nil {
		return err
	}
	actor, _ = canonicalFleetAgentName(actor)
	// Preflight every story before taking any claim.
	for _, st := range stories {
		story, err := todopkg.ResolveRef(todopkg.RepoStore(st.Repo), st.ID)
		if err != nil {
			return err
		}
		if todopkg.IsClosed(story.Status) {
			return fmt.Errorf("story %s is already closed", st.ID)
		}
		if story.Assignee != "" && !strings.EqualFold(story.Assignee, actor) {
			return fmt.Errorf("story %s is held by %s", st.ID, story.Assignee)
		}
	}
	quiet := &cobra.Command{}
	quiet.SetOut(io.Discard)
	quiet.SetErr(cmd.ErrOrStderr())
	for i := range stories {
		st := &stories[i]
		st.Actor = actor
		if err := runSprintStoryClaim(quiet, st.Sprint, st.ID, actor, st.Repo, false, &weaveOutputFlags{}); err != nil {
			return err
		}
	}
	return withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		current := findWeaveItem(q, it.ID)
		if current == nil || !current.Created.Equal(it.Created) {
			return fmt.Errorf("run changed while claiming stories")
		}
		current.WorkerStories = stories
		it.WorkerStories = stories
		return nil
	})
}

// Read the committed range natively. Excluding all ancestors of the immutable
// base prevents a merged side branch's old story trailers from proving delivery.
func weaveWorkerCommits(it *weaveItem) ([]*object.Commit, error) {
	if it.BaseSHA == "" {
		return nil, fmt.Errorf("missing immutable base for story evidence")
	}
	repo, err := gogit.PlainOpen(it.Workspace)
	if err != nil {
		return nil, err
	}
	evidenceBase := it.BaseSHA
	if it.HandoffBaseSHA != "" {
		evidenceBase = it.HandoffBaseSHA
	}
	base, err := repo.CommitObject(plumbing.NewHash(evidenceBase))
	if err != nil {
		return nil, err
	}
	head, err := repo.Head()
	if err != nil {
		return nil, err
	}
	if it.Head != "" && head.Hash().String() != it.Head {
		return nil, fmt.Errorf("worker HEAD changed after terminal measurement")
	}
	tip, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	ancestor, err := base.IsAncestor(tip)
	if err != nil {
		return nil, err
	}
	if !ancestor {
		return nil, fmt.Errorf("worker head is not descended from its base")
	}
	excluded := map[plumbing.Hash]bool{}
	old := object.NewCommitPreorderIter(base, nil, nil)
	defer old.Close()
	if err := old.ForEach(func(c *object.Commit) error { excluded[c.Hash] = true; return nil }); err != nil {
		return nil, err
	}
	var commits []*object.Commit
	walk := object.NewCommitPreorderIter(tip, excluded, nil)
	defer walk.Close()
	err = walk.ForEach(func(c *object.Commit) error { commits = append(commits, c); return nil })
	return commits, err
}

func weaveSubmitWorkerStories(cmd *cobra.Command, it *weaveItem) error {
	if len(it.WorkerStories) == 0 || it.State != "submitted" || it.AutoCommitted || it.Dirty || it.UntrackedFiles > 0 || it.IsolationViolated || it.VerifyExit != nil && *it.VerifyExit != 0 {
		return nil
	}
	commits, err := weaveWorkerCommits(it)
	if err != nil {
		return err
	}
	quiet := &cobra.Command{}
	quiet.SetOut(io.Discard)
	quiet.SetErr(cmd.ErrOrStderr())
	for _, st := range it.WorkerStories {
		var hashes []string
		for _, c := range commits {
			trace, err := parseCommitTrace(c.Message)
			if err != nil || trace.Sprint != st.Sprint || trace.SprintID != "" && trace.SprintID != st.SprintID || (trace.AgentPresent && !strings.EqualFold(trace.Agent, st.Actor)) || it.HandoffBaseSHA != "" && !trace.AgentPresent {
				continue
			}
			for _, ref := range trace.Stories {
				if ref.ID == st.ID && ref.Number == st.Seq {
					hashes = append(hashes, c.Hash.String())
					break
				}
			}
		}
		if len(hashes) == 0 {
			continue
		}
		evidence := fmt.Sprintf("weave worker Story-ID: %s; commits: %s; workspace: %s; verify: %s", st.ID, strings.Join(hashes, ","), it.Workspace, it.VerifyOutput)
		if err := runSprintStorySubmit(quiet, st.Sprint, st.ID, st.Actor, st.Repo, evidence, &weaveOutputFlags{}); err != nil {
			return err
		}
	}
	return nil
}

// Keep submission refusal durable alongside the terminal run, without rewriting
// its measured outcome or discarding any workspace evidence.
func weaveFinishWorkerStories(cmd *cobra.Command, queueDir string, it *weaveItem) error {
	submitErr := weaveSubmitWorkerStories(cmd, it)
	if len(it.WorkerStories) == 0 {
		return submitErr
	}
	saveErr := withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		current := findWeaveItem(q, it.ID)
		if current == nil || !current.Created.Equal(it.Created) {
			return fmt.Errorf("run changed while submitting story")
		}
		current.StorySubmissionError = ""
		if submitErr != nil {
			current.StorySubmissionError = submitErr.Error()
		}
		return nil
	})
	return errors.Join(submitErr, saveErr)
}
