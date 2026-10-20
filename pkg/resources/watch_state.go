package resources

import (
	"sort"
	"time"
)

// Event is one alarm transition (or re-notification) the watcher emitted.
type Event struct {
	SchemaVersion string    `json:"schema_version"`
	Type          string    `json:"type"` // alarm | clear | renotify
	Host          string    `json:"host"`
	Resource      string    `json:"resource"`
	Metric        string    `json:"metric"`
	From          Level     `json:"from"`
	To            Level     `json:"to"`
	Level         Level     `json:"level"`
	Value         float64   `json:"value"`
	Unit          string    `json:"unit"`
	Threshold     float64   `json:"threshold"`
	Detail        string    `json:"detail,omitempty"`
	At            time.Time `json:"at"`
	Renotify      bool      `json:"renotify"`
}

// WatchSchemaVersion tags the watcher's NDJSON events and state file.
const WatchSchemaVersion = "bashy-resource-watch-v1"

type watchPoint struct {
	At    time.Time `json:"at"`
	Value float64   `json:"value"`
}

// metricState is the watcher's memory for one resource+metric: the committed
// level, the hysteresis candidate, the sustained window, the re-notify clock
// and (for interfaces) that the link was seen up.
type metricState struct {
	Level       Level        `json:"level"`
	Pending     Level        `json:"pending"`
	PendingN    int          `json:"pending_n"`
	LastNotify  time.Time    `json:"last_notify"`
	Points      []watchPoint `json:"points,omitempty"`
	LinkWasUp   bool         `json:"link_was_up"`
	LinkAlerted bool         `json:"link_alerted"`
}

// Watcher evaluates successive System samples against WatchConfigs and emits
// Events on level transitions only, with hysteresis, sustained windows and a
// bounded re-notify interval.
type Watcher struct {
	cfg    WatchConfig
	byName map[string]WatchConfig

	states  map[string]*metricState
	prevNet map[string]netSample
	events  []Event
	host    string
}

const watchEventLogCap = 512

func NewWatcher(cfg WatchConfig) *Watcher { return NewWatcherWithConfigs(cfg, nil) }

// NewWatcherWithConfigs builds a watcher whose default config is cfg and
// whose per-resource configs (already fully resolved: defaults, then the
// host record's overrides, then the resource's own) override it by name.
func NewWatcherWithConfigs(cfg WatchConfig, byName map[string]WatchConfig) *Watcher {
	if cfg.Hysteresis < 1 {
		cfg.Hysteresis = 1
	}
	return &Watcher{
		cfg: cfg, byName: byName,
		states:  map[string]*metricState{},
		prevNet: map[string]netSample{},
	}
}

func (w *Watcher) cfgFor(resource string) WatchConfig {
	if c, ok := w.byName[resource]; ok {
		if c.Hysteresis < 1 {
			c.Hysteresis = 1
		}
		return c
	}
	return w.cfg
}

// Events returns the (bounded) event log, oldest first.
func (w *Watcher) Events() []Event {
	out := make([]Event, len(w.events))
	copy(out, w.events)
	return out
}

// Host reports the host the watcher last sampled.
func (w *Watcher) Host() string { return w.host }

// Observe ingests one System sample and returns the events it produced.
func (w *Watcher) Observe(at time.Time, sys *System) []Event {
	w.host = sys.Host
	readings := extractReadings(sys, w.cfgFor)
	netEvs, cur := netReadings(sys, w.prevNet, at, w.cfgFor)
	readings = append(readings, netEvs...)
	w.prevNet = cur

	sort.Slice(readings, func(i, j int) bool {
		if readings[i].resource != readings[j].resource {
			return readings[i].resource < readings[j].resource
		}
		return readings[i].metric < readings[j].metric
	})

	var emitted []Event
	for _, rd := range readings {
		key := rd.resource + "/" + rd.metric
		st := w.states[key]
		// first: the metric's opening sample (no memory of it in this run
		// or the persisted state). A value already beyond a threshold then
		// is a breach that predates the watcher, not a flap: it commits at
		// once instead of waiting out the count hysteresis.
		first := st == nil
		if first {
			st = &metricState{Level: LevelNormal, Pending: LevelNormal}
			w.states[key] = st
		}
		raw, threshold := w.rawLevel(rd, st, at)
		ev := w.advance(st, rd, raw, threshold, at, first)
		if ev != nil {
			emitted = append(emitted, *ev)
			w.appendEvent(*ev)
		}
	}
	return emitted
}

// rawLevel classifies one reading. Immediate metrics classify directly;
// sustained metrics only trip once the window has been covered by points
// all at or above the threshold. Link state is handled separately: down on
// an interface that was seen up is critical at once.
func (w *Watcher) rawLevel(rd reading, st *metricState, at time.Time) (Level, float64) {
	if rd.metric == metricNetLink {
		if !rd.ifaceUp && st.LinkWasUp {
			return LevelCritical, 1
		}
		return LevelNormal, 0
	}
	if !rd.hasValue {
		return LevelNormal, 0
	}
	if rd.sustain <= 0 {
		var lvl Level
		if rd.invert {
			switch {
			case rd.crit > 0 && rd.value <= rd.crit:
				lvl = LevelCritical
			case rd.value <= rd.warn:
				lvl = LevelWarning
			}
		} else {
			lvl = ClassifyLevel(rd.value, rd.warn, rd.crit)
		}
		return lvl, thresholdFor(lvl, rd.warn, rd.crit)
	}
	st.Points = append(st.Points, watchPoint{At: at, Value: rd.value})
	cutoff := at.Add(-rd.sustain)
	kept := st.Points[:0]
	for _, p := range st.Points {
		if !p.At.Before(cutoff) {
			kept = append(kept, p)
		}
	}
	st.Points = kept
	if len(st.Points) < 2 || st.Points[len(st.Points)-1].At.Sub(st.Points[0].At) < rd.sustain {
		return LevelNormal, 0
	}
	minV := st.Points[0].Value
	for _, p := range st.Points {
		if p.Value < minV {
			minV = p.Value
		}
	}
	switch {
	case rd.crit > 0 && minV >= rd.crit:
		return LevelCritical, rd.crit
	case rd.warn > 0 && minV >= rd.warn:
		return LevelWarning, rd.warn
	default:
		return LevelNormal, 0
	}
}

func thresholdFor(lvl Level, warn, crit float64) float64 {
	switch lvl {
	case LevelCritical:
		return crit
	case LevelWarning:
		return warn
	default:
		return warn
	}
}

// advance runs the hysteresis state machine for one reading and returns the
// transition or re-notification event, if any. first marks the metric's
// opening sample: its transition from unknown commits without the count
// hysteresis (sustained metrics still wait out their window in rawLevel);
// every transition after the first level is set keeps the full hysteresis.
func (w *Watcher) advance(st *metricState, rd reading, raw Level, threshold float64, at time.Time, first bool) *Event {
	cfg := w.cfgFor(rd.resource)
	if rd.metric == metricNetLink {
		if rd.ifaceUp {
			st.LinkWasUp = true
		}
		if raw != st.Level {
			st.Level = raw
			st.LastNotify = at
			ev := w.event(rd, raw, at, threshold)
			return &ev
		}
		return nil
	}
	if raw == st.Level {
		st.Pending, st.PendingN = raw, 0
		if st.Level != LevelNormal && cfg.Renotify > 0 && !st.LastNotify.IsZero() &&
			!at.Before(st.LastNotify.Add(cfg.Renotify)) {
			st.LastNotify = at
			ev := w.event(rd, st.Level, at, threshold)
			ev.Renotify = true
			ev.Type = "renotify"
			return &ev
		}
		return nil
	}
	need := cfg.Hysteresis
	if rd.sustain > 0 || rd.immediate || first {
		need = 1
	}
	if raw == st.Pending {
		st.PendingN++
	} else {
		st.Pending, st.PendingN = raw, 1
	}
	if st.PendingN >= need && st.Pending == raw {
		from := st.Level
		st.Level = raw
		st.Pending, st.PendingN = raw, 0
		st.LastNotify = at
		ev := w.event(rd, raw, at, threshold)
		ev.From = from
		return &ev
	}
	return nil
}

func (w *Watcher) event(rd reading, lvl Level, at time.Time, threshold float64) Event {
	ev := Event{
		SchemaVersion: WatchSchemaVersion,
		Type:          "alarm",
		Host:          w.host,
		Resource:      rd.resource,
		Metric:        rd.metric,
		From:          lvl,
		To:            lvl,
		Level:         lvl,
		Value:         rd.value,
		Unit:          rd.unit,
		Threshold:     threshold,
		Detail:        rd.detail,
		At:            at.UTC(),
	}
	if lvl == LevelNormal {
		ev.Type = "clear"
	}
	return ev
}

func (w *Watcher) appendEvent(ev Event) {
	w.events = append(w.events, ev)
	if len(w.events) > watchEventLogCap {
		w.events = w.events[len(w.events)-watchEventLogCap:]
	}
}

// --- persistent state --------------------------------------------------------

// WatchStateFile is the on-disk form of the watcher's memory, so `--once`
// runs from a scheduler keep hysteresis, sustained windows and re-notify
// clocks across invocations.
type WatchStateFile struct {
	SchemaVersion string                   `json:"schema_version"`
	Host          string                   `json:"host"`
	UpdatedAt     time.Time                `json:"updated_at"`
	Metrics       map[string]metricState   `json:"metrics"`
	Net           map[string]netSampleJSON `json:"net"`
}

type netSampleJSON struct {
	At        time.Time `json:"at"`
	RxBytes   uint64    `json:"rx_bytes"`
	TxBytes   uint64    `json:"tx_bytes"`
	RxPackets uint64    `json:"rx_packets"`
	TxPackets uint64    `json:"tx_packets"`
	RxErrors  uint64    `json:"rx_errors"`
	TxErrors  uint64    `json:"tx_errors"`
}

// ExportState snapshots the watcher's memory for persistence.
func (w *Watcher) ExportState() WatchStateFile {
	out := WatchStateFile{
		SchemaVersion: WatchSchemaVersion,
		Host:          w.host,
		UpdatedAt:     time.Now().UTC(),
		Metrics:       map[string]metricState{},
		Net:           map[string]netSampleJSON{},
	}
	for k, st := range w.states {
		out.Metrics[k] = *st
	}
	for k, p := range w.prevNet {
		out.Net[k] = netSampleJSON{At: p.at, RxBytes: p.rxBytes, TxBytes: p.txBytes,
			RxPackets: p.rxPackets, TxPackets: p.txPackets, RxErrors: p.rxErrors, TxErrors: p.txErrors}
	}
	return out
}

// ImportState restores memory written by ExportState.
func (w *Watcher) ImportState(f WatchStateFile) {
	if f.SchemaVersion != WatchSchemaVersion {
		return
	}
	for k, st := range f.Metrics {
		cp := st
		w.states[k] = &cp
	}
	for k, p := range f.Net {
		w.prevNet[k] = netSample{at: p.At, rxBytes: p.RxBytes, txBytes: p.TxBytes,
			rxPackets: p.RxPackets, txPackets: p.TxPackets, rxErrors: p.RxErrors, txErrors: p.TxErrors}
	}
	if f.Host != "" {
		w.host = f.Host
	}
}
