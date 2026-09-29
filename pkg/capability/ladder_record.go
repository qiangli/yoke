package capability

// `bashy leaderboard record ...` — append seat, certificate, correction and
// seed events to the ladder event store (design docs/bashy-band-ladder-design.md
// sections 4, 5, 8, 13).
//
// This verb's only job is the write: every subcommand appends events and
// prints one confirmation line per event carrying its id. A store error is a
// non-zero exit. Nothing here rates, ranks or renders.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
		Short: "append seat, certificate, correction and seed events to the ladder store",
		Long: `Append seat, certificate, correction and seed events to the ladder event store.

Each subcommand writes its events and prints one confirmation line per
event carrying the event id. Season comes from --season; the agent must be
canonical tool:model.`,
	}
	cmd.AddCommand(
		newLadderRecordSeatCmd(),
		newLadderRecordCertCmd(),
		newLadderRecordCorrectionCmd(),
		newLadderRecordSeedCmd(),
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
	case ladder.EventKindSeed:
		return ladderRecordNoted(fmt.Sprintf("%s seed r=%.0f rd=%.0f", e.Duty, e.SeedR, e.SeedRD), e.Note)
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

// A seed is a PRIOR: it replaces the default starting rating for one agent
// and duty and is never a rated event. Public-data seeds must lose to
// evidence quickly, so replay floors their RD at ladder.SeedRDFloor however
// confident the fit was (manager decision recorded on Sprint #331).
func newLadderRecordSeedCmd() *cobra.Command {
	var from, tool, agent, duty, reason string
	var toolMap []string
	var r, rd float64
	var season int
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "seed starting ratings from a seedfit TSV, or one agent by hand",
		Long: `Seed an agent's starting rating for a duty. A seed is a prior, not
evidence: replay starts the agent there instead of the default rating, and
floors the seed's RD at 150 so evidence overtakes it quickly.

--from-seedfit FILE reads the TSV that seedfit writes (columns model,
code_rating, rd). The TSV names models, not agents, so every current fleet
agent bound to a matching model (clones and cascades excluded) gets one code
seed keyed tool:model. --tool TOOL seeds only agents of that tool;
--tool-map MODEL=TOOL[,...] does the same per seedfit model (repeat a model
to allow several tools), with --tool, if given, covering the unlisted ones.

--agent tool:model --duty code|manage|judge --r R --rd RD --reason TEXT
records one seed by hand. --dry-run prints what would be written.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if season < 1 {
				return fmt.Errorf("record seed: --season must be >= 1")
			}
			switch {
			case from != "" && agent != "":
				return fmt.Errorf("record seed: --from-seedfit and --agent are exclusive")
			case from != "":
				tools, err := ladderSeedToolMap(toolMap)
				if err != nil {
					return err
				}
				return ladderSeedImport(cmd, from, tool, tools, season, dryRun)
			case agent == "":
				return fmt.Errorf("record seed: one of --from-seedfit or --agent is required")
			}
			if err := ladderRecordCanonicalAgent(agent); err != nil {
				return err
			}
			d := ladder.Duty(strings.ToLower(strings.TrimSpace(duty)))
			if d != ladder.DutyCode && d != ladder.DutyManage && d != ladder.DutyJudge {
				return fmt.Errorf("record seed: --duty %q must be one of code|manage|judge", duty)
			}
			if r <= 0 {
				return fmt.Errorf("record seed: --r is required and must be > 0")
			}
			if rd < 0 {
				return fmt.Errorf("record seed: --rd must be >= 0")
			}
			if strings.TrimSpace(reason) == "" {
				return fmt.Errorf("record seed: --reason is required (no seed without a reason)")
			}
			e := ladderSeedEvent(season, agent, d, r, rd, reason)
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "would seed %s %s r=%.0f rd=%.0f (dry run: nothing written)\n", e.Agent, e.Duty, e.SeedR, e.SeedRD)
				return nil
			}
			return ladderRecordAppend(cmd, e, fmt.Sprintf("recorded seed event %s for %s %s r=%.0f rd=%.0f", e.ID, e.Agent, e.Duty, e.SeedR, e.SeedRD))
		},
	}
	cmd.Flags().StringVar(&from, "from-seedfit", "", "seedfit TSV to import code seeds from")
	cmd.Flags().StringVar(&tool, "tool", "", "import: seed only agents of this tool")
	cmd.Flags().StringSliceVar(&toolMap, "tool-map", nil, "import: MODEL=TOOL pairs restricting a model's seeds to those tools")
	cmd.Flags().StringVar(&agent, "agent", "", "manual: canonical agent tool:model")
	cmd.Flags().StringVar(&duty, "duty", string(ladder.DutyCode), "manual: duty code|manage|judge")
	cmd.Flags().Float64Var(&r, "r", 0, "manual: seed rating")
	cmd.Flags().Float64Var(&rd, "rd", ladder.InitialRD, "manual: seed rating deviation (replay floors it at 150)")
	cmd.Flags().StringVar(&reason, "reason", "", "manual: why the seed was set")
	cmd.Flags().IntVar(&season, "season", 1, "season the seed is recorded in")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the seeds without writing them")
	return cmd
}

func ladderSeedEvent(season int, agent string, duty ladder.Duty, r, rd float64, note string) ladder.Event {
	e := ladderRecordBase(season)
	e.Kind = ladder.EventKindSeed
	e.Agent = agent
	e.Duty = duty
	e.SeedR = r
	e.SeedRD = rd
	e.Note = note
	return e
}

// ladderSeedToolMap parses MODEL=TOOL pairs into model → allowed tools.
func ladderSeedToolMap(pairs []string) (map[string]map[string]bool, error) {
	out := make(map[string]map[string]bool)
	for _, p := range pairs {
		model, tool, ok := strings.Cut(strings.TrimSpace(p), "=")
		model, tool = strings.TrimSpace(model), strings.TrimSpace(tool)
		if !ok || model == "" || tool == "" {
			return nil, fmt.Errorf("record seed: --tool-map %q must be MODEL=TOOL", p)
		}
		if out[model] == nil {
			out[model] = make(map[string]bool)
		}
		out[model][tool] = true
	}
	return out, nil
}

// ladderSeedRow is one model row of a seedfit TSV.
type ladderSeedRow struct {
	Model  string
	R, RD  float64
	Theta  string
	N      string
	Placed string
}

// ladderSeedReadTSV parses the seedfit TSV: '#' comment lines, then a header
// naming at least model, code_rating and rd (located by name).
func ladderSeedReadTSV(path string) ([]ladderSeedRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("record seed: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var col map[string]int
	var rows []ladderSeedRow
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Split(text, "\t")
		if col == nil {
			col = make(map[string]int)
			for i, h := range fields {
				col[strings.TrimSpace(h)] = i
			}
			for _, h := range []string{"model", "code_rating", "rd"} {
				if _, ok := col[h]; !ok {
					return nil, fmt.Errorf("record seed: %s: header lacks column %q (want seedfit TSV output)", filepath.Base(path), h)
				}
			}
			continue
		}
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(fields) {
				return strings.TrimSpace(fields[i])
			}
			return ""
		}
		row := ladderSeedRow{Model: get("model"), Theta: get("theta"), N: get("n"), Placed: get("placement")}
		if row.Model == "" {
			return nil, fmt.Errorf("record seed: %s line %d: empty model", filepath.Base(path), line)
		}
		if row.R, err = strconv.ParseFloat(get("code_rating"), 64); err != nil || row.R <= 0 {
			return nil, fmt.Errorf("record seed: %s line %d: bad code_rating %q", filepath.Base(path), line, get("code_rating"))
		}
		if row.RD, err = strconv.ParseFloat(get("rd"), 64); err != nil || row.RD < 0 {
			return nil, fmt.Errorf("record seed: %s line %d: bad rd %q", filepath.Base(path), line, get("rd"))
		}
		rows = append(rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("record seed: %w", err)
	}
	if col == nil {
		return nil, fmt.Errorf("record seed: %s: no header (want seedfit TSV output)", filepath.Base(path))
	}
	return rows, nil
}

// ladderSeedImport resolves seedfit models to fleet agents through the fleet
// catalog (read-only) and appends one code seed per agent binding.
func ladderSeedImport(cmd *cobra.Command, path, tool string, toolMap map[string]map[string]bool, season int, dryRun bool) error {
	rows, err := ladderSeedReadTSV(path)
	if err != nil {
		return err
	}
	cat := newCatalog()
	agents, _ := cat.Agents()
	// One seed per binding: nicknames of the same tool:model collapse, and
	// clones (branched copies) and cascades (not a plain binding) are skipped.
	byModel := make(map[string][]string)
	seenKey := make(map[string]bool)
	var eligible []string
	for _, a := range agents {
		if a.ClonedFrom != "" || a.Ephemeral || a.IsCascade() || a.Tool == "" || a.Model == "" {
			continue
		}
		key := a.MatrixKey()
		if seenKey[key] {
			continue
		}
		seenKey[key] = true
		eligible = append(eligible, key)
		byModel[a.Model] = append(byModel[a.Model], key)
	}
	sort.Strings(eligible)

	var events []ladder.Event
	var orphans []string
	covered := make(map[string]bool)
	source := filepath.Base(path)
	for _, row := range rows {
		canonical := row.Model
		if m, ok := cat.Model(row.Model); ok {
			canonical = m.Name
		}
		allowed := toolMap[row.Model]
		if allowed == nil && tool != "" {
			allowed = map[string]bool{tool: true}
		}
		keys := append([]string(nil), byModel[canonical]...)
		sort.Strings(keys)
		n := 0
		for _, key := range keys {
			covered[key] = true
			t, _, _ := strings.Cut(key, ":")
			if allowed != nil && !allowed[t] {
				continue
			}
			note := fmt.Sprintf("seedfit %s: model %s theta=%s n=%s placement=%s", source, row.Model, row.Theta, row.N, row.Placed)
			events = append(events, ladderSeedEvent(season, key, ladder.DutyCode, row.R, row.RD, note))
			n++
		}
		// A row every agent was filtered away from is reported too: it
		// seeded nothing.
		if n == 0 {
			orphans = append(orphans, row.Model)
		}
	}
	var unrowed []string
	for _, key := range eligible {
		if !covered[key] {
			unrowed = append(unrowed, key)
		}
	}

	out := cmd.OutOrStdout()
	var st *ladder.Store
	if !dryRun && len(events) > 0 {
		if st, err = ladder.OpenStore(ladder.DefaultStorePath()); err != nil {
			return fmt.Errorf("record: opening the ladder store: %w", err)
		}
	}
	for _, e := range events {
		if dryRun {
			fmt.Fprintf(out, "would seed %s %s r=%.0f rd=%.0f\n", e.Agent, e.Duty, e.SeedR, e.SeedRD)
			continue
		}
		if err := st.Append(e); err != nil {
			return fmt.Errorf("record: appending the event: %w", err)
		}
		fmt.Fprintf(out, "recorded seed event %s for %s %s r=%.0f rd=%.0f\n", e.ID, e.Agent, e.Duty, e.SeedR, e.SeedRD)
	}
	fmt.Fprintf(out, "agents seeded: %d\n", len(events))
	fmt.Fprintf(out, "seedfit models with no fleet agent: %s\n", ladderSeedList(orphans))
	fmt.Fprintf(out, "fleet agents with no seedfit row: %s\n", ladderSeedList(unrowed))
	if dryRun {
		fmt.Fprintln(out, "dry run: nothing written")
	}
	return nil
}

func ladderSeedList(xs []string) string {
	if len(xs) == 0 {
		return "none"
	}
	return strings.Join(xs, ", ")
}
