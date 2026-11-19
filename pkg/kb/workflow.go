package kb

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/spf13/cobra"
)

// WorkflowLearning is a reported lesson, not an attestation. Even a report
// claiming success enters the ordinary KB validation ladder as a candidate.
// Finding is a stable symptom/procedure ID, shared by reporters of one finding.
type WorkflowLearning struct {
	Workflow      string           `json:"workflow" yaml:"workflow"`
	Candidate     string           `json:"candidate" yaml:"candidate"`
	Finding       string           `json:"finding" yaml:"finding"`
	Kind          string           `json:"kind" yaml:"kind"`
	Target        string           `json:"target" yaml:"target"`
	Applicability string           `json:"applicability" yaml:"applicability"`
	Reports       []WorkflowReport `json:"reports" yaml:"reports"`
}

type WorkflowReport struct {
	Reporter string `json:"reporter" yaml:"reporter"`
	Episode  string `json:"episode" yaml:"episode"`
	Ref      string `json:"ref" yaml:"ref"`
	Outcome  string `json:"outcome" yaml:"outcome"`
	Summary  string `json:"summary" yaml:"summary"`
}

func workflowKey(w WorkflowLearning) string {
	w.Reports = nil
	b, _ := json.Marshal(w)
	h := sha256.Sum256(b)
	return "workflow-" + hex.EncodeToString(h[:])
}

func normalizeWorkflow(w *WorkflowLearning) error {
	for _, v := range []*string{&w.Workflow, &w.Candidate, &w.Finding, &w.Kind, &w.Target, &w.Applicability} {
		*v = strings.TrimSpace(*v)
		if *v == "" {
			return fmt.Errorf("kb: workflow, candidate, finding, kind, target and applicability are required")
		}
	}
	if w.Kind != "skill" && w.Kind != "incident" {
		return fmt.Errorf("kb: workflow kind must be skill or incident")
	}
	if len(w.Reports) == 0 {
		return fmt.Errorf("kb: workflow needs evidence reports")
	}
	for i := range w.Reports {
		r := &w.Reports[i]
		for _, v := range []*string{&r.Reporter, &r.Episode, &r.Ref, &r.Outcome, &r.Summary} {
			*v = strings.TrimSpace(*v)
			if *v == "" {
				return fmt.Errorf("kb: each report needs reporter, episode, ref, outcome and summary")
			}
		}
		switch r.Outcome {
		case "passed", "failed", "unverified", "cancelled":
		default:
			return fmt.Errorf("kb: invalid reported outcome %q", r.Outcome)
		}
	}
	w.Reports = mergeWorkflowReports(nil, w.Reports)
	return nil
}

func mergeWorkflowReports(a, b []WorkflowReport) []WorkflowReport {
	byKey := map[string]WorkflowReport{}
	for _, r := range append(append([]WorkflowReport(nil), a...), b...) {
		data, _ := json.Marshal(r)
		byKey[string(data)] = r
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]WorkflowReport, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return out
}

func workflowBody(w WorkflowLearning) string {
	b, _ := json.MarshalIndent(w, "", "  ")
	next := "Review the proposed procedure; use skill learn/run for contract verification and skill promote for reviewed publication."
	if w.Kind == "incident" {
		next = "Review evidence; submit through shape incident or shape wish for the target. Shape verification, preview, approve and undo retain their existing authority checks."
	}
	return "Reported group outcome; evidence references and reporter identities are supplied claims, not verified authority.\n\n" + string(b) + "\n\n" + next + "\nDesignated feedback: todo:cdae01f261b1. Use its existing agentic.<command> bus/inbox routes; importing does not send feedback or activate changes.\nKB validation remains a separate kb validate operation with evidence."
}

func workflowLock(s *Store) (*lockfile.Lock, error) {
	return lockfile.AcquireWithin(filepath.Join(s.dir, ".workflow.lock"), 5*time.Second, lockfile.Holder{Name: ToolID(), Intent: "record workflow candidate"})
}

func newWorkflowCmd(dir, ring *string) *cobra.Command {
	var file string
	var apply bool
	cmd := &cobra.Command{
		Use: "workflow", Short: "Preview or import reported group outcomes as unvalidated candidates",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var input io.Reader = c.InOrStdin()
			if file != "-" {
				f, err := os.Open(file)
				if err != nil {
					return err
				}
				defer f.Close()
				input = f
			}
			payload, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
			if err != nil {
				return err
			}
			if len(payload) > 1<<20 {
				return fmt.Errorf("kb: workflow exceeds 1 MiB")
			}
			dec := json.NewDecoder(bytes.NewReader(payload))
			dec.DisallowUnknownFields()
			var w WorkflowLearning
			if err := dec.Decode(&w); err != nil {
				return err
			}
			var extra any
			if err := dec.Decode(&extra); err != io.EOF {
				return fmt.Errorf("kb: workflow expects exactly one JSON object")
			}
			if err := normalizeWorkflow(&w); err != nil {
				return err
			}
			result, err := ImportWorkflowReport(*dir, *ring, w, apply)
			if err != nil {
				return err
			}
			return json.NewEncoder(c.OutOrStdout()).Encode(result)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "-", "workflow JSON file, or - for stdin (reported evidence only)")
	cmd.Flags().BoolVar(&apply, "apply", false, "store reported evidence as an unvalidated candidate; does not verify or activate")
	return cmd
}

// WorkflowVerifier must independently resolve every report against trusted
// completed-workflow evidence, binding its workflow, exact candidate, outcome
// and applicability. It must not trust report text or a caller-supplied verdict.
// Returning nil permits proposal storage only, never validation or activation.
type WorkflowVerifier func(WorkflowLearning) error

type WorkflowProposal struct {
	Slug      string           `json:"slug"`
	Status    string           `json:"status"`
	Applied   bool             `json:"applied"`
	Duplicate bool             `json:"duplicate"`
	Workflow  WorkflowLearning `json:"workflow"`
	Next      string           `json:"next"`
}

// ImportWorkflowReport durably retains supplied claims in the existing KB candidate
// store. It establishes no verification fact, even for a reported passed outcome.
// Completed S1/S10 reports can enter here before an independent resolver exists;
// validation and skill/shape activation remain separate operations.
func ImportWorkflowReport(dir, ring string, w WorkflowLearning, apply bool) (*WorkflowProposal, error) {
	return proposeWorkflow(dir, ring, w, apply, nil, true)
}

// ProposeWorkflow composes a trusted outcome resolver with the existing KB
// candidate writer. Without a resolver only a no-write preview is possible.
func ProposeWorkflow(dir, ring string, w WorkflowLearning, apply bool, verify WorkflowVerifier) (*WorkflowProposal, error) {
	return proposeWorkflow(dir, ring, w, apply, verify, false)
}

func proposeWorkflow(dir, ring string, w WorkflowLearning, apply bool, verify WorkflowVerifier, reportedOnly bool) (*WorkflowProposal, error) {
	w.Reports = append([]WorkflowReport(nil), w.Reports...)
	if err := normalizeWorkflow(&w); err != nil {
		return nil, err
	}
	if ring != "repo" && ring != "host" && ring != "agent" && ring != "user" && ring != "dir" {
		return nil, fmt.Errorf("kb: invalid workflow ring %q", ring)
	}
	if apply && verify == nil && !reportedOnly {
		return nil, fmt.Errorf("kb: independently verified workflow evidence is required")
	}

	// Apply the existing shareable-write check to the complete payload,
	// including references and provenance, before any store mutation.
	if ring != "agent" {
		if err := shareableWriteCheck(workflowBody(w)); err != nil {
			return nil, err
		}
	}
	slug := workflowKey(w)
	s := openRing(dir, ring)
	if apply {
		lock, err := workflowLock(s)
		if err != nil {
			return nil, err
		}
		defer lock.Release()
	}
	p, err := s.Load(slug)
	duplicate := false
	if err == nil {
		if p.Workflow == nil || workflowKey(*p.Workflow) != slug {
			return nil, fmt.Errorf("kb: workflow slug collision")
		}
		merged := mergeWorkflowReports(p.Workflow.Reports, w.Reports)
		duplicate = len(merged) == len(p.Workflow.Reports)
		if !duplicate && p.Status != StatusCandidate {
			return nil, fmt.Errorf("kb: workflow is %s; use explicit supersede/review for new evidence", p.Status)
		}
		w.Reports = merged
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if apply && !reportedOnly {
		if verify == nil {
			return nil, fmt.Errorf("kb: independently verified workflow evidence is required")
		}
		for _, r := range w.Reports {
			if r.Outcome == "unverified" || r.Outcome == "cancelled" {
				return nil, fmt.Errorf("kb: incomplete or unverified workflow evidence")
			}
		}
		// Give the resolver a copy so it cannot alter the proposal after normalization.
		evidence := w
		evidence.Reports = append([]WorkflowReport(nil), w.Reports...)
		if err := verify(evidence); err != nil {
			return nil, fmt.Errorf("kb: workflow verification: %w", err)
		}
	}
	if p == nil {
		typ := TypeRunbook
		if w.Kind == "incident" {
			typ = TypeGotcha
		}
		p = &Page{Slug: slug, Form: FormNote, Type: typ, Status: StatusCandidate,
			Title: w.Kind + ": " + w.Finding, Description: w.Applicability,
			Source: &Source{Tool: "workflow-proposer", Episode: EpisodeID()},
			Tags:   []string{"group-workflow", "todo:cdae01f261b1"}}
	}
	if !duplicate {
		p.Workflow = &w
		p.Body = workflowBody(w)
		if apply {
			if err := s.Write(p, "workflow-propose"); err != nil {
				return nil, err
			}
		}
	}
	return &WorkflowProposal{slug, p.Status, apply, duplicate, w, workflowBody(w)}, nil
}
