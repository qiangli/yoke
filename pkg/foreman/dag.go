package foreman

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/dag"
)

type DAGOptions struct {
	Path       string
	Targets    []string
	SteerPause time.Duration
}

type DAGReport struct {
	Targets []string `json:"targets"`
}

func (s *Session) RunDAG(ctx context.Context, opt DAGOptions) (DAGReport, error) {
	doc, err := dag.ParseFile(opt.Path)
	if err != nil {
		return DAGReport{}, err
	}
	g, err := dag.BuildGraph(doc)
	if err != nil {
		return DAGReport{}, err
	}
	if len(opt.Targets) > 0 {
		g, err = g.Subgraph(opt.Targets...)
		if err != nil {
			return DAGReport{}, err
		}
	}
	order, err := g.TopoSort()
	if err != nil {
		return DAGReport{}, err
	}
	pause := opt.SteerPause
	if pause == 0 {
		pause = 100 * time.Millisecond
	}
	report := DAGReport{Targets: make([]string, 0, len(order))}
	for i, node := range order {
		if err := s.ProcessPending(ctx); err != nil {
			return report, err
		}
		if s.State().Stopped {
			return report, nil
		}
		if s.shouldSkip(node.Task.Name) {
			continue
		}
		if err := s.runDAGTarget(ctx, filepath.Dir(opt.Path), node.Task); err != nil {
			return report, err
		}
		report.Targets = append(report.Targets, node.Task.Name)
		if i != len(order)-1 {
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			case <-time.After(pause):
			}
			if err := s.ProcessPending(ctx); err != nil {
				return report, err
			}
		}
	}
	return report, s.saveState()
}

func (s *Session) shouldSkip(target string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.EqualFold(s.state.CurrentStep, "skip:"+target)
}

func (s *Session) runDAGTarget(ctx context.Context, dir string, task *dag.Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setStatus(StatusWorking)
	s.state.CurrentStep = task.Name
	if err := s.commitLocked(); err != nil { // idle → working is a transition a supervisor must see
		return err
	}
	prompt := s.composeDAGPrompt(task)
	res, err := chat.Invoke(ctx, chat.Options{
		Agent:       s.state.Agent,
		Role:        s.state.Role,
		Instruction: prompt,
		Cwd:         firstNonEmpty(s.state.Cwd, dir),
		AllowUnsafe: s.state.AllowUnsafe,
	}, s.runner)
	if out := strings.TrimSpace(res.Output); out != "" {
		if rerr := s.record(RoleAgent, task.Name, out); rerr != nil {
			return s.blockAndCommit("history artifact: "+rerr.Error(), rerr)
		}
	}
	if err != nil || res.ExitCode != 0 {
		if err != nil {
			return s.blockAndCommit("runner: "+err.Error(), err)
		}
		err = fmt.Errorf("foreman: runner exited %d", res.ExitCode)
		return s.blockAndCommit(err.Error(), err)
	}
	s.setStatus(StatusIdle)
	return s.commitLocked()
}

// composeDAGPrompt is the bounded task packet for one DAG target: goal, kb
// preamble, the session checkpoint, the outputs of the target's dependencies
// BY REFERENCE (a preview plus the history seq of the verbatim result), and
// the target itself. It never concatenates predecessor conversations.
func (s *Session) composeDAGPrompt(task *dag.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Goal:\n%s\n\n", s.state.Goal)
	if note := s.kbPreamble(); note != "" {
		b.WriteString(note)
		b.WriteByte('\n')
	}
	snap := s.hist.snapshot()
	if cont := buildCheckpoint(s.state, s.store, snap).render(); cont != "" {
		b.WriteString(cont)
		b.WriteByte('\n')
	}
	if len(task.Requires) > 0 {
		fmt.Fprintf(&b, "Dependency outputs (by reference; verbatim results are in %s by seq):\n", s.store.HistoryPath())
		for _, dep := range task.Requires {
			if e, ok := snap.targets[dep]; ok {
				fmt.Fprintf(&b, "- %s: [seq %d, %d bytes] %s\n", dep, e.Seq, e.Bytes, oneLine(e.Text))
			} else {
				fmt.Fprintf(&b, "- %s: no recorded output\n", dep)
			}
		}
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "DAG target: %s\n", task.Name)
	if strings.TrimSpace(task.Desc) != "" {
		fmt.Fprintf(&b, "Description:\n%s\n\n", strings.TrimSpace(task.Desc))
	}
	if strings.TrimSpace(task.Body) != "" {
		fmt.Fprintf(&b, "Body:\n%s\n", strings.TrimSpace(task.Body))
	}
	return b.String()
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
