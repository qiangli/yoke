package weave

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/spf13/cobra"
)

type weaveHeartbeatOptions struct {
	interval    time.Duration
	idleTimeout time.Duration
	background  bool
	fleetCSV    string
	reviewAgent string
}

func newWeaveHeartbeatCmd() *cobra.Command {
	var flags weaveOutputFlags
	var interval, idleTimeout time.Duration
	var background bool
	var fleetCSV, reviewAgent string
	cmd := &cobra.Command{
		Use:   "heartbeat",
		Short: "Periodically drive autopilot in the background with idle shutdown",
		Long: `heartbeat runs the weave autopilot periodically in the background (or
foreground) on the current repository's queue.

If --background is passed, the process detaches from the terminal and logs
to .bashy/weave/<repo-hash>/heartbeat.log.

It monitors the queue and automatically exits if no work is active for the
duration specified by --idle-timeout, preventing background resource leaks.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveHeartbeat(cmd, weaveHeartbeatOptions{
				interval:    interval,
				idleTimeout: idleTimeout,
				background:  background,
				fleetCSV:    fleetCSV,
				reviewAgent: reviewAgent,
			}, &flags)
		},
	}
	flags.attach(cmd)
	cmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "Poll interval (e.g. 30s, 1m)")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 15*time.Minute, "Idle shutdown threshold (e.g. 15m, 1h)")
	cmd.Flags().BoolVar(&background, "background", false, "Run in background (detaches process)")
	cmd.Flags().StringVar(&fleetCSV, "orchestrator-fleet", "", "Roster of agents (007, claude:opus) or tools, comma-separated (default: claude,codex,opencode,agy)")
	cmd.Flags().StringVar(&reviewAgent, "review-agent", "", "Adversarial pair agent for every submitted run (default off; must differ from the coder)")
	return cmd
}

func runWeaveHeartbeat(cmd *cobra.Command, opts weaveHeartbeatOptions, flags *weaveOutputFlags) error {
	mode := flags.mode()
	const op = "weave heartbeat"

	if opts.interval <= 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op,
			weavecli.ExitInvalidArg, fmt.Errorf("--interval must be positive")))
	}

	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op,
			weavecli.ExitPrecondFail, err))
	}
	queueDir, err := ensureWeaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op,
			weavecli.ExitGenericFail, err))
	}

	logFile := filepath.Join(queueDir, "heartbeat.log")

	// Platform-specific daemonization fork
	if opts.background {
		detached, err := weaveHeartbeatDaemonize(cmd, opts, queueDir, logFile, flags)
		if err != nil {
			return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, weavecli.ExitGenericFail, err))
		}
		if detached {
			return nil
		}
	}

	// Active daemon loop (either foreground or detached child)
	fmt.Fprintf(cmd.OutOrStdout(), "[%s] heartbeat started; interval=%s, idle-timeout=%s, queue=%s\n",
		time.Now().UTC().Format(time.RFC3339), opts.interval, opts.idleTimeout, queueDir)

	idleSince := time.Time{}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(opts.interval)
	defer ticker.Stop()

	for {
		// Define the tick body as a closure to use early returns safely
		shouldExit := func() bool {
			// Reap BEFORE reading. A dead-wrapper run still recorded as
			// "working" would otherwise keep hasWork true forever: the
			// heartbeat would never idle out, and the queue would never
			// close. The idle-shutdown promise depends on the lifecycle
			// actually terminating (see weave_reaper.go).
			if _, err := weaveReapQueue(queueDir, root, weaveBaseBranch(root)); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "heartbeat: reaper: %v\n", err)
			}
			q, err := loadWeaveQueue(queueDir)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "heartbeat: failed to load queue: %v\n", err)
				return false
			}

			hasWork := false
			for _, it := range q.Items {
				if weaveRunConsumesCapacity(it.State) {
					hasWork = true
					break
				}
			}

			if !hasWork {
				if idleSince.IsZero() {
					idleSince = time.Now()
					fmt.Fprintf(cmd.OutOrStdout(), "[%s] queue is empty; entering idle monitoring\n", time.Now().UTC().Format(time.RFC3339))
				} else if time.Since(idleSince) > opts.idleTimeout {
					fmt.Fprintf(cmd.OutOrStdout(), "[%s] queue empty for %s; shutting down heartbeat cleanly\n",
						time.Now().UTC().Format(time.RFC3339), opts.idleTimeout)
					return true
				}
			} else {
				// Active work exists, reset idle timer
				idleSince = time.Time{}

				// Check if lease is active
				lease, ok, err := loadWeaveAutopilotLease(queueDir)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "heartbeat: failed to read lease: %v\n", err)
					return false
				}

				now := time.Now().UTC()
				if ok && lease.ExpiresAt.After(now) {
					// Lease is active and fresh, skip spawning autopilot to avoid conflicts
					fmt.Fprintf(cmd.OutOrStdout(), "[%s] orchestrator lease held by %s (expires %s); skipping autopilot tick\n",
						now.Format(time.RFC3339), lease.Holder, lease.ExpiresAt.Format(time.RFC3339))
				} else {
					// Lease is free or expired; spawn autopilot to drive the queue
					fmt.Fprintf(cmd.OutOrStdout(), "[%s] lease free/expired; spawning autopilot\n", now.Format(time.RFC3339))

					// Construct autopilot arguments
					apArgs := []string{"weave", "autopilot"}
					if opts.fleetCSV != "" {
						apArgs = append(apArgs, "--orchestrator-fleet", opts.fleetCSV)
					} else {
						apArgs = append(apArgs, "--orchestrator-fleet", strings.Join(weaveDefaultFleet, ","))
					}
					if opts.reviewAgent != "" {
						apArgs = append(apArgs, "--review-agent", opts.reviewAgent)
					}

					apCmd := exec.Command(self, apArgs...)
					apCmd.Dir = cwd // run in project directory so it can resolve repo root
					var apStdout bytes.Buffer
					apCmd.Stdout = io.MultiWriter(cmd.OutOrStdout(), &apStdout)
					apCmd.Stderr = cmd.ErrOrStderr()

					if err := apCmd.Run(); err != nil {
						if opts.reviewAgent != "" {
							if _, ok := weavePairVerdictFromOutput(apStdout.String()); !ok {
								reason := fmt.Sprintf("heartbeat autopilot exited without a pair verdict: %v", err)
								weaveRecordPairHarnessError(queueDir, reason)
								fmt.Fprintln(cmd.OutOrStdout(), (weavePairReviewResult{
									Verdict: weavePairHarnessError, Reason: reason,
								}).verdictLine())
							}
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "heartbeat: autopilot exited with error: %v\n", err)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "[%s] autopilot completed a pass\n", time.Now().UTC().Format(time.RFC3339))
					}
				}
			}
			return false
		}()

		if shouldExit {
			return nil
		}

		select {
		case <-cmd.Context().Done():
			fmt.Fprintf(cmd.OutOrStdout(), "[%s] context cancelled, shutting down heartbeat\n", time.Now().UTC().Format(time.RFC3339))
			return nil
		case <-ticker.C:
		}
	}
}
