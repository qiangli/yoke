package weave

// The host-kb bridge: weave does the check-before-task step FOR the worker.
// At spawn, the top host-kb pages matching the issue are dropped into the
// workspace as KB.md (beside WEAVE_MEMORY.md — memory is this repo's run
// history, kb is the host-wide wiki across all repos and all agent tools),
// and KB.md carries the write-back instruction so the retro half of the
// loop reaches every fleet CLI without any per-tool integration. Best
// effort throughout: a missing or empty kb never blocks a launch.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/recall"
)

// weaveKBFileName is the workspace drop (gitignored via .git/info/exclude,
// like WEAVE_MEMORY.md — it must never merge).
const weaveKBFileName = "KB.md"

// weaveKBBudget preserves the previous 400-rune body allowance as the hard
// assembler budget. The worker can open cited pages for more.
const weaveKBBudget = 400

// weaveInjectKBFile writes KB.md into the workspace: budgeted repo/host
// context for this issue plus the retro write-back instruction.
func weaveInjectKBFile(dir, workspace string, it *weaveItem) error {
	if it == nil || workspace == "" {
		return nil
	}
	repoRoot, ok := weaveRepoRootForQueue(dir)
	if !ok {
		repoRoot, _ = os.Getwd()
	}
	query := recall.Query{
		Text: it.Title, Rings: []string{recall.RingRepo, recall.RingHost},
		Forms: []string{kb.FormNote, kb.FormPage}, Budget: weaveKBBudget,
		Repo: weaveRepoNameFromQueueDir(dir), OS: runtime.GOOS,
	}
	assembled := recall.Context(query, recall.ContextPageReaders(repoRoot)...)
	contextText := recall.RenderContext(assembled)
	var b strings.Builder
	b.WriteString("# KB — relevant repo and host knowledge (may be stale; verify before relying on it)\n\n")
	if contextText == "" {
		// "Nothing matched" is not the same as "nothing is here", and the
		// worker cannot tell the difference from a bare sentence. The search
		// ran on the issue TITLE alone, so a miss is as likely to be the
		// query's wording as the corpus's silence — the report says which,
		// and hands over the vocabulary to re-ask with.
		pages := contextPagesForDiagnose(repoRoot)
		if len(pages) > 0 {
			q := kb.Query{Terms: kb.Terms(it.Title), Repo: query.Repo, OS: query.OS}
			b.WriteString("This search used the issue title only, and nothing matched:\n\n```\n")
			b.WriteString(kb.Diagnose(pages, q).Text())
			b.WriteString("```\n\nRe-ask in the kb's own words before assuming it is empty: `bashy kb search <query>`.\n")
			b.WriteString("If the work still teaches something durable, contribute it when you finish:\n")
		} else {
			b.WriteString("The host kb is empty. If the work teaches something durable, contribute it when you finish:\n")
		}
	} else {
		b.WriteString("Check these before you start — they may save you a failed approach:\n\n")
		b.WriteString(contextText)
		b.WriteString("\nMore: `bashy kb search <query>` (or `bashy kb show <slug>`).\n")
	}
	b.WriteString("\nAFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`\n")
	b.WriteString("— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.\n")
	if err := os.WriteFile(filepath.Join(workspace, weaveKBFileName), []byte(b.String()), 0o644); err != nil {
		return err
	}
	return weaveExcludeWorkspaceFile(workspace, weaveKBFileName)
}

func contextPagesForDiagnose(repoRoot string) []*kb.Page {
	var pages []*kb.Page
	for _, dir := range []string{filepath.Join(repoRoot, kb.RepoSub), kb.DefaultDir()} {
		ps, err := kb.Open(dir).List()
		if err == nil {
			pages = append(pages, ps...)
		}
	}
	return pages
}

// weaveRepoNameFromQueueDir recovers the repo basename from the queue dir
// tag (<base>-<8-hex fnv32a>), for kb's repo-scope filter.
var weaveQueueTagHash = regexp.MustCompile(`-[0-9a-f]{8}$`)

func weaveRepoNameFromQueueDir(dir string) string {
	return weaveQueueTagHash.ReplaceAllString(filepath.Base(dir), "")
}
