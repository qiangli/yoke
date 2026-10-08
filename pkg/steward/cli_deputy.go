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
	"github.com/qiangli/yoke/pkg/role"
)

func newDeputyCmd(o *opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deputy",
		Short: "scoped steward occupancies: grant, list and revoke deputies",
		Long: `deputy is an occupancy of the steward position, not another role.

Each deputy holds a non-overlapping scope (listed sprints or an epic), a time
box, and is fenced by the steward epoch. Several may exist per host×user.

  steward deputy add <instance-uuid-or-handle> --sprints 331,332 | --epic X [--ttl 24h]
  steward deputy list
  steward deputy revoke <deputy-id>
  steward deputy occupant <deputy:scope>

The holder resolves at grant time, through the host's instance store, to an
instance UUID snapshot; an unknown or retired instance is refused, and reusing
a handle or label later transfers nothing. Inside its scope a deputy may
perform the steward's four acts (` + "`steward act`" + `); cross-scope allocation,
release and integration stay with the steward.`,
	}
	cmd.AddCommand(newDeputyAddCmd(o), newDeputyListCmd(o), newDeputyRevokeCmd(o), newDeputyOccupantCmd(o))
	return cmd
}

func newDeputyAddCmd(o *opts) *cobra.Command {
	var sprintsStr, epic, ttlStr string
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "add <instance-uuid-or-handle> --sprints 331,332 | --epic X",
		Short: "grant a scoped deputy occupancy (steward only, fenced by epoch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scope, err := parseDeputyScope(sprintsStr, epic)
			if err != nil {
				return err
			}
			ttl, err := time.ParseDuration(strings.TrimSpace(ttlStr))
			if err != nil {
				return fmt.Errorf("deputy: --ttl %q: %w", ttlStr, err)
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			s, err := o.store()
			if err != nil {
				return err
			}
			handle := strings.TrimSpace(args[0])
			holder, err := s.DeputyResolver().Resolve(handle)
			if err != nil {
				return err
			}
			dep, err := s.DeputyAdd(Self(), ep, holder, handle, scope, ttl, time.Now())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, dep)
			}
			fmt.Fprintf(out, "deputy %s granted: instance %s (handle %q) scope %q until %s (epoch %d)\n",
				dep.ID, dep.Holder.Episode, dep.HolderHandle, dep.ScopeLabel, dep.ExpiresAt.Local().Format(time.RFC3339), dep.Epoch)
			fmt.Fprintf(out, "  role address: %s  (bus topic %s)\n", DeputyAddress(dep.Scope), DeputyTopic(dep.Scope))
			return nil
		},
	}
	cmd.Flags().StringVar(&sprintsStr, "sprints", "", "comma-separated sprint ids, e.g. 331,332")
	cmd.Flags().StringVar(&epic, "epic", "", "epic name (exclusive with --sprints)")
	cmd.Flags().StringVar(&ttlStr, "ttl", "24h", "time box (max 168h)")
	epochFlag(cmd, &epoch)
	return cmd
}

func deputyState(d Deputy, now time.Time, cur uint64) string {
	if err := d.inactiveReason(now, cur); err != nil {
		switch err.(type) {
		case *ErrDeputyRevoked:
			return "revoked"
		case *ErrDeputyExpired:
			return "expired"
		}
		return fmt.Sprintf("fenced (epoch %d, steward at %d)", d.Epoch, cur)
	}
	return "active"
}

func newDeputyListCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list deputy grants with their state (active, revoked, expired, fenced)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			list, cur, err := s.DeputyList()
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
			now := time.Now()
			for _, d := range list {
				fmt.Fprintf(out, "%s  %s  instance %s  epoch %d  %s  expires %s\n",
					d.ID, DeputyAddress(d.Scope), d.Holder.Episode, d.Epoch, deputyState(d, now, cur), d.ExpiresAt.Local().Format(time.RFC3339))
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
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			s, err := o.store()
			if err != nil {
				return err
			}
			id := strings.TrimSpace(args[0])
			if err := s.DeputyRevoke(Self(), ep, id, time.Now()); err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), map[string]string{"revoked": id})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deputy %s revoked\n", id)
			return nil
		},
	}
	epochFlag(cmd, &epoch)
	return cmd
}

func newDeputyOccupantCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "occupant [deputy:<scope>]",
		Short: "resolve deputy role addresses to their current holder instance (empty when vacant)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := o.store()
			if err != nil {
				return err
			}
			now := time.Now()
			var list []DeputyOccupancy
			if len(args) == 1 {
				occ, ok, err := s.DeputyOccupant(args[0], now)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("deputy: no deputy address %q has been granted on this seat", args[0])
				}
				list = []DeputyOccupancy{occ}
			} else if list, err = s.DeputyOccupancies(now); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, list)
			}
			for _, occ := range list {
				holder := occ.Holder
				if occ.Vacant {
					holder = "(vacant: " + occ.State + ")"
				}
				fmt.Fprintf(out, "%s  %s  %s\n", occ.Address, holder, occ.DeputyID)
			}
			return nil
		},
	}
}

// actActor is who is acting: this process's identity, carrying its instance
// UUID when the session established one (BASHY_PRINCIPAL / BASHY_INSTANCE),
// because a deputy occupancy matches on the instance UUID and nothing else.
func actActor() principal.Ref {
	self := Self()
	if id, ok := principal.SelfInstanceUUID(); ok {
		if inst := InstanceRef(id); inst.Episode != "" {
			self.Episode = inst.Episode
		}
	}
	return self
}

func newActCmd(o *opts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "act",
		Short: "the steward's four acts over a sprint or epic — steward, or a deputy inside its scope",
		Long: `act performs one of the four acts that require a seat (orchestration roles §2b):

  activate  transfer a sprint to a conductor        (--owner required)
  fence     take over a stale/stuck conductor seat  (--owner successor, or none to evict)
  judge     judge an outcome: accept or close at box (--outcome success|failed)
  gate      run the merge gate / author the convergence claim

The steward may act on any target. A deputy may act only inside its own scope,
under the steward's current epoch; a stale or zero epoch is refused, as are
cross-scope targets, conducting inside its own scope, and judging work it
conducted or authored.`,
	}
	for _, act := range []string{ActActivate, ActFence, ActJudge, ActGate} {
		cmd.AddCommand(newActVerbCmd(o, act))
	}
	return cmd
}

func newActVerbCmd(o *opts, act string) *cobra.Command {
	var (
		sprint             int
		epic, owner        string
		summary, rationale string
		outcome            string
		evidence           []string
		targetSeq, epoch   uint64
	)
	cmd := &cobra.Command{
		Use:   act + " --sprint N | --epic X",
		Short: act + " (steward, or a deputy inside its scope)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			evs, err := parseEvidenceList(evidence)
			if err != nil {
				return err
			}
			ep, err := ResolveEpoch(epoch)
			if err != nil {
				return err
			}
			s, err := o.store()
			if err != nil {
				return err
			}
			e, err := s.Act(actActor(), ep, ActRequest{
				Act:       act,
				Target:    ActTarget{Sprint: sprint, Epic: strings.TrimSpace(epic)},
				Owner:     owner,
				Summary:   summary,
				Rationale: rationale,
				Outcome:   Outcome(outcome),
				Evidence:  evs,
				TargetSeq: targetSeq,
			}, time.Now())
			if err != nil {
				return err
			}
			if o.asJSON {
				return emitJSON(cmd.OutOrStdout(), e)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s recorded on %s: seq %d (epoch %d)\n", act, e.Workstream, e.Seq, e.Epoch)
			return nil
		},
	}
	cmd.Flags().IntVar(&sprint, "sprint", 0, "the sprint acted on")
	cmd.Flags().StringVar(&epic, "epic", "", "the epic acted on (exclusive with --sprint)")
	cmd.Flags().StringVarP(&summary, "message", "m", "", "what was done")
	cmd.Flags().StringVar(&rationale, "rationale", "", "why")
	cmd.Flags().StringSliceVarP(&evidence, "evidence", "e", nil, "supporting reference (repeatable)")
	epochFlag(cmd, &epoch)
	switch act {
	case ActActivate, ActFence:
		cmd.Flags().StringVar(&owner, "owner", "", "the conductor (instance UUID) the sprint transfers to")
	case ActJudge:
		cmd.Flags().StringVar(&outcome, "outcome", string(OutcomeSuccess), "success (accept) or failed (close at box)")
		cmd.Flags().Uint64Var(&targetSeq, "target", 0, "journal seq of the claim being judged")
	case ActGate:
		cmd.Flags().Uint64Var(&targetSeq, "target", 0, "journal seq of the claim being gated")
	}
	return cmd
}

func newGlossaryCmd(o *opts) *cobra.Command {
	return &cobra.Command{
		Use:   "glossary [name]",
		Short: "the official role glossary: steward, deputy, conductor, worker — scopes, addresses, everyday words",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			entries := role.Glossary()
			if len(args) == 1 {
				e, ok := role.GlossaryByName(args[0])
				if !ok {
					return fmt.Errorf("glossary: %q is not an official role name (steward, deputy, conductor, worker)", args[0])
				}
				entries = []role.GlossaryEntry{e}
			}
			out := cmd.OutOrStdout()
			if o.asJSON {
				return emitJSON(out, entries)
			}
			for _, e := range entries {
				fmt.Fprintf(out, "%s — %s\n  scope:   %s\n  address: %s\n  holds:   %s\n", e.Name, e.Description, e.Scope, e.Address, e.Holds)
				for _, ctx := range e.AliasContexts() {
					fmt.Fprintf(out, "  %q means %s\n", ctx, e.AliasesByContext[ctx])
				}
			}
			return nil
		},
	}
}

func parseDeputyScope(sprintsStr, epic string) (DeputyScope, error) {
	var sc DeputyScope
	for _, p := range strings.Split(sprintsStr, ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return sc, fmt.Errorf("deputy: sprint %q is not an integer", p)
		}
		sc.Sprints = append(sc.Sprints, n)
	}
	sc.Epic = strings.TrimSpace(epic)
	return sc, sc.Valid()
}
