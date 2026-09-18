package fleet

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Config assembles the rings a Catalog reads. Build one with New.
type Config struct {
	root    string
	rootSet bool                          // root came from WithRoot, not the environment
	shared  map[string][]assetring.Source // noun → extra read-only sources
	baseFS  fs.FS                         // embedded baseline (nil = the compiled-in one)
	noLocal bool
	noCloud bool

	liveProbe     LiveProbe
	contextCloner ContextCloner
	reservedName  ReservedName
	commandProbe  CommandProbe
}

// ReservedName reports whether a command name is already taken by something
// the embedding shell ships — a builtin, an applet, a front-door verb, a
// managed external, an alias, a skill — and by what. INJECTED for the usual
// reason: only the binary knows its whole command surface, and this package
// must not import it. Without it `commands add` checks only the ring itself.
type ReservedName func(name string) (holder string, taken bool)

// CommandProbe is an extra per-record check the binary can wire into
// `commands verify` — a script body's syntax, say — beyond what this package
// can see. Reports !ok with a reason; a nil probe skips nothing silently
// (verify says the probe is unwired).
type CommandProbe func(rec Command) (reason string, ok bool)

// LiveProbe launches an agent on a trivial prompt and reports whether it can
// actually speak.
//
// It is INJECTED rather than implemented here, and the reason is the import
// graph: launching an agent means pkg/chat, and chat reads this registry. So the
// registry declares the hole and the binary fills it (see WithLiveProbe).
//
// Plain strings, deliberately: the status vocabulary belongs to pkg/agentctl,
// which also imports this package. Naming its type here would close the cycle.
type LiveProbe func(ctx context.Context, agent string, timeout time.Duration) (status, note string, ok bool)

// ContextCloner copies an agent's conversation context to a clone, as of now.
//
// INJECTED, for the same import-graph reason as LiveProbe: knowing where an
// agent's store lives means pkg/chat + pkg/agentlaunch, and both read this
// registry. The registry declares the hole; the binary fills it.
//
// It returns a NOTE describing what actually happened, and that return is the
// point. Only a tool whose store bashy relocates can have its context copied;
// for every other tool the clone starts fresh, and the caller must be able to
// say which of the two it got. A clone that reports inherited context it did
// not inherit is the same class of lie as an agent answering from another
// task's history — the failure this whole model exists to prevent.
type ContextCloner func(parent, clone Agent) (note string, err error)

// Option configures a Catalog.
type Option func(*Config)

// WithContextCloner supplies the copier `agents clone` uses to branch an
// agent's context. Without it, cloning still mints the record and says plainly
// that the clone starts fresh.
func WithContextCloner(f ContextCloner) Option { return func(c *Config) { c.contextCloner = f } }

// WithReservedNames supplies the embedding shell's command surface so
// `commands add`/`set` refuse a name that already resolves to something bashy
// ships, and `commands list`/`verify` report a ring entry a newer bashy has
// since shadowed.
func WithReservedNames(f ReservedName) Option { return func(c *Config) { c.reservedName = f } }

// WithCommandProbe supplies an extra verification step for registered
// commands (see CommandProbe).
func WithCommandProbe(p CommandProbe) Option { return func(c *Config) { c.commandProbe = p } }

// WithLiveProbe supplies the launcher `agents verify --live` uses.
//
// Without it, --live refuses rather than quietly falling back to the structural
// check. A verification that silently checked something weaker than it claimed
// would be the exact failure it exists to prevent: a dead binding that looks
// verified.
func WithLiveProbe(p LiveProbe) Option { return func(c *Config) { c.liveProbe = p } }

// LiveProbeAgent launches an agent and reports what happened. Reports !ok when no
// probe is wired: a caller must be able to tell "not verified" from "verified OK".
func (c *Catalog) LiveProbeAgent(ctx context.Context, name string, timeout time.Duration) (Check, bool) {
	chk := Check{Kind: KindAgent, Name: name}
	if c.cfg.liveProbe == nil {
		return chk, false
	}
	// A dangling binding cannot be launched, and saying "it did not answer" would
	// bury the actual reason under a symptom.
	if _, _, _, err := c.Binding(name); err != nil {
		chk.Reason = err.Error()
		return chk, true
	}
	status, note, ok := c.cfg.liveProbe(ctx, name, timeout)
	chk.OK = ok
	chk.Reason = status
	chk.Detail = note
	return chk, true
}

// WithRoot pins the local store's parent directory.
//
// An explicit root also disables the per-noun $BASHY_*_DIR overrides: a
// caller that named a root meant that root, and ambient environment must
// not redirect writes out of it.
func WithRoot(dir string) Option {
	return func(c *Config) { c.root, c.rootSet = dir, true }
}

// WithSource adds a read-only source for one noun, above the baseline and
// below the local store.
func WithSource(noun string, src assetring.Source) Option {
	return func(c *Config) {
		if c.shared == nil {
			c.shared = map[string][]assetring.Source{}
		}
		c.shared[noun] = append(c.shared[noun], src)
	}
}

// WithBaselineFS replaces the compiled-in baseline. Tests use it to pin a
// catalog to a known world.
func WithBaselineFS(fsys fs.FS) Option { return func(c *Config) { c.baseFS = fsys } }

// WithoutLocalStore drops the host-local ring. Tests use it to read the
// baseline without touching the developer's real store.
func WithoutLocalStore() Option { return func(c *Config) { c.noLocal = true } }

// WithoutCloudOverlay drops the org-overlay ring.
func WithoutCloudOverlay() Option { return func(c *Config) { c.noCloud = true } }

// Catalog reads the merged fleet across every ring.
type Catalog struct{ cfg Config }

// New builds a Catalog.
func New(opts ...Option) *Catalog {
	cfg := Config{root: DefaultRoot()}
	for _, o := range opts {
		o(&cfg)
	}
	return &Catalog{cfg: cfg}
}

// Root reports the local store's parent directory.
func (c *Catalog) Root() string { return c.cfg.root }

// nounDir resolves a noun's local store, honoring the per-noun environment
// overrides only when the root itself came from the environment.
func (c *Catalog) nounDir(noun string) string {
	if c.cfg.rootSet {
		return filepath.Join(c.cfg.root, noun)
	}
	return NounDir(c.cfg.root, noun)
}

// sources assembles a noun's rings in precedence order: the embedded
// baseline, then shared catalog dirs, then any injected overlay (an org
// catalog cache), then the host-local store. The last source wins, so a
// local entry shadows everything.
func (c *Catalog) sources(noun string) []assetring.Source {
	var out []assetring.Source

	base := c.cfg.baseFS
	if base == nil {
		base = baselineFS
	}
	// The seeded roster (models + agents) can be switched off; the tool
	// launch contracts cannot — see SeedsEnv.
	// Registered commands have NO embedded ring by design (rod, not fish):
	// bashy ships the mechanism and never a catalog of commands.
	seeded := (noun == dirTools || !seedsOff()) && noun != dirCommands
	if sub, err := fs.Sub(base, baselineRoot+"/"+noun); err == nil && seeded {
		out = append(out, assetring.FileFS(sub, assetring.RingEmbedded, ext))
	}
	for _, dir := range sharedDirs(noun) {
		out = append(out, assetring.FileDir(dir, assetring.RingShared, ext))
	}
	out = append(out, c.cfg.shared[noun]...)
	// The org overlay sits ABOVE the compiled-in baseline and the shared dirs,
	// and BELOW the local store: an org default beats what bashy shipped, and
	// an operator's own entry beats the org. Absent until a `sync` lands.
	if !c.cfg.noCloud {
		out = append(out, cloudSources(c.cfg.root, noun)...)
	}
	if !c.cfg.noLocal {
		out = append(out, assetring.FileDir(c.nounDir(noun), assetring.RingLocal, ext))
	}
	return out
}

// parseErr records an entry that could not be read. A catalog reports
// broken entries; it never hides them behind a shorter list.
type parseErr struct {
	Name string
	Err  error
}

func (e parseErr) Error() string { return e.Name + ": " + e.Err.Error() }

// Tools returns every agentic-CLI tool, name-sorted.
//
// The asset registry's tool namespace is shared with MCP-style function
// kits (kind func/web/system). Those are not fleet tools and are omitted;
// pass all=true to see them.
func (c *Catalog) Tools(all bool) ([]Tool, []error) {
	var errs []error
	cat := &assetring.Catalog[Tool]{
		Sources: c.sources(dirTools),
		Parse: func(n string, b []byte, s assetring.Source) Tool {
			t, err := ParseTool(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Tool{Name: n, Ring: s.Ring()}
			}
			return t
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Tool, 0, len(rows))
	for _, r := range rows {
		if !all && !r.Entry.IsCLI() {
			continue
		}
		out = append(out, r.Entry)
	}
	return out, errs
}

// Tool resolves a tool by canonical name or alias.
func (c *Catalog) Tool(name string) (Tool, bool) {
	tools, _ := c.Tools(true)
	for _, t := range tools {
		for _, n := range t.Names() {
			if n == name {
				return t, true
			}
		}
	}
	return Tool{}, false
}

// Models returns every model, name-sorted.
func (c *Catalog) Models() ([]Model, []error) {
	var errs []error
	cat := &assetring.Catalog[Model]{
		Sources: c.sources(dirModels),
		Parse: func(n string, b []byte, s assetring.Source) Model {
			m, err := ParseModel(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Model{Name: n, Ring: s.Ring()}
			}
			return m
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Model, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	decorateModels(out)
	return out, errs
}

// Model resolves a model by canonical name or alias — including the derived
// family alias, so `opus` finds whichever opus is newest.
func (c *Catalog) Model(name string) (Model, bool) {
	models, _ := c.Models()
	for _, m := range models {
		for _, n := range m.Names() {
			if n == name {
				return m, true
			}
		}
	}
	return Model{}, false
}

// agentEntries returns every agent declared by the merged file ring, including
// entries whose canonical names collide. Resolution needs the complete set so
// it can fail closed; Agents applies the user-facing one-row-per-NAME policy on
// top of it.
func (c *Catalog) agentEntries() ([]Agent, []error) {
	var errs []error
	cat := &assetring.Catalog[AgentFile]{
		Sources: c.sources(dirAgents),
		Parse: func(n string, b []byte, s assetring.Source) AgentFile {
			f, err := ParseAgentFile(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return AgentFile{}
			}
			return f
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	var out []Agent
	for _, r := range rows {
		out = append(out, r.Entry.Agents...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	// Model parse errors are not agent parse errors — they surface in
	// `models list`, and here they already show up as a dangling binding.
	models, _ := c.Models()
	decorateAgents(out, models)
	return out, errs
}

// Agents returns every agent across every file, nickname-sorted.
func (c *Catalog) Agents() ([]Agent, []error) {
	entries, errs := c.agentEntries()
	var out []Agent
	seen := map[string]Agent{}
	for _, a := range entries {
		key := strings.ToLower(strings.TrimSpace(a.Name))
		if previous, ok := seen[key]; ok {
			errs = append(errs, fmt.Errorf(
				"fleet: duplicate agent NAME %q (%s and %s); every agent identity must have one unique NAME",
				a.Name, previous.MatrixKey(), a.MatrixKey()))
			continue
		}
		seen[key] = a
		out = append(out, a)
	}
	return out, errs
}

// Agent resolves an agent by nickname, human name, family alias, or declared
// alias. It also accepts a bare tool:model binding, so `claude:opus4.8` names
// its agent even before anyone has nicknamed it.
func (c *Catalog) Agent(name string) (Agent, bool) {
	agents, _ := c.agentEntries()
	canonicalCount := map[string]int{}
	for _, a := range agents {
		canonicalCount[strings.ToLower(strings.TrimSpace(a.Name))]++
	}
	unique := func(matches []Agent) (Agent, bool) {
		if len(matches) != 1 || canonicalCount[strings.ToLower(strings.TrimSpace(matches[0].Name))] != 1 {
			return Agent{}, false
		}
		return matches[0], true
	}
	matchNames := func(equal func(string) bool) []Agent {
		var matches []Agent
		for _, a := range agents {
			for _, n := range a.Names() {
				if equal(n) {
					matches = append(matches, a)
					break
				}
			}
		}
		return matches
	}

	exact := matchNames(func(n string) bool { return n == name })
	if len(exact) > 0 {
		return unique(exact)
	}
	if tool, model, ok := strings.Cut(name, ":"); ok {
		// The model half is resolved through the catalog, not string-matched,
		// so `claude:opus` finds the agent bound to the newest Opus the same way the
		// bare name `opus` finds the model. A binding you can type is worth
		// little if it only accepts the exact version string.
		if m, found := c.Model(model); found {
			model = m.Name
		}
		var matches []Agent
		for _, a := range agents {
			if a.Tool == tool && a.Model == model {
				matches = append(matches, a)
			}
		}
		if len(matches) > 0 {
			return unique(matches)
		}
	}
	// Case-insensitive fallback, exact match having failed: a nickname is a
	// human name (`Elif`), and a human types `elif`. Only reached when nothing
	// matched exactly, so it never shadows a precise binding.
	folded := matchNames(func(n string) bool { return strings.EqualFold(n, name) })
	if len(folded) > 0 {
		return unique(folded)
	}
	return Agent{}, false
}

// People returns every human principal, handle-sorted.
func (c *Catalog) People() ([]Person, []error) {
	var errs []error
	cat := &assetring.Catalog[Person]{
		Sources: c.sources(dirPeople),
		Parse: func(n string, b []byte, s assetring.Source) Person {
			p, err := ParsePerson(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Person{Handle: n, Ring: s.Ring()}
			}
			return p
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Person, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}

// Person resolves a human by handle or alias.
func (c *Catalog) Person(name string) (Person, bool) {
	people, _ := c.People()
	for _, p := range people {
		for _, n := range p.Names() {
			if n == name {
				return p, true
			}
		}
	}
	return Person{}, false
}

// Binding resolves an agent nickname to its tool and model entries. It is
// the bridge from a name a human typed to the two assets that make the
// agent runnable. A dangling tool or model is reported, never silently
// dropped: an agent whose halves do not resolve cannot be launched.
func (c *Catalog) Binding(nick string) (Agent, Tool, Model, error) {
	a, ok := c.Agent(nick)
	if !ok {
		return Agent{}, Tool{}, Model{}, fmt.Errorf("fleet: no agent %q", nick)
	}
	t, ok := c.Tool(a.Tool)
	if !ok {
		return a, Tool{}, Model{}, fmt.Errorf("fleet: agent %q names tool %q, which is not in the catalog", nick, a.Tool)
	}
	m, ok := c.Model(a.Model)
	if !ok {
		return a, t, Model{}, fmt.Errorf("fleet: agent %q names model %q, which is not in the catalog", nick, a.Model)
	}
	return a, t, m, nil
}

// AliasCollision reports a name claimed by two entries of the same kind.
type AliasCollision struct {
	Kind  string
	Name  string
	Holds []string // canonical names of the entries claiming it
}

func (a AliasCollision) Error() string {
	return fmt.Sprintf("fleet: %s name %q is claimed by %s", a.Kind, a.Name, strings.Join(a.Holds, " and "))
}

// CheckAliases reports names claimed by more than one entry of the same
// kind. Aliasing is free — `007` and `smarty` may both name one agent —
// but one name may never mean two things, or `whois` would have to guess.
func (c *Catalog) CheckAliases() []AliasCollision {
	var out []AliasCollision
	check := func(kind string, entries [][]string) {
		holders := map[string][]string{}
		for _, ns := range entries {
			if len(ns) == 0 {
				continue
			}
			canon := ns[0]
			for _, n := range ns {
				holders[n] = append(holders[n], canon)
			}
		}
		for _, n := range sortedKeys(holders) {
			if h := dedupe(holders[n]); len(h) > 1 {
				out = append(out, AliasCollision{Kind: kind, Name: n, Holds: h})
			}
		}
	}

	tools, _ := c.Tools(true)
	tn := make([][]string, 0, len(tools))
	for _, t := range tools {
		tn = append(tn, t.Names())
	}
	check(KindTool, tn)

	models, _ := c.Models()
	mn := make([][]string, 0, len(models))
	for _, m := range models {
		mn = append(mn, m.Names())
	}
	check(KindModel, mn)

	agents, _ := c.Agents()
	an := make([][]string, 0, len(agents))
	for _, a := range agents {
		an = append(an, a.Names())
	}
	check(KindAgent, an)

	people, _ := c.People()
	pn := make([][]string, 0, len(people))
	for _, p := range people {
		pn = append(pn, p.Names())
	}
	check(KindPerson, pn)

	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
