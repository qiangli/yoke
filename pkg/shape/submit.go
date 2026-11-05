package shape

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/src"
	"github.com/qiangli/yoke/pkg/weave"
)

// SubmitOptions optionally places an approved change on an existing story.
// TrackingRoot is local trusted context, never accepted from an HTTP body.
type SubmitOptions struct {
	Story        string
	TrackingRoot string
}

// Submission records a proposal and ordinary weave run, without approving or
// merging either. SourceRevision stays the approved artifact; ProposalRevision
// identifies the final commit, including provenance, which was verified anew.
type Submission struct {
	ID               string         `json:"id"`
	App              string         `json:"app"`
	Principal        string         `json:"principal"`
	Candidate        string         `json:"candidate"`
	SourceRevision   string         `json:"source_revision"`
	ProposalRevision string         `json:"proposal_revision"`
	BaseRevision     string         `json:"base_revision"`
	Branch           string         `json:"branch"`
	Actor            string         `json:"actor"`
	RepresentedUser  string         `json:"represented_user"`
	SubmittedBy      string         `json:"submitted_by"`
	Item             string         `json:"item"`
	TrackingRoot     string         `json:"tracking_root"`
	SourceDir        string         `json:"source_dir"`
	Evidence         string         `json:"evidence,omitempty"`
	PR               string         `json:"pr_id,omitempty"`
	Run              weave.ShapeRun `json:"run"`
	Complete         bool           `json:"complete"`
}

func submissionRoot(home string) string { return filepath.Join(src.SrcDir(home), ".shape-submissions") }
func submissionKey(app, principal, revision, item string) string {
	h := sha256.Sum256([]byte(app + "\x00" + principal + "\x00" + revision + "\x00" + item))
	return hex.EncodeToString(h[:16])
}
func saveSubmission(home string, v Submission) error {
	root := submissionRoot(home)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(root, ".submission-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), filepath.Join(root, v.ID+".json"))
}
func loadSubmission(home, id string) (Submission, error) {
	var v Submission
	b, err := os.ReadFile(filepath.Join(submissionRoot(home), id+".json"))
	if err == nil {
		err = json.Unmarshal(b, &v)
	}
	return v, err
}
func (s Service) approvedRevision(ctx context.Context, caller Caller, revision string) (Candidate, error) {
	if ValidID(revision) {
		c, err := s.Show(ctx, caller, revision)
		if err == nil && c.Status == Approved {
			return c, nil
		}
	}
	root, err := s.root()
	if err != nil {
		return Candidate{}, err
	}
	entries, err := os.ReadDir(filepath.Join(root, ".approved", s.App.Name, PrincipalKey(caller.Principal)))
	if err != nil {
		return Candidate{}, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		c, err := s.Show(ctx, caller, entry.Name())
		if err == nil && c.Status == Approved && c.Rev == revision {
			return c, nil
		}
	}
	return Candidate{}, fmt.Errorf("shape: submit requires an approved version from this principal's history")
}
func gitAncestor(dir string) string {
	for dir != "" {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
func proposalMessage(c Candidate, item *issue.Issue) (string, error) {
	agent, err := weave.ShapeAgentAttribution(c.Actor, item)
	if err != nil {
		return "", err
	}
	subject := "Submit shaped " + c.App
	by := c.Actor
	if c.RepresentedUser != "" && c.RepresentedUser != c.Actor {
		by += " for " + c.RepresentedUser
	}
	trailers := "Shape: " + c.App + "@" + c.Rev + "\nShaped-By: " + by + "\nShape-Item: todo:" + item.ID + "\n"
	if item.Sprint != 0 {
		trailers += fmt.Sprintf("Sprint: #%d\nStory: #%d\nStory-ID: %s\n", item.Sprint, item.Seq, item.ID)
		if item.SprintID != "" {
			trailers += "Sprint-ID: " + item.SprintID + "\n"
		}
	} else {
		trailers += "Adhoc: submit a tracked software shaping change\n"
	}
	if agent != "" {
		trailers += "Agent: " + agent + "\n"
	}
	return subject + "\n\n" + trailers, nil
}
func createProposal(repo *gogit.Repository, c Candidate, item *issue.Issue, base, branch string) (string, error) {
	target, err := repo.CommitObject(plumbing.NewHash(c.Rev))
	if err != nil {
		return "", err
	}
	message, err := proposalMessage(c, item)
	if err != nil {
		return "", err
	}
	if err = weave.ValidateShapeDeliveryForItem(message, item); err != nil {
		return "", err
	}
	when := c.Created
	if when.IsZero() {
		when = time.Unix(0, 0)
	}
	signature := object.Signature{Name: c.Actor, Email: "shape@bashy.invalid", When: when}
	commit := object.Commit{Author: signature, Committer: signature, Message: message, TreeHash: target.TreeHash, ParentHashes: []plumbing.Hash{plumbing.NewHash(base)}}
	encoded := repo.Storer.NewEncodedObject()
	if err = commit.Encode(encoded); err != nil {
		return "", err
	}
	hash, err := repo.Storer.SetEncodedObject(encoded)
	if err != nil {
		return "", err
	}
	if err = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), hash)); err != nil {
		return "", err
	}
	return hash.String(), nil
}

// Submit produces reviewable ordinary records. It never starts an agent,
// merges a branch, or changes the principal's active revision.
func (s Service) Submit(ctx context.Context, caller Caller, revision string, opts SubmitOptions) (Submission, error) {
	if err := s.validate(caller); err != nil {
		return Submission{}, err
	}
	if strings.TrimSpace(caller.Actor) == "" {
		return Submission{}, errors.New("shape: submit requires an accountable actor")
	}
	if s.Options.Verifier == nil {
		return Submission{}, errors.New("shape: no verifier configured")
	}
	home, err := s.home()
	if err != nil {
		return Submission{}, err
	}
	// One submission pipeline per app, across principals; src PR allocation has
	// its own global lock. This never holds a candidate transition lock.
	if err := os.MkdirAll(submissionRoot(home), 0700); err != nil {
		return Submission{}, err
	}
	lock, err := lockfile.AcquireWithin(filepath.Join(submissionRoot(home), ".locks", s.App.Name+".lock"), 30*time.Second, lockfile.Holder{Name: caller.Actor, Intent: "submit approved shape"})
	if err != nil {
		return Submission{}, err
	}
	defer lock.Release()
	c, err := s.approvedRevision(ctx, caller, revision)
	if err != nil {
		return Submission{}, err
	}
	if c.Verdict == nil || !c.Verdict.Pass || c.Verdict.Evidence == "" {
		return Submission{}, errors.New("shape: approved version has no passing verifier evidence")
	}
	trackRoot := c.TrackingRoot
	if opts.TrackingRoot != "" {
		trackRoot = opts.TrackingRoot
	}
	if trackRoot == "" {
		return Submission{}, errors.New("shape: approved version has no tracking store")
	}
	store := &issue.Store{Root: trackRoot, Sub: "."}
	ref := c.TrackingItem
	if opts.Story != "" {
		ref = opts.Story
	}
	if ref == "" {
		return Submission{}, errors.New("shape: approved version has no linked item")
	}
	itemRef, err := (Tracker{Store: store}).Record(ctx, issue.ShapeChange{App: c.App, Candidate: c.ID, Version: c.Rev, Actor: c.Actor, RepresentedUser: c.RepresentedUser, Evidence: c.Verdict.Evidence}, c.Request, ref, opts.Story != "")
	if err != nil {
		return Submission{}, err
	}
	item, err := store.Resolve(strings.TrimPrefix(itemRef, "todo:"))
	if err != nil {
		return Submission{}, err
	}
	if opts.Story != "" && (item.Sprint == 0 || item.Seq <= 0) {
		return Submission{}, errors.New("shape: submission story needs its local sprint and story number")
	}
	id := submissionKey(c.App, c.Principal, c.Rev, item.ID)
	v, loadErr := loadSubmission(home, id)
	if loadErr == nil && v.Complete {
		if _, err := submittedItem(home, v); err != nil {
			return v, err
		}
		return v, nil
	}
	if loadErr != nil && !os.IsNotExist(loadErr) {
		return Submission{}, loadErr
	}
	if os.IsNotExist(loadErr) {
		v = Submission{ID: id, App: c.App, Principal: c.Principal, Candidate: c.ID, SourceRevision: c.Rev, Actor: c.Actor, RepresentedUser: c.RepresentedUser, SubmittedBy: caller.Actor, Item: itemRef, TrackingRoot: trackRoot, SourceDir: s.App.SourceDir, Branch: "shape-submit/" + id}
		if err = saveSubmission(home, v); err != nil {
			return v, err
		}
	}
	repo, err := gogit.PlainOpen(s.App.SourceDir)
	if err != nil {
		return v, err
	}
	if v.BaseRevision == "" {
		ref, err := repo.Reference(plumbing.NewBranchReferenceName("local"), true)
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			ref, err = repo.Reference(plumbing.NewBranchReferenceName("pristine"), true)
			if err == nil {
				err = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("local"), ref.Hash()))
			}
		}
		if err != nil {
			return v, err
		}
		v.BaseRevision = ref.Hash().String()
		if err = saveSubmission(home, v); err != nil {
			return v, err
		}
	}
	if v.ProposalRevision == "" {
		v.ProposalRevision, err = createProposal(repo, c, item, v.BaseRevision, v.Branch)
		if err != nil {
			return v, err
		}
		if err = saveSubmission(home, v); err != nil {
			return v, err
		}
	}
	proposal, err := repo.CommitObject(plumbing.NewHash(v.ProposalRevision))
	if err != nil {
		return v, err
	}
	if err = weave.ValidateShapeDeliveryForItem(proposal.Message, item); err != nil {
		return v, err
	}
	// Reverify the exact rewritten proposal against its integration base. Passing
	// evidence for the original approved source is deliberately insufficient.
	temp, err := os.MkdirTemp("", "shape-submit-*")
	if err != nil {
		return v, err
	}
	defer os.RemoveAll(temp)
	work := filepath.Join(temp, "work")
	if _, err = CloneAt(ctx, s.App.SourceDir, work, v.ProposalRevision); err != nil {
		return v, err
	}
	verdict, err := s.Options.Verifier.Verify(ctx, VerifyRequest{App: c.App, Principal: c.Principal, SourceDir: s.App.SourceDir, CandidateDir: work, BaseRev: v.BaseRevision, CandidateRev: v.ProposalRevision, Agent: c.Agent})
	if err != nil {
		return v, err
	}
	if !verdict.Pass || verdict.Evidence == "" {
		return v, ErrVerification
	}
	v.Evidence = verdict.Evidence
	if err = saveSubmission(home, v); err != nil {
		return v, err
	}
	runRepo := gitAncestor(trackRoot)
	if runRepo == "" {
		runRepo = s.App.SourceDir
	}
	brief := fmt.Sprintf("Review approved shape %s@%s. Proposal branch: %s; proposal revision: %s. Use bashy src pr list --repo %s and existing review/merge gates. No automatic merge is authorized.", c.App, c.Rev, v.Branch, v.ProposalRevision, c.App)
	v.Run, err = weave.LinkShapeSubmission(runRepo, store, item.ID, id, brief)
	if err != nil {
		return v, err
	}
	if err = saveSubmission(home, v); err != nil {
		return v, err
	}
	pr, err := src.OpenShapePR(home, c.App, v.Branch, "Submit shaped "+c.App, src.ShapePR{SubmittedBy: v.SubmittedBy, Submission: id, SourceRevision: c.Rev, Candidate: c.ID, Actor: c.Actor, RepresentedUser: c.RepresentedUser, Item: itemRef, TrackingRoot: trackRoot, Evidence: v.Evidence, Run: v.Run.ID, RunRepo: v.Run.Repo})
	if err != nil {
		return v, err
	}
	v.PR = pr.ID
	if err = saveSubmission(home, v); err != nil {
		return v, err
	}
	itemLock, err := lockfile.AcquireWithin(filepath.Join(store.Dir(), ".shape-tracking.lock"), 5*time.Second, lockfile.Holder{Name: caller.Actor, Intent: "link shaped proposal"})
	if err != nil {
		return v, err
	}
	item, err = store.Resolve(item.ID)
	if err == nil {
		marker := "Shape submission " + id + ":"
		if !strings.Contains(item.Body, marker) {
			item.Body = strings.TrimSpace(item.Body) + fmt.Sprintf("\n\n%s proposal %s, src PR %s, weave run %d.", marker, v.ProposalRevision, v.PR, v.Run.ID)
			_, err = store.Save(item)
		}
	}
	itemLock.Release()
	if err != nil {
		return v, err
	}
	v.Complete = true
	return v, saveSubmission(home, v)
}

// ResolveSubmittedShapeItem is the narrow commit-guard callback. The record
// supplies private store locations; commits themselves contain stable IDs only.
func ResolveSubmittedShapeItem(app, revision, itemID string) (*issue.Issue, error) {
	home := src.Home()
	entries, err := os.ReadDir(submissionRoot(home))
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		v, err := loadSubmission(home, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		if v.App != app || v.SourceRevision != revision || v.Item != "todo:"+itemID || v.PR == "" {
			continue
		}
		return submittedItem(home, v)
	}
	return nil, errors.New("no recorded shaping submission matches these trailers")
}

func submittedItem(home string, v Submission) (*issue.Issue, error) {
	pr, err := src.GetPR(home, v.PR)
	if err != nil {
		return nil, err
	}
	if pr.Shape == nil || pr.Repo != v.App || pr.Shape.Submission != v.ID || pr.HeadSHA != v.ProposalRevision || pr.Shape.SourceRevision != v.SourceRevision || pr.Shape.Item != v.Item || pr.Shape.Actor != v.Actor || pr.Shape.RepresentedUser != v.RepresentedUser {
		return nil, errors.New("proposal identity does not match submission")
	}
	repo, err := gogit.PlainOpen(v.SourceDir)
	if err != nil {
		return nil, err
	}
	branch, err := repo.Reference(plumbing.NewBranchReferenceName(v.Branch), true)
	if err != nil || branch.Hash().String() != v.ProposalRevision {
		return nil, errors.New("proposal branch changed")
	}
	proposal, err := repo.CommitObject(plumbing.NewHash(v.ProposalRevision))
	if err != nil {
		return nil, err
	}
	source, err := repo.CommitObject(plumbing.NewHash(v.SourceRevision))
	if err != nil {
		return nil, err
	}
	if proposal.TreeHash != source.TreeHash || len(proposal.ParentHashes) != 1 || proposal.ParentHashes[0].String() != v.BaseRevision {
		return nil, errors.New("proposal source or base identity changed")
	}
	store := &issue.Store{Root: v.TrackingRoot, Sub: "."}
	return store.Resolve(strings.TrimPrefix(v.Item, "todo:"))
}

func init() { weave.ShapeSubmissionResolver = ResolveSubmittedShapeItem }

// Submittable reports whether this principal has retained approved provenance
// for the revision. It performs no tracking or submission writes.
func (s Service) Submittable(ctx context.Context, caller Caller, revision string) bool {
	if s.validate(caller) != nil {
		return false
	}
	c, err := s.approvedRevision(ctx, caller, revision)
	return err == nil && c.Status == Approved && c.Verdict != nil && c.Verdict.Pass && c.Verdict.Evidence != "" && c.TrackingRoot != "" && c.TrackingItem != ""
}
