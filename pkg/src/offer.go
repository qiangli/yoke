package src

import (
	"context"
	"encoding/json"
	"errors"
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
	shapegate "github.com/qiangli/yoke/pkg/shape/gate"
	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/shape/verify"
)

// Offering records one published host offering for a shaped feature
// revision. Offerings publish tagged releases only: Version is the semver
// release tag Revision carries.
type Offering struct {
	ID        string    `json:"id"`
	App       string    `json:"app"`
	Revision  string    `json:"revision"`
	Version   string    `json:"version"`
	Origin    string    `json:"origin"`
	BaseRev   string    `json:"base_rev"`
	Requires  []string  `json:"requires,omitempty"`
	All       bool      `json:"all,omitempty"`
	To        []string  `json:"to,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Withdrawn bool      `json:"withdrawn,omitempty"`
	Notified  []string  `json:"notified,omitempty"`
}

// OfferOptions configures Offer.
type OfferOptions struct {
	To  []string
	All bool
	ID  string
}

// AdoptOptions configures Adopt.
type AdoptOptions struct {
	Preview bool
	// Gate overrides the activation gate. Nil verifies the adopter's own
	// pair (their active revision -> the merged candidate) and gates on that
	// fresh evidence, like a sealed restore: the offering's evidence was
	// minted for the origin's base and cannot bind the adopter's.
	Gate state.ActivationGate
	// Runner is the check runner for that verification; nil = production.
	Runner verify.Runner
}

// AdoptResult reports the outcome of an Adopt call.
type AdoptResult struct {
	App        string `json:"app"`
	Principal  string `json:"principal"`
	OfferID    string `json:"offer_id"`
	Revision   string `json:"revision"`
	PreviewDir string `json:"preview_dir,omitempty"`
	Activation string `json:"activation,omitempty"`
	Previous   string `json:"previous,omitempty"`
	ReviewRef  string `json:"review_ref,omitempty"`
	Conflict   bool   `json:"conflict,omitempty"`
}

func offeringsDir(home, app string) string {
	if home == "" {
		home = Home()
	}
	return filepath.Join(SrcDir(home), ".offerings", app)
}

func sanitizeOfferFilename(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		s = "offering"
	}
	return s
}

// Offer publishes a released revision of a feature as a host offering.
// Only a tagged release can be offered: an unreleased revision is refused
// with its pseudo-version and the release verb to run first. Eligible
// principals receive one notice per offering revision.
func Offer(home, app, rev, creator string, opts OfferOptions) (Offering, error) {
	creator = strings.TrimSpace(creator)
	if creator == "" {
		return Offering{}, fmt.Errorf("src: offer needs a creator principal")
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return Offering{}, err
	}
	revSHA, err := ygit.ResolveCommit(dir, rev)
	if err != nil {
		return Offering{}, fmt.Errorf("src: offer %s: unknown revision %q", app, shortSHA(rev))
	}
	if !opts.All && len(opts.To) == 0 {
		return Offering{}, fmt.Errorf("src: offer requires --to <principals> or --all")
	}
	r, err := gogit.PlainOpen(dir)
	if err != nil {
		return Offering{}, fmt.Errorf("src: open %s: %w", dir, err)
	}
	version, released, err := versionOf(r, revSHA)
	if err != nil {
		return Offering{}, err
	}
	if !released {
		return Offering{}, fmt.Errorf("src: offer refused: %s is not a released version (%s); offerings publish tagged releases only, release it first: src release %s %s",
			shortSHA(revSHA), version, app, shortSHA(revSHA))
	}

	feat, hasFeat, _ := featureAt(dir, revSHA)
	id := strings.TrimSpace(opts.ID)
	if id == "" && hasFeat && strings.TrimSpace(feat.ID) != "" {
		id = strings.TrimSpace(feat.ID)
	}
	if id == "" {
		id = shortSHA(revSHA)
	}

	baseSHA, err := mergeBase(dir, revSHA, pristineBranch)
	if err != nil {
		if c, cErr := r.CommitObject(plumbing.NewHash(revSHA)); cErr == nil && len(c.ParentHashes) > 0 {
			baseSHA = c.ParentHashes[0].String()
		}
	}
	if baseSHA == "" {
		baseSHA = revSHA
	}

	var requires []string
	if hasFeat && len(feat.Requires) > 0 {
		requires = append([]string(nil), feat.Requires...)
	}

	var toClean []string
	for _, t := range opts.To {
		if s := strings.TrimSpace(t); s != "" {
			toClean = append(toClean, s)
		}
	}

	odir := offeringsDir(home, app)
	if err := os.MkdirAll(odir, 0o755); err != nil {
		return Offering{}, fmt.Errorf("src: create offerings directory: %w", err)
	}
	offerPath := filepath.Join(odir, sanitizeOfferFilename(id)+".json")

	var existing Offering
	var notifiedList []string
	if b, readErr := os.ReadFile(offerPath); readErr == nil {
		if json.Unmarshal(b, &existing) == nil {
			notifiedList = existing.Notified
		}
	}

	offering := Offering{
		ID:        id,
		App:       app,
		Revision:  revSHA,
		Version:   version,
		Origin:    creator,
		BaseRev:   baseSHA,
		Requires:  requires,
		All:       opts.All,
		To:        toClean,
		CreatedAt: time.Now().UTC(),
		Withdrawn: false,
		Notified:  notifiedList,
	}

	// Deliver notices to eligible principals
	var eligible []string
	if opts.All {
		eligible = allKnownPrincipals(home, dir, app)
	} else {
		eligible = append(eligible, toClean...)
		for _, adopter := range adoptersOf(home, app, id) {
			if !hasPrincipal(eligible, adopter) {
				eligible = append(eligible, adopter)
			}
		}
	}

	for _, p := range eligible {
		if principalMatch(p, creator) {
			continue
		}
		key := principalSlug(p) + ":" + revSHA
		if hasString(offering.Notified, key) {
			continue
		}
		if err := deliverOfferNotice(home, app, offering, p); err == nil {
			offering.Notified = append(offering.Notified, key)
		}
	}

	data, err := json.MarshalIndent(offering, "", "  ")
	if err != nil {
		return Offering{}, fmt.Errorf("src: encode offering: %w", err)
	}
	data = append(data, '\n')
	if err := writeAtomic(offerPath, data, 0o644); err != nil {
		return Offering{}, fmt.Errorf("src: save offering: %w", err)
	}
	return offering, nil
}

func deliverOfferNotice(home, app string, offer Offering, principal string) error {
	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return err
	}
	st, err := store.Read()
	if err != nil {
		return err
	}
	for _, n := range st.Notices {
		if n.ID == offer.ID && n.Revision == offer.Revision {
			return nil
		}
	}
	st.Notices = append(st.Notices, state.Notice{
		ID:       offer.ID,
		Revision: offer.Revision,
		Message:  fmt.Sprintf("Offering %s %s (%s) from %s", offer.ID, offer.Version, shortSHA(offer.Revision), offer.Origin),
	})
	return store.Write(st)
}

// ListOffers returns all offerings visible to principal for app.
func ListOffers(home, app, principal string) ([]Offering, error) {
	if home == "" {
		home = Home()
	}
	odir := offeringsDir(home, app)
	entries, err := os.ReadDir(odir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Offering
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(odir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var o Offering
		if err := json.Unmarshal(b, &o); err != nil {
			continue
		}
		if o.Withdrawn {
			continue
		}
		if isVisible(o, principal) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// OfferedRevisions returns revisions that this principal published, even
// when an offering targets other users and is therefore not visible to its
// creator. A published revision remains supported until withdrawn.
func OfferedRevisions(home, app, principal string) ([]string, error) {
	entries, err := os.ReadDir(offeringsDir(home, app))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var revisions []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(offeringsDir(home, app), e.Name()))
		if err != nil {
			return nil, err
		}
		var o Offering
		if err := json.Unmarshal(b, &o); err != nil {
			return nil, err
		}
		if !o.Withdrawn && o.Origin == principal {
			revisions = append(revisions, o.Revision)
		}
	}
	return revisions, nil
}

func findOffering(home, app, target, principal string) (Offering, error) {
	offers, err := ListOffers(home, app, principal)
	if err != nil {
		return Offering{}, err
	}
	for _, o := range offers {
		if o.ID == target || o.Revision == target || shortSHA(o.Revision) == target {
			return o, nil
		}
	}
	return Offering{}, fmt.Errorf("src: unknown offering %q for %s", target, app)
}

// Adopt previews or accepts an offering.
func Adopt(home, app, offerTarget, principal string, opts AdoptOptions) (AdoptResult, error) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return AdoptResult{}, fmt.Errorf("src: adopt needs a principal")
	}
	if home == "" {
		home = Home()
	}
	dir, err := repoDir(home, app)
	if err != nil {
		return AdoptResult{}, err
	}
	offer, err := findOffering(home, app, offerTarget, principal)
	if err != nil {
		return AdoptResult{}, err
	}

	if opts.Preview {
		previewDir, err := Preview(home, app, principal, offer.Revision)
		if err != nil {
			return AdoptResult{}, err
		}
		prev := ""
		if store, err := state.OpenAt(home, app, principal); err == nil {
			if st, err := store.Read(); err == nil {
				prev = st.ActiveRevision
			}
		}
		return AdoptResult{
			App:        app,
			Principal:  principal,
			OfferID:    offer.ID,
			Revision:   offer.Revision,
			PreviewDir: previewDir,
			Previous:   prev,
		}, nil
	}

	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return AdoptResult{}, err
	}
	st, err := store.Read()
	if err != nil {
		return AdoptResult{}, err
	}

	line := VersionLineRef(principal)
	tip, hasLine, err := refTarget(dir, line)
	if err != nil {
		return AdoptResult{}, err
	}

	var oursSHA string
	if st.ActiveRevision != "" {
		oursSHA = st.ActiveRevision
	} else if hasLine {
		oursSHA = tip
	} else {
		if oursSHA, err = ygit.ResolveCommit(dir, pristineBranch); err != nil {
			return AdoptResult{}, fmt.Errorf("src: adopt %s: no version line and no %s branch (%v)", app, pristineBranch, err)
		}
	}

	candidateSHA, reviewRef, conflict, err := mergeRevisions(home, dir, app, principal, oursSHA, tip, hasLine, offer.Revision)
	if conflict {
		return AdoptResult{
			App:       app,
			Principal: principal,
			OfferID:   offer.ID,
			Revision:  offer.Revision,
			ReviewRef: reviewRef,
			Conflict:  true,
			Previous:  st.ActiveRevision,
		}, fmt.Errorf("src: adopt conflict: kept for review under %s (active revision unchanged)", reviewRef)
	}
	if err != nil {
		return AdoptResult{}, err
	}

	gate := opts.Gate
	if gate == nil {
		gate, err = adoptGate(home, dir, principal, oursSHA, candidateSHA, opts.Runner)
		if err != nil {
			return AdoptResult{}, err
		}
	}
	actRes, err := Activate(home, app, candidateSHA, principal, ActivateOptions{Gate: gate})
	if err != nil {
		return AdoptResult{}, err
	}

	// Update state: record adopted and clear notice
	st, _ = store.Read()
	st.Adopted = recordAdopted(st.Adopted, offer.ID, offer.Revision)
	st.Notices = removeNotice(st.Notices, offer.ID)
	_ = store.Write(st)

	return AdoptResult{
		App:        app,
		Principal:  principal,
		OfferID:    offer.ID,
		Revision:   offer.Revision,
		Activation: actRes.Activation,
		Previous:   actRes.Previous,
	}, nil
}

// adoptGate verifies base -> candidate for the adopting principal in an
// isolated workspace and returns the gate sealed to that evidence.
func adoptGate(home, dir, principal, baseSHA, candidateSHA string, runner verify.Runner) (state.ActivationGate, error) {
	ctx := context.Background()
	work, err := prepareRestoreWorkspace(ctx, dir, candidateSHA, baseSHA)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(work))
	report, err := verify.Run(ctx, verify.Options{
		Home: home, RepoDir: work, BaseSHA: baseSHA,
		CandidateSHA: candidateSHA, Principal: principal, Runner: runner,
	})
	if err != nil {
		return nil, err
	}
	if !report.Passed() {
		reason := strings.TrimSpace(report.Record.Reason)
		if reason == "" {
			reason = "the verifier did not pass this offering for you"
		}
		return nil, fmt.Errorf("src: adopt refused: %s", reason)
	}
	return shapegate.New(home, report.Path), nil
}

func mergeRevisions(home, dir, app, principal, activeSHA, lineTip string, hasLine bool, targetSHA string) (candidateSHA, reviewRef string, conflict bool, err error) {
	if activeSHA == targetSHA {
		return targetSHA, "", false, nil
	}

	anc, err := ygit.IsAncestor(dir, activeSHA, targetSHA)
	if err == nil && anc {
		// Fast-forward: activeSHA is ancestor of targetSHA, no personal divergence
		return targetSHA, "", false, nil
	}

	ancTarget, err := ygit.IsAncestor(dir, targetSHA, activeSHA)
	if err == nil && ancTarget {
		return activeSHA, "", false, nil
	}

	// Diverged: 3-way merge in an isolated scratch workspace
	scratchDir, err := os.MkdirTemp("", "shape-adopt-*")
	if err != nil {
		return "", "", false, fmt.Errorf("src: create scratch workspace: %w", err)
	}
	defer os.RemoveAll(scratchDir)

	rScratch, err := seedPreview(scratchDir, dir)
	if err != nil {
		return "", "", false, fmt.Errorf("src: seed scratch workspace: %w", err)
	}
	if err := unlockRepo(scratchDir, app); err != nil {
		return "", "", false, err
	}

	wt, err := rScratch.Worktree()
	if err != nil {
		return "", "", false, err
	}

	branchName := plumbing.NewBranchReferenceName("adopt-branch")
	if err := wt.Checkout(&gogit.CheckoutOptions{
		Branch: branchName,
		Hash:   plumbing.NewHash(activeSHA),
		Create: true,
	}); err != nil {
		return "", "", false, fmt.Errorf("src: checkout in scratch: %w", err)
	}

	_, mergeErr := ygit.Merge(ygit.MergeOptions{
		RepoPath: scratchDir,
		Ref:      targetSHA,
	})

	if mergeErr != nil {
		var conflictErr *ygit.ConflictError
		if errors.As(mergeErr, &conflictErr) || strings.Contains(mergeErr.Error(), "conflict") {
			// Merge conflict: record review version and keep active revision unchanged.
			slug := principalSlug(principal)
			stuckTip := activeSHA
			if hasLine && lineTip != "" {
				stuckTip = lineTip
			}
			ref, recErr := recordReviewVersion(dir, slug, stuckTip, targetSHA)
			if recErr != nil {
				return "", "", true, fmt.Errorf("src: adopt conflict: record review version: %w", recErr)
			}
			return "", ref, true, nil
		}
		return "", "", false, fmt.Errorf("src: merge in scratch: %w", mergeErr)
	}

	head, err := rScratch.Head()
	if err != nil {
		return "", "", false, err
	}
	mergedSHA := head.Hash().String()
	if mergedSHA == targetSHA {
		return targetSHA, "", false, nil
	}

	// Push merged commit to dir under a temporary ref
	slug := principalSlug(principal)
	tmpRef := fmt.Sprintf("refs/shaping/%s/tmp-merge", slug)
	pushErr := rScratch.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("+refs/heads/adopt-branch:" + tmpRef)},
	})
	if pushErr != nil && pushErr != gogit.NoErrAlreadyUpToDate {
		return "", "", false, fmt.Errorf("src: push merged commit: %w", pushErr)
	}
	defer func() {
		rDir, err := gogit.PlainOpen(dir)
		if err == nil {
			_ = rDir.Storer.RemoveReference(plumbing.ReferenceName(tmpRef))
		}
	}()

	return mergedSHA, "", false, nil
}

// Decline records the decision not to adopt an offering and clears its notice.
func Decline(home, app, offerTarget, principal string) error {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return fmt.Errorf("src: decline needs a principal")
	}
	if home == "" {
		home = Home()
	}
	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return err
	}
	st, err := store.Read()
	if err != nil {
		return err
	}

	var remaining []state.Notice
	for _, n := range st.Notices {
		if n.ID == offerTarget || n.Revision == offerTarget || shortSHA(n.Revision) == offerTarget {
			continue
		}
		remaining = append(remaining, n)
	}
	st.Notices = remaining
	return store.Write(st)
}

// Notices lists pending shaping notices for principal.
func Notices(home, app, principal string) ([]state.Notice, error) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return nil, fmt.Errorf("src: notices needs a principal")
	}
	if home == "" {
		home = Home()
	}
	store, err := state.OpenAt(home, app, principal)
	if err != nil {
		return nil, err
	}
	st, err := store.Read()
	if err != nil {
		return nil, err
	}
	if st.Notices == nil {
		return []state.Notice{}, nil
	}
	return st.Notices, nil
}

// Withdraw marks an offering as withdrawn. It does not touch adopters.
func Withdraw(home, app, offerTarget, creator string) error {
	if home == "" {
		home = Home()
	}
	offer, err := findOffering(home, app, offerTarget, creator)
	if err != nil {
		return err
	}
	offer.Withdrawn = true
	odir := offeringsDir(home, app)
	p := filepath.Join(odir, sanitizeOfferFilename(offer.ID)+".json")
	data, err := json.MarshalIndent(offer, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(p, data, 0o644)
}

func isVisible(offer Offering, principal string) bool {
	if principal == "" {
		return offer.All
	}
	if principalMatch(offer.Origin, principal) {
		return true
	}
	if offer.All {
		return true
	}
	for _, to := range offer.To {
		if principalMatch(to, principal) {
			return true
		}
	}
	return false
}

func principalMatch(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if strings.EqualFold(a, b) {
		return true
	}
	slugA := principalSlug(a)
	slugB := principalSlug(b)
	if slugA != "" && slugA == slugB {
		return true
	}
	normA := strings.TrimPrefix(strings.TrimPrefix(slugA, "device-"), "dhnt-")
	normB := strings.TrimPrefix(strings.TrimPrefix(slugB, "device-"), "dhnt-")
	return normA != "" && normA == normB
}

func hasPrincipal(list []string, p string) bool {
	for _, item := range list {
		if principalMatch(item, p) {
			return true
		}
	}
	return false
}

func hasString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func recordAdopted(adopted []state.Adopted, id, rev string) []state.Adopted {
	for i, a := range adopted {
		if a.ID == id {
			adopted[i].Rev = rev
			return adopted
		}
	}
	return append(adopted, state.Adopted{ID: id, Rev: rev})
}

func removeNotice(notices []state.Notice, id string) []state.Notice {
	var out []state.Notice
	for _, n := range notices {
		if n.ID == id || n.Revision == id || shortSHA(n.Revision) == id {
			continue
		}
		out = append(out, n)
	}
	return out
}

func allKnownPrincipals(home, dir, app string) []string {
	seen := map[string]bool{}
	var out []string
	if lines, err := PrincipalLines(dir); err == nil {
		for _, ln := range lines {
			if !seen[ln.Principal] {
				seen[ln.Principal] = true
				out = append(out, ln.Principal)
			}
		}
	}
	shapeAppDir := filepath.Join(home, "shape", app)
	if entries, err := os.ReadDir(shapeAppDir); err == nil {
		for _, e := range entries {
			if e.IsDir() && !seen[e.Name()] {
				seen[e.Name()] = true
				out = append(out, e.Name())
			}
		}
	}
	return out
}

func adoptersOf(home, app, offerID string) []string {
	shapeAppDir := filepath.Join(home, "shape", app)
	entries, err := os.ReadDir(shapeAppDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stStore, err := state.OpenAt(home, app, e.Name())
		if err != nil {
			continue
		}
		st, err := stStore.Read()
		if err != nil {
			continue
		}
		for _, a := range st.Adopted {
			if a.ID == offerID {
				out = append(out, e.Name())
				break
			}
		}
	}
	return out
}

func writeAtomic(path string, data []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		_ = f.Close()
		if retErr != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
