// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package reduce

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// Recover writes the complete spilled bytes identified by handle to w. It is the
// engine behind the `bashy out <id>` recovery verb: the handle in an elision
// marker is a runnable command, not an opaque id, so an agent restores the
// elided region with a tool it already has and can compose it with anything else
// (`bashy out 9c2d4f1a | rg FAIL`).
func Recover(store *Store, handle string, w io.Writer) error {
	content, _, err := store.Get(handle)
	if err != nil {
		return err
	}
	_, err = w.Write(content)
	return err
}

// NewOutCmd returns the `out` cobra command a front end mounts as `bashy out`.
// resolve supplies the spill store lazily so the host binds it to its existing
// command/session/run artifact path (no seventh store).
func NewOutCmd(resolve func() (*Store, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "out <handle>",
		Short: "Print the complete output that a reduction elided",
		Long: `out reprints the full, un-reduced bytes that an elision marker spilled to a
content-addressed artifact. The handle is the digest prefix shown in the marker
(for example 'bashy out 9c2d4f1a'). An ambiguous prefix is reported so you can
lengthen it; the recovered bytes are byte-identical to the original output and
compose with anything else (` + "`bashy out 9c2d4f1a | rg FAIL`" + `).`,
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			if resolve == nil {
				return fmt.Errorf("out: no spill store configured")
			}
			store, err := resolve()
			if err != nil {
				return err
			}
			return Recover(store, args[0], c.OutOrStdout())
		},
	}
}
