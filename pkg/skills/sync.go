// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/spf13/cobra"
)

// CloudRingDir is where `skills sync` writes the org catalog, and therefore
// where a host must mount a SharedDirSource to read it.
//
// It lives here, not at the mount site, because a pull and a read that disagree
// about the path is the worst failure this can have: sync reports "N pulled"
// and the skills never appear, which looks like an empty org catalog rather
// than a wiring bug. One expression, used by both ends.
//
// The layout is fleet's, deliberately: tools, models and agents already cache
// their overlays under the same root, so a paired host has ONE directory to
// inspect, back up, or delete.
func CloudRingDir() string {
	return filepath.Join(fleet.CloudCacheRoot(fleet.DefaultRoot()), "skills")
}

// newSyncCmd builds the `sync` verb: pull the org skill catalog into the
// shared ring.
//
// skills was the only asset kind without it. fleet.Sync has accepted "skills"
// since it was written ("dirSkills is accepted by Sync even though this package
// does not read skills; pkg/skills owns that ring") and SharedDirSource has
// always documented itself as serving "any git clone or synced folder" — the
// two ends existed and were never connected.
//
// Failing to reach the control plane is an error here, as it is for the other
// nouns: the caller explicitly asked to pull. Every other verb keeps working
// from the cached ring, and an unpaired host never needed it.
func newSyncCmd() *cobra.Command {
	var cfg fleet.CloudConfig
	var asJSON bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "Pull the org skill catalog into the shared ring",
		Long: "Pull the org skill catalog into the shared ring.\n\n" +
			"The shared ring sits above the embedded baseline and below the\n" +
			"host-local store: an org skill beats what bashy shipped, and a skill\n" +
			"you added or learned here beats the org. Everything works without it —\n" +
			"pairing only enhances.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			res, err := client.Sync(fleet.CloudCacheRoot(fleet.DefaultRoot()), "skills")
			if err != nil {
				return err
			}
			unpacked, err := expandRecords(res.Dir)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					fleet.SyncResult
					Unpacked int `json:"unpacked,omitempty"`
				}{res, unpacked})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "skills: %d pulled into %s\n", res.Fetched, res.Dir)
			if unpacked > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "skills: %d unpacked from records into full folders\n", unpacked)
			}
			return nil
		},
	}
	c.Flags().StringVar(&cfg.URL, "url", "", "control-plane base URL (default $BASHY_CLOUDBOX_URL)")
	c.Flags().StringVar(&cfg.Token, "token", "", "Bearer token (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return c
}

// expandRecords rewrites every pulled skill whose SKILL.md is in fact a
// `kind: skill` record into the full folder that record carries, and reports
// how many it unpacked.
//
// fleet.Sync writes whatever the registry served as <name>/SKILL.md. That is
// right for the plain-SKILL.md registry of today and wrong the moment an entry
// is published as a record: the ring would serve a YAML document as a skill
// body, and reference.md / skill.dhnt would be nowhere. A Content blob that
// ParseRecord accepts (strict fields, kind: skill, files carrying SKILL.md) is
// unpacked in place under the registry's entry name; anything else is left
// exactly as written, so the current cloudbox keeps working unchanged. A plain
// SKILL.md cannot be mistaken for a record — its frontmatter has no kind and
// carries keys the strict decoder refuses.
func expandRecords(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		folder := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(filepath.Join(folder, skillMarker))
		if err != nil {
			continue
		}
		rec, err := ParseRecord(body)
		if err != nil {
			continue
		}
		if err := os.RemoveAll(folder); err != nil {
			return n, err
		}
		if err := WriteFolder(rec, folder); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
