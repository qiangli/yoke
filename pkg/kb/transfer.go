package kb

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// kb transfer is the retro-shaped helper for agent-to-agent knowledge
// transfer: it prints the deterministic ground truth (detected sources,
// what each source already contributed, the pages that already exist for
// the topic) and the checklist of literal commands — then gets out of the
// way. The judgment (what to transfer, how to distill) is the agent's,
// guided by the knowledge-transfer skill. Writes nothing, ever.

func newTransferCmd(dir *string) *cobra.Command {
	var k int
	var from string
	cmd := &cobra.Command{
		Use:   "transfer [<topic term>...]",
		Short: "Structure a knowledge transfer: sources, existing pages, and the checklist (writes nothing)",
		Long: `Run when one agent's knowledge (private memory, in-context recall) should
become team knowledge other agents on this host inherit. Prints the ground
truth — detected source stores, what each already contributed (xfer:<source>
tags), existing pages related to the topic — and the transfer checklist.

Deterministic (no LLM) and read-only: the judgment is yours, transfer
structures it. The full procedure is the knowledge-transfer skill:
bashy skill show knowledge-transfer.

--from memex [<dir>...] is the one exception to read-only: it MOVES an agent's
OWN ycode memex store into the current ring (use with --ring agent) as
form: note candidates tagged xfer:memex. The memex source dir is left untouched
— the operator deletes it after review. With no <dir>, the two locations kb
sources probes are used: ~/.agents/ycode/memory and <repo>/.agents/ycode/memory.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(c *cobra.Command, args []string) error {
			if src := strings.TrimSpace(from); src != "" {
				if src != memexSource {
					return fmt.Errorf("kb: transfer --from %q not supported (only: memex)", from)
				}
				return runMemexTransfer(c, *dir, args)
			}
			store := Open(*dir)
			pages, err := store.List()
			if err != nil {
				return err
			}
			home, err := os.UserHomeDir()
			if err != nil {
				home = ""
			}
			cwd, _ := os.Getwd()
			repo := ""
			if cwd != "" {
				if root := repoRootOf(cwd); root != "" {
					repo = filepath.Base(root)
				}
			}
			out := c.OutOrStdout()
			fmt.Fprintln(out, "kb transfer — one agent's knowledge into the team kb")

			fmt.Fprintln(out, "sources on this host (read-only — kb never writes them):")
			writeSourcesSummary(out, DetectSources(home, cwd), TransferredCounts(pages))

			// Related pages for the topic — free text tokenized through the
			// shared Terms() tokenizer, so quoted task-shaped queries match.
			terms := Terms(strings.Join(args, " "))
			if len(args) > 0 {
				fmt.Fprintf(out, "existing pages (query: %s):\n", strings.Join(args, " "))
				hits := Search(pages, Query{Terms: terms, Repo: repo, OS: runtime.GOOS, K: k})
				if len(hits) == 0 {
					fmt.Fprintln(out, "  (none — greenfield topic)")
				}
				for _, h := range hits {
					p := h.Page
					fmt.Fprint(out, Renderer{Resolution: ResLine, Bullet: "  ", Sep: "  "}.Page(p))
				}
			} else {
				fmt.Fprintln(out, "existing pages: pass topic terms to see them (kb transfer <topic>)")
			}

			fmt.Fprint(out, `per selected claim (durable + team-relevant + non-derivable, redacted):
  FACT/GOTCHA  bashy kb add --type gotcha --title "..." --description "<what + WHEN>" --tags xfer:<source> --evidence "..."
  PROCEDURE    bashy skill learn <dir>            # executable + checkable contract -> a skill, not a page
  EXISTS-OK    bashy kb update <slug> ...          # page was right - extend it
  EXISTS-WRONG bashy kb supersede <slug> ...       # page was wrong - correction stays linked
  SKIP         (record the reason in your transfer report)
tag every transferred page xfer:<source> (claude-memory|memex|weave-memory|repo-graph|recall);
transferred pages land as CANDIDATE - a SECOND agent validates through use:
  bashy kb validate <slug> --evidence "used in <task>, held"
full procedure: bashy skill show knowledge-transfer
`)
			return nil
		},
	}
	cmd.Flags().IntVar(&k, "k", DefaultK, "max related pages shown")
	cmd.Flags().StringVar(&from, "from", "", "move a source store into the current ring instead of printing the checklist (only: memex; use with --ring agent)")
	return cmd
}

// runMemexTransfer moves the owner's own ycode memex store into the current
// ring as form: note candidates. Unlike the checklist path (and unlike a
// transfer of ANOTHER agent's store, which stays pointers-not-copies), this is
// a MOVE: the notes are written here and the source dir is left untouched for
// the operator to delete. Idempotent — a memory already present (by content
// hash, then near-duplicate title) is skipped, so a second run moves nothing.
func runMemexTransfer(c *cobra.Command, dir string, srcDirs []string) error {
	if len(srcDirs) == 0 {
		srcDirs = defaultMemexDirs()
	}
	// The target store is read UNSCOPED (every page physically in the ring),
	// so dedup sees the memex-stamped pages a prior run wrote even though their
	// Source.Tool is "memex" rather than this principal.
	store := Open(dir)
	existing, err := store.List()
	if err != nil {
		return err
	}

	// Hashes already transferred: recomputed from the stored body so a second
	// run recognises them without persisting the hash in a field.
	seen := map[string]bool{}
	for _, p := range existing {
		if hasXferTag(p.Tags, memexSource) {
			seen[memexContentHash(p.Body)] = true
		}
	}

	// Gather every source memory first, so the supersedes chain (memex records
	// only the forward SupersededBy pointer) can be inverted into kb's Supersedes.
	var mems []*memexMemory
	for _, d := range srcDirs {
		ms, err := readMemexDir(d)
		if err != nil {
			return err
		}
		mems = append(mems, ms...)
	}
	predecessorOf := map[string]string{} // successor name -> predecessor name
	for _, m := range mems {
		if m.SupersededBy != "" {
			predecessorOf[m.SupersededBy] = m.Name
		}
	}

	now := time.Now()
	var moved, expired, dup int
	for _, m := range mems {
		if strings.TrimSpace(m.Name) == "" {
			continue
		}
		if m.ValidUntil != nil && m.ValidUntil.Before(now) {
			expired++
			continue
		}
		if seen[m.ContentHash] {
			dup++
			continue
		}
		if NearDuplicate(existing, m.Name, m.Description) != nil {
			seen[m.ContentHash] = true
			dup++
			continue
		}
		p := &Page{
			Slug:        Slugify(m.Name),
			Form:        FormNote,
			Type:        memexTypeToKB(m.Type),
			Title:       m.Name,
			Description: m.Description,
			Tags:        withXferTag(m.Tags, memexSource),
			Status:      StatusCandidate,
			Source:      &Source{Tool: memexSource, Host: HostID(), Episode: EpisodeID()},
			Body:        m.Content,
		}
		if m.SupersededBy != "" {
			p.SupersededBy = Slugify(m.SupersededBy)
		}
		if pred, ok := predecessorOf[m.Name]; ok {
			p.Supersedes = Slugify(pred)
		}
		if err := store.Write(p, "transfer-memex"); err != nil {
			return err
		}
		existing = append(existing, p)
		seen[m.ContentHash] = true
		moved++
	}

	out := c.OutOrStdout()
	fmt.Fprintf(out, "transferred %d memex note(s) into the ring at %s (candidate, tagged xfer:%s)\n", moved, store.Dir(), memexSource)
	if expired > 0 || dup > 0 {
		fmt.Fprintf(out, "skipped: %d expired, %d already present\n", expired, dup)
	}
	for _, d := range srcDirs {
		fmt.Fprintf(out, "source left untouched (delete after review): %s\n", d)
	}
	return nil
}

// defaultMemexDirs are the two frontmatter-md memex locations kb sources
// probes: the global store under home, and the current repo's store.
func defaultMemexDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	var dirs []string
	if home != "" {
		dirs = append(dirs, filepath.Join(home, ".agents", "ycode", "memory"))
	}
	if cwd, err := os.Getwd(); err == nil {
		if root := repoRootOf(cwd); root != "" {
			dirs = append(dirs, filepath.Join(root, ".agents", "ycode", "memory"))
		}
	}
	return dirs
}

// memexTypeToKB maps a memex memory Type onto the OKF page type kb requires.
// memex's vocabulary (user|feedback|project|reference|episodic|procedural|task)
// is richer than kb's; an unknown or empty type folds to the note default.
func memexTypeToKB(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "procedural":
		return TypeRunbook
	case "project", "task":
		return TypeDecision
	case "user", "reference", "fact":
		return TypeFact
	case "feedback", "episodic", "lesson":
		return TypeLesson
	default:
		return TypeLesson
	}
}

// hasXferTag reports whether tags already carry xfer:<source> (case-folded).
func hasXferTag(tags []string, source string) bool {
	want := "xfer:" + strings.ToLower(source)
	for _, t := range tags {
		if strings.ToLower(strings.TrimSpace(t)) == want {
			return true
		}
	}
	return false
}

// withXferTag returns tags with xfer:<source> appended when not already present.
func withXferTag(tags []string, source string) []string {
	if hasXferTag(tags, source) {
		return tags
	}
	return append(append([]string{}, tags...), "xfer:"+source)
}
