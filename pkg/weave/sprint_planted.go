package weave

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

// Private review outcomes are intentionally separate from sprint cards.
type sprintPlantedRecord struct {
	Run         string `json:"run"`
	Kind        string `json:"kind"`
	File        string `json:"file"`
	Line        int    `json:"line"`
	PatchDigest string `json:"patch_digest"`
	Ref         string `json:"ref,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	Verdict     string `json:"verdict,omitempty"`
	Outcome     string `json:"outcome,omitempty"`
}

type sprintPlantedFile struct {
	Records []sprintPlantedRecord `json:"records"`
}

func sprintPlantedReviewPath(n int64) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("invalid sprint")
	}
	home := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if home == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		home = filepath.Join(h, ".bashy")
	}
	return filepath.Join(home, "sprint", "planted", strconv.FormatInt(n, 10)+".json"), nil
}
func sprintPlantedRead(n int64) (sprintPlantedFile, error) {
	path, e := sprintPlantedReviewPath(n)
	if e != nil {
		return sprintPlantedFile{}, e
	}
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return sprintPlantedFile{}, nil
	}
	if e != nil {
		return sprintPlantedFile{}, e
	}
	var f sprintPlantedFile
	e = json.Unmarshal(b, &f)
	return f, e
}
func sprintPlantedWrite(n int64, f sprintPlantedFile) error {
	path, e := sprintPlantedReviewPath(n)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(f, "", "  ")
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(filepath.Dir(path), ".planted-")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	if e = tmp.Chmod(0600); e != nil {
		tmp.Close()
		return e
	}
	if _, e = tmp.Write(b); e != nil {
		tmp.Close()
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	return os.Rename(tmp.Name(), path)
}
func sprintPlantedAdd(n int64, r sprintPlantedRecord) error {
	if r.Run == "" || r.Kind == "" || r.File == "" || r.Line <= 0 || r.PatchDigest == "" {
		return fmt.Errorf("incomplete plant")
	}
	f, e := sprintPlantedRead(n)
	if e != nil {
		return e
	}
	for _, old := range f.Records {
		if old.Run == r.Run && old.Kind != "" {
			return fmt.Errorf("run already planted")
		}
	}
	f.Records = append(f.Records, r)
	return sprintPlantedWrite(n, f)
}
func sprintPlantedReview(n int64, run, reviewer, verdict string, defects []string, overturned bool) error {
	if run == "" || reviewer == "" || (verdict != "approve" && verdict != "request-changes") {
		return fmt.Errorf("invalid review")
	}
	f, e := sprintPlantedRead(n)
	if e != nil {
		return e
	}
	index := -1
	for i := range f.Records {
		if f.Records[i].Run == run {
			index = i
			break
		}
	}
	if index < 0 {
		f.Records = append(f.Records, sprintPlantedRecord{Run: run})
		index = len(f.Records) - 1
	}
	r := &f.Records[index]
	if overturned {
		if r.Kind != "" || r.Verdict != "request-changes" || verdict != "approve" || r.Outcome == "false-rejection" {
			return fmt.Errorf("only a clean rejected item can be overturned")
		}
		r.Outcome = "false-rejection"
	} else {
		if r.Verdict != "" {
			return fmt.Errorf("review already recorded")
		}
		r.Reviewer = reviewer
		r.Verdict = verdict
		if r.Kind != "" {
			r.Outcome = "missed"
			if verdict == "request-changes" {
				for _, d := range defects {
					file, line, e := sprintPlantedLocation(d)
					if e == nil && file == r.File && line >= r.Line-3 && line <= r.Line+3 {
						r.Outcome = "caught"
						break
					}
				}
			}
		}
	}
	return sprintPlantedWrite(n, f)
}
func sprintPlantedLocation(s string) (string, int, error) {
	file, line, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, fmt.Errorf("want FILE:LINE")
	}
	n, e := strconv.Atoi(line)
	if e != nil || n <= 0 {
		return "", 0, fmt.Errorf("invalid line")
	}
	return file, n, nil
}
func sprintPlantedCounts(n int64) (ladder.ScorecardInput, error) {
	f, e := sprintPlantedRead(n)
	if e != nil {
		return ladder.ScorecardInput{}, e
	}
	var out ladder.ScorecardInput
	for _, r := range f.Records {
		if r.Kind != "" {
			out.PlantedDefects++
			if r.Outcome == "caught" {
				out.PlantedCaught++
			}
		}
		if r.Outcome == "false-rejection" {
			out.FalseRejections++
		}
	}
	return out, nil
}

// A patch is restricted to one existing file and one or more unified hunks.
func sprintPlantedApply(dir, file string, patch []byte) error {
	if file == "" || filepath.IsAbs(file) || strings.HasPrefix(filepath.Clean(file), "..") {
		return fmt.Errorf("unsafe file")
	}
	target := filepath.Join(dir, file)
	old, e := os.ReadFile(target)
	if e != nil {
		return e
	}
	src := strings.Split(strings.TrimSuffix(string(old), "\n"), "\n")
	lines := strings.Split(strings.TrimSuffix(string(patch), "\n"), "\n")
	var dst []string
	pos := 0
	hunks := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") {
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			var start, count int
			if _, e = fmt.Sscanf(line, "@@ -%d,%d", &start, &count); e != nil {
				if _, e = fmt.Sscanf(line, "@@ -%d", &start); e != nil {
					return fmt.Errorf("invalid hunk")
				}
			}
			if start < 1 || start-1 < pos || start-1 > len(src) {
				return fmt.Errorf("hunk position")
			}
			dst = append(dst, src[pos:start-1]...)
			pos = start - 1
			hunks++
			continue
		}
		if hunks == 0 || line == "\\ No newline at end of file" {
			continue
		}
		if line == "" {
			return fmt.Errorf("invalid patch line")
		}
		switch line[0] {
		case ' ':
			if pos >= len(src) || src[pos] != line[1:] {
				return fmt.Errorf("context mismatch")
			}
			dst = append(dst, src[pos])
			pos++
		case '-':
			if pos >= len(src) || src[pos] != line[1:] {
				return fmt.Errorf("removal mismatch")
			}
			pos++
		case '+':
			dst = append(dst, line[1:])
		default:
			return fmt.Errorf("invalid patch line")
		}
	}
	if hunks == 0 {
		return fmt.Errorf("patch has no hunk")
	}
	dst = append(dst, src[pos:]...)
	return os.WriteFile(target, []byte(strings.Join(dst, "\n")+"\n"), 0600)
}

func sprintPlantedRef(grade sprintGradeEvent, patch []byte, file string) (string, error) {
	if grade.Verdict != "pass" || grade.Checkout == "" {
		return "", fmt.Errorf("passing grade required")
	}
	parent, e := os.MkdirTemp(filepath.Dir(grade.Checkout), "plant-")
	if e != nil {
		return "", e
	}
	r, e := gogit.PlainInit(parent, false)
	if e != nil {
		return "", e
	}
	source, e := gogit.PlainOpen(grade.Checkout)
	if e != nil {
		return "", e
	}
	commit := plumbing.NewHash(grade.Commit)
	obj, e := source.CommitObject(commit)
	if e != nil {
		return "", e
	}
	// Copy the graded attempt through a local fetch, retaining its ancestry.
	remote, e := r.CreateRemote(&gogitconfig.RemoteConfig{Name: "grade", URLs: []string{grade.Checkout}})
	if e != nil {
		return "", e
	}
	e = remote.Fetch(&gogit.FetchOptions{RefSpecs: []gogitconfig.RefSpec{"+refs/sprint-grade/attempt:refs/sprint-plant/source"}, Tags: gogit.NoTags})
	if e != nil && e != gogit.NoErrAlreadyUpToDate {
		return "", e
	}
	if obj.Hash != commit {
		return "", fmt.Errorf("grade commit unavailable")
	}
	w, e := r.Worktree()
	if e != nil {
		return "", e
	}
	if e = w.Checkout(&gogit.CheckoutOptions{Hash: commit}); e != nil {
		return "", e
	}
	if e = sprintPlantedApply(parent, file, patch); e != nil {
		return "", e
	}
	if _, e = w.Add(file); e != nil {
		return "", e
	}
	hash, e := w.Commit("Plant review case", &gogit.CommitOptions{Author: &object.Signature{Name: "Sprint review", Email: "review@localhost.invalid", When: time.Now()}})
	if e != nil {
		return "", e
	}
	digest := sha256.Sum256(append([]byte(grade.Commit+file), patch...))
	id := hex.EncodeToString(digest[:])
	ref := "refs/sprint-plant/" + id[:16]
	if e = r.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(ref), hash)); e != nil {
		return "", e
	}
	return parent + ":" + ref, nil
}

func newSprintPlantCmd() *cobra.Command {
	var run, kind, file, patchPath string
	var line int
	cmd := &cobra.Command{Use: "plant N --run REPO#ID --defect KIND --file PATH --line L --patch FILE", Args: cobra.ExactArgs(1), Short: "Queue a private planted review case"}
	cmd.Flags().StringVar(&run, "run", "", "linked run")
	cmd.Flags().StringVar(&kind, "defect", "", "defect kind")
	cmd.Flags().StringVar(&file, "file", "", "file in attempt")
	cmd.Flags().IntVar(&line, "line", 0, "defect line")
	cmd.Flags().StringVar(&patchPath, "patch", "", "unified patch file")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, e := sprintArg(cmd, (&weaveOutputFlags{}).mode(), "sprint plant", args[0])
		if e != nil {
			return e
		}
		if run == "" || kind == "" || file == "" || line <= 0 || patchPath == "" {
			return fmt.Errorf("run, defect, file, line and patch required")
		}
		patch, e := os.ReadFile(patchPath)
		if e != nil {
			return e
		}
		digest := sha256.Sum256(patch)
		dir, e := sprintStoreDir()
		if e != nil {
			return e
		}
		return withWeaveQueueLock(dir, func(q *weaveQueue) error {
			s := findWeaveStory(q, n)
			if s == nil {
				return fmt.Errorf("sprint not found")
			}
			if e := authorizeSprintLeaseToken(cmd, s, "plant"); e != nil {
				return e
			}
			if e := sprintGradeManager(s); e != nil {
				return e
			}
			var link *sprintRun
			for i := range s.Runs {
				r := &s.Runs[i]
				if fmt.Sprintf("%s#%d", r.Repo, r.ID) == run {
					if link != nil {
						return fmt.Errorf("ambiguous run")
					}
					link = r
				}
			}
			if link == nil {
				return fmt.Errorf("run not linked")
			}
			queue, e := weaveQueueDirForSprintRun(*link)
			if e != nil {
				return e
			}
			runs, e := loadWeaveQueue(queue)
			if e != nil {
				return e
			}
			it := findWeaveItem(runs, link.ID)
			if it == nil || (!link.Born.IsZero() && !link.Born.Equal(it.Created)) {
				return fmt.Errorf("run generation missing")
			}
			generation := filepath.Base(queue) + ":" + it.Created.UTC().Format(time.RFC3339Nano)
			grade, e := sprintGradeLatest(s, run, generation)
			if e != nil {
				return e
			}
			ref, e := sprintPlantedRef(grade, patch, file)
			if e != nil {
				return e
			}
			r := sprintPlantedRecord{Run: run, Kind: kind, File: file, Line: line, PatchDigest: hex.EncodeToString(digest[:]), Ref: ref}
			if e = sprintPlantedAdd(n, r); e != nil {
				return e
			}
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "review-requested", run)
			fmt.Fprintln(cmd.OutOrStdout(), "review requested for", run)
			return nil
		})
	}
	return cmd
}

func newSprintReviewResultCmd() *cobra.Command {
	var run, reviewer, verdict string
	var defects []string
	var overturned bool
	cmd := &cobra.Command{Use: "review-result N --run REPO#ID --reviewer AGENT --verdict approve|request-changes", Args: cobra.ExactArgs(1), Short: "Record a review outcome"}
	cmd.Flags().StringVar(&run, "run", "", "linked run")
	cmd.Flags().StringVar(&reviewer, "reviewer", "", "reviewer")
	cmd.Flags().StringVar(&verdict, "verdict", "", "review verdict")
	cmd.Flags().StringArrayVar(&defects, "defect", nil, "observed FILE:LINE (repeatable)")
	cmd.Flags().BoolVar(&overturned, "overturned", false, "panel or owner overturned a clean rejection")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, e := sprintArg(cmd, (&weaveOutputFlags{}).mode(), "sprint review-result", args[0])
		if e != nil {
			return e
		}
		for _, d := range defects {
			if _, _, e = sprintPlantedLocation(d); e != nil {
				return e
			}
		}
		dir, e := sprintStoreDir()
		if e != nil {
			return e
		}
		return withWeaveQueueLock(dir, func(q *weaveQueue) error {
			s := findWeaveStory(q, n)
			if s == nil {
				return fmt.Errorf("sprint not found")
			}
			if e := authorizeSprintLeaseToken(cmd, s, "review-result"); e != nil {
				return e
			}
			if e := sprintGradeManager(s); e != nil {
				return e
			}
			linked := false
			for _, r := range s.Runs {
				if fmt.Sprintf("%s#%d", r.Repo, r.ID) == run {
					linked = true
					break
				}
			}
			if !linked {
				return fmt.Errorf("run not linked")
			}
			if e := sprintPlantedReview(n, run, reviewer, verdict, defects, overturned); e != nil {
				return e
			}
			// The public event carries no plant metadata or defect locations.
			b, _ := json.Marshal(struct {
				Run, Reviewer, Verdict string
				Overturned             bool
			}{run, reviewer, verdict, overturned})
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "review", string(b))
			fmt.Fprintln(cmd.OutOrStdout(), "review recorded for", run)
			return nil
		})
	}
	return cmd
}

func newSprintShadowPlanCmd() *cobra.Command {
	var planPath string
	var days, slots int
	cmd := &cobra.Command{Use: "shadow-plan N --plan FILE.json", Args: cobra.ExactArgs(1), Short: "Compare a shadow plan with sprint assignments"}
	cmd.Flags().StringVar(&planPath, "plan", "", "JSON plan steps")
	cmd.Flags().IntVar(&days, "days", 14, "simulation horizon")
	cmd.Flags().IntVar(&slots, "slots", 2, "parallel slots")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, e := sprintArg(cmd, (&weaveOutputFlags{}).mode(), "sprint shadow-plan", args[0])
		if e != nil {
			return e
		}
		b, e := os.ReadFile(planPath)
		if e != nil {
			return e
		}
		var proposed []ladder.PlanStep
		if e = json.Unmarshal(b, &proposed); e != nil {
			return e
		}
		dir, e := sprintStoreDir()
		if e != nil {
			return e
		}
		q, e := loadWeaveQueue(dir)
		if e != nil {
			return e
		}
		s := findWeaveStory(q, n)
		if s == nil {
			return fmt.Errorf("sprint not found")
		}
		var actual []ladder.PlanStep
		events, e := sprintAssignReadEvents()
		if e != nil {
			return e
		}
		pointsByStory := map[string]int{}
		for _, event := range events {
			if event.Sprint == int(n) && event.Kind == ladder.EventKindDelivery && event.Points > 0 {
				pointsByStory[event.Story] = int(event.Points)
			}
		}
		state := ladder.Replay(events, ladder.SeasonOf(time.Now()))
		for _, c := range s.Thread {
			if c.Kind != "assign" {
				continue
			}
			var a sprintAssignEvent
			if json.Unmarshal([]byte(c.Body), &a) != nil || a.Story == "" {
				continue
			}
			rating := ladder.NewRating()
			if agent := state.Agents[a.Agent]; agent != nil {
				st := agent.Standings[ladder.DutyCode]
				if st.R != 0 {
					rating = ladder.Rating{R: st.R, RD: st.RD, Vol: ladder.InitialVol}
				}
			}
			points := pointsByStory[a.Story]
			if points == 0 {
				points = 3 // assignments before rated deliveries have no point ledger yet
			}
			storyR, _ := ladder.StoryInitialRating(ladder.Points(points))
			actual = append(actual, ladder.PlanStep{Story: a.Story, Points: points, StoryRating: storyR, Agent: rating})
		}
		if len(actual) == 0 {
			return fmt.Errorf("sprint has no assign events")
		}
		shadow, e := ladder.SimulatePlan(proposed, days, slots)
		if e != nil {
			return e
		}
		live, e := ladder.SimulatePlan(actual, days, slots)
		if e != nil {
			return e
		}
		fmt.Fprintf(cmd.OutOrStdout(), "shadow points=%.2f makespan=%.2f days\nactual points=%.2f makespan=%.2f days\n", shadow.ExpectedPoints, shadow.ExpectedMakespan, live.ExpectedPoints, live.ExpectedMakespan)
		return nil
	}
	return cmd
}
