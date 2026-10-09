// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

// Package instance is the minimal operator verb over fleet.InstanceStore:
// list the conversations, open a fresh one on a family, retire one.
package instance

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

// NewCmd builds `instance list|open|retire` over the host's instance store.
// A nil store means the host default; tests pass a temp one.
func NewCmd(store *fleet.InstanceStore) *cobra.Command {
	if store == nil {
		store = fleet.NewInstanceStore("")
	}
	cmd := &cobra.Command{
		Use:           "instance",
		Short:         "conversations (instances) on a family: list, open, retire",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newListCmd(store), newOpenCmd(store), newRetireCmd(store))
	return cmd
}

func newListCmd(store *fleet.InstanceStore) *cobra.Command {
	return &cobra.Command{
		Use:           "list",
		Short:         "list instances, live and retired",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			all, err := store.List()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "UUID\tLABEL\tFAMILY\tSTATE")
			for _, i := range all {
				state := "active"
				if !i.Active() {
					state = "retired"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", i.UUID, i.Label, i.Family, state)
			}
			return w.Flush()
		},
	}
}

func newOpenCmd(store *fleet.InstanceStore) *cobra.Command {
	var label, handle string
	cmd := &cobra.Command{
		Use:   "open <agent|tool:model>",
		Short: "open a fresh instance (new UUID, empty mailbox) and print its export line",
		Long: `open starts a fresh conversation on a family: a catalog agent (a cascade
agent is a predefined composite) or a raw tool:model binding. It prints the
line an external session evals once so that inbox and authored commands need
no --as:

  eval "$(bashy instance open claude:opus5.5)"`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			family, err := familyFor(args[0])
			if err != nil {
				return err
			}
			inst, err := store.Open(family, fleet.OpenOptions{Label: label, Handle: handle})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "opened %s (%s) on %s\n", inst.Label, inst.UUID, inst.Family)
			fmt.Fprintf(cmd.OutOrStdout(), "export BASHY_PRINCIPAL=%s BASHY_INSTANCE=%s\n", inst.URN(), inst.UUID)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "display label to hold (default: next free, e.g. Esme-2)")
	cmd.Flags().StringVar(&handle, "handle", "", "optional short address for this instance")
	return cmd
}

func newRetireCmd(store *fleet.InstanceStore) *cobra.Command {
	return &cobra.Command{
		Use:           "retire <uuid|label>",
		Short:         "archive an instance's mail and release its label",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			inst, err := find(store, args[0])
			if err != nil {
				return err
			}
			archive, err := store.Retire(inst.UUID, bus.InstanceMail)
			if err != nil {
				return err
			}
			principalRelease(inst)
			fmt.Fprintf(cmd.OutOrStdout(), "retired %s (%s); mail archived at %s\n", inst.Label, inst.UUID, archive)
			return nil
		},
	}
}

// principalRelease drops the retired instance's ownership claim, if any.
func principalRelease(inst fleet.Instance) {
	if owner, ok := principal.InstanceOwner(inst); ok {
		principal.ReleaseInstance(inst, owner.SessionClaim)
	}
}

// find resolves a UUID, or a live label or handle.
func find(store *fleet.InstanceStore, ref string) (fleet.Instance, error) {
	ref = strings.TrimSpace(ref)
	if id, ok := bus.ExplicitInstanceID(ref); ok {
		return store.Get(id)
	}
	if inst, ok, err := store.ResolveLabel(ref); err != nil {
		return fleet.Instance{}, err
	} else if ok {
		return inst, nil
	}
	if inst, ok, err := store.ResolveHandle(ref); err != nil {
		return fleet.Instance{}, err
	} else if ok {
		return inst, nil
	}
	return fleet.Instance{}, fmt.Errorf("%w: %q (try `bashy instance list`)", fleet.ErrInstanceUnknown, ref)
}

// familyFor derives the family a name declares: a catalog agent (a cascade is
// the predefined composite) or a raw tool:model binding as its own single family.
func familyFor(name string) (fleet.Family, error) {
	name = strings.TrimSpace(name)
	f, ok, err := fleet.New().FamilyOf(name)
	if err != nil {
		return fleet.Family{}, err
	}
	if ok {
		return f, nil
	}
	if tool, model, cut := strings.Cut(name, ":"); cut && tool != "" && model != "" {
		return fleet.Family{Name: name, Display: tool, Policy: fleet.PolicySingle, Bindings: []string{name}}, nil
	}
	return fleet.Family{}, fmt.Errorf("instance: %q is neither a catalog agent nor a tool:model binding (`bashy agent list`)", name)
}
