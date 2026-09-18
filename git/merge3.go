package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
)

// ConflictError reports the paths a 3-way merge could not reconcile.
// threeWayMerge returns it WITHOUT having mutated the repository — the
// all-or-nothing contract of `git merge` followed by `git merge
// --abort`. Callers (e.g. weave) match on it to set a conflict state
// instead of treating the merge as a generic failure.
type ConflictError struct {
	Files []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("merge conflict in %d file(s): %s", len(e.Files), strings.Join(e.Files, ", "))
}

// changeInfo is one path's net change between two trees. deleted=true
// means the path was removed; otherwise hash+mode describe the new blob.
type changeInfo struct {
	deleted bool
	hash    plumbing.Hash
	mode    filemode.FileMode
}

// collectChanges maps path → net change from base to tip. Renames
// surface as a delete of the old path plus an insert of the new one, so
// a path-keyed map is the natural representation.
func collectChanges(r *gogit.Repository, base, tip plumbing.Hash) (map[string]changeInfo, error) {
	changes, err := treeChanges(r, base, tip)
	if err != nil {
		return nil, err
	}
	out := make(map[string]changeInfo, len(changes))
	for _, ch := range changes {
		action, err := ch.Action()
		if err != nil {
			return nil, fmt.Errorf("change action: %w", err)
		}
		switch action {
		case merkletrie.Delete:
			out[ch.From.Name] = changeInfo{deleted: true}
		case merkletrie.Insert, merkletrie.Modify:
			out[ch.To.Name] = changeInfo{hash: ch.To.TreeEntry.Hash, mode: ch.To.TreeEntry.Mode}
		}
	}
	return out, nil
}

// threeWayMerge integrates theirs into ours (the checked-out branch)
// when the two histories have diverged. It computes the full merged
// state and detects every conflict BEFORE touching the index, worktree,
// or refs; on any conflict it returns a *ConflictError and leaves the
// repository untouched. On a clean merge it records a merge commit with
// parents [ours, theirs].
//
// Conflict policy: a path changed on both sides is a conflict unless the
// two sides produced an identical result, or a line-level diff3 of the
// two text edits merges cleanly (no overlapping hunks). Delete/modify
// disagreements and divergent binary edits always conflict.
func threeWayMerge(r *gogit.Repository, w *gogit.Worktree, ours, theirs plumbing.Hash, message string, sig *object.Signature) (*Result, error) {
	st, err := w.Status()
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	if !st.IsClean() {
		return nil, errors.New("merge: working tree is not clean — commit or discard local changes first")
	}

	base, err := mergeBaseHash(r, ours, theirs)
	if err != nil {
		return nil, err
	}

	oursCh, err := collectChanges(r, base, ours)
	if err != nil {
		return nil, err
	}
	theirsCh, err := collectChanges(r, base, theirs)
	if err != nil {
		return nil, err
	}

	// applyOp records one mutation to perform once the merge is known
	// clean. merged!=nil means write that blob (a diff3 result);
	// otherwise take theirs' tree entry (or delete it).
	type applyOp struct {
		name   string
		info   changeInfo
		merged []byte
	}
	var ops []applyOp
	var conflicts []string

	for name, ti := range theirsCh {
		oi, both := oursCh[name]
		if !both {
			// Only theirs touched this path — take it verbatim.
			ops = append(ops, applyOp{name: name, info: ti})
			continue
		}
		switch {
		case ti.deleted && oi.deleted:
			// Both removed it — already gone in ours; nothing to do.
		case ti.deleted != oi.deleted:
			// One deleted while the other modified — irreconcilable.
			conflicts = append(conflicts, name)
		case ti.hash == oi.hash && ti.mode == oi.mode:
			// Both made the identical change — ours already has it.
		default:
			// Both modified differently — try a line-level diff3.
			merged, ok, err := tryDiff3(r, base, name, oi, ti)
			if err != nil {
				return nil, err
			}
			if !ok {
				conflicts = append(conflicts, name)
				continue
			}
			ops = append(ops, applyOp{name: name, info: changeInfo{mode: oi.mode}, merged: merged})
		}
	}

	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, &ConflictError{Files: conflicts}
	}

	// No conflicts — apply theirs-only and diff3-merged changes onto the
	// (clean, at-ours) worktree and index.
	idx, err := r.Storer.Index()
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	root := w.Filesystem.Root()
	for _, op := range ops {
		switch {
		case op.merged != nil:
			h, size, err := writeBytesBlob(r, root, op.name, op.merged, op.info.mode)
			if err != nil {
				return nil, err
			}
			upsertIndexEntry(idx, op.name, h, op.info.mode, size)
		case op.info.deleted:
			full := filepath.Join(root, filepath.FromSlash(op.name))
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove %s: %w", op.name, err)
			}
			if _, err := idx.Remove(op.name); err != nil && !errors.Is(err, index.ErrEntryNotFound) {
				return nil, fmt.Errorf("index remove %s: %w", op.name, err)
			}
			removeEmptyParents(root, op.name)
		default:
			if op.info.mode == filemode.Submodule {
				return nil, fmt.Errorf("merge touches submodule %q, which is not supported", op.name)
			}
			size, err := writeBlobToWorktree(r, root, op.name, object.TreeEntry{Mode: op.info.mode, Hash: op.info.hash})
			if err != nil {
				return nil, err
			}
			upsertIndexEntry(idx, op.name, op.info.hash, op.info.mode, size)
		}
	}
	if err := r.Storer.SetIndex(idx); err != nil {
		return nil, fmt.Errorf("write index: %w", err)
	}

	// Record the merge commit from the merged index, with both parents.
	commit, err := w.Commit(message, &gogit.CommitOptions{
		Author:            sig,
		Committer:         sig,
		Parents:           []plumbing.Hash{ours, theirs},
		AllowEmptyCommits: true,
	})
	if err != nil {
		return nil, fmt.Errorf("merge commit: %w", err)
	}
	return &Result{
		Success: true,
		Message: fmt.Sprintf("Merge made by the 3-way strategy.\n %s", shortHash(commit)),
	}, nil
}

// mergeBaseHash returns the first merge base of two commits, erroring
// when they share no common ancestor (unrelated histories).
func mergeBaseHash(r *gogit.Repository, a, b plumbing.Hash) (plumbing.Hash, error) {
	ca, err := r.CommitObject(a)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit %s: %w", a, err)
	}
	cb, err := r.CommitObject(b)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit %s: %w", b, err)
	}
	bases, err := ca.MergeBase(cb)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("merge-base: %w", err)
	}
	if len(bases) == 0 {
		return plumbing.ZeroHash, errors.New("merge: the branches have no common ancestor — refusing to merge unrelated histories")
	}
	if len(bases) > 1 {
		// Criss-cross history: real git synthesizes a virtual base
		// (recursive merge). Picking one base arbitrarily could silently
		// produce a wrong merge, so refuse rather than guess — the caller
		// can fall back to full git. (weave's linear sequential merges
		// don't create this shape.)
		return plumbing.ZeroHash, errors.New("merge: multiple merge bases (criss-cross history) — the pure-Go 3-way merge cannot synthesize a virtual base; reconcile with full git")
	}
	return bases[0].Hash, nil
}

// upsertIndexEntry sets (or creates) the index entry for name to the
// given blob, mirroring how ffUpdate stages a fast-forwarded change.
func upsertIndexEntry(idx *index.Index, name string, hash plumbing.Hash, mode filemode.FileMode, size int64) {
	entry, err := idx.Entry(name)
	if err != nil {
		entry = idx.Add(name)
	}
	entry.Hash = hash
	entry.Mode = mode
	entry.Size = uint32(size)
	now := time.Now()
	entry.ModifiedAt = now
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
}

// writeBytesBlob stores data as a blob object, materializes it under
// root, and returns the blob hash and size. Used for diff3-merged
// content that exists in neither input tree.
func writeBytesBlob(r *gogit.Repository, root, name string, data []byte, mode filemode.FileMode) (plumbing.Hash, int64, error) {
	obj := r.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	obj.SetSize(int64(len(data)))
	wr, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("blob writer: %w", err)
	}
	if _, err := wr.Write(data); err != nil {
		wr.Close()
		return plumbing.ZeroHash, 0, fmt.Errorf("write blob: %w", err)
	}
	if err := wr.Close(); err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("close blob: %w", err)
	}
	h, err := r.Storer.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("store blob: %w", err)
	}
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("mkdir for %s: %w", name, err)
	}
	osMode, err := mode.ToOSFileMode()
	if err != nil {
		osMode = 0o644
	}
	if err := os.WriteFile(full, data, osMode.Perm()); err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("write %s: %w", name, err)
	}
	// WriteFile leaves an existing file's mode untouched; chmod so a
	// merged file ends up with the intended permissions.
	if err := os.Chmod(full, osMode.Perm()); err != nil {
		return plumbing.ZeroHash, 0, fmt.Errorf("chmod %s: %w", name, err)
	}
	return h, int64(len(data)), nil
}

// tryDiff3 attempts a line-level 3-way merge of one path that both
// sides modified. Returns (merged, true, nil) on a clean merge,
// (nil, false, nil) when it conflicts or cannot be attempted (binary
// content, base lacks the path), and a non-nil error only on a real
// object-store failure.
func tryDiff3(r *gogit.Repository, base plumbing.Hash, name string, ours, theirs changeInfo) ([]byte, bool, error) {
	baseData, err := blobBytesAtPath(r, base, name)
	if err != nil {
		// Base has no such path (both sides added it differently) — a
		// genuine add/add conflict; nothing to 3-way against.
		return nil, false, nil
	}
	oursData, err := blobBytes(r, ours.hash)
	if err != nil {
		return nil, false, err
	}
	theirsData, err := blobBytes(r, theirs.hash)
	if err != nil {
		return nil, false, err
	}
	if isBinary(baseData) || isBinary(oursData) || isBinary(theirsData) {
		return nil, false, nil
	}
	merged, ok := diff3Merge(baseData, oursData, theirsData)
	if !ok {
		return nil, false, nil
	}
	return merged, true, nil
}

// blobBytes reads a blob object's full contents.
func blobBytes(r *gogit.Repository, h plumbing.Hash) ([]byte, error) {
	blob, err := r.BlobObject(h)
	if err != nil {
		return nil, fmt.Errorf("blob %s: %w", h, err)
	}
	rd, err := blob.Reader()
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", h, err)
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

// blobBytesAtPath reads the contents of one path in a commit's tree.
func blobBytesAtPath(r *gogit.Repository, commitHash plumbing.Hash, path string) ([]byte, error) {
	c, err := r.CommitObject(commitHash)
	if err != nil {
		return nil, err
	}
	t, err := c.Tree()
	if err != nil {
		return nil, err
	}
	f, err := t.File(path)
	if err != nil {
		return nil, err
	}
	rd, err := f.Reader()
	if err != nil {
		return nil, err
	}
	defer rd.Close()
	return io.ReadAll(rd)
}

// isBinary reports whether data looks binary (contains a NUL byte), the
// same cheap heuristic git uses to refuse a textual merge.
func isBinary(data []byte) bool {
	return slices.Contains(data, 0)
}

// hunk is one contiguous edit of base[baseStart:baseEnd] (a deletion or
// replacement range; baseStart==baseEnd is a pure insertion point)
// replaced by repl.
type hunk struct {
	baseStart int
	baseEnd   int
	repl      []string
}

// diffHunks expresses base→side as a list of non-overlapping edit hunks
// in ascending base order, derived from the LCS of lines.
func diffHunks(base, side []string) []hunk {
	m := matchMap(base, side) // base index → side index for unchanged lines
	matched := make([]int, 0, len(m))
	for b := range m {
		matched = append(matched, b)
	}
	sort.Ints(matched)

	var hunks []hunk
	emit := func(b0, b1, s0, s1 int) {
		if b0 == b1 && s0 == s1 {
			return // no change
		}
		hunks = append(hunks, hunk{baseStart: b0, baseEnd: b1, repl: append([]string(nil), side[s0:s1]...)})
	}
	bPrev, sPrev := 0, 0
	for _, b := range matched {
		s := m[b]
		emit(bPrev, b, sPrev, s)
		bPrev, sPrev = b+1, s+1
	}
	emit(bPrev, len(base), sPrev, len(side))
	return hunks
}

// rangesOverlap reports whether two base-coordinate hunks touch the same
// region — in which case both sides edited it and the merge must treat
// them together (conflict unless the edits are identical).
//
// Insertions are zero-width, so plain half-open intersection misses an
// insertion landing adjacent to (or inside) the other side's delete or
// modify — which would otherwise let one side's deletion be silently
// undone by the other's nearby insertion. An insertion at point p
// therefore conflicts with any hunk whose range [s,e] satisfies
// s <= p <= e (inclusive of both boundaries). This is deliberately
// conservative: it may report a conflict where git's recursive strategy
// would auto-merge an insertion immediately abutting a change, but it
// never produces a silent wrong merge.
func rangesOverlap(a, b hunk) bool {
	aIns := a.baseStart == a.baseEnd
	bIns := b.baseStart == b.baseEnd
	switch {
	case aIns && bIns:
		return a.baseStart == b.baseStart
	case aIns:
		return a.baseStart >= b.baseStart && a.baseStart <= b.baseEnd
	case bIns:
		return b.baseStart >= a.baseStart && b.baseStart <= a.baseEnd
	default:
		return a.baseStart < b.baseEnd && b.baseStart < a.baseEnd
	}
}

// diff3Merge performs a line-level 3-way merge of base/ours/theirs.
// Lines retain their trailing newline so the join reproduces the inputs
// exactly. It diffs each side against base into hunks and applies them
// in base order: non-overlapping edits from both sides merge cleanly;
// an overlapping region conflicts unless both sides made the identical
// edit. Returns (nil, false) on conflict.
func diff3Merge(baseB, oursB, theirsB []byte) ([]byte, bool) {
	base := splitLines(baseB)
	ours := splitLines(oursB)
	theirs := splitLines(theirsB)

	oh := diffHunks(base, ours)
	th := diffHunks(base, theirs)

	var out []string
	bi, oi, ti := 0, 0, 0
	for oi < len(oh) || ti < len(th) {
		var O, T *hunk
		if oi < len(oh) {
			O = &oh[oi]
		}
		if ti < len(th) {
			T = &th[ti]
		}

		// Apply the hunk that begins earliest; the other side's current
		// hunk is the only one that could overlap it (hunks are sorted and
		// non-overlapping within a side).
		useOurs := T == nil || (O != nil && O.baseStart <= T.baseStart)
		cur, other := O, T
		if !useOurs {
			cur, other = T, O
		}

		// Copy unchanged base lines up to this hunk.
		for bi < cur.baseStart {
			out = append(out, base[bi])
			bi++
		}

		if other != nil && rangesOverlap(*cur, *other) {
			// Both sides edit the same region: clean only if it's the
			// identical edit, otherwise a genuine conflict.
			if cur.baseStart == other.baseStart && cur.baseEnd == other.baseEnd && equalLines(cur.repl, other.repl) {
				out = append(out, cur.repl...)
				bi = cur.baseEnd
				oi++
				ti++
				continue
			}
			return nil, false
		}

		out = append(out, cur.repl...)
		bi = cur.baseEnd
		if useOurs {
			oi++
		} else {
			ti++
		}
	}
	// Copy any trailing unchanged base lines.
	for bi < len(base) {
		out = append(out, base[bi])
		bi++
	}
	return []byte(strings.Join(out, "")), true
}

// matchMap returns base-index → other-index for the longest common
// subsequence of lines between base and other.
func matchMap(base, other []string) map[int]int {
	n, m := len(base), len(other)
	// dp[i][j] = LCS length of base[i:] and other[j:].
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if base[i] == other[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	matches := map[int]int{}
	i, j := 0, 0
	for i < n && j < m {
		if base[i] == other[j] {
			matches[i] = j
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			i++
		} else {
			j++
		}
	}
	return matches
}

// splitLines splits b into lines that each keep their trailing newline
// (the final line keeps none if b doesn't end in '\n'), so a plain
// concatenation reconstructs b byte-for-byte.
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := string(b)
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i+1])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
