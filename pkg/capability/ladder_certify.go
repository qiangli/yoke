package capability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

// All certification evidence is host-local, under BASHY_HOME (or ~/.bashy).
// Never write certificates to an embedded, repository, or synced fleet ring:
// another host's tool installation and credentials are not this host's evidence.
type ladderCertifyDeps struct {
	run     func(context.Context, string, string, ...string) (string, error)
	resolve func(string) (fleet.Agent, fleet.Tool, fleet.Model, error)
	now     func() time.Time
}

func newLadderCertifyCmd() *cobra.Command {
	return newLadderCertifyCmdWith(ladderCertifyDeps{
		run:     ladderCertifyRun,
		resolve: fleet.New().Binding,
		now:     time.Now,
	})
}

func newLadderCertifyCmdWith(deps ladderCertifyDeps) *cobra.Command {
	var agents, packs []string
	var bench string
	var k int
	var dry bool
	cmd := &cobra.Command{
		Use:   "certify --agent A[,B]",
		Short: "run owner-requested certificate packs on this host",
		Long:  "Run agent-bench packs on this host and record only passing certificates. Each fresh attempt runs the DAG smoke prerequisite, bench, then verdict; results and logs stay under BASHY_HOME/ladder/certify. No certificates are shipped or synced.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(agents) == 0 {
				return fmt.Errorf("certify: --agent is required")
			}
			if len(packs) == 0 || k < 1 {
				return fmt.Errorf("certify: --packs must be nonempty and --k must be positive")
			}
			for _, p := range packs {
				if _, err := ladderCertifyKind(p); err != nil {
					return err
				}
			}
			home := os.Getenv("BASHY_HOME")
			if home == "" {
				h, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				home = filepath.Join(h, ".bashy")
			}
			home, err := filepath.Abs(home)
			if err != nil {
				return err
			}
			bench, err = ladderCertifyBench(bench, home)
			if err != nil {
				return err
			}
			type binding struct {
				agent fleet.Agent
				tool  fleet.Tool
				model fleet.Model
			}
			bindings := make([]binding, 0, len(agents))
			for _, name := range agents {
				if !ladderCertifySafeName(name) {
					return fmt.Errorf("certify: invalid agent name %q", name)
				}
				a, t, m, err := deps.resolve(name)
				if err != nil {
					return fmt.Errorf("certify: %w", err)
				}
				if a.IsCascade() || !ladderCertifySafeName(a.Name) {
					return fmt.Errorf("certify: agent %q must be a single safe tool:model binding", name)
				}
				a.Tool, a.Model = t.Name, m.Name
				if err := ladderRecordCanonicalAgent(a.MatrixKey()); err != nil {
					return err
				}
				bindings = append(bindings, binding{a, t, m})
			}
			var failures []error
			for _, b := range bindings {
				version := ""
				if !dry {
					ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
					output, err := deps.run(ctx, bench, b.tool.Binary(), "--version")
					cancel()
					if err != nil || strings.TrimSpace(output) == "" {
						return fmt.Errorf("certify: cannot resolve local tool version for %s: %v", b.agent.Name, err)
					}
					version = strings.TrimSpace(output) + " / " + b.model.Name
				}
				for _, pack := range packs {
					root := filepath.Join(home, "ladder", "certify", b.agent.Name, pack)
					runs := filepath.Join(root, "planned-run")
					if !dry {
						if err := os.MkdirAll(root, 0700); err != nil {
							return err
						}
						runs, err = os.MkdirTemp(root, "attempt-")
						if err != nil {
							return err
						}
					}
					if err := ladderCertifyAttempt(cmd, deps, b.agent, pack, bench, runs, home, version, k, dry); err != nil {
						failures = append(failures, err)
					}
					if cmd.Context().Err() != nil {
						return errors.Join(append(failures, cmd.Context().Err())...)
					}
				}
			}
			return errors.Join(failures...)
		},
	}
	cmd.Flags().StringSliceVar(&agents, "agent", nil, "fleet agents to certify, comma-separated")
	cmd.Flags().StringSliceVar(&packs, "packs", []string{"l1", "l2", "steer", "review", "manager", "judge", "l5"}, "certificate packs, comma-separated")
	cmd.Flags().StringVar(&bench, "bench", "", "agent-bench checkout (default: checkout sibling, then BASHY_HOME/agent-bench)")
	cmd.Flags().IntVar(&k, "k", 1, "runs per task")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "print agent × pack commands without running or writing")
	return cmd
}

func ladderCertifyAttempt(cmd *cobra.Command, deps ladderCertifyDeps, a fleet.Agent, pack, bench, runs, home, version string, k int, dry bool) error {
	var log io.Writer = io.Discard
	if !dry {
		f, err := os.OpenFile(filepath.Join(runs, "certify.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		log = f
	}
	var verdict string
	for _, stage := range []string{"smoke", "bench", "verdict"} {
		args := []string{"dag", stage, "AGENT=" + a.Name, "PACK=" + pack, "RUNS=" + runs, "K=" + strconv.Itoa(k)}
		if stage == "smoke" {
			args = append(args, "SMOKE_AGENT="+a.Name)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s × %s (in %q): bashy", a.Name, pack, bench)
		for _, arg := range args {
			if strings.ContainsAny(arg, " \t\n\"'`$;&|<>()") {
				fmt.Fprintf(cmd.OutOrStdout(), " %s", strconv.Quote(arg))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), " %s", arg)
			}
		}
		fmt.Fprintln(cmd.OutOrStdout())
		if dry {
			continue
		}
		fmt.Fprintf(log, "%s bashy %q\n", deps.now().UTC().Format(time.RFC3339Nano), args)
		timeout := 24 * time.Hour
		if stage == "verdict" {
			timeout = time.Minute
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		output, err := deps.run(ctx, bench, "bashy", args...)
		cancel()
		fmt.Fprintln(log, output)
		if err != nil {
			fmt.Fprintf(log, "ERROR: %v\n", err)
			return fmt.Errorf("certify: %s %s %s: %w (see %s)", a.Name, pack, stage, err, runs)
		}
		if stage == "verdict" {
			verdict = output
		}
	}
	if dry {
		return nil
	}
	detail, err := ladderCertifyVerdict(verdict, a.Name, pack)
	if err != nil {
		fmt.Fprintf(log, "ERROR: %v\n", err)
		return err
	}
	now := deps.now().UTC()
	e := ladder.Event{ID: ladderRecordNewID(now), At: now, Kind: ladder.EventKindCert, Agent: a.MatrixKey(), Season: ladder.SeasonOf(now), Reviewer: ladderRecordReviewer()}
	kind, _ := ladderCertifyKind(pack)
	e.Cert = ladder.Certificate{Kind: kind, ModelVersion: version, Season: e.Season}
	e.Note = "host-certified: " + pack + " " + detail
	st, err := ladder.OpenStore(filepath.Join(home, "ladder", "events.jsonl"))
	if err != nil {
		return err
	}
	if err := st.Append(e); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "certified %s %s: %s\n", e.Agent, pack, detail)
	return nil
}

func ladderCertifyKind(pack string) (ladder.CertKind, error) {
	switch ladder.CertKind(pack) {
	case ladder.CertL1, ladder.CertL2, ladder.CertL3, ladder.CertSteer, ladder.CertManager, ladder.CertReview, ladder.CertJudge, ladder.CertL5:
		return ladder.CertKind(pack), nil
	}
	return "", fmt.Errorf("certify: unsupported pack %q (want l1,l2,l3,steer,manager,review,judge,l5)", pack)
}

// The runner exits zero for FAIL too. Accept exactly one verdict for this
// agent and pack, never a stray PASS in a task log or another agent's result.
func ladderCertifyVerdict(output, agent, pack string) (string, error) {
	prefix := agent + " [" + pack + "]: "
	var matches []string
	for _, line := range strings.Split(output, "\n") {
		if detail, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			matches = append(matches, strings.TrimSpace(detail))
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("certify: expected one verdict for %s [%s], got %d", agent, pack, len(matches))
	}
	fields := strings.Fields(matches[0])
	if len(fields) == 0 || fields[0] != "PASS" {
		return "", fmt.Errorf("certify: %s [%s]: %s", agent, pack, matches[0])
	}
	return matches[0], nil
}

func ladderCertifySafeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\=\x00") && strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func ladderCertifyBench(explicit, home string) (string, error) {
	candidates := []string{explicit}
	if explicit == "" {
		candidates = nil
		cwd, _ := os.Getwd()
		_, source, _, _ := runtime.Caller(0)
		exe, _ := os.Executable()
		for _, start := range []string{cwd, filepath.Dir(source), filepath.Dir(exe)} {
			for dir := start; dir != ""; dir = filepath.Dir(dir) {
				data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
				if err == nil && (strings.Contains(string(data), "module github.com/qiangli/yoke\n") || strings.Contains(string(data), "module github.com/qiangli/bashy\n")) {
					candidates = append(candidates, filepath.Join(filepath.Dir(dir), "agent-bench"))
					break
				}
				if filepath.Dir(dir) == dir {
					break
				}
			}
		}
		candidates = append(candidates, filepath.Join(home, "agent-bench"))
	}
	for _, path := range candidates {
		info, err := os.Stat(filepath.Join(path, "DAG.md"))
		if err == nil && info.Mode().IsRegular() {
			return filepath.Abs(path)
		}
	}
	return "", fmt.Errorf("certify: agent-bench checkout absent (need DAG.md); supply --bench PATH or install under %s", filepath.Join(home, "agent-bench"))
}

func ladderCertifyRun(ctx context.Context, dir, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	return string(out), err
}
