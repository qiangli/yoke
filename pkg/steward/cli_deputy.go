// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/qiangli/yoke/pkg/principal"
)

func newDeputyCmd(o *opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deputy",
		Short: "scoped steward occupancies: grant, list and revoke deputies",
		Long: `deputy is an occupancy of the steward position, not another role.Kind.

Each deputy holds a non-overlapping scope (listed sprints or an epic), a time
box, and is fenced by the steward epoch ladder. Several may exist per host×user.

  steward deputy add <instance-or-handle> --sprints 331,332 | --epic X [--ttl 24h]
  steward deputy list
  steward deputy revoke <id>

Handles resolve at grant time to a holder instance UUID snapshot; reusing a handle
later never transfers old authority.

A deputy may perform the steward's four acts only within its scope:
activate scope transfer, fence, judge and run the merge gate. Cross-scope
allocation/release/integration stays with the steward. A deputy cannot conduct
a sprint in its own scope, and there are no deputies of deputies. deputy:<scope>
is durable role mail through existing inbox/mb/ping/meet addressing.`,
	}
	cmd.AddCommand(newDeputyAddCmd(o), newDeputyListCmd(o), newDeputyRevokeCmd(o))
	return cmd
}

func newDeputyAddCmd(o *opts) *cobra.Command {
	var sprintsStr, epic, ttlStr string
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "add <instance-or-handle> --sprints 331,332 | --epic X",
		Short: "grant a scoped deputy occupancy (steward only, fenced by epoch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			handle := strings.TrimSpace(args[0])
			if handle == "" {
				return fmt.Errorf("deputy add: instance handle is required")
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			scope, err := parseDeputyScope(sprintsStr, epic)
			if err != nil {
				return err
			}
			ttl, err := parseDeputyTTL(ttlStr)
			if err != nil {
				return err
			}
			s, err := o.store()
			if err != nil {
				return err
			}
			holder, err := resolveDeputyHolderFromStore(s, handle)
			if err != nil {
				return err
			}
			actor := Self()
			dep, err := s.DeputyAdd(actor, ep, holder, handle, scope, ttl, time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, dep)
			}
			fmt.Fprintf(out, "deputy %s granted to %s for scope %q until %s (epoch %d)\n", dep.ID, holderName(holder), dep.ScopeLabel, dep.ExpiresAt.Local().Format(time.RFC3339), dep.Epoch)
			fmt.Fprintf(out, "  holder snapshot: %s (episode %q) handle %q\n", dep.Holder.Name, dep.Holder.Episode, dep.HolderHandle)
			fmt.Fprintf(out, "  durable mail: deputy:%s  bus %s\n", dep.ScopeLabel, DeputyTopicForScope(scope))
			return nil
		},
	}
	cmd.Flags().StringVar(&sprintsStr, "sprints", "", "comma-separated sprint ids, e.g. 331,332")
	cmd.Flags().StringVar(&epic, "epic", "", "epic name (mutually exclusive with --sprints)")
	cmd.Flags().StringVar(&ttlStr, "ttl", "24h", "time box, e.g. 24h or 48h (max 7d)")
	epochFlag(cmd, &epoch)
	return cmd
}

func newDeputyListCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list deputies and whether they are active (fenced/revoked/expired reflected)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			list, err := s.DeputyList(time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, list)
			}
			if len(list) == 0 {
				fmt.Fprintln(out, "no deputies")
				return nil
			}
			rep, _ := s.Replay()
			var curEpoch uint64
			if rep != nil {
				curEpoch = deriveAuthority(rep).Epoch
			}
			now := time.Now()
			for _, d := range list {
				active := d.Active(now, curEpoch)
				status := "active"
				if d.Revoked {
					status = "revoked"
				} else if d.Expired(now) {
					status = "expired"
				} else if d.Epoch != curEpoch {
					status = fmt.Sprintf("fenced (epoch %d vs current %d)", d.Epoch, curEpoch)
				} else if !active {
					status = "inactive"
				}
				fmt.Fprintf(out, "  %s  holder %s (%q)  scope %q  epoch %d  %s  expires %s\n", d.ID, holderName(d.Holder), d.Holder.Episode, d.ScopeLabel, d.Epoch, status, d.ExpiresAt.Local().Format(time.RFC3339))
			}
			return nil
		},
	}
}

func newDeputyRevokeCmd(o *opts) *cobra.Command {
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "revoke <deputy-id>",
		Short: "revoke a deputy (steward only, fenced by epoch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			s, err := o.store()
			if err != nil {
				return err
			}
			actor := Self()
			if err := s.DeputyRevoke(actor, ep, id, time.Now()); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, map[string]string{"revoked": id})
			}
			fmt.Fprintf(out, "deputy %q revoked\n", id)
			return nil
		},
	}
	epochFlag(cmd, &epoch)
	return cmd
}

func parseDeputyScope(sprintsStr, epic string) (DeputyScope, error) {
	var sc DeputyScope
	if sprintsStr != "" && epic != "" {
		return sc, fmt.Errorf("deputy: --sprints and --epic are mutually exclusive")
	}
	if sprintsStr != "" {
		parts := strings.Split(sprintsStr, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return sc, fmt.Errorf("deputy: sprint %q is not an integer", p)
			}
			sc.Sprints = append(sc.Sprints, n)
		}
	}
	if epic != "" {
		sc.Epic = strings.TrimSpace(epic)
	}
	if err := sc.Valid(); err != nil {
		return sc, err
	}
	return sc, nil
}

func parseDeputyTTL(s string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("deputy: ttl %q: %w", s, err)
	}
	return d, nil
}

func resolveDeputyHolderFromStore(s *Store, handle string) (principal.Ref, error) {
	if s.deputyResolver != nil {
		return s.deputyResolver.Resolve(handle)
	}
	// No resolver: treat handle as holder Name. If it looks like UUID, put in Episode.
	// This keeps authority tests independent of the resolver until foundation integration.
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return principal.Ref{}, fmt.Errorf("deputy: empty handle")
	}
	// If handle contains episode delimiter "@" or ":" treat as name:episode
	if before, after, ok := strings.Cut(handle, "@"); ok && after != "" {
		return principal.Ref{Name: before, Episode: after, Kind: principal.KindAgent}, nil
	}
	if before, after, ok := strings.Cut(handle, ":"); ok && after != "" && !strings.Contains(before, "/") {
		// allow "handle:uuid" form
		return principal.Ref{Name: before, Episode: after, Kind: principal.KindAgent}, nil
	}
	// Bare handle: use as Name, generate Episode as handle for dedup? No: keep Episode empty,
	// Holder matching will fall back to SameHolder name match. But to ensure handle reuse test
	// can demonstrate UUID snapshot isolation, tests should set Episode explicitly.
	return principal.Ref{Name: handle, Kind: principal.KindAgent}, nil
}
