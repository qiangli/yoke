package watch

// BASH# SCRIPT HOOKS — A ROD, NOT A FISH.
//
// Two holes in the engine, and user code fills both:
//
//   - a PROBE hook is a source. It prints NDJSON samples and the engine
//     applies its rules to them exactly as to a built-in sampler, so a vendor
//     GPU tool, an app metric, a model quota, a remote API or a queue depth
//     becomes watchable without rebuilding bashy.
//   - an ACTION hook is a sink. It runs on a transition — warning, critical,
//     clear, idle, unanswered — so reclaiming a disk, pausing a run or paging
//     a seat is a script in the asset ring rather than a verb in this binary.
//
// The alternative is a verb per resource and a verb per response, which is the
// shape this whole engine exists to collapse.
//
// THE EVENT GOES ON STDIN AND THE ARGV IS FIXED. Nothing from an alert is ever
// interpolated into a script or its arguments: there is no shell string to
// build, so there is nothing for a locator containing a quote, a newline or a
// `$(…)` to break out of. A hook that wants a field reads its stdin.
//
// Four safety rules, applied in ONE place (Hooks.Run) so no caller can forget
// one:
//
//   - a per-hook timeout (60s probe, 5m action);
//   - concurrency 1 per hook — an overlapping invocation is SKIPPED, never
//     queued, because a queue of reclaims is a reclaim storm with a delay;
//   - a non-zero exit or a timeout is itself an ALARM EVENT, never a silent
//     failure: a hook that quietly did not run leaves the operator believing
//     it did;
//   - A4, the master never runs user hooks; A6, a destructive hook runs only
//     when something explicitly said it may.

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Hook kinds.
const (
	HookProbe  = "probe"
	HookAction = "action"
)

// Default per-hook timeouts. A probe is a sample and must not hold up a cycle;
// an action may legitimately do work.
const (
	DefaultProbeTimeout  = 60 * time.Second
	DefaultActionTimeout = 5 * time.Minute
)

// EnvWatchRole is how a master declares itself to the hook runner.
//
// There is no master/worker role check in this module to reuse, so A4 is
// enforced from an explicit signal: a process that sets BASHY_WATCH_ROLE=master
// (or constructs Hooks with Master set) refuses to execute user hook scripts.
// The master's job is to decide; running a user's script is a worker's.
const EnvWatchRole = "BASHY_WATCH_ROLE"

// Hook refusals.
var (
	// ErrHookMaster is A4: the master never runs user hooks. Both sentinels
	// carry no "watch:" prefix because every path wraps them with the hook's
	// kind and name, and a doubled prefix reads like two failures.
	ErrHookMaster = errors.New("the master does not run user hooks (" + EnvWatchRole + "=master)")
	// ErrHookUnconfirmed is A6: a destructive action needs explicit
	// confirmation in its configuration, not a crossed threshold.
	ErrHookUnconfirmed = errors.New("a destructive hook needs \"confirm\": true in its configuration")
)

// hookExt is the one file shape: a Bash# script.
const hookExt = ".bsh"

//go:embed hooks/*.bsh
var hookFS embed.FS

// HookSpec declares a hook — in a rule file, or built from flags.
type HookSpec struct {
	Name string `json:"name,omitempty"`
	// Type is the sink/source discriminator in a rule file ("hook").
	Type string `json:"type,omitempty"`
	// Script is the asset name in the hook ring, or a path to a .bsh file.
	Script string `json:"script"`
	// Args is the FIXED argv handed to the script. No event value is ever
	// interpolated into it.
	Args    []string `json:"args,omitempty"`
	Timeout Duration `json:"timeout,omitempty"`
	// Destructive marks a hook that deletes or overwrites; Confirm is the
	// explicit permission it then requires (A6).
	Destructive bool `json:"destructive,omitempty"`
	Confirm     bool `json:"confirm,omitempty"`
	// When selects the alerts an action hook runs for. Empty means every one.
	// The alert's own fields are matched as event attrs, so
	// {"attrs": {"level": "critical"}} reads the way a rule does.
	When Criteria `json:"when,omitempty"`
	// Locator, Class and Quiet are a probe's declarations about its task.
	Locator string   `json:"locator,omitempty"`
	Class   string   `json:"class,omitempty"`
	Quiet   Duration `json:"quiet_window,omitempty"`
}

// Hook is a resolved hook: the script, the ring it won from, and its policy.
type Hook struct {
	Name        string
	Kind        string
	Script      string
	Path        string // on-disk path when the winning ring has one
	Body        []byte
	Args        []string
	Timeout     time.Duration
	Destructive bool
	Confirm     bool
	Ring        assetring.Ring
	Origin      string
}

// id is the concurrency identity: one in-flight run per resolved script.
func (h Hook) id() string { return h.Kind + "|" + h.Script }

// label names the hook in an alert.
func (h Hook) label() string {
	if h.Name != "" {
		return h.Name
	}
	return h.Script
}

// HookResult is one hook invocation.
type HookResult struct {
	Stdout   []byte
	Exit     int
	Err      error
	TimedOut bool
	// Skipped reports that the previous invocation of this hook was still
	// running. It is not a failure and not an alarm: it is the concurrency
	// limit doing its job.
	Skipped  bool
	Duration time.Duration
}

// Failed reports whether this invocation is an alarm.
func (r HookResult) Failed() bool {
	return !r.Skipped && (r.Err != nil || r.TimedOut || r.Exit != 0)
}

// HookExec runs a resolved hook. It is injected so a unit test never needs a
// real bashy, and so the ONE place that knows how to reach the installed shell
// stays replaceable.
type HookExec func(ctx context.Context, h Hook, stdin []byte) (stdout []byte, exit int, err error)

// HookRunner is what the rest of the engine needs from a hook runner.
type HookRunner interface {
	Resolve(script, kind string) (Hook, error)
	Run(ctx context.Context, h Hook, stdin []byte) HookResult
}

// Hooks resolves hooks through the asset rings and runs them through the
// installed bashy, applying the safety rules once for every caller.
type Hooks struct {
	// Exec runs a resolved hook. Default: BashyExec.
	Exec HookExec
	// Master refuses every user hook (A4). It is also read from the
	// environment, so a master that did not set the field is still refused.
	Master bool
	Now    func() time.Time

	mu   sync.Mutex
	busy map[string]bool
}

// NewHooks builds the default runner: the asset rings for discovery, the
// installed bashy for execution.
func NewHooks() *Hooks {
	return &Hooks{Exec: BashyExec, Now: time.Now, busy: map[string]bool{}}
}

// HooksDir resolves the LOCAL hook ring, with the same three rungs as the
// preset ring — including $BASHY_HOME, so a run sandboxed with a relocated
// bashy home cannot reach the operator's real hook scripts.
//
//	$BASHY_WATCH_DIR/hooks   the specific override, most precise
//	$BASHY_HOME/watch/hooks  a relocated bashy home (tests, sandboxed runs)
//	~/.bashy/watch/hooks     the default
func HooksDir() string {
	if d := strings.TrimSpace(os.Getenv("BASHY_WATCH_DIR")); d != "" {
		return filepath.Join(d, "hooks")
	}
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "watch", "hooks")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".bashy", "watch", "hooks")
}

// HookAsset is one catalog entry: a hook script is its own body.
type HookAsset struct {
	Name   string
	Body   []byte
	Ring   assetring.Ring
	Origin string
}

// HookCatalog merges the embedded examples with the local ring, local last so
// a user's script of the same name shadows the shipped one.
func HookCatalog() *assetring.Catalog[HookAsset] {
	var sources []assetring.Source
	if sub, err := fs.Sub(hookFS, "hooks"); err == nil {
		sources = append(sources, assetring.FileFS(sub, assetring.RingEmbedded, hookExt))
	}
	if dir := HooksDir(); dir != "" {
		sources = append(sources, assetring.FileDir(dir, assetring.RingLocal, hookExt))
	}
	return &assetring.Catalog[HookAsset]{Sources: sources, Parse: parseHookAsset}
}

func parseHookAsset(name string, body []byte, src assetring.Source) HookAsset {
	a := HookAsset{Name: name, Body: body, Ring: src.Ring()}
	if ds, ok := src.(assetring.DirSourcer); ok {
		a.Origin = ds.Dir()
	}
	return a
}

// HookNames lists the merged hook catalog.
func HookNames() []string {
	rows, err := HookCatalog().Rows()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// Resolve finds a hook script: a path if it looks like one, an asset otherwise.
//
// An unresolvable hook is an ERROR, never a hook that does nothing — a watch
// configured with a sink that silently never ran is a watch that reports a
// problem and then ignores it.
func (hs *Hooks) Resolve(script, kind string) (Hook, error) {
	script = strings.TrimSpace(script)
	if script == "" {
		return Hook{}, fmt.Errorf("watch: a %s hook needs a script", kind)
	}
	h := Hook{Kind: kind, Script: script, Timeout: defaultHookTimeout(kind)}

	if strings.ContainsAny(script, `/\`) || strings.HasSuffix(script, hookExt) {
		path := script
		if !strings.HasSuffix(path, hookExt) {
			path += hookExt
		}
		body, err := os.ReadFile(path)
		if err == nil {
			abs, _ := filepath.Abs(path)
			h.Name = strings.TrimSuffix(filepath.Base(path), hookExt)
			h.Body, h.Path, h.Origin = body, abs, filepath.Dir(abs)
			h.Ring = assetring.RingLocal
			return h, nil
		}
		if !strings.ContainsAny(script, `/\`) {
			// A bare "gpu.bsh" that is not on disk is still a catalog name.
			script = strings.TrimSuffix(script, hookExt)
		} else {
			return Hook{}, fmt.Errorf("watch: %s hook %q: %w", kind, script, err)
		}
	}

	entry, src, ok := HookCatalog().Get(script)
	if !ok {
		return Hook{}, fmt.Errorf("watch: no %s hook %q (have: %s; local ring: %s)",
			kind, script, strings.Join(HookNames(), ", "), HooksDir())
	}
	h.Name, h.Body, h.Ring, h.Origin = entry.Name, entry.Body, entry.Ring, entry.Origin
	if ds, ok := src.(assetring.DirSourcer); ok {
		h.Path = filepath.Join(ds.Dir(), script+hookExt)
	}
	return h, nil
}

func defaultHookTimeout(kind string) time.Duration {
	if kind == HookProbe {
		return DefaultProbeTimeout
	}
	return DefaultActionTimeout
}

// masterRole reports whether this process is the master.
func (hs *Hooks) masterRole() bool {
	if hs.Master {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv(EnvWatchRole)), "master")
}

// Run executes a hook under the safety rules. It is the only way a hook runs.
func (hs *Hooks) Run(ctx context.Context, h Hook, stdin []byte) HookResult {
	if ctx == nil {
		ctx = context.Background()
	}
	now := hs.Now
	if now == nil {
		now = time.Now
	}
	if hs.masterRole() {
		// A4. Reported, not ignored: a configuration that expected the hook to
		// run has to hear that it did not.
		return HookResult{Err: ErrHookMaster}
	}
	if h.Destructive && !h.Confirm {
		// A6. A persistent destroy is authorized by configuration, never by a
		// threshold being crossed.
		return HookResult{Err: ErrHookUnconfirmed}
	}

	// Concurrency 1 per hook. SKIPPED, not queued.
	id := h.id()
	hs.mu.Lock()
	if hs.busy == nil {
		hs.busy = map[string]bool{}
	}
	if hs.busy[id] {
		hs.mu.Unlock()
		return HookResult{Skipped: true}
	}
	hs.busy[id] = true
	hs.mu.Unlock()
	defer func() {
		hs.mu.Lock()
		delete(hs.busy, id)
		hs.mu.Unlock()
	}()

	timeout := h.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout(h.Kind)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	exe := hs.Exec
	if exe == nil {
		exe = BashyExec
	}
	start := now()
	out, exit, err := exe(runCtx, h, stdin)
	res := HookResult{Stdout: out, Exit: exit, Err: err, Duration: now().Sub(start)}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		// A timeout is an alarm in its own right, and it replaces whatever the
		// killed process's exit status happened to be.
		res.TimedOut, res.Err = true, nil
		res.Exit = 0
	}
	return res
}

// LookBashy resolves the INSTALLED bashy, at run time, from PATH.
//
// Deliberately not os.Executable(): the process that owns this engine is a
// watch, a test binary or an embedding host, and exec'ing itself with a script
// argument would run something other than a shell. The lookup happens per call
// so a host that installs bashy mid-run is picked up.
func LookBashy() (string, error) {
	if p := strings.TrimSpace(os.Getenv("BASHY_WATCH_BASHY")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	path, err := exec.LookPath("bashy")
	if err != nil {
		return "", fmt.Errorf("watch: hooks need the installed bashy on PATH: %w", err)
	}
	return path, nil
}

// BashyExec is the default HookExec: `bashy --bashsharp <script> <fixed argv>`
// with the event JSON on stdin.
func BashyExec(ctx context.Context, h Hook, stdin []byte) ([]byte, int, error) {
	shell, err := LookBashy()
	if err != nil {
		return nil, 0, err
	}
	path := h.Path
	if path == "" || !fileExists(path) {
		// An embedded example has no path on disk. Materialize it 0700 for the
		// length of the run rather than teaching bashy to read a Go FS.
		tmp, err := os.CreateTemp("", "watch-hook-*"+hookExt)
		if err != nil {
			return nil, 0, err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(h.Body); err != nil {
			tmp.Close()
			return nil, 0, err
		}
		if err := tmp.Chmod(0o700); err != nil {
			tmp.Close()
			return nil, 0, err
		}
		if err := tmp.Close(); err != nil {
			return nil, 0, err
		}
		path = tmp.Name()
	}

	argv := append([]string{"--bashsharp", path}, h.Args...)
	c := exec.CommandContext(ctx, shell, argv...)
	c.Stdin = bytes.NewReader(stdin)
	var out, errBuf bytes.Buffer
	c.Stdout, c.Stderr = &out, &errBuf
	runErr := c.Run()
	if runErr == nil {
		return out.Bytes(), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		// stderr joins the captured output so a failure alarm can quote the
		// reason; a probe's SAMPLES are read off stdout only.
		return out.Bytes(), exitErr.ExitCode(), nil
	}
	return out.Bytes(), 0, runErr
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// hookAlarm is the alert a failed hook raises. Reason is ReasonError, so a
// sink filtering for real conditions does not confuse a broken hook with the
// condition it was supposed to handle.
func hookAlarm(h Hook, at time.Time, res HookResult, detail string) Alert {
	fields := map[string]any{"hook": h.label(), "kind": h.Kind, "script": h.Script}
	msg := fmt.Sprintf("%s hook %s", h.Kind, h.label())
	switch {
	case res.TimedOut:
		fields["timeout"] = h.Timeout.String()
		msg += fmt.Sprintf(": timed out after %s", h.Timeout)
	case res.Err != nil:
		msg += fmt.Sprintf(": %v", res.Err)
	case res.Exit != 0:
		fields["exit"] = res.Exit
		msg += fmt.Sprintf(": exited %d", res.Exit)
	}
	if detail != "" {
		msg += ": " + detail
	}
	if out := strings.TrimSpace(SanitizeText(res.Stdout)); out != "" {
		fields["output"] = firstLine(out)
	}
	return Alert{
		At: at, Rule: "hook:" + h.label(), State: StateFire, Reason: ReasonError,
		Locator: h.label(), Message: msg, Fields: fields,
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// hookError turns a result into the error a Source or Sink returns.
func hookError(h Hook, res HookResult) error {
	switch {
	case res.TimedOut:
		return fmt.Errorf("%s hook %s timed out after %s", h.Kind, h.label(), h.Timeout)
	case res.Err != nil:
		return fmt.Errorf("%s hook %s: %w", h.Kind, h.label(), res.Err)
	case res.Exit != 0:
		return fmt.Errorf("%s hook %s exited %d", h.Kind, h.label(), res.Exit)
	}
	return nil
}

// --- the action sink --------------------------------------------------

// HookSink runs an action hook on a transition.
type HookSink struct {
	Hooks HookRunner
	Hook  Hook
	// When selects the alerts this hook runs for. nil means every one.
	When Predicate
	// Alarm receives the alert a failed hook raises, and is the PREFERRED
	// route: the alert stream is where a watcher is looking, and it is deduped,
	// so a hook failing on every cycle is restated on the re-notify cadence
	// rather than printed per tick. With no Alarm wired the failure still
	// surfaces through the returned error — the engine reports a sink error —
	// because the one thing it must never be is silent.
	Alarm func(Alert)
	Now   func() time.Time
}

// NewHookSink resolves the spec and builds the sink.
func NewHookSink(hooks HookRunner, spec HookSpec) (*HookSink, error) {
	if hooks == nil {
		return nil, fmt.Errorf("watch: this host does not run hook scripts")
	}
	h, err := hooks.Resolve(spec.Script, HookAction)
	if err != nil {
		return nil, err
	}
	applySpec(&h, spec)
	s := &HookSink{Hooks: hooks, Hook: h, Now: time.Now}
	if !emptyCriteria(spec.When) {
		pred, err := spec.When.Compile()
		if err != nil {
			return nil, err
		}
		s.When = pred
	}
	return s, nil
}

func applySpec(h *Hook, spec HookSpec) {
	if spec.Name != "" {
		h.Name = spec.Name
	}
	h.Args = spec.Args
	if spec.Timeout.D() > 0 {
		h.Timeout = spec.Timeout.D()
	}
	h.Destructive, h.Confirm = spec.Destructive, spec.Confirm
}

// Deliver implements Sink.
func (s *HookSink) Deliver(a Alert) error {
	if s.When != nil && !s.When(alertAsEvent(a)) {
		return nil
	}
	body, err := json.Marshal(alertRecord{SchemaVersion: SchemaVersion, Alert: a})
	if err != nil {
		return err
	}
	res := s.Hooks.Run(context.Background(), s.Hook, append(body, '\n'))
	if res.Skipped {
		// Not an alarm: the previous run of this hook is still working.
		return nil
	}
	if !res.Failed() {
		return nil
	}
	at := a.At
	if at.IsZero() && s.Now != nil {
		at = s.Now()
	}
	if s.Alarm != nil {
		s.Alarm(hookAlarm(s.Hook, at, res, ""))
		return nil
	}
	return hookError(s.Hook, res)
}

// alertAsEvent lets a hook's `when` reuse the rule vocabulary: an alert's
// rule, reason, level and fields are matched as a source, a kind and attrs.
func alertAsEvent(a Alert) Event {
	attrs := map[string]string{
		"rule": a.Rule, "state": a.State, "reason": a.Reason, "level": a.Level,
	}
	for k, v := range a.Fields {
		if _, taken := attrs[k]; !taken {
			attrs[k] = scalar(v)
		}
	}
	return Event{
		At: a.At, Source: a.Rule, Locator: a.Locator, Kind: a.State,
		Text: a.Message, Attrs: attrs,
	}
}

func emptyCriteria(c Criteria) bool {
	return c.Source == "" && len(c.Kinds) == 0 && c.Locator == "" && c.Key == "" &&
		c.Field == "" && c.After == "" && c.Before == "" && c.Text == "" &&
		len(c.Attrs) == 0 && len(c.Any) == 0 && len(c.All) == 0 && !c.Negate
}

// --- the probe source -------------------------------------------------

// HookSample is one line of a probe hook's NDJSON output.
//
// The shape is the smallest thing a sampler can say: WHAT was measured, WHICH
// property, the VALUE, and optionally its unit and when. Anything richer would
// be a schema a vendor script has to learn.
type HookSample struct {
	Resource string          `json:"resource"`
	Field    string          `json:"field"`
	Value    json.RawMessage `json:"value"`
	Unit     string          `json:"unit,omitempty"`
	TS       string          `json:"ts,omitempty"`
	Text     string          `json:"text,omitempty"`
}

// HookSource is a probe hook as a Source.
type HookSource struct {
	Hooks HookRunner
	Hook  Hook
	// Locator is the resource name for a sample that does not carry one.
	Locator string
	Class   string
	Quiet   time.Duration
	Alarm   func(Alert)

	prev map[string]string
}

// NewHookSource resolves the spec and builds the source.
func NewHookSource(hooks HookRunner, spec HookSpec) (*HookSource, error) {
	if hooks == nil {
		return nil, fmt.Errorf("watch: this host does not run hook scripts")
	}
	h, err := hooks.Resolve(spec.Script, HookProbe)
	if err != nil {
		return nil, err
	}
	applySpec(&h, spec)
	return &HookSource{
		Hooks: hooks, Hook: h, Locator: spec.Locator,
		Class: spec.Class, Quiet: spec.Quiet.D(), prev: map[string]string{},
	}, nil
}

// Name implements Source.
func (s *HookSource) Name() string { return "hook:" + s.Hook.label() }

// Expected implements Expecter: a probe pointed at a named resource arms it,
// so "this probe has reported nothing since I started" is reportable.
func (s *HookSource) Expected() []string {
	if s.Locator == "" {
		return nil
	}
	return []string{s.Locator}
}

// Tasks implements TaskDeclarer.
func (s *HookSource) Tasks() []Task {
	if s.Locator == "" {
		return nil
	}
	return []Task{{Locator: s.Locator, Class: s.Class, Quiet: s.Quiet}}
}

// Poll implements Source: run the probe, read its samples.
//
// A probe that failed produces an alarm and NO events — the same rule the poll
// source follows, for the same reason: a script that could not run has told us
// nothing about the world, and the one thing a watcher must never do is turn
// that into a reading.
func (s *HookSource) Poll(ctx context.Context, now time.Time) ([]Event, error) {
	res := s.Hooks.Run(ctx, s.Hook, nil)
	if res.Skipped {
		return nil, nil
	}
	if res.Failed() {
		if s.Alarm != nil {
			s.Alarm(hookAlarm(s.Hook, now, res, ""))
		}
		return nil, hookError(s.Hook, res)
	}

	var evs []Event
	var bad []string
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var smp HookSample
		if err := json.Unmarshal(line, &smp); err != nil || smp.Field == "" {
			// One unreadable line does not discard the readable ones, and it is
			// not dropped in silence either: a probe whose output drifted has
			// to be heard about while its good lines keep working.
			bad = append(bad, firstLine(SanitizeText(line)))
			continue
		}
		evs = append(evs, s.events(smp, now)...)
	}
	if err := sc.Err(); err != nil {
		if s.Alarm != nil {
			s.Alarm(hookAlarm(s.Hook, now, HookResult{Err: err}, "reading samples"))
		}
		return evs, fmt.Errorf("probe hook %s: %w", s.Hook.label(), err)
	}
	if len(bad) > 0 {
		detail := fmt.Sprintf("%d unreadable sample line(s): %s", len(bad), strings.Join(bad, " | "))
		if s.Alarm != nil {
			s.Alarm(hookAlarm(s.Hook, now, HookResult{Exit: 0, Err: errors.New(detail)}, ""))
		}
	}
	return evs, nil
}

// events turns one sample into a KindSample observation, plus a KindChanged
// when the value moved — so a change rule works over a user's probe with no
// threshold rule in sight.
func (s *HookSource) events(smp HookSample, now time.Time) []Event {
	loc := smp.Resource
	if loc == "" {
		loc = s.Locator
	}
	at := now
	if smp.TS != "" {
		if t, err := time.Parse(time.RFC3339, smp.TS); err == nil {
			at = t
		}
	}
	value := strings.TrimSpace(string(smp.Value))
	value = strings.Trim(value, `"`)
	attrs := map[string]string{AttrTask: s.Class}
	if s.Class == "" {
		attrs = map[string]string{}
	}
	if smp.Unit != "" {
		attrs["unit"] = smp.Unit
	}
	key := loc + "|" + smp.Field
	base := Event{
		At: at, Source: s.Name(), Locator: loc, Key: smp.Field, Field: smp.Field,
		After: value, Text: smp.Text, Attrs: attrs,
	}
	sample := base
	sample.Kind = KindSample
	out := []Event{sample}
	if prev, seen := s.prev[key]; seen && prev != value {
		changed := base
		changed.Kind, changed.Before = KindChanged, prev
		out = append(out, changed)
	}
	s.prev[key] = value
	return out
}
