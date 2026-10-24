package prune

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type Result struct {
	Noun    string `json:"noun"`
	Applied bool   `json:"applied"`
	Items   []Item `json:"items"`
	Counts  Counts `json:"counts"`
}

// NewNounCommand gives a rod the common --older, --apply, and --json surface.
func NewNounCommand(n Noun) *cobra.Command {
	var older string
	var apply, asJSON, dryRun bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "List stale items and optionally remove them",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if apply && dryRun {
				return fmt.Errorf("--apply and --dry-run cannot be combined")
			}
			opt := Options{Apply: apply, JSON: asJSON}
			if older != "" {
				var err error
				opt.Older, err = ParseOlder(older)
				if err != nil {
					return err
				}
			}
			items, err := n.DryRun(c.Context(), opt)
			if err != nil {
				return err
			}
			if items == nil {
				items = []Item{}
			}
			result := Result{Noun: n.Name(), Applied: apply, Items: items}
			if apply {
				result.Counts, err = n.Apply(c.Context(), items)
				if err != nil {
					return err
				}
			}
			if asJSON {
				return json.NewEncoder(c.OutOrStdout()).Encode(result)
			}
			PrintItems(c.OutOrStdout(), items, apply, result.Counts)
			return nil
		},
	}
	c.Flags().StringVar(&older, "older", "", "only items older than duration (e.g. 30d)")
	c.Flags().BoolVar(&apply, "apply", false, "remove eligible items after rechecking them")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "list eligible items without removing them (default)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit structured items and counts")
	return c
}

type OverviewRow struct {
	Noun    string `json:"noun"`
	Count   int    `json:"count"`
	Size    int64  `json:"size"`
	Command string `json:"command"`
	Items   []Item `json:"items"`
	Error   string `json:"error,omitempty"`
}

// Overview is read-only even if a caller constructs Options{Apply:true}.
func (r *Registry) Overview(ctx context.Context) []OverviewRow {
	rows := make([]OverviewRow, 0, len(r.nouns))
	for _, n := range r.Nouns() {
		row := OverviewRow{Noun: n.Name(), Command: "bashy " + n.Name() + " prune --apply", Items: []Item{}}
		items, err := n.DryRun(ctx, Options{})
		if err != nil {
			row.Error = err.Error()
		} else {
			row.Items = items
			if row.Items == nil {
				row.Items = []Item{}
			}
			row.Count = len(items)
			row.Size = SumSize(items)
		}
		rows = append(rows, row)
	}
	return rows
}

// NewOverviewCommand is mounted as bare `bashy prune` by the shell repo.
func NewOverviewCommand(r *Registry) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "prune",
		Short: "Show stale items for every registered noun; never delete",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			rows := r.Overview(c.Context())
			if asJSON {
				return json.NewEncoder(c.OutOrStdout()).Encode(rows)
			}
			for _, row := range rows {
				if row.Error != "" {
					fmt.Fprintf(c.OutOrStdout(), "%s  error: %s  -> %s\n", row.Noun, strings.ReplaceAll(row.Error, "\n", " "), row.Command)
					continue
				}
				fmt.Fprintf(c.OutOrStdout(), "%s  %d item(s) (%s)  -> %s\n", row.Noun, row.Count, FormatSize(row.Size), row.Command)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit one structured row per noun")
	return c
}
