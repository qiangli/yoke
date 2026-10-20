package weave

// WRAP-UP SCOPE — what a closing sprint is actually answerable for.
//
// Closing checks dirt and stale pins per linked checkout, which is right until
// the checkout is shared. Sprint 454 ended with every story accepted and the
// gate green, and could not close: another live session's uncommitted files sat
// in a checkout 454 linked, and two sibling pins trailed commits other sprints
// had made in deps 454 never touched. Neither was 454's to fix — the only exit
// was `stop --force`, recorded as an unclean close. Sprint 476 hit it again.
//
// A refusal a sprint cannot answer is worse than no check: it teaches
// conductors to --force habitually, and then the check means nothing for the
// case it was built for — a sprint walking away from its OWN unfinished work.
//
// So wrap-up scopes itself, from evidence the sprint already records rather
// than a new tracking store: delivery commits carry `Sprint: #N`, so the paths
// a sprint worked on in a checkout are readable from its own history. Dirt
// outside that footprint is a WARNING naming the owner where the trailers know
// it; dirt inside it still refuses. Same for a stale pin: it belongs to
// whoever produces in the sibling.
//
// Containment in a directory counts only where the path has no history of its
// own — see sprintFootprint for why a tracked file is attributed path-level
// only.
//
// Every way this can fail degrades to the old behavior. No commits of ours in
// a checkout means no footprint to scope by — the whole checkout stays ours,
// which is also the right answer, because a sprint with nothing committed is
// exactly the one whose work is all still uncommitted. A git log that cannot
// be read, a path git had to quote, a window that does not reach back far
// enough: all leave the path unclaimed by us, and unclaimed means refused.

import (
	"path"
	"strconv"
	"strings"
)

// sprintFootprintWindow bounds the history read for a footprint. Sprint
// numbers are unique and monotonic, so an older commit carrying our trailer is
// genuinely ours and the window is only about cost; a sprint whose commits
// fall outside it reads as having no footprint, which refuses as before.
const sprintFootprintWindow = "2000"

// sprintFootprint is which sprints' delivery commits touched which paths in
// one checkout: the files they changed, and the directories those files live
// in.
//
// The two are not interchangeable, and which one answers depends on the dirty
// path. A TRACKED path has a history of its own — git knows exactly which
// commits touched it — so containment adds no evidence and costs correctness:
// every sprint commits its plan doc straight into the umbrella's docs/, so the
// directory rule would make docs/ "ours" and hand us another live session's
// modified docs/sprint-479-plan.md, which is the Sprint 454 shape this scoping
// exists to remove. An UNTRACKED path has no history at all, and then the
// directory holding it is the only evidence there is: a new file of a sprint's
// own work lands beside its committed work.
//
// The repo root is deliberately not a directory claim either. Nearly every
// sprint commits a root-level file (go.mod alone would do it), so claiming the
// root directory would hand each sprint everyone else's root-level dirt.
type sprintFootprint struct {
	paths   map[string]map[int64]bool
	dirs    map[string]map[int64]bool
	sprints map[int64]bool
}

// sprintCommitFootprint reads the delivery history of the tree at root.
//
// -m --first-parent counts a merge as the change it brought in, so a sprint
// delivered by merging a worker branch has the same footprint as one that
// committed directly.
//
// A subdirectory walk is pathspec-limited, because a sibling module living
// INSIDE a repo (`replace => ./pkg/oci`) raises a question about that
// directory, not about every commit in the enclosing repo. A repo root is
// walked WITHOUT a pathspec: `--` would also drop commits that changed no
// file, and an empty delivery commit still says this sprint worked here.
//
// Claimed paths are repo-relative, as git reports them, so path lookups are
// meaningful only when root is the repo root — which is how the dirt check
// calls it. The pin check asks only whether the sprint delivered here at all.
func sprintCommitFootprint(root string) *sprintFootprint {
	fp := &sprintFootprint{
		paths:   map[string]map[int64]bool{},
		dirs:    map[string]map[int64]bool{},
		sprints: map[int64]bool{},
	}
	args := []string{"-c", "core.quotePath=false", "log", "-m", "--first-parent",
		"--max-count=" + sprintFootprintWindow, "--name-only",
		"--format=%x00%(trailers:key=Sprint,valueonly=true,separator=;)"}
	if prefix, err := gitOutput(root, "rev-parse", "--show-prefix"); err == nil && strings.TrimSpace(prefix) != "" {
		args = append(args, "--", ".")
	}
	out, err := gitOutput(root, args...)
	if err != nil {
		return fp
	}
	for _, record := range strings.Split(out, "\x00") {
		trailer, files, _ := strings.Cut(record, "\n")
		ids := sprintTrailerIDs(trailer)
		if len(ids) == 0 {
			continue
		}
		for _, id := range ids {
			fp.sprints[id] = true
		}
		for _, name := range strings.Split(files, "\n") {
			name = strings.TrimSpace(name)
			// A quoted path cannot be compared with the NUL-separated paths
			// `status -z` reports, so leaving it unclaimed is the honest answer.
			if name == "" || strings.HasPrefix(name, `"`) {
				continue
			}
			fp.claim(fp.paths, name, ids)
			if dir := path.Dir(name); dir != "." && dir != "/" {
				fp.claim(fp.dirs, dir, ids)
			}
		}
	}
	return fp
}

func (fp *sprintFootprint) claim(into map[string]map[int64]bool, key string, ids []int64) {
	owners := into[key]
	if owners == nil {
		owners = map[int64]bool{}
		into[key] = owners
	}
	for _, id := range ids {
		owners[id] = true
	}
}

// records reports that this sprint delivered something here, which is what
// makes its footprint usable as a scope at all.
func (fp *sprintFootprint) records(id int64) bool { return fp.sprints[id] }

// owners names the sprints whose commits touched rel. Only when rel is
// untracked — and so has no history to read — does the directory holding it
// stand in for one.
func (fp *sprintFootprint) owners(rel string, untracked bool) map[int64]bool {
	rel = strings.TrimPrefix(path.Clean(strings.ReplaceAll(rel, `\`, "/")), "./")
	out := map[int64]bool{}
	for id := range fp.paths[rel] {
		out[id] = true
	}
	if dir := path.Dir(rel); untracked && dir != "." && dir != "/" {
		for id := range fp.dirs[dir] {
			out[id] = true
		}
	}
	return out
}

// touched reports whether rel is inside this sprint's own footprint.
func (fp *sprintFootprint) touched(id int64, rel string, untracked bool) bool {
	return fp.owners(rel, untracked)[id]
}

// sprintTrailerIDs reads the sprint numbers out of a `Sprint:` trailer value.
func sprintTrailerIDs(value string) []int64 {
	var ids []int64
	for _, field := range strings.Split(value, ";") {
		field = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(field), "#"))
		if field == "" {
			continue
		}
		if id, err := strconv.ParseInt(field, 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

// sprintStalePinMovers names the sprint that last moved a sibling's HEAD,
// which is the commit a trailing pin does not yet include and so the most
// useful thing to tell whoever reads the warning. An untrailered HEAD names
// nobody, and the warning says unattributed rather than guessing.
func sprintStalePinMovers(sibling string) map[int64]bool {
	out := map[int64]bool{}
	head, err := gitOutput(sibling, "log", "-1", "--format=%(trailers:key=Sprint,valueonly=true,separator=;)")
	if err != nil {
		return out
	}
	for _, id := range sprintTrailerIDs(head) {
		out[id] = true
	}
	return out
}

// sprintTouchedAny reports whether any of one status entry's paths is ours. A
// rename carries both its new and its original path and either one being in
// our footprint makes the rename ours; a rename is tracked on both sides, so
// untracked is false for it as for every other entry git has a history for.
func sprintTouchedAny(fp *sprintFootprint, id int64, paths []string, untracked bool) bool {
	for _, rel := range paths {
		if fp.touched(id, rel, untracked) {
			return true
		}
	}
	return false
}

// sprintOwnsRootExactly reports that abs IS a checkout s links, not a path
// inside one. The state of the checkout itself — its gitlink in the parent —
// is repo-level, and linking the repo is the evidence for it; only paths
// inside a checkout have path-level evidence to scope by.
func sprintOwnsRootExactly(roots []sprintOwnedRoot, abs string, id int64) bool {
	for _, owned := range roots {
		if owned.id == id && owned.path == abs {
			return true
		}
	}
	return false
}
