package weave

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// Isolation guard — the cheap tier.
//
// A weave workspace is a full local clone and the subagent's cwd, but that
// is SOFT isolation: nothing stops an agent cd-ing out, or naming an
// absolute path, and writing into the LIVE checkout it was cloned from.
// That happened (an agent wrote and applied a patch script in the parent
// repo). Two things go wrong when it does: the human's working tree is
// mutated behind their back, and the run stops being reproducible from its
// own branch — the merged diff is no longer the whole story.
//
// Real containment is an OS-sandbox problem (a separate tier). This is the
// detector, not the fence: fingerprint the live checkout's working-tree
// state at `weave start`, re-check at submit/status/list/pull, and if it
// moved during the run, say so by name and refuse to auto-merge. It cannot
// PREVENT the escape; it makes the escape impossible to merge silently,
// which is the property that was actually missing.
//
// The fingerprint is `git status --porcelain --untracked-files=all` over
// the live root. Deliberately coarse: it cannot tell an escaped agent from
// a human editing their own checkout during the run, and it does not try.
// A false positive costs one `--force`; a false negative costs a corrupted
// tree and a lying branch. Ignored files are excluded (git already omits
// them), so build litter in the live root is not mistaken for an escape.

// weaveLiveBaselineMaxLines caps the porcelain lines stored per item. A
// live checkout dirtier than this keeps its SHA (change is still DETECTED
// — the hash is over the full output either way) but drops the line
// bodies, so a pathological tree can't bloat queue.json. Only the ability
// to NAME the escaped paths is lost.
const weaveLiveBaselineMaxLines = 500

// weaveEscapedPathsMax caps the paths reported on a violation; the warning
// says how many more there were.
const weaveEscapedPathsMax = 20

// weaveLiveSnapshot is the live checkout's working-tree state at one
// moment: a hash over the full porcelain output plus (when small enough)
// the lines themselves, which is what lets a violation name paths.
type weaveLiveSnapshot struct {
	SHA       string
	Lines     []string
	Truncated bool
}

// weaveSnapshotLiveTree fingerprints root's working tree. An error means
// no snapshot — the guard then stays silent rather than guessing, because
// a fabricated baseline would flag every run.
func weaveSnapshotLiveTree(root string) (weaveLiveSnapshot, error) {
	if root == "" {
		return weaveLiveSnapshot{}, fmt.Errorf("no repo root")
	}
	out, err := exec.Command("git", "-C", root, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		return weaveLiveSnapshot{}, fmt.Errorf("git status in %s: %w", root, err)
	}
	sum := sha256.Sum256(out)
	snap := weaveLiveSnapshot{SHA: hex.EncodeToString(sum[:])}
	for _, ln := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if ln != "" {
			snap.Lines = append(snap.Lines, ln)
		}
	}
	if len(snap.Lines) > weaveLiveBaselineMaxLines {
		snap.Lines = nil
		snap.Truncated = true
	}
	return snap, nil
}

// weaveRecordLiveBaseline stamps the item with the live checkout's state
// at claim time. Best-effort: a repo we can't stat just gets no guard.
// Call it under the queue lock, alongside the other claim bookkeeping.
func weaveRecordLiveBaseline(it *weaveItem, root string) {
	snap, err := weaveSnapshotLiveTree(root)
	if err != nil {
		return
	}
	it.LiveRoot = root
	it.LiveTreeSHA = snap.SHA
	it.LiveTreeLines = snap.Lines
	it.LiveTreeTruncated = snap.Truncated
}

// weavePorcelainPath extracts the path from one porcelain v1 line
// ("XY path", or "XY orig -> path" for a rename). Quoted paths (git
// quotes non-ASCII unless core.quotePath=false) are reported as-is —
// naming the file imperfectly beats dropping it from the warning.
func weavePorcelainPath(line string) string {
	if len(line) < 4 {
		return strings.TrimSpace(line)
	}
	p := strings.TrimSpace(line[3:])
	if i := strings.Index(p, " -> "); i >= 0 {
		p = p[i+4:]
	}
	return strings.TrimSpace(p)
}

// weaveIsolationStatus compares the item's baseline against a snapshot
// taken now, returning whether the live checkout moved during the run and
// which paths differ. Both arms of the symmetric difference count: an
// agent that DELETED a file the human had modified escaped just as surely
// as one that created a patch script.
//
// The SHA is the verdict; the lines only name paths. When the baseline was
// truncated, or the current tree is too dirty to line up, the change is
// still reported — with no paths, which the caller renders honestly.
func weaveIsolationStatus(it *weaveItem, now weaveLiveSnapshot) (violated bool, escaped []string) {
	if it == nil || it.LiveTreeSHA == "" || now.SHA == "" {
		return false, nil
	}
	if it.LiveTreeSHA == now.SHA {
		return false, nil
	}
	if it.LiveTreeTruncated || now.Truncated {
		return true, nil
	}
	was := make(map[string]bool, len(it.LiveTreeLines))
	for _, ln := range it.LiveTreeLines {
		was[ln] = true
	}
	is := make(map[string]bool, len(now.Lines))
	for _, ln := range now.Lines {
		is[ln] = true
	}
	paths := map[string]bool{}
	for ln := range is {
		if !was[ln] {
			paths[weavePorcelainPath(ln)] = true
		}
	}
	for ln := range was {
		if !is[ln] {
			paths[weavePorcelainPath(ln)] = true
		}
	}
	for p := range paths {
		if p != "" {
			escaped = append(escaped, p)
		}
	}
	sort.Strings(escaped)
	return true, escaped
}

// weaveApplyIsolationCheck re-checks the item against its live root and
// records the verdict on the item. Returns true when this call is what
// flipped it (so the caller can warn once).
//
// The flag is STICKY: a run that escaped and then tidied up after itself
// still escaped, and the record of it must not evaporate because the tree
// happens to match again by the time anyone looks.
func weaveApplyIsolationCheck(it *weaveItem) bool {
	if it == nil || it.LiveTreeSHA == "" {
		return false
	}
	snap, err := weaveSnapshotLiveTree(it.LiveRoot)
	if err != nil {
		return false
	}
	violated, escaped := weaveIsolationStatus(it, snap)
	if !violated {
		return false
	}
	newly := !it.IsolationViolated
	it.IsolationViolated = true
	if len(escaped) > 0 {
		it.EscapedPaths = escaped
	}
	return newly
}

// weaveComputeIsolation is the READ-TIME check for list/status: compare
// every item in the queue against one snapshot of the shared live root.
// One git call for the whole queue, and — because all items share the root
// — every item is judged against the same instant.
//
// Display-only, exactly like weaveComputeBlocked: these paths load the
// queue without the lock, so nothing here is persisted. The durable record
// is written at submit (weaveApplyIsolationCheck under the lock).
func weaveComputeIsolation(root string, q *weaveQueue) {
	if q == nil {
		return
	}
	snap, err := weaveSnapshotLiveTree(root)
	if err != nil {
		return
	}
	for _, it := range q.Items {
		if violated, escaped := weaveIsolationStatus(it, snap); violated {
			it.IsolationViolated = true
			if len(escaped) > 0 {
				it.EscapedPaths = escaped
			}
		}
	}
}

// weaveIsolationDetail is the one-line reason recorded on a refused pull
// and shown by status — it names the escaped paths, because "isolation
// violated" without a path list is a dead end for whoever has to decide
// whether this is a real escape or their own edit.
func weaveIsolationDetail(it *weaveItem) string {
	if it == nil || !it.IsolationViolated {
		return ""
	}
	root := it.LiveRoot
	if root == "" {
		root = "the live checkout"
	}
	if len(it.EscapedPaths) == 0 {
		return fmt.Sprintf("the live checkout (%s) changed during this run; paths unavailable "+
			"(it was already too dirty at start to diff) — review it by hand, then `weave pull --force` if the change is yours", root)
	}
	paths := it.EscapedPaths
	suffix := ""
	if len(paths) > weaveEscapedPathsMax {
		suffix = fmt.Sprintf(" (+%d more)", len(paths)-weaveEscapedPathsMax)
		paths = paths[:weaveEscapedPathsMax]
	}
	return fmt.Sprintf("the live checkout (%s) changed during this run — the run escaped its workspace, "+
		"or you edited the repo while it ran. Paths: %s%s. Review them by hand; `weave pull --force` merges anyway",
		root, strings.Join(paths, ", "), suffix)
}

// weavePrintIsolationFooter explains the "!" marker under `weave list`,
// naming each escaped run and its paths. Mirrors the stale "*" footer: the
// marker in the table is the flag, the footer is what to DO about it.
func weavePrintIsolationFooter(w io.Writer, violated []*weaveItem) {
	if len(violated) == 0 {
		return
	}
	fmt.Fprintln(w, "! ISOLATION VIOLATED — the live checkout changed while these runs held their workspaces.")
	fmt.Fprintln(w, "  Their branches are not the whole diff; `weave pull` refuses them without --force.")
	for _, it := range violated {
		paths := "(paths unavailable — the live tree was already too dirty at start to diff)"
		if len(it.EscapedPaths) > 0 {
			shown := it.EscapedPaths
			extra := ""
			if len(shown) > weaveEscapedPathsMax {
				extra = fmt.Sprintf(" (+%d more)", len(shown)-weaveEscapedPathsMax)
				shown = shown[:weaveEscapedPathsMax]
			}
			paths = strings.Join(shown, ", ") + extra
		}
		fmt.Fprintf(w, "  #%d: %s\n", it.ID, paths)
	}
}

// weaveIsolationWarning is the human-facing block printed to stderr when a
// run is first found to have escaped. Loud on purpose: this is the one
// signal that the branch about to be merged is not the whole diff.
func weaveIsolationWarning(it *weaveItem) string {
	if it == nil || !it.IsolationViolated {
		return ""
	}
	return fmt.Sprintf("weave: WARNING run #%d is isolation-violated — %s\n", it.ID, weaveIsolationDetail(it))
}
