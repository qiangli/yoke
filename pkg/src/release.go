package src

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/verify"
	"golang.org/x/mod/semver"
)

// Releases of a shaped app are semver tags (vX.Y.Z) in its src history.
// Per-principal version lines stay internal; a tagged release is what
// offerings publish and what other apps depend on. The bump follows the
// change kind, derived from the B6 surface compare against the previous
// release rather than declared by the candidate:
//
//	patch  a repair: the same declared surface, the caller says --repair
//	minor  an additive enhancement: no declared element removed or renamed
//	major  the explicit contract step: a caller-approved removal or rename
//
// A removal or rename the caller has not approved refuses the release
// outright, so no breaking change ships without a major version. Untagged
// versions are named by Go-style pseudo-versions
// (v0.0.0-<UTC yyyymmddhhmmss>-<12-hex>), the same on every OS.

// Release kinds, as recorded in the tag's Kind trailer.
const (
	ReleaseInitial = "initial"
	ReleasePatch   = "patch"
	ReleaseMinor   = "minor"
	ReleaseMajor   = "major"
)

// FirstRelease is the version the first release of an app receives.
const FirstRelease = "v1.0.0"

// ReleaseTag is one published release: its semver tag, the commit it
// names, the kind it was released as, and when.
type ReleaseTag struct {
	Version  string    `json:"version"`
	Revision string    `json:"revision"`
	Kind     string    `json:"kind,omitempty"`
	When     time.Time `json:"when,omitempty"`
}

// ReleaseOptions tunes Release. Repair declares the candidate a repair:
// same declared surface, patch bump. Contract lists the surface elements
// (verify.Surface keys such as "property:title") the caller explicitly
// approved dropping: the separate contract step that turns a removal or
// rename into a major release. When overrides the tag time; zero means the
// server clock now (UTC).
type ReleaseOptions struct {
	Repair   bool
	Contract []string
	When     time.Time
}

// ReleaseResult reports one release: the version it received, the previous
// release it was compared against, the kind of bump, and the surface
// elements the compare found added or (with approval) dropped.
type ReleaseResult struct {
	App        string   `json:"app"`
	Version    string   `json:"version"`
	Previous   string   `json:"previous,omitempty"`
	Revision   string   `json:"revision"`
	Kind       string   `json:"kind"`
	Added      []string `json:"added,omitempty"`
	Contracted []string `json:"contracted,omitempty"`
}

// isReleaseTag reports whether name is a canonical release tag: vX.Y.Z,
// no prerelease, no build metadata.
func isReleaseTag(name string) bool {
	return semver.IsValid(name) && semver.Canonical(name) == name &&
		semver.Prerelease(name) == "" && semver.Build(name) == ""
}

// releaseTags lists every release tag in the repo at r, newest version
// first. Annotated tags are peeled to the commit they name; a tag that
// does not name a commit is not a release.
func releaseTags(r *gogit.Repository) ([]ReleaseTag, error) {
	iter, err := r.Tags()
	if err != nil {
		return nil, fmt.Errorf("src: list tags: %w", err)
	}
	defer iter.Close()
	var out []ReleaseTag
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if !isReleaseTag(name) {
			return nil
		}
		rt := ReleaseTag{Version: name}
		if tag, terr := r.TagObject(ref.Hash()); terr == nil {
			c, cerr := tag.Commit()
			if cerr != nil {
				return nil
			}
			rt.Revision = c.Hash.String()
			rt.Kind = commitTrailer(tag.Message, "Kind")
			rt.When = tag.Tagger.When.UTC()
		} else {
			c, cerr := r.CommitObject(ref.Hash())
			if cerr != nil {
				return nil
			}
			rt.Revision = c.Hash.String()
			rt.When = c.Committer.When.UTC()
		}
		out = append(out, rt)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("src: list tags: %w", err)
	}
	sortReleases(out)
	return out, nil
}

// sortReleases orders release tags newest version first.
func sortReleases(tags []ReleaseTag) {
	for i := 1; i < len(tags); i++ {
		for j := i; j > 0 && semver.Compare(tags[j-1].Version, tags[j].Version) < 0; j-- {
			tags[j-1], tags[j] = tags[j], tags[j-1]
		}
	}
}

// releasedAs maps every released commit to its highest release tag.
func releasedAs(tags []ReleaseTag) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		if _, seen := out[t.Revision]; !seen {
			out[t.Revision] = t.Version
		}
	}
	return out
}

// Releases lists an app's published releases, newest version first.
func Releases(home, app string) ([]ReleaseTag, error) {
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return nil, err
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return nil, fmt.Errorf("src: open %s: %w", dir, err)
	}
	return releaseTags(r)
}

// pseudoVersion names an unreleased commit the way Go names an untagged
// module revision: v0.0.0-<UTC commit time yyyymmddhhmmss>-<12-hex>. The
// commit time is the committer's, so the same commit names the same
// version on every host.
func pseudoVersion(c *object.Commit) string {
	return "v0.0.0-" + c.Committer.When.UTC().Format("20060102150405") + "-" + c.Hash.String()[:12]
}

// Version names a revision of app: its release tag when the commit was
// released, else a Go-style pseudo-version. The boolean reports whether
// the revision is a published release.
func Version(home, app, rev string) (string, bool, error) {
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return "", false, err
	}
	revSHA, err := ygit.ResolveCommit(dir, rev)
	if err != nil {
		return "", false, fmt.Errorf("src: version %s: unknown revision %q", app, shortSHA(rev))
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return "", false, fmt.Errorf("src: open %s: %w", dir, err)
	}
	return versionOf(r, revSHA)
}

// versionOf is Version over an open repository and a resolved commit.
func versionOf(r *gogit.Repository, revSHA string) (string, bool, error) {
	tags, err := releaseTags(r)
	if err != nil {
		return "", false, err
	}
	if v, ok := releasedAs(tags)[revSHA]; ok {
		return v, true, nil
	}
	c, err := r.CommitObject(plumbing.NewHash(revSHA))
	if err != nil {
		return "", false, fmt.Errorf("src: commit %s: %w", shortSHA(revSHA), err)
	}
	return pseudoVersion(c), false, nil
}

// bump returns the next version after prev for kind.
func bump(prev, kind string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(prev, "v"), ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("src: release tag %q is not vX.Y.Z", prev)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil {
			return "", fmt.Errorf("src: release tag %q is not vX.Y.Z", prev)
		}
		n[i] = v
	}
	switch kind {
	case ReleaseMajor:
		n = [3]int{n[0] + 1, 0, 0}
	case ReleaseMinor:
		n = [3]int{n[0], n[1] + 1, 0}
	case ReleasePatch:
		n[2]++
	default:
		return "", fmt.Errorf("src: unknown release kind %q", kind)
	}
	return fmt.Sprintf("v%d.%d.%d", n[0], n[1], n[2]), nil
}

// exportTree writes the tree of commit sha into a fresh temporary
// directory, so the surface reader sees it the way it sees a checkout.
// Only regular files are written; the caller removes the directory.
func exportTree(r *gogit.Repository, sha string) (string, error) {
	c, err := r.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return "", fmt.Errorf("src: commit %s: %w", shortSHA(sha), err)
	}
	tr, err := c.Tree()
	if err != nil {
		return "", fmt.Errorf("src: tree %s: %w", shortSHA(sha), err)
	}
	dir, err := os.MkdirTemp("", "shape-release-")
	if err != nil {
		return "", err
	}
	err = tr.Files().ForEach(func(f *object.File) error {
		if !f.Mode.IsRegular() {
			return nil
		}
		body, err := f.Contents()
		if err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(f.Name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(body), 0o644)
	})
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("src: export %s: %w", shortSHA(sha), err)
	}
	return dir, nil
}

// compareReleases reads the declared surfaces of the previous release and
// the candidate and compares them, with approved listing the elements the
// caller agreed to drop.
func compareReleases(r *gogit.Repository, prevSHA, candSHA string, approved []string) (verify.Compat, error) {
	baseDir, err := exportTree(r, prevSHA)
	if err != nil {
		return verify.Compat{}, err
	}
	defer os.RemoveAll(baseDir)
	candDir, err := exportTree(r, candSHA)
	if err != nil {
		return verify.Compat{}, err
	}
	defer os.RemoveAll(candDir)
	base, err := verify.ReadSurface(baseDir)
	if err != nil {
		return verify.Compat{}, err
	}
	cand, err := verify.ReadSurface(candDir)
	if err != nil {
		return verify.Compat{}, err
	}
	return verify.CompareSurface(base, cand, approved), nil
}

func quoteAll(items []string) string {
	q := make([]string, len(items))
	for i, it := range items {
		q[i] = strconv.Quote(it)
	}
	return strings.Join(q, " ")
}

// Release tags rev of app as its next semver release, bumping from the
// change kind: the first release is v1.0.0; after that the candidate's
// declared surface is compared against the previous release — a
// caller-approved drop (opts.Contract) is a major bump, a repair
// (opts.Repair, same surface) a patch bump, and any other change a minor
// bump. A removal or rename without approval, a repair that adds surface,
// a revision already released, or a tree identical to the previous
// release are refused and nothing is tagged. The tag is annotated with the
// kind and the compare result; principal is recorded as the tagger.
func Release(home, app, rev, principal string, opts ReleaseOptions) (ReleaseResult, error) {
	var out ReleaseResult
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return out, fmt.Errorf("src: release needs a principal")
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return out, err
	}
	revSHA, err := ygit.ResolveCommit(dir, rev)
	if err != nil {
		return out, fmt.Errorf("src: release %s: unknown revision %q", app, shortSHA(rev))
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return out, fmt.Errorf("src: open %s: %w", dir, err)
	}
	tags, err := releaseTags(r)
	if err != nil {
		return out, err
	}
	if v, ok := releasedAs(tags)[revSHA]; ok {
		return out, fmt.Errorf("src: release refused: %s is already released as %s", shortSHA(revSHA), v)
	}
	out = ReleaseResult{App: app, Revision: revSHA}
	if len(tags) == 0 {
		out.Version, out.Kind = FirstRelease, ReleaseInitial
	} else {
		prev := tags[0]
		out.Previous = prev.Version
		prevTree, candTree, err := treesOf(r, prev.Revision, revSHA)
		if err != nil {
			return out, err
		}
		if prevTree == candTree {
			return out, fmt.Errorf("src: release refused: %s has the same tree as %s (%s); nothing changed to release",
				shortSHA(revSHA), prev.Version, shortSHA(prev.Revision))
		}
		compat, err := compareReleases(r, prev.Revision, revSHA, opts.Contract)
		if err != nil {
			return out, err
		}
		if compat.Failed() {
			return out, fmt.Errorf("src: release refused: %s Release it as a major version through the contract step: --contract %s",
				compat.Detail(), quoteAll(compat.Removed))
		}
		out.Added, out.Contracted = compat.Added, compat.Contracted
		switch {
		case len(compat.Contracted) > 0:
			out.Kind = ReleaseMajor
		case opts.Repair && len(compat.Added) > 0:
			return out, fmt.Errorf("src: release refused: a repair keeps the declared surface, but %s adds %s; release it as an enhancement (minor) instead",
				shortSHA(revSHA), quoteAll(compat.Added))
		case opts.Repair:
			out.Kind = ReleasePatch
		default:
			out.Kind = ReleaseMinor
		}
		if out.Version, err = bump(prev.Version, out.Kind); err != nil {
			return out, err
		}
	}
	when := opts.When
	if when.IsZero() {
		when = time.Now()
	}
	when = when.UTC()
	msg := fmt.Sprintf("Release %s %s (%s)\n\nRevision: %s\nKind: %s\nPrevious: %s\nAdded: %s\nContracted: %s\nPrincipal: %s\n",
		app, out.Version, out.Kind, revSHA, out.Kind, out.Previous,
		strings.Join(out.Added, ","), strings.Join(out.Contracted, ","), principal)
	_, err = r.CreateTag(out.Version, plumbing.NewHash(revSHA), &gogit.CreateTagOptions{
		Tagger:  &object.Signature{Name: principal, Email: "bashy@localhost", When: when},
		Message: msg,
	})
	if err != nil {
		return out, fmt.Errorf("src: release %s %s: %w", app, out.Version, err)
	}
	return out, nil
}

// treesOf returns the tree hashes of two commits.
func treesOf(r *gogit.Repository, a, b string) (string, string, error) {
	ca, err := r.CommitObject(plumbing.NewHash(a))
	if err != nil {
		return "", "", fmt.Errorf("src: commit %s: %w", shortSHA(a), err)
	}
	cb, err := r.CommitObject(plumbing.NewHash(b))
	if err != nil {
		return "", "", fmt.Errorf("src: commit %s: %w", shortSHA(b), err)
	}
	return ca.TreeHash.String(), cb.TreeHash.String(), nil
}
