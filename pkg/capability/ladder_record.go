package capability

// `bashy leaderboard record ...` — append seat, certificate and correction
// events to the ladder event store (design docs/bashy-band-ladder-design.md
// sections 5, 8, 13).
//
// This verb's only job is the write: every subcommand appends exactly one
// event and prints one confirmation line carrying the event id. A store
// error is a non-zero exit. Nothing here rates, ranks or renders.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/principal"
)

// newLadderRecordCmd builds the `record` subcommand of `leaderboard`.
func newLadderRecordCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "record",
		Short: "append seat, certificate and correction events to the ladder store",
		Long: `Append seat, certificate and correction events to the ladder event store.

Each subcommand writes exactly one event and prints one confirmation line
carrying the event id. Season comes from --season; the agent must be
canonical tool:model.`,
	}
	cmd.AddCommand(
		newLadderRecordSeatCmd(),
		newLadderRecordCertCmd(),
		newLadderRecordCorrectionCmd(),
		newLadderRecordListCmd(),
	)
	return cmd
}

func newLadderRecordSeatCmd() *cobra.Command {
	var agent, reason string
	var band, season int
	cmd := &cobra.Command{
		Use:   "seat",
		Short: "seat a provisional band (0 clears it)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ladderRecordCanonicalAgent(agent); err != nil {
				return err
			}
			if band < 0 || band > 5 {
				return fmt.Errorf("record seat: --band must be 0..5 (0 clears the seat)")
			}
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("record seat: --reason is required (no seat without a reason)")
			}
			if season < 1 {
				return fmt.Errorf("record seat: --season is required")
			}
			e := ladderRecordBase(season)
			e.Kind = ladder.EventKindSeat
			e.Agent = agent
			e.Provisional = band
			e.Note = reason
			return ladderRecordAppend(cmd, e, fmt.Sprintf("recorded seat event %s for %s (season %d)", e.ID, e.Agent, e.Season))
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "canonical agent tool:model")
	cmd.Flags().IntVar(&band, "band", -1, "provisional band 0..5 (0 clears the seat)")
	cmd.Flags().StringVar(&reason, "reason", "", "why the seat was set")
	cmd.Flags().IntVar(&season, "season", 0, "season the seat was set in")
	return cmd
}

func newLadderRecordCertCmd() *cobra.Command {
	var agent, kind, modelVersion, evidence string
	var season int
	cmd := &cobra.Command{
		Use:   "cert",
		Short: "record a certificate backed by evidence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ladderRecordCanonicalAgent(agent); err != nil {
				return err
			}
			ck, err := ladderRecordCertKind(kind)
			if err != nil {
				return err
			}
			if strings.TrimSpace(modelVersion) == "" {
				return fmt.Errorf("record cert: --model-version is required")
			}
			if strings.TrimSpace(evidence) == "" {
				return fmt.Errorf("record cert: --evidence is required (no certificate without evidence)")
			}
			if season < 1 {
				return fmt.Errorf("record cert: --season is required")
			}
			e := ladderRecordBase(season)
			e.Kind = ladder.EventKindCert
			e.Agent = agent
			e.Cert = ladder.Certificate{Kind: ck, ModelVersion: modelVersion, Season: season}
			e.Note = evidence
			return ladderRecordAppend(cmd, e, fmt.Sprintf("recorded cert event %s for %s (season %d)", e.ID, e.Agent, e.Season))
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "canonical agent tool:model")
	cmd.Flags().StringVar(&kind, "kind", "", "certificate kind: l1|l2|l3|steer|manager|review|judge|l5")
	cmd.Flags().StringVar(&modelVersion, "model-version", "", "certified model version")
	cmd.Flags().StringVar(&evidence, "evidence", "", "run/report ref backing the certificate")
	cmd.Flags().IntVar(&season, "season", 0, "season the certificate was earned in")
	return cmd
}

func newLadderRecordCorrectionCmd() *cobra.Command {
	var supersedes, reason string
	var season int
	cmd := &cobra.Command{
		Use:   "correction",
		Short: "supersede an earlier event",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(supersedes) == "" {
				return fmt.Errorf("record correction: --supersedes is required")
			}
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("record correction: --reason is required")
			}
			if season < 1 {
				return fmt.Errorf("record correction: --season is required")
			}
			e := ladderRecordBase(season)
			e.Kind = ladder.EventKindCorrection
			e.Agent = e.Reviewer
			e.Supersedes = supersedes
			e.Note = reason
			return ladderRecordAppend(cmd, e, fmt.Sprintf("recorded correction event %s superseding %s (season %d)", e.ID, e.Supersedes, e.Season))
		},
	}
	cmd.Flags().StringVar(&supersedes, "supersedes", "", "id of the event this corrects")
	cmd.Flags().StringVar(&reason, "reason", "", "why the correction was recorded")
	cmd.Flags().IntVar(&season, "season", 0, "season the correction was recorded in")
	return cmd
}

// ladderRecordJSON is one `record list --json` row.
type ladderRecordJSON struct {
	ID      string `json:"id"`
	At      string `json:"at"`
	Season  int    `json:"season"`
	Kind    string `json:"kind"`
	Agent   string `json:"agent"`
	Summary string `json:"summary"`
}

func newLadderRecordListCmd() *cobra.Command {
	var agent, kind string
	var n int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list recent ladder events, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if n < 0 {
				return fmt.Errorf("record list: -n must be >= 0")
			}
			events, err := ladderRecordRead()
			if err != nil {
				return err
			}
			kept := events[:0:0]
			for _, e := range events {
				if agent != "" && e.Agent != agent {
					continue
				}
				if kind != "" && string(e.Kind) != kind {
					continue
				}
				kept = append(kept, e)
			}
			sort.SliceStable(kept, func(i, j int) bool {
				if !kept[i].At.Equal(kept[j].At) {
					return kept[i].At.After(kept[j].At)
				}
				return kept[i].ID > kept[j].ID
			})
			if n < len(kept) {
				kept = kept[:n]
			}
			out := cmd.OutOrStdout()
			if asJSON {
				rows := make([]ladderRecordJSON, 0, len(kept))
				for _, e := range kept {
					rows = append(rows, ladderRecordJSON{
						ID:      e.ID,
						At:      e.At.UTC().Format(time.RFC3339Nano),
						Season:  e.Season,
						Kind:    string(e.Kind),
						Agent:   e.Agent,
						Summary: ladderRecordSummary(e),
					})
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, e := range kept {
				fmt.Fprintf(out, "%s %s season=%d kind=%s agent=%s %s\n",
					e.ID, e.At.UTC().Format(time.RFC3339Nano),
					e.Season, e.Kind, e.Agent, ladderRecordSummary(e))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "show only this agent")
	cmd.Flags().StringVar(&kind, "kind", "", "show only this event kind")
	cmd.Flags().IntVarP(&n, "n", "n", 50, "show at most this many events")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable rows")
	return cmd
}

// ladderRecordBase stamps the fields every recorded event carries: a unique
// time-ordered id, now in UTC, and the reviewer who recorded it.
func ladderRecordBase(season int) ladder.Event {
	now := time.Now().UTC()
	return ladder.Event{
		ID:       ladderRecordNewID(now),
		At:       now,
		Season:   season,
		Reviewer: ladderRecordReviewer(),
	}
}

// ladderRecordNewID mints a unique time-ordered id: RFC3339Nano in UTC (so
// lexicographic order is time order) plus 4 random hex digits.
func ladderRecordNewID(now time.Time) string {
	var suffix [2]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		suffix = [2]byte{byte(now.UnixNano()), byte(now.UnixNano() >> 8)}
	}
	return now.UTC().Format(time.RFC3339Nano) + "-" + hex.EncodeToString(suffix[:])
}

// ladderRecordReviewer is the actor recording the event: BASHY_AGENT when the
// launcher stamped one, else the local principal name from the existing
// principal helpers, else "operator".
func ladderRecordReviewer() string {
	if v := strings.TrimSpace(os.Getenv("BASHY_AGENT")); v != "" {
		return v
	}
	if u := principal.DefaultEnv().LocalUser; u != "" {
		return u
	}
	return "operator"
}

// ladderRecordCanonicalAgent rejects anything that is not tool:model: exactly
// one colon, no slash, no whitespace, and neither half empty.
func ladderRecordCanonicalAgent(agent string) error {
	if strings.Count(agent, ":") != 1 {
		return fmt.Errorf("record: --agent %q must be canonical tool:model (exactly one ':')", agent)
	}
	if strings.Contains(agent, "/") {
		return fmt.Errorf("record: --agent %q must be canonical tool:model (no '/')", agent)
	}
	if strings.IndexFunc(agent, unicode.IsSpace) >= 0 {
		return fmt.Errorf("record: --agent %q must be canonical tool:model (no whitespace)", agent)
	}
	tool, model, _ := strings.Cut(agent, ":")
	if tool == "" || model == "" {
		return fmt.Errorf("record: --agent %q must be canonical tool:model (neither half empty)", agent)
	}
	return nil
}

// ladderRecordCertKind maps a --kind flag to its certificate kind.
func ladderRecordCertKind(kind string) (ladder.CertKind, error) {
	switch ladder.CertKind(strings.ToLower(strings.TrimSpace(kind))) {
	case ladder.CertL1:
		return ladder.CertL1, nil
	case ladder.CertL2:
		return ladder.CertL2, nil
	case ladder.CertL3:
		return ladder.CertL3, nil
	case ladder.CertSteer:
		return ladder.CertSteer, nil
	case ladder.CertManager:
		return ladder.CertManager, nil
	case ladder.CertReview:
		return ladder.CertReview, nil
	case ladder.CertJudge:
		return ladder.CertJudge, nil
	case ladder.CertL5:
		return ladder.CertL5, nil
	default:
		return "", fmt.Errorf("record cert: --kind %q must be one of l1|l2|l3|steer|manager|review|judge|l5", kind)
	}
}

// ladderRecordSummary is the one-line human reading of an event.
func ladderRecordSummary(e ladder.Event) string {
	switch e.Kind {
	case ladder.EventKindSeat:
		if e.Provisional == 0 {
			return ladderRecordNoted("seat cleared", e.Note)
		}
		return ladderRecordNoted(fmt.Sprintf("provisional L%d", e.Provisional), e.Note)
	case ladder.EventKindCert:
		return fmt.Sprintf("%s %s evidence %s", e.Cert.Kind, e.Cert.ModelVersion, e.Note)
	case ladder.EventKindCorrection:
		return fmt.Sprintf("supersedes %s: %s", e.Supersedes, e.Note)
	default:
		return e.Note
	}
}

func ladderRecordNoted(base, note string) string {
	if strings.TrimSpace(note) == "" {
		return base
	}
	return base + ": " + note
}

// ladderRecordAppend writes one event; the write is the whole job, so a
// store error is the command's error (a non-zero exit).
func ladderRecordAppend(cmd *cobra.Command, e ladder.Event, confirm string) error {
	st, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		return fmt.Errorf("record: opening the ladder store: %w", err)
	}
	if err := st.Append(e); err != nil {
		return fmt.Errorf("record: appending the event: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), confirm)
	return nil
}

// ladderRecordRead loads the store for listing. A store that was never
// written is an empty listing, not an error — reading must not create it.
func ladderRecordRead() ([]ladder.Event, error) {
	path := ladder.DefaultStorePath()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("record: reading the ladder store: %w", err)
	}
	st, err := ladder.OpenStore(path)
	if err != nil {
		return nil, fmt.Errorf("record: reading the ladder store: %w", err)
	}
	events, err := st.Read()
	if err != nil {
		return nil, fmt.Errorf("record: reading the ladder store: %w", err)
	}
	return events, nil
}
