package weave

// Commit provenance is deliberately a Git trailer contract rather than a
// subject prefix. Subjects stay useful to humans and changelog tooling; the
// final trailer paragraph is stable machine-readable metadata.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/spf13/cobra"
)

var (
	commitTrailerLine = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*):[ \t]*(.*)$`)
	commitSprintRef   = regexp.MustCompile(`^#([1-9][0-9]*)$`)
	commitStoryID     = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

type commitStoryRef struct {
	Number int    `json:"number"`
	ID     string `json:"id"`
}

type commitTrace struct {
	Sprint  int64            `json:"sprint"`
	Stories []commitStoryRef `json:"stories"`
}

// weaveMergeCommitMessage carries provenance from the commits being integrated
// onto the merge commit itself. A commit-msg hook validates the commit Git is
// about to create, so valid source commits do not exempt an untraced merge.
//
// Only a fully attributable range is propagated: every source commit must have
// valid provenance and all of them must name the same sprint. Otherwise the
// historical subject-only message is retained, leaving any repository hook to
// make the same accept/reject decision it made before this propagation existed.
func weaveMergeCommitMessage(root, branch, subject string) string {
	raw, err := gitOut(root, "log", "--reverse", "--format=%B%x00", "HEAD.."+branch)
	if err != nil {
		return subject
	}

	var merged commitTrace
	seenNumbers := make(map[int]string)
	seenIDs := make(map[string]int)
	commits := 0
	for _, message := range strings.Split(raw, "\x00") {
		message = strings.TrimSpace(message)
		if message == "" {
			continue
		}
		trace, err := parseCommitTrace(message)
		if err != nil {
			return subject
		}
		commits++
		if merged.Sprint == 0 {
			merged.Sprint = trace.Sprint
		} else if merged.Sprint != trace.Sprint {
			return subject
		}
		for _, story := range trace.Stories {
			if id, ok := seenNumbers[story.Number]; ok {
				if id != story.ID {
					return subject
				}
				continue
			}
			if number, ok := seenIDs[story.ID]; ok && number != story.Number {
				return subject
			}
			seenNumbers[story.Number] = story.ID
			seenIDs[story.ID] = story.Number
			merged.Stories = append(merged.Stories, story)
		}
	}
	if commits == 0 || merged.Sprint == 0 || len(merged.Stories) == 0 {
		return subject
	}

	var b strings.Builder
	b.WriteString(subject)
	fmt.Fprintf(&b, "\n\nSprint: #%d", merged.Sprint)
	for _, story := range merged.Stories {
		fmt.Fprintf(&b, "\nStory: #%d\nStory-ID: %s", story.Number, story.ID)
	}
	return b.String()
}

func parseCommitTrace(message string) (commitTrace, error) {
	message = strings.TrimRight(strings.ReplaceAll(message, "\r\n", "\n"), " \t\n")
	rawLines := strings.Split(message, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, line := range rawLines {
		// Git's default editor template appends comment lines to COMMIT_EDITMSG
		// and strips them only after the hook returns. Validate what Git will
		// commit, not its instructional template.
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) < 3 {
		return commitTrace{}, fmt.Errorf("commit message needs a subject, a blank line, and provenance trailers")
	}

	start := len(lines) - 1
	for start >= 0 && strings.TrimSpace(lines[start]) != "" {
		start--
	}
	if start <= 0 || start == len(lines)-1 {
		return commitTrace{}, fmt.Errorf("provenance trailers must be the final paragraph after a blank line")
	}

	var sprintValues, storyValues, idValues []string
	for _, line := range lines[start+1:] {
		m := commitTrailerLine.FindStringSubmatch(line)
		if m == nil {
			return commitTrace{}, fmt.Errorf("final trailer paragraph contains a non-trailer line %q", line)
		}
		switch strings.ToLower(m[1]) {
		case "sprint":
			sprintValues = append(sprintValues, strings.TrimSpace(m[2]))
		case "story":
			storyValues = append(storyValues, strings.TrimSpace(m[2]))
		case "story-id":
			idValues = append(idValues, strings.TrimSpace(m[2]))
		}
	}

	if len(sprintValues) != 1 {
		return commitTrace{}, fmt.Errorf("want exactly one `Sprint: #N` trailer, got %d", len(sprintValues))
	}
	sprintMatch := commitSprintRef.FindStringSubmatch(sprintValues[0])
	if sprintMatch == nil {
		return commitTrace{}, fmt.Errorf("Sprint trailer must look like `Sprint: #87`, got %q", sprintValues[0])
	}
	sprint, _ := strconv.ParseInt(sprintMatch[1], 10, 64)

	if len(storyValues) == 0 {
		return commitTrace{}, fmt.Errorf("want at least one `Story: #N` trailer")
	}
	if len(storyValues) != len(idValues) {
		return commitTrace{}, fmt.Errorf("each Story trailer needs one matching Story-ID trailer (got %d Story, %d Story-ID)", len(storyValues), len(idValues))
	}

	trace := commitTrace{Sprint: sprint, Stories: make([]commitStoryRef, 0, len(storyValues))}
	seenNumbers, seenIDs := map[int]bool{}, map[string]bool{}
	for i, value := range storyValues {
		storyMatch := commitSprintRef.FindStringSubmatch(value)
		if storyMatch == nil {
			return commitTrace{}, fmt.Errorf("Story trailer must look like `Story: #110`, got %q", value)
		}
		number64, _ := strconv.ParseInt(storyMatch[1], 10, 32)
		number := int(number64)
		id := idValues[i]
		if !commitStoryID.MatchString(id) {
			return commitTrace{}, fmt.Errorf("Story-ID must be the full 12-character lowercase hex id, got %q", idValues[i])
		}
		if seenNumbers[number] || seenIDs[id] {
			return commitTrace{}, fmt.Errorf("duplicate story provenance pair Story: #%d / Story-ID: %s", number, id)
		}
		seenNumbers[number], seenIDs[id] = true, true
		trace.Stories = append(trace.Stories, commitStoryRef{Number: number, ID: id})
	}
	return trace, nil
}

func validateCommitTraceStories(trace commitTrace, stories []sprintStoryState) error {
	byNumber := make(map[int]sprintStoryState, len(stories))
	byID := make(map[string]sprintStoryState, len(stories))
	for _, story := range stories {
		byNumber[story.Seq] = story
		byID[story.Ref.ID] = story
	}
	for _, ref := range trace.Stories {
		bySeq, numberOK := byNumber[ref.Number]
		byStable, idOK := byID[ref.ID]
		if !numberOK {
			return fmt.Errorf("Story: #%d is not linked to Sprint: #%d; a story is a repo todo, so file it with `bashy todo add --sprint %d ...` (a weave run is execution, not a story), then use `bashy sprint track %d --repo <repo>` when it lives in another repo", ref.Number, trace.Sprint, trace.Sprint, trace.Sprint)
		}
		if !idOK {
			return fmt.Errorf("Story-ID: %s is not linked to Sprint: #%d", ref.ID, trace.Sprint)
		}
		if bySeq.Ref.ID != byStable.Ref.ID {
			return fmt.Errorf("Story: #%d resolves to %s, not Story-ID: %s", ref.Number, bySeq.Ref.ID, ref.ID)
		}
	}
	return nil
}

func newSprintCommitMsgCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "commit-msg <file>",
		Short: "Validate mandatory Sprint/Story/Story-ID Git trailers",
		Args:  cobra.ExactArgs(1),
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		fail := func(err error) error {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "sprint commit-msg", weavecli.ExitGenericFail, err))
		}
		raw, err := os.ReadFile(args[0])
		if err != nil {
			return fail(fmt.Errorf("read commit message: %w", err))
		}
		trace, err := parseCommitTrace(string(raw))
		if err != nil {
			return fail(fmt.Errorf("commit provenance: %w\n\nrequired form:\n\n  Sprint: #87\n  Story: #110\n  Story-ID: d1e86f29d7a7", err))
		}
		dir, err := weaveStoryDir(cmd, flags.mode(), "sprint commit-msg")
		if err != nil {
			return fail(err)
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return fail(err)
		}
		sprint := findWeaveStory(q, trace.Sprint)
		if sprint == nil {
			return fail(fmt.Errorf("commit provenance: Sprint: #%d is not on this host's sprint board", trace.Sprint))
		}
		stories, err := loadSprintStories(sprint)
		if err != nil {
			return fail(fmt.Errorf("commit provenance: load Sprint #%d stories: %w", trace.Sprint, err))
		}
		if err := validateCommitTraceStories(trace, stories); err != nil {
			return fail(fmt.Errorf("commit provenance: %w", err))
		}
		if flags.mode() == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), flags.mode(), "sprint commit-msg", trace))
		}
		fmt.Fprintf(cmd.OutOrStdout(), "commit provenance: Sprint #%d, %d story reference(s) verified\n", trace.Sprint, len(trace.Stories))
		return nil
	}
	flags.attach(cmd)
	return cmd
}

const managedCommitHook = `#!/bin/sh
set -eu
hook_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
if [ -x "$hook_dir/commit-msg.before-bashy" ]; then
  "$hook_dir/commit-msg.before-bashy" "$@"
fi
if ! command -v bashy >/dev/null 2>&1; then
  echo "commit-msg: bashy is required to validate Sprint/Story provenance" >&2
  exit 1
fi
exec bashy sprint commit-msg "$1"
`

func gitOutput(repo string, args ...string) (string, error) {
	argv := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", argv...)
	// A hook invocation exports GIT_DIR/GIT_WORK_TREE. Passing those through to
	// `git -C another-repo` makes Git keep operating on the caller's repository
	// (or treat an empty override as an invalid repo). Remove, do not blank,
	// both variables before resolving or configuring the target checkout.
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_DIR=") || strings.HasPrefix(entry, "GIT_WORK_TREE=") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(string(raw)), nil
}

func installSprintCommitHook(repo string) (string, error) {
	root, err := gitOutput(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	gitDir, err := gitOutput(root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	oldHooks, err := gitOutput(root, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(oldHooks) {
		oldHooks = filepath.Join(root, oldHooks)
	}
	oldHooks = filepath.Clean(oldHooks)
	managed := filepath.Join(gitDir, "bashy-hooks")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		return "", fmt.Errorf("prepare managed hooks: %w", err)
	}

	if oldHooks != managed {
		entries, readErr := os.ReadDir(oldHooks)
		if readErr != nil && !os.IsNotExist(readErr) {
			return "", fmt.Errorf("read existing hooks: %w", readErr)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || strings.HasSuffix(name, ".sample") || name == "commit-msg" {
				continue
			}
			target := filepath.Join(managed, name)
			if _, err := os.Lstat(target); err == nil {
				continue
			}
			if err := os.Symlink(filepath.Join(oldHooks, name), target); err != nil {
				raw, readErr := os.ReadFile(filepath.Join(oldHooks, name))
				if readErr != nil {
					return "", fmt.Errorf("preserve hook %s: %w", name, readErr)
				}
				if writeErr := os.WriteFile(target, raw, 0o755); writeErr != nil {
					return "", fmt.Errorf("copy hook %s: %w", name, writeErr)
				}
			}
		}
		oldCommit := filepath.Join(oldHooks, "commit-msg")
		if info, statErr := os.Stat(oldCommit); statErr == nil && info.Mode()&0o111 != 0 {
			prior := filepath.Join(managed, "commit-msg.before-bashy")
			if _, err := os.Stat(prior); os.IsNotExist(err) {
				raw, readErr := os.ReadFile(oldCommit)
				if readErr != nil {
					return "", fmt.Errorf("preserve existing commit-msg hook: %w", readErr)
				}
				if writeErr := os.WriteFile(prior, raw, 0o755); writeErr != nil {
					return "", fmt.Errorf("preserve existing commit-msg hook: %w", writeErr)
				}
			}
		}
	}

	hook := filepath.Join(managed, "commit-msg")
	if err := os.WriteFile(hook, []byte(managedCommitHook), 0o755); err != nil {
		return "", fmt.Errorf("install commit-msg hook: %w", err)
	}
	if _, err := gitOutput(root, "config", "--local", "core.hooksPath", managed); err != nil {
		return "", err
	}
	return root, nil
}

func newSprintHooksCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "hooks", Short: "Install sprint/story provenance enforcement into Git"}
	cmd.AddCommand(&cobra.Command{
		Use:   "install",
		Short: "Install the commit-msg guard in the current repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := installSprintCommitHook(".")
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "sprint hooks: commit provenance enforced in %s\n", root)
			return nil
		},
	})
	return cmd
}
