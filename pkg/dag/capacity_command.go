package dag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// AddCapacityCommands mounts the explicit planning/dispatch endpoint alongside
// normal DAG execution. Its target-local policy is checked on every request.
func AddCapacityCommands(cmd *cobra.Command, services CapacityServices) {
	root := &cobra.Command{Use: "capacity", Short: "Inspect and explicitly dispatch compatible work to permitted fleet targets"}
	for _, verb := range []string{"plan", "dispatch"} {
		var file string
		child := &cobra.Command{Use: verb + " --request <request.json>", Args: cobra.NoArgs}
		child.Flags().StringVar(&file, "request", "", "versioned task request; dispatch is the explicit execution action")
		_ = child.MarkFlagRequired("request")
		child.RunE = func(cmd *cobra.Command, _ []string) error {
			f, e := os.Open(file)
			if e != nil {
				return e
			}
			defer f.Close()
			var r CapacityRequest
			if e = decodeCapacity(f, &r); e != nil {
				return e
			}
			if e = validateCapacityRequest(r); e != nil {
				return e
			}
			client, e := NewCapacityClient()
			if e != nil {
				if verb == "dispatch" {
					if e = queueCapacityRequest(r, "no explicit remote policy"); e != nil {
						return e
					}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(CapacityReply{Version: 1, Decision: "queued-local", Reason: "no explicit remote policy; work remains queued locally"})
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), r.Spec.Timeout+15*time.Second)
			defer cancel()
			var reply *CapacityReply
			if verb == "plan" {
				reply, e = client.Plan(ctx, r)
			} else {
				reply, e = client.Dispatch(ctx, r)
			}
			if e != nil {
				return e
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(reply)
		}
		root.AddCommand(child)
	}
	receive := &cobra.Command{Use: "receive", Short: "Serve one bounded authenticated-transport request using local authority", Args: cobra.NoArgs}
	receive.RunE = func(cmd *cobra.Command, _ []string) error {
		var wire capacityWire
		if e := decodeCapacity(cmd.InOrStdin(), &wire); e != nil {
			return e
		}
		p, e := LoadCapacityPolicy()
		if e != nil {
			return e
		}
		limit := 10 * time.Second
		if wire.Operation == "execute" {
			limit = wire.Request.Spec.Timeout + 5*time.Second
			if limit > 11*time.Minute {
				return errors.New("capacity duration exceeds receiver bound")
			}
		}
		signalCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(signalCtx, limit)
		defer cancel()
		reply, e := receiveCapacity(ctx, p, services, wire)
		if e != nil {
			return e
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(reply)
	}
	var target, run string
	reconcile := &cobra.Command{Use: "reconcile --target <alias> --run <id>", Short: "Release retained target demand only after verified termination", Args: cobra.NoArgs}
	reconcile.Flags().StringVar(&target, "target", "", "permitted registered target")
	reconcile.Flags().StringVar(&run, "run", "", "original capacity request ID")
	_ = reconcile.MarkFlagRequired("target")
	_ = reconcile.MarkFlagRequired("run")
	reconcile.RunE = func(cmd *cobra.Command, _ []string) error {
		client, e := NewCapacityClient()
		if e != nil {
			return e
		}
		reply, e := client.Reconcile(cmd.Context(), target, run)
		if e != nil {
			return e
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(reply)
	}
	queued := &cobra.Command{Use: "queued", Short: "Read pending local capacity requests without launching or retrying", Args: cobra.NoArgs}
	queued.RunE = func(cmd *cobra.Command, _ []string) error {
		dir := filepath.Join(filepath.Dir(CapacityPolicyPath()), "remote-capacity", "pending")
		d, e := os.Open(dir)
		if os.IsNotExist(e) {
			return json.NewEncoder(cmd.OutOrStdout()).Encode([]capacityQueuedRequest{})
		}
		if e != nil {
			return e
		}
		defer d.Close()
		entries, e := d.ReadDir(257)
		if e != nil && e != io.EOF {
			return e
		}
		if len(entries) > 256 {
			return errors.New("capacity queue listing exceeds bound")
		}
		var rows []capacityQueuedRequest
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			f, e := os.Open(filepath.Join(dir, entry.Name()))
			if e != nil {
				return e
			}
			var row capacityQueuedRequest
			e = decodeCapacity(f, &row)
			f.Close()
			if e != nil {
				return e
			}
			rows = append(rows, row)
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
	}
	root.AddCommand(receive, reconcile, queued)
	cmd.AddCommand(root)
}
func capacityCheckoutRevision(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		return "", errors.New("receiver checkout unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		var out limitedCapacityBuffer
		out.limit = 4096
		cmd.Stdout = &out
		cmd.Stderr = io.Discard
		if e := cmd.Run(); e != nil {
			return "", fmt.Errorf("receiver checkout verification failed")
		}
		return strings.TrimSpace(out.String()), nil
	}
	revision, e := run("rev-parse", "HEAD")
	if e != nil {
		return "", e
	}
	if _, e = run("diff", "--quiet", "HEAD", "--"); e != nil {
		return "", e
	}
	untracked, e := run("ls-files", "--others")
	if e != nil {
		return "", e
	}
	if untracked != "" {
		return "", errors.New("receiver checkout contains untracked files")
	}
	return revision, nil
}
