// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newAppNew is `app new NAME`: a blank shaped app, made the way a document
// editor makes a new document (NewBlankShapedApp).
func newAppNew(opts []Option) *cobra.Command {
	var a App
	c := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a blank app to shape from an empty page",
		Long: "Create a new app from a blank page. The app starts as one empty page with\n" +
			"a contract and a smoke check, registered as a shaped app. Open it and use\n" +
			"the Genie button to describe what it should do: every kept change is a\n" +
			"verified version of your own. There is no project, build or deploy step.",
		Example: "  bashy app new signup --label \"Sign-up sheet\"",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Name = args[0]
			saved, err := NewBlankShapedApp(New(opts...), a)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s -> /%s/ (new blank app, %s)\n", saved.Name, saved.Name, saved.Source)
			fmt.Fprintln(cmd.ErrOrStderr(), "note: "+AppsHint)
			return nil
		},
	}
	c.Flags().StringVar(&a.Label, "label", "", "tile label and page title (default: the name)")
	c.Flags().StringVar(&a.Icon, "icon", "", "one emoji, or SVG path data on a 24 grid")
	c.Flags().StringVar(&a.Tip, "tip", "", "tile tooltip")
	return c
}
