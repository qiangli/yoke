package chat

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// CloneAgentContext branches an agent's conversation context onto a clone.
//
// It is the implementation behind `bashy agent clone`, injected into the
// registry as a fleet.ContextCloner because knowing where an agent's store
// lives means this package, and this package reads the registry.
//
// WHAT "SHARES CONTEXT UP TO THE POINT OF CLONING" ACTUALLY IS: a copy of the
// parent's conversation store, taken now. After the copy the two stores are
// unrelated — the clone remembers everything the parent knew at this instant and
// nothing it learns afterwards, which is the whole point. There is no ongoing
// link to maintain, and no way for the two to interfere later.
//
// It is honest about its limits, and the honesty is the feature:
//
//   - Only a tool whose store bashy relocates (YCODE_DATA_DIR) can be branched.
//     Every other harness keeps its history in its own home directory under its
//     own naming, and bashy neither owns nor understands that. Those clones start
//     fresh, and say so.
//   - A LIVE parent is not copied. Its store is being written; a copy taken
//     mid-write is a torn one, and a clone silently seeded from a half-written
//     transcript is precisely the confidently-wrong-answer failure this model
//     exists to prevent. Stop the parent, or clone --fresh.
func CloneAgentContext(parent, clone fleet.Agent) (string, error) {
	if parent.Tool != agentlaunch.YcodeToolName {
		return "", nil // caller renders the "tool keeps its own state" note
	}

	src := agentStoreDir(parent)
	dst := agentStoreDir(clone)
	if src == "" || dst == "" {
		return "", fmt.Errorf("no state directory on this host")
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Sprintf("fresh context — %s has no conversation store yet", parent.Name), nil
	}

	// The liveness check is deliberately on the parent's IDENTITY, which is what
	// holds the store — not on any particular process.
	claimID := room.AgentClaimID(parent.Name)
	card, live, _ := room.Find(claimID)
	if !live && claimID != parent.Name {
		// A pre-contract watcher may still hold the raw fleet name. It remains a
		// real writer until it exits, so cloning must not take a torn snapshot.
		card, live, _ = room.Find(parent.Name)
	}
	if live && (card.ID == claimID || card.ID == parent.Name) {
		return "", fmt.Errorf("%s is live (pid %d): its store is being written, and a copy taken "+
			"mid-write is a torn transcript — stop it, or clone --fresh", parent.Name, card.PID)
	}

	if err := copyTree(src, dst); err != nil {
		return "", err
	}
	return fmt.Sprintf("inherited %s's context as of now; the two diverge from here", parent.Name), nil
}

// agentStoreDir is where an agent's conversation store lives. It goes through
// the SAME helper the launcher uses, with an empty parent environment so an
// operator's own YCODE_DATA_DIR cannot make this compute a different answer than
// the one the agent will actually be launched with.
func agentStoreDir(a fleet.Agent) string {
	return agentlaunch.YcodeDataDir(nil, agentlaunch.Launch{
		ToolName:  a.Tool,
		ModelName: a.Model,
		Nick:      a.Name,
	}, chatStateDir(), agentIDFor(a.Name))
}

// agentIDFor is agentID for a name that is already an agent's name — the same
// sanitizing, so the store a clone is written to is the one it will read.
func agentIDFor(name string) string {
	return agentID(Launch{Nick: name})
}

// copyTree copies a directory recursively. Refuses an existing destination
// rather than merging into it: merging two conversation stores is how a clone
// would end up with a history that never happened.
func copyTree(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("%s already has a conversation store; remove it or clone under another name", filepath.Base(dst))
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o700)
		case info.Mode()&os.ModeSymlink != 0:
			// A store is data. A symlink in one would make the copy reach outside
			// the tree, so it is skipped rather than followed.
			return nil
		case !info.Mode().IsRegular():
			return nil
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// CloneNote renders what a caller should tell the operator when no store could
// be branched. Exported so the weave/conductor paths report cloning in the same
// words the `agents clone` verb does.
func CloneNote(tool string) string {
	if strings.TrimSpace(tool) == "" {
		return "fresh context"
	}
	return "fresh context — " + tool + " keeps its own state outside bashy"
}
