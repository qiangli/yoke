package src

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	ygit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/shape/state"
)

// Per-principal version lines live inside the app's own src checkout, one
// line per principal. Named versions and review versions share the same
// per-principal namespace:
//
//	refs/shaping/<principal>/line            the version line; every
//	                                          activation appends a commit
//	refs/shaping/<principal>/named/<name>     a named version (a later story
//	                                          owns naming)
//	refs/shaping/<principal>/review/<id>      a version kept for review after
//	                                          an upgrade conflict
//
// <principal> is the slug form below. The line is ordinary git history, so
// concurrent principals never share a ref and concurrent activations never
// touch each other; the state record (active revision plus last-known-good)
// switches separately through the atomic per-pair store.
const shapingRefRoot = "refs/shaping/"

// principalSlug maps a principal to its ref-namespace segment: lowercase,
// runs of anything outside a-z0-9 become one dash, edges trimmed. It is a
// display slug, not a secret: "device:alice" and "dhnt:agent/007" become
// "device-alice" and "dhnt-agent-007".
func principalSlug(principal string) string {
	s := strings.ToLower(strings.TrimSpace(principal))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteByte(byte(r))
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 128 {
		out = strings.Trim(out[:128], "-")
	}
	return out
}

// VersionLineRef returns the version-line ref for a principal.
func VersionLineRef(principal string) string {
	return shapingRefRoot + principalSlug(principal) + "/line"
}

// NamedVersionRef returns the named-version ref for a principal.
func NamedVersionRef(principal, name string) string {
	return shapingRefRoot + principalSlug(principal) + "/named/" + name
}

// ReviewVersionRef returns the review-version ref for a principal.
func ReviewVersionRef(principal, id string) string {
	return shapingRefRoot + principalSlug(principal) + "/review/" + id
}

// validVersionName rejects names that could escape the version's ref
// segment. Names are single path segments; history and restore (a later
// story) build on them.
func validVersionName(name string) error {
	if name == "" || len(name) > 128 {
		return fmt.Errorf("src: invalid version name %q", name)
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return fmt.Errorf("src: invalid version name %q", name)
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("src: invalid version name %q", name)
	}
	return nil
}

// PrincipalLine is one principal's version line: the principal as recorded
// on the line, its namespace slug, and the line tip commit.
type PrincipalLine struct {
	Principal string
	Slug      string
	Tip       string
}

// PrincipalLines lists every per-principal version line in the repo at dir,
// sorted by slug. The principal is read from the tip commit's trailers and
// falls back to the slug when the tip carries none.
func PrincipalLines(dir string) ([]PrincipalLine, error) {
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return nil, fmt.Errorf("src: open %s: %w", dir, err)
	}
	iter, err := r.References()
	if err != nil {
		return nil, fmt.Errorf("src: list refs: %w", err)
	}
	var out []PrincipalLine
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if !strings.HasPrefix(name, shapingRefRoot) || ref.Type() != plumbing.HashReference {
			return nil
		}
		rest := strings.TrimPrefix(name, shapingRefRoot)
		if !strings.HasSuffix(rest, "/line") {
			return nil
		}
		slug := strings.TrimSuffix(rest, "/line")
		if slug == "" || strings.Contains(slug, "/") {
			return nil
		}
		principal := slug
		if c, err := r.CommitObject(ref.Hash()); err == nil {
			if p := commitTrailer(c.Message, "Principal"); p != "" {
				principal = p
			}
		}
		out = append(out, PrincipalLine{Principal: principal, Slug: slug, Tip: ref.Hash().String()})
		return nil
	}); err != nil {
		return nil, fmt.Errorf("src: list refs: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

// commitTrailer returns the value of the "Key: value" trailer in msg, or "".
// Only the closing trailer block (past the last blank line) counts, and the
// last occurrence wins.
func commitTrailer(msg, key string) string {
	prefix := key + ":"
	var block []string
	lines := strings.Split(msg, "\n")
	start := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			break
		}
		start = i
	}
	block = lines[start:]
	for i := len(block) - 1; i >= 0; i-- {
		if strings.HasPrefix(block[i], prefix) {
			return strings.TrimSpace(strings.TrimPrefix(block[i], prefix))
		}
	}
	return ""
}

// refTarget resolves a full ref name to its commit SHA, reporting absence
// without an error.
func refTarget(dir, ref string) (tip string, ok bool, err error) {
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return "", false, fmt.Errorf("src: open %s: %w", dir, err)
	}
	obj, err := r.Storer.Reference(plumbing.ReferenceName(ref))
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("src: read %s: %w", ref, err)
	}
	if obj.Type() != plumbing.HashReference {
		return "", false, nil
	}
	return obj.Hash().String(), true, nil
}

// setRefTarget points a full ref name at sha.
func setRefTarget(dir, ref, sha string) error {
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("src: open %s: %w", dir, err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(ref), plumbing.NewHash(sha))); err != nil {
		return fmt.Errorf("src: point %s: %w", ref, err)
	}
	return nil
}

// recordLineCommit appends a commit with treeHash on parentHash to the repo's
// object store without touching the worktree, and returns its SHA. The
// author/committer carry name and the activation time; a zero when means the
// server clock now (UTC).
func recordLineCommit(dir string, parentHash, treeHash plumbing.Hash, msg, name, email string, when time.Time, extraParents ...plumbing.Hash) (string, error) {
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return "", fmt.Errorf("src: open %s: %w", dir, err)
	}
	if when.IsZero() {
		when = time.Now().UTC()
	}
	when = when.UTC()
	parents := append([]plumbing.Hash{parentHash}, extraParents...)
	c := &object.Commit{
		Author:       object.Signature{Name: name, Email: email, When: when},
		Committer:    object.Signature{Name: name, Email: email, When: when},
		Message:      msg,
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	eo := r.Storer.NewEncodedObject()
	if err := c.Encode(eo); err != nil {
		return "", fmt.Errorf("src: encode activation commit: %w", err)
	}
	hash, err := r.Storer.SetEncodedObject(eo)
	if err != nil {
		return "", fmt.Errorf("src: store activation commit: %w", err)
	}
	return hash.String(), nil
}

// recordReviewVersion keeps the stuck line tip for review under the
// principal's namespace, named for the release it could not follow. The id
// gains a numeric suffix while taken. It returns the new ref name.
func recordReviewVersion(dir, slug, tipSHA, targetSHA string) (string, error) {
	base := shapingRefRoot + slug + "/review/" + shortSHA(targetSHA)
	ref := base
	for n := 2; ; n++ {
		_, ok, err := refTarget(dir, ref)
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		ref = fmt.Sprintf("%s-%d", base, n)
	}
	if err := setRefTarget(dir, ref, tipSHA); err != nil {
		return "", err
	}
	return ref, nil
}

// ActivateOptions tunes Activate. Gate is required and fails the activation
// closed when it refuses. PreSwitchHook runs after the activation commit is
// recorded but before the active revision switches; a non-nil error aborts
// the switch and keeps the previous active revision. Tests use it to stand
// in for a crash mid-activation. Request is the user's request text recorded
// in the activation commit; empty falls back to the revision SHA for older
// callers. When overrides the activation time; zero means the server clock
// now (UTC).
type ActivateOptions struct {
	Gate          state.ActivationGate
	PreSwitchHook func() error
	Request       string
	When          time.Time
}

// oneLineRequest collapses request text to one trailer-safe line.
func oneLineRequest(s string) string { return strings.Join(strings.Fields(s), " ") }

// ActivateResult reports one activation: the requested revision, the audit
// commit recorded on the principal's version line, and the previously
// active revision.
type ActivateResult struct {
	App        string
	Principal  string
	Revision   string
	Activation string
	Previous   string
}

// cliGate is the process-wide activation gate the `src activate` command
// consults. The verifier wires it; until then activation through the CLI
// refuses rather than runs ungated. Tests pass their gate to Activate
// directly instead of touching this.
var cliGate state.ActivationGate

// RegisterActivationGate wires the gate `src activate` consults.
func RegisterActivationGate(g state.ActivationGate) { cliGate = g }

// Activate records a commit on the principal's version line carrying the
// request, the gate verdict, the evidence reference, and the principal,
// then switches that principal's active revision atomically and keeps the
// previous revision as last-known-good. The switch is the atomic state
// write and runs last, so a crash anywhere before it leaves the previous
// active revision in place. Only the calling principal's line and record
// move; nothing shared and no data directory is touched.
func Activate(home, app, rev, principal string, opts ActivateOptions) (ActivateResult, error) {
	var out ActivateResult
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return out, fmt.Errorf("src: activate needs a principal")
	}
	if principalSlug(principal) == "" {
		return out, fmt.Errorf("src: principal %q has no usable identity", principal)
	}
	if opts.Gate == nil {
		return out, fmt.Errorf("src: activate refused: no activation gate is wired")
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
		return out, fmt.Errorf("src: activate %s: unknown revision %q", app, shortSHA(rev))
	}
	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return out, err
	}
	st, err := store.Read()
	if err != nil {
		return out, err
	}
	line := VersionLineRef(principal)
	tip, ok, err := refTarget(dir, line)
	if err != nil {
		return out, err
	}
	lineParent := tip
	if !ok {
		if lineParent, err = ygit.ResolveCommit(dir, pristineBranch); err != nil {
			return out, fmt.Errorf("src: activate %s: no version line and no %s branch (%v)", app, pristineBranch, err)
		}
	}
	gateBase := st.ActiveRevision
	if gateBase == "" {
		gateBase = lineParent
	}
	verdict, err := opts.Gate.Check(revSHA, gateBase, principal)
	if err != nil {
		return out, fmt.Errorf("src: activate refused: %w", err)
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return out, fmt.Errorf("src: open %s: %w", dir, err)
	}
	candidate, err := r.CommitObject(plumbing.NewHash(revSHA))
	if err != nil {
		return out, fmt.Errorf("src: commit %s: %w", shortSHA(revSHA), err)
	}
	reqText := oneLineRequest(opts.Request)
	if reqText == "" {
		reqText = revSHA
	}
	msg := fmt.Sprintf("Activate %s %s for %s\n\nRevision: %s\nRequest: %s\nVerdict: %s\nEvidence: %s\nPrincipal: %s\n",
		app, shortSHA(revSHA), principal, revSHA, reqText, verdict.Verdict, verdict.EvidenceID, principal)
	// Keep the candidate commit reachable from the version line as a second
	// parent. Its tree is what state.ActiveRevision names and what the next
	// shaping request clones; the first parent remains the linear audit line.
	activation, err := recordLineCommit(dir, plumbing.NewHash(lineParent), candidate.TreeHash, msg,
		principal, "bashy@localhost", opts.When, candidate.Hash)
	if err != nil {
		return out, err
	}
	if err := setRefTarget(dir, line, activation); err != nil {
		return out, err
	}
	if opts.PreSwitchHook != nil {
		if err := opts.PreSwitchHook(); err != nil {
			return out, fmt.Errorf("src: activate interrupted before the switch: %w", err)
		}
	}
	out = ActivateResult{App: app, Principal: principal, Revision: revSHA, Activation: activation, Previous: st.ActiveRevision}
	if st.ActiveRevision != "" && st.ActiveRevision != revSHA {
		st.LastKnownGood = st.ActiveRevision
	}
	st.ActiveRevision = revSHA
	// A version that declares what it can read leaves the data schema it
	// stores behind, so a later restore can refuse a target that cannot
	// read the current data. Versions without metadata leave the recorded
	// schema alone.
	if schema, err := dataSchemaOf(dir, revSHA); err == nil && schema != 0 {
		st.DataSchema = schema
	}
	if err := store.Write(st); err != nil {
		return out, err
	}
	return out, nil
}

// SetNamedVersion pins a name to a revision under the principal's namespace.
// It is the seam a later story's naming verb builds on.
func SetNamedVersion(home, app, principal, name, rev string) (string, error) {
	if strings.TrimSpace(principal) == "" {
		return "", fmt.Errorf("src: name a version needs a principal")
	}
	if principalSlug(principal) == "" {
		return "", fmt.Errorf("src: principal %q has no usable identity", principal)
	}
	if err := validVersionName(name); err != nil {
		return "", err
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return "", err
	}
	revSHA, err := ygit.ResolveCommit(dir, rev)
	if err != nil {
		return "", fmt.Errorf("src: name %s: unknown revision %q", app, shortSHA(rev))
	}
	ref := NamedVersionRef(principal, name)
	if tip, ok, err := refTarget(dir, ref); err != nil {
		return "", err
	} else if ok && tip != revSHA {
		return "", fmt.Errorf("src: version %q already names %s", name, shortSHA(tip))
	} else if ok {
		return ref, nil
	}
	if err := setRefTarget(dir, ref, revSHA); err != nil {
		return "", err
	}
	return ref, nil
}
