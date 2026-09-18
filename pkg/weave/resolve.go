package weave

// The ref resolver: weave answers `sprint:<n>` and `run:<repo-basename>-<n>` for
// the uniform addressing grammar (pkg/ref). Registration is one call the
// embedding shell makes; weave keeps resolving only its own kinds.
//
// A sprint card lives in the one global sprint store; a run lives in a per-repo
// weave queue, and the same run id (an id is queue-local) can exist in two
// checkouts that share a repo basename. So a run reference resolves only when it
// is UNAMBIGUOUS: the current checkout's queue first, then every queue on the
// machine (weave list --all's set). Exactly one match resolves; two is an error
// that names both repo paths and refuses to pick; none is ErrNotFound. Conflating
// "absent" with "cannot read" is the one thing the ref contract forbids.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
)

// RegisterRefs installs the weave resolvers on g: the sprint card and the weave
// run. Both read their stores from the environment/home the weave CLI uses, so
// there are no store options to pass.
func RegisterRefs(g *ref.Registry) {
	g.Register(ref.Sprint, ref.ResolverFunc(resolveSprint))
	g.Register(ref.Run, ref.ResolverFunc(resolveRun))
}

// resolveSprint answers sprint:<n> from the global sprint store.
func resolveSprint(id string) (ref.Node, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
	if err != nil {
		return ref.Node{}, fmt.Errorf("sprint: %q is not a sprint number", id)
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return ref.Node{}, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ref.Node{}, fmt.Errorf("weave: read sprint store %s: %w", dir, err)
	}
	s := findWeaveStory(q, n)
	if s == nil {
		return ref.Node{}, fmt.Errorf("sprint:%s: %w", id, ref.ErrNotFound)
	}
	node := ref.NewNode(ref.Sprint, strconv.FormatInt(s.ID, 10))
	node.Title = s.Title
	node.Status = s.Column // backlog|doing|done — the sprint's stage word
	node.Where = dir
	node.Open = fmt.Sprintf("bashy sprint show %d", s.ID)
	return node, nil
}

// runHit is one queue that carries the requested run.
type runHit struct {
	repo string
	item *weaveItem
}

// resolveRun answers run:<repo-basename>-<n>. The current checkout's queue is
// AUTHORITATIVE when it carries run n under that basename — the same rule
// `weave status <n>` follows, so the two agree — and only when it does not is
// the search widened to every queue on the machine, where exactly one hit
// resolves and two is an error that names both paths and refuses to pick
// (plan D7). Ordering the queues without stopping on the first would make
// "current checkout first" decorative: the gate's two same-named checkouts
// reported ambiguity from INSIDE one of them.
func resolveRun(id string) (ref.Node, error) {
	base, n, err := parseRunID(id)
	if err != nil {
		return ref.Node{}, err
	}
	var hits []runHit
	dirs, current := runQueueDirs()
	for _, dir := range dirs {
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return ref.Node{}, fmt.Errorf("weave: read queue %s: %w", dir, err)
		}
		root := strings.TrimSpace(q.Root)
		if root == "" {
			continue // a queue with no root cannot be named by basename
		}
		if filepath.Base(filepath.Clean(root)) != base {
			continue
		}
		if it := findWeaveItem(q, n); it != nil {
			hits = append(hits, runHit{repo: root, item: it})
			if dir == current {
				break // the checkout we are standing in owns the answer
			}
		}
	}
	switch len(hits) {
	case 0:
		return ref.Node{}, fmt.Errorf("run:%s: %w", id, ref.ErrNotFound)
	case 1:
		node := ref.NewNode(ref.Run, fmt.Sprintf("%s-%d", base, n))
		node.Title = hits[0].item.Title
		node.Status = hits[0].item.State
		node.Where = hits[0].repo
		node.Open = fmt.Sprintf("bashy weave status %d", hits[0].item.ID)
		return node, nil
	default:
		paths := make([]string, 0, len(hits))
		for _, h := range hits {
			paths = append(paths, h.repo)
		}
		return ref.Node{}, fmt.Errorf("run:%s is ambiguous — %d queues share basename %q: %s; cd into the repo you mean",
			id, len(hits), base, strings.Join(paths, ", "))
	}
}

// runQueueDirs lists the queue directories to consult, current checkout first,
// then every queue on the machine — deduped, since the machine-wide scan already
// enumerates the current queue when it exists on disk. current is the
// checkout's own queue dir ("" outside any repo) so the caller can stop there.
func runQueueDirs() (dirs []string, current string) {
	seen := map[string]bool{}
	add := func(d string) {
		if d != "" && !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		if root, err := weaveRepoRoot(cwd); err == nil {
			if qd, err := weaveQueueDir(root); err == nil {
				current = qd
				add(qd)
			}
		}
	}
	for _, d := range weaveAllQueueDirs() {
		add(d)
	}
	return dirs, current
}

// parseRunID splits <repo-basename>-<n> on the LAST dash — a repo basename may
// itself contain dashes, but the run number never does.
func parseRunID(id string) (base string, n int64, err error) {
	id = strings.TrimSpace(id)
	i := strings.LastIndex(id, "-")
	if i <= 0 || i == len(id)-1 {
		return "", 0, fmt.Errorf("run: %q is not <repo-basename>-<n>", id)
	}
	base = id[:i]
	n, err = strconv.ParseInt(id[i+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("run: %q is not <repo-basename>-<n>", id)
	}
	return base, n, nil
}

// ScopeLookup is the one map from a scope segment to a checkout root that the
// embedding shell hands to every store resolving scoped refs (`kb:<repo>/…`,
// `todo:<repo>/…`; ref.ScopeLookup). A scope names a checkout by BASENAME, the
// same rule `run:<repo>-<n>` uses, and the answer comes from the same place:
// the checkout we are standing in first, then every weave queue on the
// machine. Two known checkouts sharing a basename is an error naming both —
// a guess would open the wrong #N silently. No registry: weave already knows
// every repo it has run for, and a repo nothing has run in is reached by
// `cd`-ing into it (the unscoped ref).
func ScopeLookup() ref.ScopeLookup {
	return func(scope string) (string, error) {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return "", fmt.Errorf("empty scope")
		}
		if cwd, err := os.Getwd(); err == nil {
			if root, err := weaveRepoRoot(cwd); err == nil && filepath.Base(filepath.Clean(root)) == scope {
				return root, nil
			}
		}
		seen := map[string]bool{}
		var roots []string
		dirs, _ := runQueueDirs()
		for _, dir := range dirs {
			q, err := loadWeaveQueue(dir)
			if err != nil {
				continue
			}
			root := filepath.Clean(strings.TrimSpace(q.Root))
			if root == "." || filepath.Base(root) != scope || seen[root] {
				continue
			}
			if _, err := os.Stat(root); err != nil {
				continue // a queue whose checkout is gone cannot be a scope
			}
			seen[root] = true
			roots = append(roots, root)
		}
		switch len(roots) {
		case 0:
			return "", fmt.Errorf("no checkout named %q is known here (a scope is a repo basename weave has run in, or `user`); cd into the repo and use the unscoped ref", scope)
		case 1:
			return roots[0], nil
		}
		return "", fmt.Errorf("scope %q is ambiguous — %d checkouts share that basename: %s; cd into the one you mean", scope, len(roots), strings.Join(roots, ", "))
	}
}
