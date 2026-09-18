package weave

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	todopkg "github.com/qiangli/yoke/pkg/todo"
)

type sprintOutcomeCostReport struct {
	Metric          string        `json:"primary_metric"`
	MetricChange    int           `json:"primary_metric_change"`
	Elapsed         time.Duration `json:"elapsed"`
	Stagnant        bool          `json:"stagnant"`
	TokensAvailable bool          `json:"tokens_available"`
	InputTokens     int64         `json:"input_tokens,omitempty"`
	OutputTokens    int64         `json:"output_tokens,omitempty"`
	CachedTokens    int64         `json:"cached_tokens,omitempty"`
	DiffAvailable   bool          `json:"diff_available"`
	ProductionLines int           `json:"production_lines,omitempty"`
	TestLines       int           `json:"test_lines,omitempty"`
	ScaffoldLines   int           `json:"scaffolding_lines,omitempty"`
}

func sprintOutcomeCost(s *weaveStory, now time.Time) sprintOutcomeCostReport {
	r := sprintOutcomeCostReport{}
	for _, b := range s.Boxes {
		r.Elapsed += b.Elapsed(now)
	}
	var itemsClosed []time.Time
	total := 0
	for _, root := range sprintDeclaredStoryRoots(s) {
		items, err := todopkg.List(todopkg.RepoStore(root), "")
		if err != nil {
			continue
		}
		for _, it := range items {
			if it.Sprint != s.ID {
				continue
			}
			total++
			if it.Closed != nil {
				itemsClosed = append(itemsClosed, *it.Closed)
			}
		}
	}
	if total == 0 {
		r.Metric = "missing"
	} else {
		r.Metric = fmt.Sprintf("%d/%d stories closed", len(itemsClosed), total)
	}
	var checkpoints []time.Time
	for _, c := range s.Thread {
		if c.Kind == "progress" && strings.EqualFold(strings.TrimSpace(c.Body), "checkpoint") {
			checkpoints = append(checkpoints, c.At)
		}
	}
	closedAt := func(at time.Time) int {
		n := 0
		for _, t := range itemsClosed {
			if !t.After(at) {
				n++
			}
		}
		return n
	}
	if total > 0 && len(checkpoints) > 0 {
		r.MetricChange = len(itemsClosed) - closedAt(checkpoints[len(checkpoints)-1])
	}
	if total > 0 && len(checkpoints) >= 2 {
		a := closedAt(checkpoints[len(checkpoints)-2])
		b := closedAt(checkpoints[len(checkpoints)-1])
		r.Stagnant = a == b && b == len(itemsClosed)
	}
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		for _, it := range q.Items {
			if it.ID != run.ID {
				continue
			}
			in, out, cached, ok := sprintLogTokens(it.LogPath)
			if ok {
				r.TokensAvailable = true
				r.InputTokens += in
				r.OutputTokens += out
				r.CachedTokens += cached
			}
			if prod, test, scaffold, ok := sprintRunDiff(it); ok {
				r.DiffAvailable = true
				r.ProductionLines += prod
				r.TestLines += test
				r.ScaffoldLines += scaffold
			}
		}
	}
	return r
}

var sprintTokenField = regexp.MustCompile(`"(input_tokens|output_tokens|cached_input_tokens|cache_read_tokens)"\s*:\s*([0-9]+)`)

func sprintLogTokens(path string) (input, output, cached int64, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, 0, false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	buf := make([]byte, 64*1024)
	s.Buffer(buf, 4*1024*1024)
	for s.Scan() {
		var lineInput, lineOutput, lineCached int64
		for _, m := range sprintTokenField.FindAllStringSubmatch(s.Text(), -1) {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			switch m[1] {
			case "input_tokens":
				lineInput = n
			case "output_tokens":
				lineOutput = n
			default:
				lineCached = n
			}
		}
		if lineInput+lineOutput+lineCached > input+output+cached {
			input, output, cached, ok = lineInput, lineOutput, lineCached, true
		}
	}
	return
}

func sprintRunDiff(it *weaveItem) (production, test, scaffold int, ok bool) {
	if strings.TrimSpace(it.Workspace) == "" || strings.TrimSpace(it.BaseSHA) == "" || strings.TrimSpace(it.Head) == "" {
		return 0, 0, 0, false
	}
	cmd := exec.Command("git", "diff", "--numstat", it.BaseSHA+".."+it.Head)
	cmd.Dir = it.Workspace
	b, err := cmd.Output()
	if err != nil {
		return 0, 0, 0, false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		a, errA := strconv.Atoi(fields[0])
		d, errD := strconv.Atoi(fields[1])
		if errA != nil || errD != nil {
			continue
		}
		n := a + d
		path := filepath.ToSlash(fields[2])
		switch {
		case strings.Contains(path, "testdata/") || strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") || strings.HasSuffix(path, "_test.go"):
			test += n
		case strings.HasPrefix(path, "docs/") || strings.HasPrefix(path, "scripts/") || strings.HasPrefix(path, "script/") || strings.Contains(path, "fixture"):
			scaffold += n
		default:
			production += n
		}
	}
	return production, test, scaffold, true
}

func renderSprintOutcomeCost(w io.Writer, r sprintOutcomeCostReport) {
	fmt.Fprintf(w, "  primary metric: %s; change %+d since last checkpoint\n", r.Metric, r.MetricChange)
	fmt.Fprintf(w, "  elapsed:     %s\n", roundDur(r.Elapsed))
	if r.TokensAvailable {
		fmt.Fprintf(w, "  tokens:      input %d; output %d; cached %d\n", r.InputTokens, r.OutputTokens, r.CachedTokens)
	} else {
		fmt.Fprintln(w, "  tokens:      missing")
	}
	if r.DiffAvailable {
		fmt.Fprintf(w, "  diff lines:  production %d; test %d; scaffolding %d\n", r.ProductionLines, r.TestLines, r.ScaffoldLines)
	} else {
		fmt.Fprintln(w, "  diff lines:  production missing; test missing; scaffolding missing")
	}
	if r.Stagnant {
		fmt.Fprintln(w, "  STAGNANT: primary metric did not move across two checkpoints")
	}
}
