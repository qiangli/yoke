package weave

import (
	"fmt"
	"os"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/fleet"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

type sprintCommitAttributionRefusal string

func (e sprintCommitAttributionRefusal) Error() string { return "commit attribution: " + string(e) }

// Resolve registered clones through their ancestry, never by stripping a name
// suffix: a person or an unrelated agent may legitimately have that suffix.
func sprintCommitAgentKey(cat *fleet.Catalog, name string) (string, bool) {
	a, ok := cat.Agent(name)
	if !ok {
		// A binding may name several registered clones. It remains unambiguous
		// when all of those records resolve to the same base binding.
		if tool, model, binding := strings.Cut(name, ":"); binding {
			if m, found := cat.Model(model); found {
				model = m.Name
			}
			agents, _ := cat.Agents()
			key := ""
			for _, candidate := range agents {
				if candidate.Tool != tool || candidate.Model != model {
					continue
				}
				if candidate.Name == name {
					return "", false
				}
				resolved, found := sprintCommitAgentKey(cat, candidate.Name)
				if !found || (key != "" && key != resolved) {
					return "", false
				}
				key = resolved
			}
			return key, key != ""
		}
	}
	seen := map[string]bool{}
	for ok && a.ClonedFrom != "" {
		if seen[a.Name] {
			return "", false
		}
		seen[a.Name] = true
		a, ok = cat.Agent(a.ClonedFrom)
	}
	if !ok || a.Tool == "" || a.Model == "" {
		return "", false
	}
	if m, found := cat.Model(a.Model); found {
		a.Model = m.Name
	}
	return a.MatrixKey(), true
}

func sprintCommitIdentityMatches(cat *fleet.Catalog, assignee, found string) bool {
	// People require verbatim attribution even when their catalog aliases match.
	if _, kind, err := cat.ResolvePrincipal(assignee); err == nil && kind == fleet.KindPerson {
		return assignee == found
	}
	want, ok := sprintCommitAgentKey(cat, assignee)
	if !ok {
		return assignee == found
	}
	got, ok := sprintCommitAgentKey(cat, found)
	return ok && want == got
}

func sprintCommitAttribution(cmd *cobra.Command, dir string, sprint *weaveStory, trace commitTrace, stories []sprintStoryState, sha string) error {
	if sprint == nil || sprint.Lease == nil || sprint.Lease.TokenHash == "" {
		return nil
	}
	found := trace.Agent
	// Environment identity belongs only to the pending commit, never history.
	if !trace.AgentPresent && sha == "" {
		found = strings.TrimSpace(os.Getenv("BASHY_AGENT"))
	}
	actor := found
	if actor == "" {
		actor = "unknown"
	}
	cat := fleet.New(fleet.WithoutCloudOverlay())
	var problems []string
	for _, ref := range trace.Stories {
		for _, story := range stories {
			if story.Seq != ref.Number || story.Ref.ID != ref.ID {
				continue
			}
			item, err := todopkg.ResolveRef(todopkg.RepoStore(story.Ref.Repo), story.Ref.ID)
			if err != nil {
				return fmt.Errorf("commit attribution: read Story #%d: %w", ref.Number, err)
			}
			if item.Assignee == "" || sprintCommitIdentityMatches(cat, item.Assignee, found) {
				continue
			}
			problems = append(problems, fmt.Sprintf("Story #%d assignee %q, found identity %q; use Agent: %s (or BASHY_AGENT for a pending commit)", ref.Number, item.Assignee, actor, item.Assignee))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	detail := strings.Join(problems, "; ")
	must := strings.EqualFold(strings.TrimSpace(os.Getenv("BASHY_SPRINT_ENFORCE")), "must")
	if sha != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", sha, detail)
	} else if !must {
		fmt.Fprintf(cmd.ErrOrStderr(), "WARN commit attribution: %s\n", detail)
	}
	// A refused pending commit never landed; historical bypasses did, including
	// when discovered in must mode. Dedupe under the queue lock to avoid races.
	if !must || sha != "" {
		err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
			s := findWeaveStory(q, sprint.ID)
			if s == nil {
				return fmt.Errorf("commit attribution: sprint #%d disappeared", sprint.ID)
			}
			marker := "commit SHA: " + sha
			if sha != "" {
				for _, e := range s.Thread {
					if e.Kind == "bypass" && strings.HasPrefix(e.Body, marker+"\n") {
						return nil
					}
				}
			}
			body := "commit: " + detail
			if sha != "" {
				body = marker + "\n" + body
			}
			weaveStoryAppend(s, actor, "bypass", body)
			return nil
		})
		if err != nil {
			return fmt.Errorf("record commit attribution bypass: %w", err)
		}
	}
	if must {
		return sprintCommitAttributionRefusal(detail)
	}
	return nil
}

func sprintCommitCheckRange(cmd *cobra.Command, mode weavecli.OutputMode, revRange string) error {
	dir, err := weaveStoryDir(cmd, mode, "sprint commit-msg")
	if err != nil {
		return err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return err
	}
	// --end-of-options prevents revisions from becoming Git options.
	raw, err := gitOutput(".", "rev-list", "--reverse", "--end-of-options", revRange)
	if err != nil {
		return err
	}
	var failures []string
	for _, sha := range strings.Fields(raw) {
		message, err := gitOutput(".", "show", "-s", "--format=%B", sha)
		if err != nil {
			return err
		}
		trace, err := parseCommitTrace(message)
		if err != nil {
			// Commits without delivery trailers (e.g. initial repository creation)
			// have no story attribution contract.
			if !strings.Contains(strings.ToLower(message), "\nstory:") {
				continue
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: commit provenance: %v\n", sha, err)
			failures = append(failures, sha)
			continue
		}
		sprint := findWeaveStory(q, trace.Sprint)
		if sprint == nil || sprint.Lease == nil || sprint.Lease.TokenHash == "" {
			continue
		}
		stories, err := loadSprintStories(sprint)
		if err != nil {
			return err
		}
		if err := validateCommitTraceStories(trace, stories); err != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: commit provenance: %v\n", sha, err)
			failures = append(failures, sha)
			continue
		}
		if err := sprintCommitAttribution(cmd, dir, sprint, trace, stories, sha); err != nil {
			if _, refused := err.(sprintCommitAttributionRefusal); !refused {
				return err
			}
			failures = append(failures, sha)
		}
	}
	if len(failures) > 0 && strings.EqualFold(strings.TrimSpace(os.Getenv("BASHY_SPRINT_ENFORCE")), "must") {
		return fmt.Errorf("commit attribution: %d commit(s) need matching Agent trailers; amend the listed commits", len(failures))
	}
	return nil
}
