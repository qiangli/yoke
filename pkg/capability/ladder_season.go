package capability

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/ladder"
)

type ladderSeasonOutput struct {
	Season  int                           `json:"season"`
	Lines   ladder.Lines                  `json:"lines"`
	Changes []ladder.BandChange           `json:"changes"`
	States  map[string]ladder.SeasonState `json:"states"`
}

// newLadderSeasonEndCmd closes one ladder season and persists its derived state.
func newLadderSeasonEndCmd() *cobra.Command {
	var season int
	var dryRun, asJSON, force bool
	var anchors string
	cmd := &cobra.Command{
		Use:   "season-end",
		Short: "compute and record the ladder season transition",
		Long: `Replay ladder evidence through the selected season, apply promotion and
demotion rules, and save the next season state. Band changes live in the season
state file; the event ledger receives a seat event only when a provisional seat
is confirmed or expires, so replay no longer treats that seat as active.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if season < 1 {
				return fmt.Errorf("season-end: --season must be >= 1")
			}
			base := filepath.Join(ladderSeasonHome(), "ladder")
			seasonDir := filepath.Join(base, "seasons")
			statePath := filepath.Join(seasonDir, fmt.Sprintf("%d.json", season))
			if _, err := os.Stat(statePath); err == nil && !force {
				return fmt.Errorf("season-end: season %d already exists (use --force to replace)", season)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			events, err := ladderRecordRead()
			if err != nil {
				return err
			}
			rep := ladder.Replay(events, season)
			lines := LadderLines(rep, season)
			anchorNames, err := ladderSeasonAnchors(anchors, filepath.Join(base, "anchors.txt"))
			if err != nil {
				return fmt.Errorf("season-end: reading anchors: %w", err)
			}
			ladderSeasonAnchorLines(&lines, rep, anchorNames)

			states := make(map[string]ladder.SeasonState)
			if season > 1 {
				prevPath := filepath.Join(seasonDir, fmt.Sprintf("%d.json", season-1))
				if raw, readErr := os.ReadFile(prevPath); readErr == nil {
					var prev ladderSeasonOutput
					if err := json.Unmarshal(raw, &prev); err != nil {
						return fmt.Errorf("season-end: reading previous state: %w", err)
					}
					states = prev.States
				} else if !errors.Is(readErr, os.ErrNotExist) {
					return readErr
				}
			}
			for name := range rep.Agents {
				rec := rep.Agents[name]
				if _, ok := states[name]; !ok {
					states[name] = ladder.SeasonState{Agent: name}
				}
				if states[name].ProvisionalSince == 0 && rec.Provisional > 0 {
					for _, e := range events {
						if e.RatingAgent() == name && e.Kind == ladder.EventKindSeat && e.Provisional > 0 && e.Season <= season {
							states[name] = ladder.SeasonState{Agent: name, Band: states[name].Band, Demoted: states[name].Demoted, ModelVersion: states[name].ModelVersion, ProvisionalSince: e.Season}
						}
					}
				}
			}
			profiles := make(map[string]ladder.Profile, len(rep.Agents))
			for name, rec := range rep.Agents {
				profiles[name] = dutyProfile(rec, rec.Provisional)
				states[name] = ladderSeasonEnsureVersion(states[name], rec)
			}
			next, changes := ladder.SeasonEnd(states, profiles, ladder.SeasonEndOptions{Season: season, Lines: lines})
			result := ladderSeasonOutput{Season: season, Lines: lines, Changes: changes, States: next}
			if !dryRun {
				if err := os.MkdirAll(seasonDir, 0700); err != nil {
					return err
				}
				for name, profile := range profiles {
					if profile.Provisional > 0 && next[name].ProvisionalSince == 0 {
						clear := ladderRecordBase(season)
						clear.Kind = ladder.EventKindSeat
						clear.Agent = name
						clear.Provisional = 0
						clear.Note = "season-end provisional seat resolved"
						if err := ladderSeasonAppend(clear); err != nil {
							return err
						}
					}
				}
				data, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					return err
				}
				data = append(data, '\n')
				if !force {
					f, err := os.OpenFile(statePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
					if err != nil {
						return fmt.Errorf("season-end: creating state: %w", err)
					}
					_, werr := f.Write(data)
					cerr := f.Close()
					if werr != nil {
						return werr
					}
					if cerr != nil {
						return cerr
					}
				} else if err := os.WriteFile(statePath, data, 0600); err != nil {
					return err
				}
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "season %d changes\n", season)
			if len(changes) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(none)")
			}
			for _, c := range changes {
				fmt.Fprintf(cmd.OutOrStdout(), "%-28s %d -> %d  %s\n", c.Agent, c.From, c.To, c.Reason)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&season, "season", 0, "season to close")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "compute without writing files or events")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable transition")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing season state file")
	cmd.Flags().StringVar(&anchors, "l4-anchors", "", "two comma-separated L4 anchor agents")
	return cmd
}

func ladderSeasonHome() string {
	if h := os.Getenv("BASHY_HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".bashy")
	}
	return ".bashy"
}
func ladderSeasonAnchors(flag, path string) ([]string, error) {
	raw := flag
	if raw == "" {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		raw = string(b)
	}
	raw = strings.ReplaceAll(raw, "\n", ",")
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" && !strings.HasPrefix(s, "#") {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	if len(out) != 2 || out[0] == out[1] {
		return nil, fmt.Errorf("exactly two distinct agents are required")
	}
	return out, nil
}
func ladderSeasonAnchorLines(lines *ladder.Lines, rep ladder.ReplayResult, anchors []string) {
	if len(anchors) != 2 {
		return
	}
	a, b := rep.Agents[anchors[0]], rep.Agents[anchors[1]]
	if a == nil || b == nil {
		return
	}
	// The L4 anchor pair is ready only after both agents have established
	// code and manage histories; do not fit one L4 line early.
	for _, rec := range []*ladder.AgentRecord{a, b} {
		if !rec.Standings[ladder.DutyCode].Established() || !rec.Standings[ladder.DutyManage].Established() {
			return
		}
	}
	for _, d := range []ladder.Duty{ladder.DutyCode, ladder.DutyManage} {
		x, y := a.Standings[d], b.Standings[d]
		if x.Established() && y.Established() {
			v := x.Lower()
			if y.Lower() < v {
				v = y.Lower()
			}
			if d == ladder.DutyCode {
				lines.L4Code = v
			} else {
				lines.L4Manage = v
			}
		}
	}
}
func ladderSeasonEnsureVersion(st ladder.SeasonState, rec *ladder.AgentRecord) ladder.SeasonState {
	if st.Agent == "" {
		st.Agent = rec.Agent
	}
	if st.ModelVersion == "" {
		st.ModelVersion = dutyModelVersion(rec.Certs)
	}
	return st
}
func ladderSeasonAppend(e ladder.Event) error {
	st, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		return err
	}
	return st.Append(e)
}
