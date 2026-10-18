package recall

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/qiangli/yoke/pkg/kb"
	"github.com/spf13/cobra"
)

const (
	AskStepVersion  = "bashy-kb-ask-step-v1"
	AskFinalVersion = "bashy-kb-ask-final-v1"
)

type AskEvidence struct {
	Ref  string   `json:"ref"`
	Ring string   `json:"ring"`
	Text string   `json:"text"`
	Path []string `json:"path,omitempty"`
}
type AskContradiction struct {
	A        string `json:"a"`
	B        string `json:"b"`
	Relation string `json:"relation"`
}
type AskStep struct {
	SchemaVersion      string             `json:"schema_version"`
	Session            string             `json:"session"`
	Round              int                `json:"round"`
	Evidence           []AskEvidence      `json:"evidence"`
	OpenQuestions      []string           `json:"open_questions"`
	Contradictions     []AskContradiction `json:"contradictions"`
	SuggestedFollowUps []string           `json:"suggested_follow_ups"`
	Coverage           float64            `json:"coverage"`
}
type AskCitation struct {
	Ref  string   `json:"ref"`
	Path []string `json:"path,omitempty"`
}
type AskFinal struct {
	SchemaVersion string        `json:"schema_version"`
	Session       string        `json:"session"`
	Abstained     bool          `json:"abstained"`
	Answer        string        `json:"answer,omitempty"`
	Reasons       []string      `json:"reasons,omitempty"`
	BlockingRefs  []string      `json:"blocking_refs,omitempty"`
	Citations     []AskCitation `json:"citations,omitempty"`
	Coverage      float64       `json:"coverage"`
}
type askState struct {
	Principal   string            `json:"principal"`
	Question    string            `json:"question"`
	Round       int               `json:"round"`
	MaxRounds   int               `json:"max_rounds"`
	MinCoverage float64           `json:"min_coverage"`
	Evidence    []AskEvidence     `json:"evidence"`
	Blocking    map[string]string `json:"blocking"`
	Closed      bool              `json:"closed"`
}

// Completer supplies optional local reasoning without coupling recall to a model runtime.
type Completer interface {
	Complete(context.Context, string) (string, error)
}

type askDeps struct {
	readers  []Reader
	show     func(string, string) kb.ShowEnvelope
	complete Completer
	stateDir string
}

// NewAskCmd builds kb ask. The embedding shell mounts it beside NewContextCmd.
func NewAskCmd(extra ...Reader) *cobra.Command {
	return newAskCmd(askDeps{readers: append(askDefaultReaders(), extra...)})
}

// NewAskCmdWithCompleter injects the optional local completion seam.
func NewAskCmdWithCompleter(c Completer, extra ...Reader) *cobra.Command {
	return newAskCmd(askDeps{readers: append(askDefaultReaders(), extra...), complete: c})
}

func askDefaultReaders() []Reader {
	var out []Reader
	for _, reader := range openContextRings() {
		switch r := reader.(type) {
		case RepoRing:
			if isDir(r.Path) {
				out = append(out, reader)
			}
		case HostRing:
			if isDir(r.Path) {
				out = append(out, reader)
			}
		case RelationRing:
			if (r.RingName == RingRepo || r.RingName == RingHost) && isDir(r.Path) {
				out = append(out, reader)
			}
		}
	}
	return out
}

func newAskCmd(d askDeps) *cobra.Command {
	var session, followup, reasoner string
	var final, asJSON bool
	var rounds int
	var minCoverage float64
	cmd := &cobra.Command{
		Use: "ask [QUESTION]", Short: "retrieve evidence in bounded rounds and cite or abstain",
		Long:          "kb ask QUESTION returns a JSON evidence step. Continue with --session ID --continue QUERY, or close with --session ID --final. A local model can drive the same rounds with --reasoner local; the default path never calls a model.",
		SilenceErrors: true, SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if rounds < 1 || minCoverage < 0 || minCoverage > 1 {
				return fmt.Errorf("kb ask: rounds must be positive and min-coverage must be in [0,1]")
			}
			if reasoner != "" && reasoner != "local" {
				return fmt.Errorf("kb ask: unknown reasoner %q", reasoner)
			}
			if reasoner == "local" && d.complete == nil {
				return fmt.Errorf("kb ask: no local reasoner wired")
			}
			if d.show == nil {
				d.show = showAskRef
			}
			if d.stateDir == "" {
				d.stateDir = filepath.Join(filepath.Dir(kb.DefaultDir()), "ask")
			}
			if session == "" {
				if final || followup != "" || len(args) == 0 {
					return fmt.Errorf("kb ask: QUESTION is required for a new session")
				}
				id, err := newAskID()
				if err != nil {
					return err
				}
				session = id
				st := askState{Principal: kb.ToolID(), Question: strings.TrimSpace(strings.Join(args, " ")), MaxRounds: rounds, MinCoverage: minCoverage, Evidence: []AskEvidence{}, Blocking: map[string]string{}}
				step, err := askRound(&st, session, st.Question, d)
				if err != nil {
					return err
				}
				if err = saveAsk(d.stateDir, session, st); err != nil {
					return err
				}
				if reasoner == "local" {
					return driveAsk(c, &st, session, step, d, asJSON)
				}
				return printAsk(c, step, asJSON)
			}
			if len(args) != 0 {
				return fmt.Errorf("kb ask: QUESTION cannot accompany --session")
			}
			st, err := loadAsk(d.stateDir, session)
			if err != nil {
				return err
			}
			if st.Principal != kb.ToolID() {
				return fmt.Errorf("kb ask: session unavailable")
			}
			if st.Closed {
				return fmt.Errorf("kb ask: session is closed")
			}
			if final {
				if followup != "" {
					return fmt.Errorf("kb ask: --final and --continue are exclusive")
				}
				result := finishAsk(&st, session, d.show, d.readers)
				st.Closed = true
				if err := saveAsk(d.stateDir, session, st); err != nil {
					return err
				}
				return printAsk(c, result, asJSON)
			}
			if strings.TrimSpace(followup) == "" {
				return fmt.Errorf("kb ask: --continue FOLLOW-UP or --final is required")
			}
			if st.Round >= st.MaxRounds {
				return fmt.Errorf("kb ask: round budget exhausted; use --final")
			}
			if coverage, _ := askCoverage(st.Question, st.Evidence); coverage >= st.MinCoverage {
				return fmt.Errorf("kb ask: coverage threshold reached; use --final")
			}
			step, err := askRound(&st, session, followup, d)
			if err != nil {
				return err
			}
			if err := saveAsk(d.stateDir, session, st); err != nil {
				return err
			}
			if reasoner == "local" {
				return driveAsk(c, &st, session, step, d, asJSON)
			}
			return printAsk(c, step, asJSON)
		},
	}
	f := cmd.Flags()
	f.StringVar(&session, "session", "", "resume a saved ask session")
	f.StringVar(&followup, "continue", "", "query for the next evidence round")
	f.BoolVar(&final, "final", false, "close the session with citations or abstention")
	f.IntVar(&rounds, "rounds", 4, "maximum evidence rounds")
	f.Float64Var(&minCoverage, "min-coverage", 0.6, "minimum original-question term coverage")
	f.StringVar(&reasoner, "reasoner", "", "optional local completion driver")
	f.BoolVar(&asJSON, "json", false, "emit the ask JSON envelope (also the default)")
	return cmd
}

func askRound(st *askState, id, query string, d askDeps) (AskStep, error) {
	q := Query{Text: query, Rings: []string{RingRepo, RingHost}, Forms: []string{kb.FormNote, kb.FormPage}, Budget: PreambleBudget, K: 3, OS: hostOS(), Repo: repoName()}
	res := Context(q, d.readers...)
	for _, r := range res.Rings {
		if !r.OK {
			return AskStep{}, fmt.Errorf("kb ask: ring %s: %s", r.Name, r.Error)
		}
	}
	for _, b := range res.Blocks {
		if slices.ContainsFunc(st.Evidence, func(e AskEvidence) bool { return e.Ref == b.Ref }) {
			continue
		}
		shown := d.show(b.Ref, b.Ring)
		if reason := askShowBlock(shown); reason != "" {
			st.Blocking[b.Ref] = reason
			continue
		}
		path := append([]string{}, b.Path...)
		if len(path) == 0 {
			path = append(path, b.Ref)
			for _, src := range b.Source {
				path = append(path, src.URI)
			}
		}
		st.Evidence = append(st.Evidence, AskEvidence{Ref: b.Ref, Ring: b.Ring, Text: b.Text, Path: path})
	}
	st.Round++
	return makeAskStep(*st, id, d.readers), nil
}

func makeAskStep(st askState, id string, readers []Reader) AskStep {
	coverage, missing := askCoverage(st.Question, st.Evidence)
	step := AskStep{SchemaVersion: AskStepVersion, Session: id, Round: st.Round, Evidence: append([]AskEvidence{}, st.Evidence...), OpenQuestions: []string{}, Contradictions: askContradictions(st.Evidence, readers), SuggestedFollowUps: []string{}, Coverage: coverage}
	for _, term := range missing {
		step.OpenQuestions = append(step.OpenQuestions, "What evidence addresses "+term+"?")
	}
	if len(missing) > 0 && st.Round < st.MaxRounds && coverage < st.MinCoverage {
		step.SuggestedFollowUps = append(step.SuggestedFollowUps, strings.Join(missing, " "))
	}
	return step
}

func askCoverage(question string, evidence []AskEvidence) (float64, []string) {
	terms := kb.Terms(question)
	seen := map[string]bool{}
	var unique []string
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			unique = append(unique, t)
		}
	}
	if len(unique) == 0 {
		return 0, nil
	}
	corpus := ""
	for _, e := range evidence {
		corpus += " " + strings.ToLower(e.Text)
	}
	var missing []string
	matched := 0
	for _, t := range unique {
		if strings.Contains(corpus, t) {
			matched++
		} else {
			missing = append(missing, t)
		}
	}
	return float64(matched) / float64(len(unique)), missing
}

func askContradictions(evidence []AskEvidence, readers []Reader) []AskContradiction {
	visible := map[string]bool{}
	for _, e := range evidence {
		visible[e.Ref] = true
	}
	out := []AskContradiction{}
	seen := map[string]bool{}
	for _, rd := range readers {
		r, ok := rd.(RelationRing)
		if !ok {
			continue
		}
		var relations []kb.Relation
		var err error
		if r.RingName == RingAgent {
			relations, err = (kb.OwnerRelationRing{Dir: r.Path, Owner: r.Owner}).Live()
		} else {
			relations, err = (kb.RelationRing{Dir: r.Path}).Live()
		}
		if err != nil {
			continue
		}
		for _, rel := range relations {
			if rel.Relation != "contradicts" || !visible[rel.Target] || !visible[rel.Dst] {
				continue
			}
			a, b := rel.Target, rel.Dst
			if a > b {
				a, b = b, a
			}
			key := a + "\x00" + b
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, AskContradiction{A: a, B: b, Relation: "contradicts"})
		}
	}
	return out
}

func finishAsk(st *askState, id string, show func(string, string) kb.ShowEnvelope, readers []Reader) AskFinal {
	result := AskFinal{SchemaVersion: AskFinalVersion, Session: id, Coverage: 0}
	result.Coverage, _ = askCoverage(st.Question, st.Evidence)
	reasons := map[string]bool{}
	blockers := map[string]bool{}
	if result.Coverage < st.MinCoverage {
		reasons["insufficient-coverage"] = true
	}
	for ref, outcome := range st.Blocking {
		reasons[outcome] = true
		blockers[ref] = true
	}
	for _, pair := range askContradictions(st.Evidence, readers) {
		reasons["contradiction"] = true
		blockers[pair.A] = true
		blockers[pair.B] = true
	}
	for _, e := range st.Evidence {
		shown := show(e.Ref, e.Ring)
		if reason := askShowBlock(shown); reason != "" {
			reasons[reason] = true
			blockers[e.Ref] = true
			continue
		}
		result.Citations = append(result.Citations, AskCitation{Ref: e.Ref, Path: e.Path})
		if result.Answer != "" {
			result.Answer += "\n"
		}
		result.Answer += e.Text + " [" + e.Ref + "]"
	}
	if len(result.Citations) == 0 {
		reasons["no-support"] = true
	}
	if len(reasons) > 0 {
		result.Abstained = true
		result.Answer = ""
		result.Citations = nil
	}
	for r := range reasons {
		result.Reasons = append(result.Reasons, r)
	}
	slices.Sort(result.Reasons)
	for r := range blockers {
		result.BlockingRefs = append(result.BlockingRefs, r)
	}
	slices.Sort(result.BlockingRefs)
	return result
}

func askShowBlock(shown kb.ShowEnvelope) string {
	if shown.Outcome != string(kb.OutcomeFound) {
		if shown.Outcome == "" {
			return string(kb.OutcomeNotFound)
		}
		return shown.Outcome
	}
	if shown.Status == kb.StatusStale || shown.Status == kb.StatusSuperseded {
		return string(kb.OutcomeExpired)
	}
	return ""
}

func showAskRef(ref, ring string) kb.ShowEnvelope {
	cmd := kb.NewKBCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	args := []string{"--ring", ring, "show", ref, "--json"}
	cmd.SetArgs(args)
	_ = cmd.Execute()
	var env kb.ShowEnvelope
	if json.Unmarshal(out.Bytes(), &env) != nil || env.Outcome == "" {
		env = kb.ShowEnvelope{Ref: ref, Outcome: string(kb.OutcomeNotFound)}
	}
	return env
}

var askIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func newAskID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func askFile(dir, id string) (string, error) {
	if !askIDPattern.MatchString(id) {
		return "", fmt.Errorf("kb ask: invalid session ID")
	}
	return filepath.Join(dir, id+".json"), nil
}
func saveAsk(dir, id string, st askState) error {
	path, err := askFile(dir, id)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ask-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
func loadAsk(dir, id string) (askState, error) {
	path, err := askFile(dir, id)
	if err != nil {
		return askState{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return askState{}, fmt.Errorf("kb ask: session unavailable")
	}
	var st askState
	if err = json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	return st, nil
}
func printAsk(c *cobra.Command, value any, _ bool) error {
	enc := json.NewEncoder(c.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func driveAsk(c *cobra.Command, st *askState, id string, step AskStep, d askDeps, asJSON bool) error {
	completion := d.complete
	for st.Round < st.MaxRounds && step.Coverage < st.MinCoverage && len(step.Contradictions) == 0 {
		promptBytes, _ := json.Marshal(step)
		followup, err := completion.Complete(c.Context(), "Suggest one short follow-up search query for the unanswered parts of this question. Return only the query. Evidence step: "+string(promptBytes))
		if err != nil {
			return err
		}
		followup = strings.TrimSpace(followup)
		if followup == "" {
			break
		}
		step, err = askRound(st, id, followup, d)
		if err != nil {
			return err
		}
		if err = saveAsk(d.stateDir, id, *st); err != nil {
			return err
		}
	}
	result := finishAsk(st, id, d.show, d.readers)
	st.Closed = true
	if err := saveAsk(d.stateDir, id, *st); err != nil {
		return err
	}
	return printAsk(c, result, asJSON)
}
