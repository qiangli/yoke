package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/lens/agentworld"
	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/quotaobs"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/spf13/cobra"
)

const quotaImmuneInterval = 2 * time.Minute

type quotaImmuneRecord struct {
	Account           string    `json:"account,omitempty"`
	Level             string    `json:"level,omitempty"`
	Detail            string    `json:"detail,omitempty"`
	ResetAt           time.Time `json:"reset_at,omitempty"`
	WarnedAt          time.Time `json:"warned_at,omitempty"`
	CriticalAt        time.Time `json:"critical_at,omitempty"`
	KillAfter         time.Time `json:"kill_after,omitempty"`
	KillIssuedAt      time.Time `json:"kill_issued_at,omitempty"`
	KilledAt          time.Time `json:"killed_at,omitempty"`
	NeedsSalvage      bool      `json:"needs_salvage,omitempty"`
	NeedsReassignment bool      `json:"needs_reassignment,omitempty"`
	Reason            string    `json:"reason,omitempty"`
}

type quotaImmuneSeat struct {
	Agent   string
	Tool    string
	Model   string
	Account string
}

type quotaImmuneObservation struct {
	Level   string
	Detail  string
	ResetAt time.Time
}

type quotaImmuneAssessment struct {
	Level   string    `json:"level"`
	Account string    `json:"account"`
	Detail  string    `json:"detail"`
	ResetAt time.Time `json:"reset_at,omitempty"`
}

type quotaImmuneAction struct {
	Kind      string    `json:"kind"`
	Run       int64     `json:"run,omitempty"`
	Sprint    int64     `json:"sprint,omitempty"`
	Account   string    `json:"account"`
	Level     string    `json:"level"`
	Detail    string    `json:"detail"`
	ResetAt   time.Time `json:"reset_at,omitempty"`
	KillAfter time.Time `json:"kill_after,omitempty"`
	DryRun    bool      `json:"dry_run,omitempty"`
}

// quotaImmuneTickInputs shares one delta-backed observation view across seats.
// Forecasts are fresh for each tick, including dry runs.
type quotaImmuneTickInputs struct {
	joined    []quotaImmuneObservedPair
	forecasts []llmbudget.AccountForecast
	loaded    bool
	err       error
	busErr    error
}

type quotaImmuneDeltaState struct {
	Cursor          quotaobs.Cursor           `json:"cursor"`
	Rows            []quotaobs.Observation    `json:"rows"`
	Pending         []quotaobs.Observation    `json:"pending,omitempty"`
	States          []llmbudget.ObservedState `json:"states,omitempty"`
	Classified      bool                      `json:"classified,omitempty"`
	AnnotationStamp string                    `json:"annotation_stamp,omitempty"`
	BusCursor       string                    `json:"bus_cursor,omitempty"`
}

func quotaImmuneAnnotationStamp() string {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".bashy")
		}
	}
	if home == "" {
		return ""
	}
	info, err := os.Stat(filepath.Join(home, "quota", "analysis.jsonl"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

func quotaImmuneSameRow(a, b quotaobs.Observation) bool {
	return a.At.Equal(b.At) && a.ExcerptSHA == b.ExcerptSHA && a.Source == b.Source &&
		a.Account == b.Account && a.Plan == b.Plan && a.Agent == b.Agent && a.Model == b.Model && a.Tool == b.Tool
}

func quotaImmuneAppendRows(state *quotaImmuneDeltaState, rows []quotaobs.Observation, now time.Time) {
	var fresh []quotaobs.Observation
	for _, row := range rows {
		if row.Schema != quotaobs.Schema || row.At.Before(now.Add(-24*time.Hour)) {
			continue
		}
		duplicate := false
		for _, old := range state.Rows {
			if quotaImmuneSameRow(old, row) {
				duplicate = true
				break
			}
		}
		for _, old := range state.Pending {
			if quotaImmuneSameRow(old, row) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			if row.At.After(now) {
				state.Pending = append(state.Pending, row)
				continue
			}
			state.Rows = append(state.Rows, row)
			fresh = append(fresh, row)
		}
	}
	if len(fresh) != 0 {
		state.States = append(state.States, quotaImmuneClassify(fresh, now)...)
	}
}

func quotaImmuneBusRows(events []room.Event) []quotaobs.Observation {
	var rows []quotaobs.Observation
	for _, event := range events {
		if event.Type != room.EventUsage || event.Topic != room.TopicQuotaObservation || event.Actor != "quotaobs" {
			continue
		}
		var row quotaobs.Observation
		if json.Unmarshal([]byte(event.Body), &row) == nil && row.Schema == quotaobs.Schema {
			rows = append(rows, row)
		}
	}
	return rows
}

func loadQuotaImmuneDelta(ctx context.Context, dir string, now time.Time) (*quotaImmuneTickInputs, quotaImmuneDeltaState, error) {
	name := filepath.Join(dir, "quota-immune-cursor.json")
	var state quotaImmuneDeltaState
	if b, err := os.ReadFile(name); err == nil {
		if err := json.Unmarshal(b, &state); err != nil {
			return nil, state, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, state, err
	}
	rows, next, err := quotaImmuneDelta(state.Cursor)
	if errors.Is(err, quotaobs.ErrCursorExpired) {
		// Replay from the surviving journal after rotation. Run and sprint
		// records still own action dedupe, including a critical grace timer.
		state.Cursor = quotaobs.Cursor{}
		rows, next, err = quotaImmuneDelta(state.Cursor)
	}
	if err != nil {
		return nil, state, err
	}
	state.Cursor = next
	annotationStamp := quotaImmuneAnnotationStamp()
	if (!state.Classified || state.AnnotationStamp != annotationStamp) && len(state.Rows) != 0 {
		state.States = quotaImmuneClassify(state.Rows, now)
	}
	state.Classified = true
	state.AnnotationStamp = annotationStamp
	pending := state.Pending
	state.Pending = nil
	quotaImmuneAppendRows(&state, append(pending, rows...), now)
	kept := state.Rows[:0]
	for _, row := range state.Rows {
		if !row.At.Before(now.Add(-24 * time.Hour)) {
			kept = append(kept, row)
		}
	}
	state.Rows = kept
	keptStates := state.States[:0]
	for _, classified := range state.States {
		if !classified.At.Before(now.Add(-24 * time.Hour)) {
			keptStates = append(keptStates, classified)
		}
	}
	state.States = keptStates
	// Resume the same bound bus://timeline --since source used by lens.
	events, busCursor, busErr := agentworld.ReadTimeline(ctx, state.BusCursor)
	if agentworld.IsExpired(busErr) {
		events, busCursor, busErr = agentworld.ReadTimeline(ctx, "")
	}
	if busErr == nil {
		quotaImmuneAppendRows(&state, quotaImmuneBusRows(events), now)
		state.BusCursor = busCursor
	}
	inputs := &quotaImmuneTickInputs{loaded: true}
	inputs.busErr = busErr
	inputs.joined = joinQuotaImmuneObservations(state.Rows, state.States)
	return inputs, state, nil
}

func saveQuotaImmuneDelta(dir string, state quotaImmuneDeltaState) error {
	name := filepath.Join(dir, "quota-immune-cursor.json")
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".quota-immune-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}

type quotaImmuneObservedPair struct {
	observation quotaobs.Observation
	state       llmbudget.ObservedState
}

type quotaImmuneInputsKey struct{}

func joinQuotaImmuneObservations(observations []quotaobs.Observation, states []llmbudget.ObservedState) []quotaImmuneObservedPair {
	bySHA := make(map[string][]quotaobs.Observation, len(observations))
	for _, o := range observations {
		bySHA[o.ExcerptSHA] = append(bySHA[o.ExcerptSHA], o)
	}
	var joined []quotaImmuneObservedPair
	for _, state := range states {
		for _, o := range bySHA[state.ExcerptSHA] {
			if o.At.Equal(state.At) && o.Account == state.Account && o.Plan == state.Plan && o.Tool == state.Tool {
				joined = append(joined, quotaImmuneObservedPair{o, state})
			}
		}
	}
	return joined
}

func quotaImmuneInputs(ctx context.Context, now time.Time) (*quotaImmuneTickInputs, bool) {
	inputs, ok := ctx.Value(quotaImmuneInputsKey{}).(*quotaImmuneTickInputs)
	if !ok {
		return nil, false
	}
	if !inputs.loaded {
		inputs.loaded = true
		observations, err := quotaImmuneRead(now.Add(-24*time.Hour), nil)
		inputs.err = err
		if inputs.err == nil {
			inputs.joined = joinQuotaImmuneObservations(observations, quotaImmuneObserved(ctx, now))
		}
	}
	return inputs, true
}

var (
	quotaImmuneNow         = func() time.Time { return time.Now().UTC() }
	quotaImmunePolicy      = llmbudget.ImmuneSettingsFromPolicy
	quotaImmuneHeadroom    = llmbudget.CachedMeasuredHeadroom
	quotaImmuneObserve     = readQuotaImmuneObservation
	quotaImmuneResolveSeat = resolveQuotaImmuneSeat
	quotaImmuneForecast    = llmbudget.Forecast
	quotaImmuneRead        = quotaobs.Read
	quotaImmuneObserved    = llmbudget.Observed
	quotaImmuneDelta       = quotaobs.ReadDelta
	quotaImmuneClassify    = llmbudget.ObservedRows
	quotaImmuneSay         = func(cmd *cobra.Command, id int64, message string) error {
		return runWeaveSay(cmd, id, message, false, false, "", &weaveOutputFlags{})
	}
	quotaImmuneKill = func(cmd *cobra.Command, id int64) error {
		return runWeaveKill(cmd, id, "quota-critical", true, &weaveOutputFlags{})
	}
)

func newWeaveImmuneCmd() *cobra.Command {
	var flags weaveOutputFlags
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "immune",
		Short: "Protect live runs from quota exhaustion",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cwd, _ := filepath.Abs(".")
			root, err := weaveRepoRoot(cwd)
			if err != nil {
				return err
			}
			dir, err := weaveQueueDir(root)
			if err != nil {
				return err
			}
			actions, err := runQuotaImmuneTick(cmd, root, dir, dryRun)
			if err != nil {
				return err
			}
			if flags.mode() == weavecli.OutputJSON {
				return ec(emitOK(cmd.OutOrStdout(), flags.mode(), "weave immune", map[string]any{"actions": actions, "dry_run": dryRun}))
			}
			if len(actions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "weave immune: no quota actions")
				return nil
			}
			for _, action := range actions {
				target := fmt.Sprintf("run #%d", action.Run)
				if action.Sprint != 0 && action.Run == 0 {
					target = fmt.Sprintf("sprint #%d conductor", action.Sprint)
				}
				prefix := "fired"
				if dryRun {
					prefix = "would fire"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s %s for %s (%s)\n", prefix, action.Level, action.Kind, target, action.Detail)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print actions without steering, messaging, killing, or recording")
	flags.attach(cmd)
	return cmd
}

func resolveQuotaImmuneSeat(agent, tool, model string) (quotaImmuneSeat, bool) {
	cat := fleetCatalog()
	var a fleet.Agent
	var m fleet.Model
	var err error
	if strings.TrimSpace(agent) != "" {
		a, _, m, err = cat.Binding(agent)
	}
	if err != nil || m.Name == "" {
		if candidate, ok := cat.Model(model); ok {
			m = candidate
		} else {
			return quotaImmuneSeat{}, false
		}
	}
	account := strings.TrimSpace(m.Plan)
	if account == "" {
		account = strings.TrimSpace(m.APIKeyRef)
	}
	if account == "" {
		return quotaImmuneSeat{}, false
	}
	if a.Name != "" {
		agent = a.Name
	}
	return quotaImmuneSeat{Agent: agent, Tool: tool, Model: m.Name, Account: account}, true
}

func assessQuotaImmune(ctx context.Context, seat quotaImmuneSeat, deadline, now time.Time, policy llmbudget.ImmuneSettings) (quotaImmuneAssessment, error) {
	out := quotaImmuneAssessment{Account: seat.Account}
	set := func(level, detail string, reset time.Time) {
		if level == "critical" || out.Level == "" || out.Level == "warn" && level == "warn" {
			if out.Level != "critical" {
				out.Level, out.Detail, out.ResetAt = level, detail, reset
			}
		}
	}
	remaining, _, known, err := quotaImmuneHeadroom(ctx, seat.Model, seat.Agent)
	if err != nil {
		return out, err
	}
	if known {
		switch {
		case remaining <= policy.CriticalHeadroom:
			set("critical", fmt.Sprintf("headroom %.1f%%", remaining*100), time.Time{})
		case remaining <= policy.WarnHeadroom:
			set("warn", fmt.Sprintf("headroom %.1f%%", remaining*100), time.Time{})
		}
	}
	obs, ok, err := quotaImmuneObserve(ctx, seat, now)
	if err != nil {
		return out, err
	}
	if ok {
		set(obs.Level, obs.Detail, obs.ResetAt)
	}
	var forecasts []llmbudget.AccountForecast
	if inputs, ok := ctx.Value(quotaImmuneInputsKey{}).(*quotaImmuneTickInputs); ok {
		forecasts = inputs.forecasts
	} else {
		forecasts = quotaImmuneForecast(ctx, now)
	}
	for _, forecast := range forecasts {
		if !strings.EqualFold(forecast.Account, seat.Account) {
			continue
		}
		if forecast.P90 != nil && forecast.P90.Before(now.Add(policy.ForecastMargin)) {
			set("critical", "forecast P90 exhausts "+forecast.P90.Local().Format(time.RFC3339), time.Time{})
		} else if !deadline.IsZero() && forecast.P50 != nil && forecast.P50.Before(deadline.Add(policy.ForecastMargin)) {
			set("warn", "forecast P50 exhausts "+forecast.P50.Local().Format(time.RFC3339), time.Time{})
		}
	}
	return out, nil
}

func readQuotaImmuneObservation(ctx context.Context, seat quotaImmuneSeat, now time.Time) (quotaImmuneObservation, bool, error) {
	var joined []quotaImmuneObservedPair
	if inputs, ok := quotaImmuneInputs(ctx, now); ok {
		if inputs.err != nil {
			return quotaImmuneObservation{}, false, inputs.err
		}
		joined = inputs.joined
	} else {
		observations, err := quotaImmuneRead(now.Add(-24*time.Hour), nil)
		if err != nil {
			return quotaImmuneObservation{}, false, err
		}
		joined = joinQuotaImmuneObservations(observations, quotaImmuneObserved(ctx, now))
	}
	var best quotaImmuneObservation
	policy, err := quotaImmunePolicy()
	if err != nil {
		return best, false, err
	}
	for _, pair := range joined {
		o, state := pair.observation, pair.state
		accountMatch := strings.EqualFold(o.Account, seat.Account) || strings.EqualFold(o.Plan, seat.Account)
		bindingMatch := o.Agent != "" && strings.EqualFold(o.Agent, seat.Agent) || o.Model != "" && strings.EqualFold(o.Model, seat.Model) || o.Tool != "" && strings.EqualFold(o.Tool, seat.Tool)
		if !accountMatch && (o.Account != "" || o.Plan != "" || !bindingMatch) {
			continue
		}
		structured := state.Matcher != nil && weaveVendorUsageEvent(o.Tool, strings.TrimSpace(o.Excerpt))
		if state.Classification != "observed" && !(state.Classification == "observed-unconfirmed" && structured) {
			continue
		}
		if state.Kind == "" {
			continue
		}
		level := ""
		if state.Kind == "exhausted" && state.ResetAt != nil && state.ResetAt.After(now) {
			level = "critical"
		} else if state.Kind == "warning" || state.UsedPercent != nil && *state.UsedPercent >= 100-policy.WarnHeadroom*100 {
			level = "warn"
		}
		if level == "" || best.Level == "critical" {
			continue
		}
		reset := time.Time{}
		if state.ResetAt != nil {
			reset = *state.ResetAt
		}
		detail := strings.Join(strings.Fields(o.Excerpt), " ")
		if len(detail) > 120 {
			detail = detail[:117] + "..."
		}
		best = quotaImmuneObservation{Level: level, Detail: detail, ResetAt: reset}
	}
	return best, best.Level != "", nil
}

func quotaImmuneMessage(a quotaImmuneAssessment) string {
	reset := "unknown"
	if !a.ResetAt.IsZero() {
		reset = a.ResetAt.Local().Format(time.RFC3339)
	}
	if a.Level == "critical" {
		return fmt.Sprintf("bashy quota: your %s quota is critical (%s, resets %s). Stop now: commit your work, write handoff notes (what is done, what remains, next command) to your run report, then submit and stop. Do not start new work.", a.Account, a.Detail, reset)
	}
	return fmt.Sprintf("bashy quota: your %s quota is low (%s, resets %s). Wind down now: finish the current step, commit your work, write handoff notes (what is done, what remains, next command) to your run report, then submit and stop. Do not start new work.", a.Account, a.Detail, reset)
}

func runQuotaImmuneTick(cmd *cobra.Command, _ string, dir string, dryRun bool) ([]quotaImmuneAction, error) {
	now := quotaImmuneNow()
	policy, err := quotaImmunePolicy()
	if err != nil {
		return nil, err
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return nil, err
	}
	inputs := &quotaImmuneTickInputs{}
	var deltaState quotaImmuneDeltaState
	if quotaImmuneDelta != nil {
		inputs, deltaState, err = loadQuotaImmuneDelta(cmd.Context(), dir, now)
		if err != nil {
			return nil, err
		}
		if inputs.busErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "weave immune: bus delta: %v\n", inputs.busErr)
		}
	}
	inputs.forecasts = quotaImmuneForecast(cmd.Context(), now)
	ctx := context.WithValue(cmd.Context(), quotaImmuneInputsKey{}, inputs)
	var actions []quotaImmuneAction
	assessmentFailed := false
	for _, it := range q.Items {
		if it.State != "working" || it.LaunchSpec == nil {
			continue
		}
		seat, ok := quotaImmuneResolveSeat(it.LaunchSpec.Agent, it.Tool, it.LaunchSpec.Model)
		if !ok {
			continue
		}
		deadline := time.Time{}
		if it.LaunchSpec.MaxRuntime > 0 {
			deadline = it.StartedAt.Add(it.LaunchSpec.MaxRuntime)
		}
		assessment, err := assessQuotaImmune(ctx, seat, deadline, now, policy)
		if err != nil {
			assessmentFailed = true
			fmt.Fprintf(cmd.ErrOrStderr(), "weave immune: run #%d: %v\n", it.ID, err)
			continue
		}
		record := quotaImmuneRecord{}
		if it.QuotaImmune != nil {
			record = *it.QuotaImmune
		}
		if assessment.Level != "" && (assessment.Level == "critical" && record.CriticalAt.IsZero() || assessment.Level == "warn" && record.WarnedAt.IsZero() && record.CriticalAt.IsZero()) {
			action := quotaImmuneAction{Kind: "steer", Run: it.ID, Account: seat.Account, Level: assessment.Level, Detail: assessment.Detail, ResetAt: assessment.ResetAt, DryRun: dryRun}
			if assessment.Level == "critical" {
				action.KillAfter = now.Add(policy.GracePeriod)
			}
			if !dryRun {
				claimed, err := recordRunQuotaImmune(dir, it.ID, assessment, now, policy)
				if err != nil {
					return actions, err
				}
				if !claimed {
					continue
				}
				actions = append(actions, action)
				message := quotaImmuneMessage(assessment)
				if err := quotaImmuneSay(cmd, it.ID, message); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "weave immune: run #%d steer: %v\n", it.ID, err)
				}
				alarm := fmt.Sprintf("quota %s for %s run #%d: %s", assessment.Level, seat.Account, it.ID, assessment.Detail)
				if assessment.Level == "critical" {
					alarm += fmt.Sprintf("; workspace and branch will be preserved; inspect then run `bashy weave salvage %d`", it.ID)
				}
				if err := weaveSupervisionAlarm(cmd, dir, it, "quota-"+assessment.Level, alarm); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "weave immune: alarm run #%d: %v\n", it.ID, err)
				}
			} else {
				actions = append(actions, action)
			}
		}

		killAfter := record.KillAfter
		if assessment.Level == "critical" && record.CriticalAt.IsZero() {
			killAfter = now.Add(policy.GracePeriod)
		}
		if !killAfter.IsZero() && !now.Before(killAfter) && record.KilledAt.IsZero() {
			action := quotaImmuneAction{Kind: "kill-preserve", Run: it.ID, Account: seat.Account, Level: "critical", Detail: "quota-critical grace expired", DryRun: dryRun}
			if !dryRun {
				claimed, err := claimRunQuotaImmuneKill(dir, it.ID, now)
				if err != nil {
					return actions, err
				}
				if !claimed {
					continue
				}
				actions = append(actions, action)
				if err := quotaImmuneKill(cmd, it.ID); err != nil {
					clearRunQuotaImmuneKillClaim(dir, it.ID)
					fmt.Fprintf(cmd.ErrOrStderr(), "weave immune: run #%d kill: %v\n", it.ID, err)
					continue
				}
				if err := markRunQuotaImmuneKilled(dir, it.ID, now); err != nil {
					return actions, err
				}
			} else {
				actions = append(actions, action)
			}
		}
	}
	managerActions, err := checkQuotaImmuneManagers(cmd, ctx, policy, now, dryRun)
	if err != nil {
		return actions, err
	}
	actions = append(actions, managerActions...)
	silent, err := checkSilentSprintManagers(now, dryRun)
	if err != nil {
		return actions, err
	}
	if !dryRun && !assessmentFailed && quotaImmuneDelta != nil {
		if err := saveQuotaImmuneDelta(dir, deltaState); err != nil {
			return actions, err
		}
	}
	return append(actions, silent...), nil
}

func recordRunQuotaImmune(dir string, id int64, a quotaImmuneAssessment, now time.Time, policy llmbudget.ImmuneSettings) (bool, error) {
	claimed := false
	err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil || it.State != "working" {
			return nil
		}
		if it.QuotaImmune == nil {
			it.QuotaImmune = new(quotaImmuneRecord)
		}
		if a.Level == "critical" && !it.QuotaImmune.CriticalAt.IsZero() || a.Level == "warn" && (!it.QuotaImmune.WarnedAt.IsZero() || !it.QuotaImmune.CriticalAt.IsZero()) {
			return nil
		}
		claimed = true
		it.QuotaImmune.Account, it.QuotaImmune.Level, it.QuotaImmune.Detail, it.QuotaImmune.ResetAt = a.Account, a.Level, a.Detail, a.ResetAt
		if a.Level == "critical" {
			if it.QuotaImmune.CriticalAt.IsZero() {
				it.QuotaImmune.CriticalAt = now
				it.QuotaImmune.KillAfter = now.Add(policy.GracePeriod)
			}
		} else if it.QuotaImmune.WarnedAt.IsZero() {
			it.QuotaImmune.WarnedAt = now
		}
		weaveAppendComment(it, "quota-watchdog", "quota-"+a.Level, quotaImmuneMessage(a))
		return nil
	})
	return claimed, err
}

func markRunQuotaImmuneKilled(dir string, id int64, now time.Time) error {
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		if it := findWeaveItem(q, id); it != nil {
			if it.QuotaImmune == nil {
				it.QuotaImmune = new(quotaImmuneRecord)
			}
			it.QuotaImmune.KilledAt = now
			it.QuotaImmune.NeedsSalvage = true
			it.QuotaImmune.NeedsReassignment = true
			it.QuotaImmune.Reason = "quota-critical"
			it.NeedsSteward = true
			it.StewardReason = "quota-critical: salvage preserved work and reassign"
			weaveAppendComment(it, "quota-watchdog", "quota-critical", "stopped after quota grace; workspace and branch preserved; salvage and reassignment required")
		}
		return nil
	})
}

func claimRunQuotaImmuneKill(dir string, id int64, now time.Time) (bool, error) {
	claimed := false
	err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil || it.State != "working" || it.QuotaImmune == nil || it.QuotaImmune.KillAfter.IsZero() || now.Before(it.QuotaImmune.KillAfter) || !it.QuotaImmune.KillIssuedAt.IsZero() || !it.QuotaImmune.KilledAt.IsZero() {
			return nil
		}
		it.QuotaImmune.KillIssuedAt = now
		claimed = true
		return nil
	})
	return claimed, err
}

func clearRunQuotaImmuneKillClaim(dir string, id int64) {
	_ = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		if it := findWeaveItem(q, id); it != nil && it.QuotaImmune != nil && it.QuotaImmune.KilledAt.IsZero() {
			it.QuotaImmune.KillIssuedAt = time.Time{}
		}
		return nil
	})
}

func checkQuotaImmuneManagers(cmd *cobra.Command, ctx context.Context, policy llmbudget.ImmuneSettings, now time.Time, dryRun bool) ([]quotaImmuneAction, error) {
	store, err := sprintStoreDir()
	if err != nil {
		return nil, err
	}
	q, err := loadWeaveQueue(store)
	if err != nil {
		return nil, err
	}
	var actions []quotaImmuneAction
	for _, snapshot := range q.Stories {
		if snapshot.Column != "doing" || snapshot.Lease == nil || snapshot.currentBox() == nil {
			continue
		}
		seat, ok := quotaImmuneResolveSeat(snapshot.Lease.Holder, "", "")
		if !ok {
			continue
		}
		a, err := assessQuotaImmune(ctx, seat, snapshot.currentBox().Cutoff, now, policy)
		if err != nil || a.Level == "" {
			continue
		}
		record := quotaImmuneRecord{}
		if snapshot.QuotaImmune != nil {
			record = *snapshot.QuotaImmune
		}
		if a.Level == "warn" && (!record.WarnedAt.IsZero() || !record.CriticalAt.IsZero()) || a.Level == "critical" && !record.CriticalAt.IsZero() {
			continue
		}
		action := quotaImmuneAction{Kind: "conductor-alarm", Sprint: snapshot.ID, Account: seat.Account, Level: a.Level, Detail: a.Detail, ResetAt: a.ResetAt, DryRun: dryRun}
		if dryRun {
			actions = append(actions, action)
			continue
		}
		message := fmt.Sprintf("quota %s for sprint #%d conductor %s (%s); wind down and hand off before exhaustion", a.Level, snapshot.ID, seat.Account, a.Detail)
		claimed := false
		err = withWeaveQueueLock(store, func(board *weaveQueue) error {
			s := findWeaveStory(board, snapshot.ID)
			if s == nil {
				return nil
			}
			if s.QuotaImmune == nil {
				s.QuotaImmune = new(quotaImmuneRecord)
			}
			if a.Level == "critical" && !s.QuotaImmune.CriticalAt.IsZero() || a.Level == "warn" && (!s.QuotaImmune.WarnedAt.IsZero() || !s.QuotaImmune.CriticalAt.IsZero()) {
				return nil
			}
			claimed = true
			s.QuotaImmune.Account, s.QuotaImmune.Level, s.QuotaImmune.Detail, s.QuotaImmune.ResetAt = a.Account, a.Level, a.Detail, a.ResetAt
			if a.Level == "critical" {
				s.QuotaImmune.CriticalAt = now
			} else {
				s.QuotaImmune.WarnedAt = now
			}
			weaveStoryAppend(s, "quota-watchdog", "quota-"+a.Level, message)
			return nil
		})
		if err != nil {
			return actions, err
		}
		if !claimed {
			continue
		}
		actions = append(actions, action)
		if err := weaveSupervisionSend(snapshot.Lease.Holder, takeoverRedact(message)); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "weave immune: conductor alarm sprint #%d: %v\n", snapshot.ID, err)
		}
	}
	return actions, nil
}
