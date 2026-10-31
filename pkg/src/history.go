package src

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/state"
)

// previewRefspecs mirrors every version-bearing ref into an isolated
// preview workspace: release branches, the per-principal version lines,
// and tags.
var previewRefspecs = []config.RefSpec{
	"+refs/heads/*:refs/heads/*",
	"+refs/shaping/*:refs/shaping/*",
	"+refs/tags/*:refs/tags/*",
}

// VersionEntry is one commit on a principal's version line: the request it
// recorded, the gate verdict behind it, and the evidence it rests on.
type VersionEntry struct {
	Activation string    `json:"activation"`
	Revision   string    `json:"revision"`
	Kind       string    `json:"kind"`
	Request    string    `json:"request,omitempty"`
	Verdict    string    `json:"verdict,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
	Principal  string    `json:"principal,omitempty"`
	Author     string    `json:"author,omitempty"`
	When       time.Time `json:"when,omitempty"`
}

// NamedVersion pins a human label to one revision under the principal's
// namespace.
type NamedVersion struct {
	Name     string `json:"name"`
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
}

// ReviewVersion keeps a version line tip that an upgrade could not follow,
// named for the release it could not reach.
type ReviewVersion struct {
	ID       string `json:"id"`
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
}

// VersionHistory is one principal's version line: every recorded version
// newest-first, plus the named labels, the review versions, and the
// currently active revision. Base is the oldest restorable version: the
// shared release initially, or this principal's tidy base after compaction.
//
// Versions mirrors Entries under the name the web history view uses, so the
// app view and `src history --json` are the same history view. Entries
// stays as an alias for one release.
type VersionHistory struct {
	App       string          `json:"app"`
	Principal string          `json:"principal"`
	Line      string          `json:"line"`
	Tip       string          `json:"tip,omitempty"`
	Active    string          `json:"active,omitempty"`
	Base      string          `json:"base,omitempty"`
	Entries   []VersionEntry  `json:"entries,omitempty"`
	Versions  []VersionEntry  `json:"versions,omitempty"`
	Named     []NamedVersion  `json:"named,omitempty"`
	Reviews   []ReviewVersion `json:"reviews,omitempty"`
}

// History reads one principal's version line: who recorded each version,
// when, the request text, the verifier verdict, and the evidence reference,
// plus the named labels and review versions. A principal with no versions
// yet reports an empty line, never an error.
func History(home, app, principal string) (VersionHistory, error) {
	var out VersionHistory
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return out, fmt.Errorf("src: history needs a principal")
	}
	if principalSlug(principal) == "" {
		return out, fmt.Errorf("src: principal %q has no usable identity", principal)
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return out, err
	}
	out = VersionHistory{App: app, Principal: principal, Line: VersionLineRef(principal)}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return out, fmt.Errorf("src: open %s: %w", dir, err)
	}
	if sha, err := ygit.ResolveCommit(dir, pristineBranch); err == nil {
		out.Base = sha
	}
	tip, ok, err := refTarget(dir, out.Line)
	if err != nil {
		return out, err
	}
	if ok {
		out.Tip = tip
		out.Entries = walkLine(r, tip)
		out.Versions = out.Entries
		if len(out.Entries) > 0 {
			oldest, err := r.CommitObject(plumbing.NewHash(out.Entries[len(out.Entries)-1].Activation))
			if err == nil && oldest.NumParents() > 0 {
				parent, err := oldest.Parent(0)
				if err == nil && strings.HasPrefix(parent.Message, "Tidy base ") {
					out.Base = parent.Hash.String()
				}
			}
		}
	}
	out.Named, err = namedVersions(dir, principal)
	if err != nil {
		return out, err
	}
	out.Reviews, err = reviewVersions(dir, principal)
	if err != nil {
		return out, err
	}
	if store, err := state.OpenAt(home, app, principal); err == nil {
		if st, err := store.Read(); err == nil {
			out.Active = st.ActiveRevision
		}
	}
	return out, nil
}

// walkLine follows the version line back from tip, newest-first. Line
// commits carry the Principal trailer; the first commit without one is the
// release the line started from, where the walk stops. Revision prefers the
// Revision trailer and falls back to Request so commits written before the
// Revision trailer still resolve.
func walkLine(r *gogit.Repository, tip string) []VersionEntry {
	var out []VersionEntry
	hash := plumbing.NewHash(tip)
	for {
		c, err := r.CommitObject(hash)
		if err != nil {
			return out
		}
		if commitTrailer(c.Message, "Principal") == "" {
			return out
		}
		subject := strings.TrimSpace(strings.SplitN(c.Message, "\n", 2)[0])
		kind := "activate"
		if strings.HasPrefix(subject, "Restore ") {
			kind = "restore"
		}
		if strings.HasPrefix(subject, "Undo ") {
			kind = "undo"
		}
		if original := commitTrailer(c.Message, "Original-Kind"); original != "" {
			kind = original
		}
		revision := commitTrailer(c.Message, "Revision")
		if revision == "" {
			revision = commitTrailer(c.Message, "Request")
		}
		out = append(out, VersionEntry{
			Activation: c.Hash.String(),
			Revision:   revision,
			Kind:       kind,
			Request:    commitTrailer(c.Message, "Request"),
			Verdict:    commitTrailer(c.Message, "Verdict"),
			Evidence:   commitTrailer(c.Message, "Evidence"),
			Principal:  commitTrailer(c.Message, "Principal"),
			Author:     c.Author.Name,
			When:       c.Author.When,
		})
		if c.NumParents() == 0 {
			return out
		}
		hash = c.ParentHashes[0]
	}
}

// namespaceRefs lists every hash ref under prefix as short name to target,
// sorted by name.
func namespaceRefs(dir, prefix string) (names []string, targets map[string]string, err error) {
	targets = map[string]string{}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("src: open %s: %w", dir, err)
	}
	iter, err := r.References()
	if err != nil {
		return nil, nil, fmt.Errorf("src: list refs: %w", err)
	}
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if !strings.HasPrefix(name, prefix) || ref.Type() != plumbing.HashReference {
			return nil
		}
		short := strings.TrimPrefix(name, prefix)
		if short == "" || strings.Contains(short, "/") {
			return nil
		}
		names = append(names, short)
		targets[short] = ref.Hash().String()
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("src: list refs: %w", err)
	}
	sort.Strings(names)
	return names, targets, nil
}

func namedVersions(dir, principal string) ([]NamedVersion, error) {
	names, targets, err := namespaceRefs(dir, shapingRefRoot+principalSlug(principal)+"/named/")
	if err != nil {
		return nil, err
	}
	var out []NamedVersion
	for _, n := range names {
		out = append(out, NamedVersion{
			Name:     n,
			Ref:      NamedVersionRef(principal, n),
			Revision: targets[n],
		})
	}
	return out, nil
}

func reviewVersions(dir, principal string) ([]ReviewVersion, error) {
	names, targets, err := namespaceRefs(dir, shapingRefRoot+principalSlug(principal)+"/review/")
	if err != nil {
		return nil, err
	}
	var out []ReviewVersion
	for _, n := range names {
		out = append(out, ReviewVersion{
			ID:       n,
			Ref:      ReviewVersionRef(principal, n),
			Revision: targets[n],
		})
	}
	return out, nil
}

// Name pins label to rev under the principal's namespace: the version a
// later history, preview, diff, or restore can name instead of spelling a
// commit. It returns the new ref.
func Name(home, app, principal, rev, label string) (string, error) {
	return SetNamedVersion(home, app, principal, label, rev)
}

// resolveVersion resolves anything a principal can mean by a version: a
// commit SHA, a branch, a tag, "base" (the release line), or one of their
// own named or review labels.
func resolveVersion(dir, principal, rev string) (string, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", fmt.Errorf("src: empty version")
	}
	if rev == "base" || rev == pristineBranch {
		if principal != "" {
			h, err := History(filepath.Dir(filepath.Dir(dir)), filepath.Base(dir), principal)
			if err != nil {
				return "", err
			}
			if h.Base != "" {
				return h.Base, nil
			}
		}
		sha, err := ygit.ResolveCommit(dir, pristineBranch)
		if err != nil {
			return "", fmt.Errorf("src: no base release in this checkout (%v)", err)
		}
		return sha, nil
	}
	if sha, err := ygit.ResolveCommit(dir, rev); err == nil {
		return sha, nil
	}
	if principal != "" {
		for _, ref := range []string{NamedVersionRef(principal, rev), ReviewVersionRef(principal, rev)} {
			if tip, ok, err := refTarget(dir, ref); err == nil && ok {
				return tip, nil
			}
		}
	}
	return "", fmt.Errorf("src: unknown version %q", shortSHA(rev))
}

// Diff renders the unified diff from version a to version b for app: any
// two versions the principal can name, or the base release.
func Diff(home, app, principal, a, b string) (string, error) {
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return "", err
	}
	aSHA, err := resolveVersion(dir, principal, a)
	if err != nil {
		return "", err
	}
	bSHA, err := resolveVersion(dir, principal, b)
	if err != nil {
		return "", err
	}
	return diffText(dir, aSHA, bSHA)
}

// Preview prepares an isolated workspace at rev under
// $BASHY_HOME/src/.previews and checks that revision out there. The
// checkout the version line and the active revision live in is never
// touched, and the active revision does not move. It returns the preview
// directory.
func Preview(home, app, principal, rev string) (string, error) {
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return "", err
	}
	sha, err := resolveVersion(dir, principal, rev)
	if err != nil {
		return "", err
	}
	previewDir := filepath.Join(SrcDir(home), ".previews", app, shortSHA(sha))
	r, err := gogit.PlainOpen(previewDir)
	if err != nil {
		r, err = seedPreview(previewDir, dir)
		if err != nil {
			return "", err
		}
	} else if err := refreshPreview(r); err != nil {
		return "", err
	}
	// Guard-rail files land read-only in checkouts; loosen the preview copy
	// so checking another tree out cannot fail on file modes. The source
	// checkout keeps its own modes.
	if err := unlockRepo(previewDir, app); err != nil {
		return "", err
	}
	wt, err := r.Worktree()
	if err != nil {
		return "", fmt.Errorf("src: preview workspace: %w", err)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{Hash: plumbing.NewHash(sha)}); err != nil {
		return "", fmt.Errorf("src: preview %s: %w", shortSHA(sha), err)
	}
	return previewDir, nil
}

// seedPreview creates an isolated workspace by fetching every
// version-bearing ref from the app checkout: a plain clone would leave the
// per-principal version lines behind.
func seedPreview(previewDir, dir string) (*gogit.Repository, error) {
	if err := os.MkdirAll(previewDir, 0o755); err != nil {
		return nil, fmt.Errorf("src: preview workspace: %w", err)
	}
	r, err := gogit.PlainInit(previewDir, false)
	if err != nil {
		return nil, fmt.Errorf("src: preview workspace: %w", err)
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{dir}}); err != nil {
		_ = os.RemoveAll(previewDir)
		return nil, fmt.Errorf("src: preview workspace: %w", err)
	}
	if err := refreshPreview(r); err != nil {
		_ = os.RemoveAll(previewDir)
		return nil, err
	}
	return r, nil
}

func refreshPreview(r *gogit.Repository) error {
	err := r.Fetch(&gogit.FetchOptions{RemoteName: "origin", RefSpecs: previewRefspecs})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return fmt.Errorf("src: fetch preview refs: %w", err)
	}
	return nil
}

// featureAt reports whether rev's tree declares feature metadata and what
// it says. A tree without shape.yaml predates the contract and carries no
// constraint.
func featureAt(dir, revSHA string) (state.Feature, bool, error) {
	var f state.Feature
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return f, false, fmt.Errorf("src: open %s: %w", dir, err)
	}
	c, err := r.CommitObject(plumbing.NewHash(revSHA))
	if err != nil {
		return f, false, fmt.Errorf("src: commit %s: %w", shortSHA(revSHA), err)
	}
	tr, err := c.Tree()
	if err != nil {
		return f, false, fmt.Errorf("src: tree %s: %w", shortSHA(revSHA), err)
	}
	file, err := tr.File("shape.yaml")
	if err != nil {
		return f, false, nil
	}
	body, err := file.Contents()
	if err != nil {
		return f, false, fmt.Errorf("src: read feature metadata at %s: %w", shortSHA(revSHA), err)
	}
	f, err = state.ParseFeature([]byte(body))
	if err != nil {
		return f, false, fmt.Errorf("src: restore refused: version %s declares unreadable feature metadata (%v)",
			shortSHA(revSHA), err)
	}
	return f, true, nil
}

// dataSchemaOf declares which data-schema version a versioned tree leaves
// behind: the top of what it can read. Trees without metadata leave the
// recorded schema alone.
func dataSchemaOf(dir, revSHA string) (int, error) {
	f, ok, err := featureAt(dir, revSHA)
	if err != nil || !ok {
		return 0, err
	}
	if f.Schema.Max != 0 {
		return f.Schema.Max, nil
	}
	return f.Schema.Min, nil
}

// checkReadable refuses a restore whose target version cannot read the
// principal's current data schema, with an explanation. An unknown current
// schema, or a target that declares no range, carries no constraint.
func checkReadable(dir, targetSHA string, current int) error {
	if current == 0 {
		return nil
	}
	f, ok, err := featureAt(dir, targetSHA)
	if err != nil {
		return err
	}
	if !ok || (f.Schema.Min == 0 && f.Schema.Max == 0) {
		return nil
	}
	if (f.Schema.Min != 0 && current < f.Schema.Min) ||
		(f.Schema.Max != 0 && current > f.Schema.Max) {
		return fmt.Errorf("src: restore refused: version %s reads data schemas %d..%s but the current data schema is %d",
			shortSHA(targetSHA), f.Schema.Min, schemaMax(f.Schema.Max), current)
	}
	return nil
}

func schemaMax(max int) string {
	if max == 0 {
		return "any"
	}
	return itoaSmall(max)
}

func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// RestoreOptions tunes Restore. Gate is required and fails the restore
// closed when it refuses, exactly like Activate. Request overrides the
// recorded request line; empty derives "restore to <name or short sha>" from
// the rev argument. The undo verbs pass "undo". When overrides the commit
// time; zero means the server clock now (UTC).
type RestoreOptions struct {
	Gate    state.ActivationGate
	Request string
	When    time.Time
}

// restoreDisplay names the restore target the way the request line does: the
// caller's label when they named one, else the short SHA.
func restoreDisplay(rev, targetSHA string) string {
	rev = strings.TrimSpace(rev)
	if rev == "" || strings.EqualFold(rev, targetSHA) {
		return shortSHA(targetSHA)
	}
	if len(rev) >= 7 && len(rev) <= 64 && isHexSHA(rev) {
		return shortSHA(targetSHA)
	}
	return rev
}

func isHexSHA(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' {
			continue
		}
		return false
	}
	return true
}

// Restore returns the principal's active revision to rev through a NEW
// forward commit on the principal's version line whose tree equals rev's:
// history is never rewritten, so undoing a restore is restoring the
// pre-restore version. The candidate passes the same ActivationGate an
// activation would, then activates. Only code and config move — the
// principal data directory is untouched, and no other principal's line or
// record is affected.
//
// A target that cannot read the principal's current data schema, or a gate
// refusal, fails before anything is recorded or switched: the active
// revision stays where it was.
func Restore(home, app, rev, principal string, opts RestoreOptions) (ActivateResult, error) {
	var out ActivateResult
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return out, fmt.Errorf("src: restore needs a principal")
	}
	if principalSlug(principal) == "" {
		return out, fmt.Errorf("src: principal %q has no usable identity", principal)
	}
	if opts.Gate == nil {
		return out, fmt.Errorf("src: restore refused: no activation gate is wired")
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return out, err
	}
	targetSHA, err := resolveVersion(dir, principal, rev)
	if err != nil {
		return out, err
	}
	line := VersionLineRef(principal)
	tip, ok, err := refTarget(dir, line)
	if err != nil {
		return out, err
	}
	base := tip
	if !ok {
		if base, err = ygit.ResolveCommit(dir, pristineBranch); err != nil {
			return out, fmt.Errorf("src: restore %s: no version line and no %s branch (%v)", app, pristineBranch, err)
		}
	}
	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return out, err
	}
	st, err := store.Read()
	if err != nil {
		return out, err
	}
	if err := checkReadable(dir, targetSHA, st.DataSchema); err != nil {
		return out, err
	}
	verdict, err := opts.Gate.Check(targetSHA, base, principal)
	if err != nil {
		return out, fmt.Errorf("src: restore refused: %w", err)
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return out, fmt.Errorf("src: open %s: %w", dir, err)
	}
	target, err := r.CommitObject(plumbing.NewHash(targetSHA))
	if err != nil {
		return out, fmt.Errorf("src: commit %s: %w", shortSHA(targetSHA), err)
	}
	reqText := oneLineRequest(opts.Request)
	if reqText == "" {
		reqText = "restore to " + restoreDisplay(rev, targetSHA)
	}
	subject := fmt.Sprintf("Restore %s %s for %s", app, shortSHA(targetSHA), principal)
	if reqText == "undo" {
		subject = fmt.Sprintf("Undo %s %s for %s", app, shortSHA(targetSHA), principal)
	}
	msg := fmt.Sprintf("%s\n\nRevision: %s\nRequest: %s\nVerdict: %s\nEvidence: %s\nPrincipal: %s\n",
		subject, targetSHA, reqText, verdict.Verdict, verdict.EvidenceID, principal)
	restoration, err := recordLineCommit(dir, plumbing.NewHash(base), target.TreeHash, msg, principal, "bashy@localhost", opts.When)
	if err != nil {
		return out, err
	}
	if err := setRefTarget(dir, line, restoration); err != nil {
		return out, err
	}
	out = ActivateResult{App: app, Principal: principal, Revision: targetSHA, Activation: restoration, Previous: st.ActiveRevision}
	if st.ActiveRevision != "" && st.ActiveRevision != targetSHA {
		st.LastKnownGood = st.ActiveRevision
	}
	st.ActiveRevision = targetSHA
	if err := store.Write(st); err != nil {
		return out, err
	}
	return out, nil
}
