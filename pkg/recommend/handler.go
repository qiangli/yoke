package recommend

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/qiangli/coreutils/pkg/nudge"
	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/autoretry"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/skills"
	"mvdan.cc/sh/v3/interp"
)

// GuidancePolicy describes the active, process-local hint rules.
type GuidancePolicy struct {
	ScriptThreshold    int    `json:"script_threshold"`
	DedupScope         string `json:"dedup_scope"`
	SuppressionEnv     string `json:"suppression_env"`
	ShapeExcludedExits []int  `json:"shape_excluded_exits"`
	Precedence         []Kind `json:"precedence"`
}

// Policy returns a fresh inspection record; changing it does not change policy.
func Policy() GuidancePolicy {
	return GuidancePolicy{3, "process-session", "BASHY_HINTS=off", []int{0, 1, 2, 125, 127}, []Kind{KindCorrect, KindReuse, KindRegister, KindShape}}
}

var guidanceSession = struct {
	sync.Mutex
	scripts map[string]int
	emitted map[string]bool
}{scripts: map[string]int{}, emitted: map[string]bool{}}

// stderrContext replaces only the interpreter's stderr. The interpreter exposes
// HandlerCtx for reading but no setter; delegating all other context values
// preserves cancellation, environment, and the rest of the handler contract.
type stderrContext struct {
	context.Context
	stderr io.Writer
}

func (c stderrContext) Value(key any) any {
	v := c.Context.Value(key)
	if hc, ok := v.(interp.HandlerContext); ok {
		hc.Stderr = c.stderr
		return hc
	}
	return v
}

// stderrSample forwards bytes immediately and retains a bounded diagnostic tail.
// Commands may write stderr concurrently.
type stderrSample struct {
	sync.Mutex
	dst  io.Writer
	tail string
}

func (s *stderrSample) Write(p []byte) (int, error) {
	s.Lock()
	defer s.Unlock()
	n, err := s.dst.Write(p)
	s.tail += string(p[:n])
	const limit = 8192
	if len(s.tail) > limit {
		s.tail = s.tail[len(s.tail)-limit:]
	}
	return n, err
}

func (s *stderrSample) text() string {
	s.Lock()
	defer s.Unlock()
	return s.tail
}

// Handler observes completed commands without rewriting argv, stdout, or errors.
// All instances share process-session counts and (kind, key) deduplication.
func Handler() func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if !nudge.Enabled() || len(args) == 0 {
				return next(ctx, args)
			}
			hc := interp.HandlerCtx(ctx)
			if hc.Stderr == nil {
				return next(ctx, args)
			}
			sample := &stderrSample{dst: hc.Stderr}
			err := next(stderrContext{ctx, sample}, args)
			code := 0
			if err != nil {
				code = 1
				if status, ok := interp.IsExitStatus(err); ok {
					code = int(status)
				}
			}
			diagnostic := sample.text()
			if err != nil {
				diagnostic += "\n" + err.Error()
			}
			g, key, ok := commandGuidance(hc.Dir, args, code, diagnostic)
			if !ok || !nudge.Enabled() {
				return err
			}
			guidanceSession.Lock()
			id := string(g.Kind) + "\x00" + key
			seen := guidanceSession.emitted[id]
			guidanceSession.emitted[id] = true
			guidanceSession.Unlock()
			if !seen {
				// Keep the record on its own line even after an unterminated diagnostic.
				if tail := sample.text(); tail != "" && !strings.HasSuffix(tail, "\n") {
					fmt.Fprintln(hc.Stderr)
				}
				if weavecli.IsAgentDriven() {
					b, _ := MarshalGuidance(g)
					fmt.Fprintf(hc.Stderr, "%s\n", b)
				} else {
					io.WriteString(hc.Stderr, g.Line())
				}
			}
			return err
		}
	}
}

func commandGuidance(dir string, args []string, code int, stderr string) (Guidance, string, bool) {
	name := args[0]
	key := name
	script := localScript(dir, args)
	count := 0
	if script != "" {
		guidanceSession.Lock()
		guidanceSession.scripts[script]++
		count = guidanceSession.scripts[script]
		guidanceSession.Unlock()
	}
	front := filepath.Base(name) == "bashy" && len(args) > 1 && !strings.HasPrefix(args[1], "-") && !strings.ContainsAny(args[1], `/\`) && filepath.Ext(args[1]) == ""
	if front {
		name, key = args[1], "bashy "+args[1]
	}
	low := strings.ToLower(stderr)
	_, transient := autoretry.Classify(stderr)
	prerequisite := containsAny(low, "missing prerequisite", "prerequisite missing", "not initialized", "not initialised", "no such file or directory", "not a git repository", "requires initialization")
	if code == 2 || (code != 0 && (transient || prerequisite)) {
		reason := "correct the command usage; inspect bashy " + name + " --help"
		if prerequisite {
			reason = "check the app workspace and initialize missing prerequisites before retrying"
		}
		if transient {
			reason = "resolve the transient failure before retrying; inspect any autoretry report"
		}
		g := NewGuidance(KindCorrect, reason)
		g.Evidence = []string{key, stderr}
		return g, key, true
	}
	missing := code == 127 || (code != 0 && (strings.Contains(low, "command not found") || strings.Contains(low, name+": not found")))
	if missing {
		if g, ok := existingCapability(name); ok {
			return g, key, true
		}
		g := NewGuidance(KindRegister, "command not found; add a capability for "+name)
		g.Command = []string{"bashy", "command", "add", name}
		g.Prerequisites = []string{"choose an existing executable or script adapter"}
		g.Evidence = []string{key, "not-found"}
		g.Verify = "bashy commands verify " + name
		return g, key, true
	}
	if front && ((code != 0 && code != 1 && code != 2 && code != 125 && code != 127) || containsAny(low, "panic:", "internal error", "internal-error")) {
		summary := "correct " + key + " failure"
		g := NewGuidance(KindShape, summary)
		g.Command = []string{"bashy", "shape", "wish", "bashy", summary}
		g.Authorization = AuthApproval
		g.Evidence = []string{key, stderr}
		return g, key, true
	}
	// Routine errors above always take precedence over script promotion.
	if script != "" && count >= Policy().ScriptThreshold {
		name := strings.TrimSuffix(filepath.Base(script), filepath.Ext(script))
		if _, ok := existingCapability(name); ok {
			return Guidance{}, "", false
		}
		g := NewGuidance(KindRegister, "repeated local script can be registered as a command")
		g.Command = []string{"bashy", "command", "add", name, "--set", "exec.0=" + script}
		g.Verify = "bashy commands verify " + name
		g.Evidence = []string{script}
		g.Prerequisites = []string{"review the executable, arguments, and effects before registration"}
		return g, script, true
	}
	return Guidance{}, "", false
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func existingCapability(name string) (Guidance, bool) {
	if cmd, ok := fleet.New().Command(name); ok {
		g := NewGuidance(KindReuse, "reuse registered command "+cmd.Name)
		g.Command = []string{"bashy", cmd.Name}
		g.Evidence = []string{name, cmd.Name}
		return g, true
	}
	catalog, _ := skills.NewCatalog()
	if skill, _, ok := catalog.Get(name); ok {
		g := NewGuidance(KindReuse, "reuse skill "+skill.Name)
		g.Command = []string{"bashy", "skill", "show", skill.Name}
		g.Evidence = []string{name, skill.Name}
		return g, true
	}
	return Guidance{}, false
}

func localScript(dir string, args []string) string {
	candidate := args[0]
	switch filepath.Base(candidate) {
	case "bash", "bashy", "sh", "python", "python3", "node", "ruby", "perl":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return ""
		}
		candidate = args[1]
	default:
		if !strings.ContainsAny(candidate, `/\`) && filepath.Ext(candidate) == "" {
			return ""
		}
	}
	full := candidate
	if !filepath.IsAbs(full) {
		full = filepath.Join(dir, full)
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	// An extension or shebang distinguishes scripts from local native binaries.
	switch strings.ToLower(filepath.Ext(full)) {
	case ".sh", ".bash", ".py", ".js", ".rb", ".pl":
		return filepath.Clean(full)
	}
	f, err := os.Open(full)
	if err != nil {
		return ""
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err == nil && string(magic[:]) == "#!" {
		return filepath.Clean(full)
	}
	return ""
}
