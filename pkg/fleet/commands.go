package fleet

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// A registered command is the sixth fleet noun (`bashy commands add`): a
// command the OPERATOR (or an org, through a shared ring) taught bashy, so
// that it is listed, classified and dispatched exactly like a shipped one.
//
// The record carries exactly ONE implementation — an argv template naming an
// executable the host already has (exec), a pinned release binary bashy
// provisions (download), or an inline body run through bashy itself (script)
// — plus the atlas metadata every shipped command carries: effects, caps,
// group, tier, stage, shape, platforms. The mechanism defaults what it knows
// for certain and refuses to guess the rest; in particular a script body's
// EFFECTS are the author's to declare, because a body can do anything and
// "curated, never inferred" holds for the operator's record too.
//
// Dispatch precedence is builtin → applet → front-door verb → REGISTERED →
// PATH: a registered name may shadow a PATH binary (that is the point) but
// never anything bashy ships — `add` refuses the collision, and a ring entry
// a newer bashy has since claimed is reported as shadowed, never silently
// resolved (see Catalog.CommandShadows).

// Command is one registered command record: `name:` + `kind: command`.
type Command struct {
	Name     string `yaml:"name" json:"name" doc:"canonical command name (a bare identifier)"`
	Kind     string `yaml:"kind" json:"kind" doc:"always command"`
	Synopsis string `yaml:"synopsis,omitempty" json:"synopsis,omitempty" doc:"one line for bashy commands"`
	Long     string `yaml:"long,omitempty" json:"long,omitempty" doc:"help body for bashy commands NAME"`

	// Exactly one of exec / download / script.
	Exec     []string         `yaml:"exec,omitempty" json:"exec,omitempty" doc:"argv template: an absolute path or PATH name plus fixed args; user args are appended, or spliced at a single {args} element"`
	Download *CommandDownload `yaml:"download,omitempty" json:"download,omitempty" doc:"pinned release to provision (binmgr); the sha256 in the record is the trust root"`
	Script   string           `yaml:"script,omitempty" json:"script,omitempty" doc:"inline body run as bashy -c BODY NAME ARGS ($0 = NAME)"`
	Dialect  string           `yaml:"dialect,omitempty" json:"dialect,omitempty" doc:"script dialect: bashpp (default) or bash"`

	// Atlas metadata (closed vocabularies from pkg/atlas).
	Effects []string `yaml:"effects,omitempty" json:"effects,omitempty" doc:"security effects (atlas vocabulary); at least one — script bodies must declare theirs"`
	Caps    []string `yaml:"caps,omitempty" json:"caps,omitempty" doc:"agentic capabilities (atlas vocabulary)"`
	Group   string   `yaml:"group,omitempty" json:"group,omitempty" doc:"functional group (atlas vocabulary); default shellutils"`
	Tier    string   `yaml:"tier,omitempty" json:"tier,omitempty" doc:"execution tier (atlas vocabulary); default userland"`
	Stage   string   `yaml:"stage,omitempty" json:"stage,omitempty" doc:"SDLC stage (atlas vocabulary); default cross"`
	Shape   string   `yaml:"shape,omitempty" json:"shape,omitempty" doc:"output shape: result (default) or verdict"`
	OS      []string `yaml:"os,omitempty" json:"os,omitempty" doc:"supported platforms (darwin linux windows); default all, or the platforms a download carries a digest for"`

	Aliases []string `yaml:"aliases,omitempty" json:"aliases,omitempty" doc:"alternate names (each passes the same collision filter)"`
	Hidden  bool     `yaml:"hidden,omitempty" json:"hidden,omitempty" doc:"omit from default listings (--all shows it)"`
	Env     []string `yaml:"env,omitempty" json:"env,omitempty" doc:"extra KEY=VALUE entries for the child process"`
	Cwd     string   `yaml:"cwd,omitempty" json:"cwd,omitempty" doc:"working directory for the child (absolute); default the caller's"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// CommandDownload pins a release binary. The record IS the pin: a digest
// for the running platform is required before anything is fetched, and the
// download is verified against it (fail-closed, like every bashy download).
type CommandDownload struct {
	GitHub  string            `yaml:"github,omitempty" json:"github,omitempty" doc:"owner/repo of a GitHub release (one of github/url)"`
	URL     string            `yaml:"url,omitempty" json:"url,omitempty" doc:"download URL template with {version} {goos} {goarch} {ext} (one of github/url)"`
	Version string            `yaml:"version,omitempty" json:"version,omitempty" doc:"release tag; required and never latest — there is nothing to pin otherwise"`
	Member  string            `yaml:"member,omitempty" json:"member,omitempty" doc:"executable path inside a .tar.gz/.zip; empty = the download is the raw binary"`
	SHA256  map[string]string `yaml:"sha256,omitempty" json:"sha256,omitempty" doc:"goos/goarch -> hex sha256 of the downloaded artifact; required per platform"`
}

// Dialects a script: body may declare.
const (
	DialectBashPP = "bashpp"
	DialectBash   = "bash"
)

// ArgsToken is the one element of an exec: template that user args replace.
// Without it they are appended.
const ArgsToken = "{args}"

// reservedCommandWords are the words `bashy commands` itself owns: the CRUD
// verbs and the two spellings of the lister. A registered command may not
// take one, whatever the host's catalog says.
var reservedCommandWords = []string{
	"add", "show", "set", "rm", "edit", "schema", "list", "verify", "sync", "command", "commands",
}

// ReservedCommandWords returns the words the `commands` verb keeps for itself.
func ReservedCommandWords() []string { return append([]string(nil), reservedCommandWords...) }

var (
	hex64      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	envKeyLine = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	goarches   = []string{"amd64", "arm64", "386", "arm", "riscv64", "ppc64le", "s390x", "loong64", "mips64le"}
)

// ParseCommand reads a command asset and applies the mechanism's defaults
// (never written back). name is the fallback identity when the document
// carries none.
func ParseCommand(name string, body []byte, src assetring.Source) (Command, error) {
	var c Command
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !strings.Contains(err.Error(), "EOF") {
		return Command{}, fmt.Errorf("fleet: command %q: %w", name, err)
	}
	if c.Name == "" {
		c.Name = name
	}
	if src != nil {
		c.Ring = src.Ring()
	}
	c.applyDefaults()
	return c, nil
}

func (c *Command) applyDefaults() {
	if c.Kind == "" {
		c.Kind = KindCommand
	}
	if c.Group == "" {
		c.Group = atlas.GroupShellutils
	}
	if c.Tier == "" {
		c.Tier = atlas.TierUserland
	}
	if c.Stage == "" {
		c.Stage = atlas.StageCross
	}
	if c.Shape == "" {
		c.Shape = string(atlas.ShapeResult)
	}
	if c.Script != "" && c.Dialect == "" {
		c.Dialect = DialectBashPP
	}
	if len(c.Effects) == 0 {
		switch c.Mode() {
		case atlas.RegisteredExec:
			c.Effects = []string{atlas.EffExec}
		case atlas.RegisteredDownload:
			c.Effects = []string{atlas.EffExec, atlas.EffNet}
		}
	}
	if len(c.OS) == 0 {
		if c.Download != nil && len(c.Download.SHA256) > 0 {
			for k := range c.Download.SHA256 {
				if goos, _, ok := strings.Cut(k, "/"); ok && !slices.Contains(c.OS, goos) {
					c.OS = append(c.OS, goos)
				}
			}
		} else {
			c.OS = atlas.OSes()
		}
	}
	sort.Strings(c.OS)
}

// Mode reports which implementation the record carries: exec, download,
// script, or "" when it carries none (or more than one — Validate says which).
func (c Command) Mode() string {
	n, mode := 0, ""
	if len(c.Exec) > 0 {
		n, mode = n+1, atlas.RegisteredExec
	}
	if c.Download != nil {
		n, mode = n+1, atlas.RegisteredDownload
	}
	if c.Script != "" {
		n, mode = n+1, atlas.RegisteredScript
	}
	if n != 1 {
		return ""
	}
	return mode
}

// Names returns the canonical name and every alias.
func (c Command) Names() []string { return names(c.Name, c.Aliases) }

// AtlasSpec projects the record for atlas.RegisteredEntry.
func (c Command) AtlasSpec() atlas.RegisteredSpec {
	return atlas.RegisteredSpec{
		Kind:    c.Mode(),
		Group:   c.Group,
		Tier:    c.Tier,
		Stage:   c.Stage,
		Shape:   atlas.OutputShape(c.Shape),
		Caps:    c.Caps,
		Effects: c.Effects,
		OS:      c.OS,
	}
}

// Validate checks the record against the closed vocabularies and the
// one-implementation rule. reserved, when non-nil, is the embedding shell's
// answer to "does this name already resolve to something you ship" (see
// WithReservedNames); the ring's own names are checked by the caller.
func (c Command) Validate(reserved ReservedName) error {
	for _, n := range c.Names() {
		if err := validName(n); err != nil {
			return err
		}
		if strings.ContainsAny(n, " \t\n") {
			return fmt.Errorf("fleet: command name %q must be a bare identifier", n)
		}
		switch n {
		case "var", "const", "func", "import", "package", "goto":
			return fmt.Errorf("fleet: %q is reserved by Bash++; rename the registered command or alias; to invoke an external program, use command %s, %q, or an explicit path", n, n, n)
		}
		if slices.Contains(reservedCommandWords, n) {
			return fmt.Errorf("fleet: %q is a word `bashy commands` keeps for itself (%s)", n, strings.Join(reservedCommandWords, " "))
		}
		if reserved != nil {
			if holder, taken := reserved(n); taken {
				return fmt.Errorf("fleet: %q already resolves to %s; a registered command may shadow a PATH binary, never a command bashy ships", n, holder)
			}
		}
	}
	if c.Kind != "" && c.Kind != KindCommand {
		return fmt.Errorf("fleet: command %q: kind must be %q, got %q", c.Name, KindCommand, c.Kind)
	}
	switch c.Mode() {
	case atlas.RegisteredExec:
		if strings.TrimSpace(c.Exec[0]) == "" {
			return fmt.Errorf("fleet: command %q: exec.0 (the executable) is empty", c.Name)
		}
		if n := countToken(c.Exec, ArgsToken); n > 1 {
			return fmt.Errorf("fleet: command %q: exec may carry %s at most once (found %d)", c.Name, ArgsToken, n)
		}
	case atlas.RegisteredDownload:
		if err := c.Download.validate(c.Name); err != nil {
			return err
		}
	case atlas.RegisteredScript:
		switch c.Dialect {
		case "", DialectBashPP, DialectBash:
		default:
			return fmt.Errorf("fleet: command %q: dialect must be %s or %s, got %q", c.Name, DialectBashPP, DialectBash, c.Dialect)
		}
	default:
		return fmt.Errorf("fleet: command %q: set exactly one of exec, download, script (--set exec.0=… | download.url=… | script=…)", c.Name)
	}
	if len(c.Effects) == 0 {
		return fmt.Errorf("fleet: command %q: effects is empty — a %s body's effects are the author's to declare (--set effects.0=<%s>)",
			c.Name, c.Mode(), strings.Join(atlas.Effects(), "|"))
	}
	if err := inVocab("effects", c.Effects, atlas.Effects()); err != nil {
		return fmt.Errorf("fleet: command %q: %w", c.Name, err)
	}
	if err := inVocab("caps", c.Caps, atlas.Capabilities()); err != nil {
		return fmt.Errorf("fleet: command %q: %w", c.Name, err)
	}
	if err := inVocab("os", c.OS, atlas.OSes()); err != nil {
		return fmt.Errorf("fleet: command %q: %w", c.Name, err)
	}
	if !slices.Contains(atlas.Groups(), c.Group) || c.Group == atlas.GroupShell {
		return fmt.Errorf("fleet: command %q: group %q is not one of %s", c.Name, c.Group, strings.Join(atlas.Groups(), "|"))
	}
	if !slices.Contains(atlas.Tiers(), c.Tier) {
		return fmt.Errorf("fleet: command %q: tier %q is not one of %s", c.Name, c.Tier, strings.Join(atlas.Tiers(), "|"))
	}
	if !slices.Contains(atlas.Stages(), c.Stage) {
		return fmt.Errorf("fleet: command %q: stage %q is not one of %s", c.Name, c.Stage, strings.Join(atlas.Stages(), "|"))
	}
	if c.Shape != string(atlas.ShapeResult) && c.Shape != string(atlas.ShapeVerdict) {
		return fmt.Errorf("fleet: command %q: shape %q is not result|verdict", c.Name, c.Shape)
	}
	for _, kv := range c.Env {
		if !envKeyLine.MatchString(kv) {
			return fmt.Errorf("fleet: command %q: env entry %q is not KEY=VALUE", c.Name, kv)
		}
	}
	if c.Cwd != "" && !filepath.IsAbs(c.Cwd) {
		return fmt.Errorf("fleet: command %q: cwd %q must be absolute", c.Name, c.Cwd)
	}
	return nil
}

func (d *CommandDownload) validate(name string) error {
	if (d.GitHub == "") == (d.URL == "") {
		return fmt.Errorf("fleet: command %q: download needs exactly one of github (owner/repo) or url (template)", name)
	}
	if d.GitHub != "" && strings.Count(d.GitHub, "/") != 1 {
		return fmt.Errorf("fleet: command %q: download.github must be owner/repo, got %q", name, d.GitHub)
	}
	if v := strings.ToLower(strings.TrimSpace(d.Version)); v == "" || v == "latest" {
		return fmt.Errorf("fleet: command %q: download.version is required and never latest — a record is a pin", name)
	}
	if len(d.SHA256) == 0 {
		return fmt.Errorf("fleet: command %q: download.sha256 is empty — a digest per platform is required (--set download.sha256.%s=<hex>); bashy never installs unverified bytes", name, binmgr.Platform())
	}
	for k, v := range d.SHA256 {
		goos, goarch, ok := strings.Cut(k, "/")
		if !ok || !slices.Contains(atlas.OSes(), goos) || !slices.Contains(goarches, goarch) {
			return fmt.Errorf("fleet: command %q: download.sha256 key %q is not goos/goarch", name, k)
		}
		if !hex64.MatchString(v) {
			return fmt.Errorf("fleet: command %q: download.sha256.%s is not a 64-hex sha256", name, k)
		}
	}
	return nil
}

func inVocab(field string, vals, vocab []string) error {
	for _, v := range vals {
		if !slices.Contains(vocab, v) {
			return fmt.Errorf("%s value %q is not one of %s", field, v, strings.Join(vocab, "|"))
		}
	}
	return nil
}

func countToken(argv []string, tok string) int {
	n := 0
	for _, a := range argv {
		if a == tok {
			n++
		}
	}
	return n
}

// Argv builds the child argv for one invocation. self is the bashy
// executable (scripts re-enter it); bin is the provisioned binary for a
// download record (see Ensure) and is ignored otherwise.
func (c Command) Argv(self, bin string, args []string) []string {
	switch c.Mode() {
	case atlas.RegisteredExec:
		out := make([]string, 0, len(c.Exec)+len(args))
		spliced := false
		for _, a := range c.Exec {
			if a == ArgsToken {
				out = append(out, args...)
				spliced = true
				continue
			}
			out = append(out, a)
		}
		if !spliced {
			out = append(out, args...)
		}
		return out
	case atlas.RegisteredDownload:
		return append([]string{bin}, args...)
	case atlas.RegisteredScript:
		out := []string{self}
		if c.Dialect == DialectBash {
			out = append(out, "--no-bashpp")
		}
		out = append(out, "-c", c.Script, c.Name)
		return append(out, args...)
	}
	return nil
}

// Ensure provisions a download record's binary for this platform, cache
// first (no network on a hit), and returns its path. The record's digest for
// this platform is the trust root: absent → refused before any fetch; present
// → the fetched bytes are verified against it by binmgr.Ensure.
func (c Command) Ensure(ctx context.Context) (string, error) {
	if c.Mode() != atlas.RegisteredDownload {
		return "", fmt.Errorf("fleet: command %q is not a download record", c.Name)
	}
	if p := binmgr.CachedBinary(c.Name); p != "" {
		return p, nil
	}
	plat := binmgr.Platform()
	digest, ok := c.Download.SHA256[plat]
	if !ok {
		return "", fmt.Errorf("fleet: command %q: no sha256 for %s in the record; refusing to download unverified bytes", c.Name, plat)
	}
	var tool binmgr.Tool
	var err error
	if c.Download.GitHub != "" {
		tool, err = binmgr.ResolveGitHub(ctx, binmgr.GitHubSpec{
			Name: c.Name, Repo: c.Download.GitHub, Version: c.Download.Version, Member: c.Download.Member,
		})
		if err != nil {
			return "", err
		}
		// The record's digest overrides whatever the release advertised: the
		// reviewed YAML is the trust root, exactly as pins.go is for the
		// compiled-in externals.
		a := tool.Assets[plat]
		a.SHA256, a.SHA512, a.MD5 = digest, "", ""
		tool.Assets[plat] = a
	} else {
		tool = binmgr.ResolveURLPinned(binmgr.URLSpec{
			Name: c.Name, Version: c.Download.Version, URLTemplate: c.Download.URL, Member: c.Download.Member,
		}, digest)
	}
	return binmgr.Ensure(ctx, tool)
}

// Executable reports the program an exec record names and whether the host
// has it (absolute path, or on PATH).
func (c Command) Executable() (string, bool) {
	if c.Mode() != atlas.RegisteredExec {
		return "", false
	}
	prog := c.Exec[0]
	if filepath.IsAbs(prog) {
		_, err := exec.LookPath(prog)
		return prog, err == nil
	}
	p, err := exec.LookPath(prog)
	return p, err == nil
}

// --- catalog ---------------------------------------------------------------

// Commands returns every registered command, name-sorted, across the
// shared and local rings. Entries that fail to parse are reported, never
// hidden.
func (c *Catalog) Commands() ([]Command, []error) {
	var errs []error
	cat := &assetring.Catalog[Command]{
		Sources: c.sources(dirCommands),
		Parse: func(n string, b []byte, s assetring.Source) Command {
			r, err := ParseCommand(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Command{Name: n, Kind: KindCommand, Ring: s.Ring()}
			}
			return r
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Command, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}

// Command resolves a registered command by canonical name or alias.
func (c *Catalog) Command(name string) (Command, bool) {
	cmds, _ := c.Commands()
	for _, r := range cmds {
		if slices.Contains(r.Names(), name) {
			return r, true
		}
	}
	return Command{}, false
}

// CommandDirs returns every directory the commands ring is read from, in
// precedence order (shared dirs, then the local store). A hot-path caller
// fingerprints these to know when to reload.
func (c *Catalog) CommandDirs() []string {
	dirs := append([]string(nil), sharedDirs(dirCommands)...)
	if !c.cfg.noLocal {
		dirs = append(dirs, c.nounDir(dirCommands))
	}
	return dirs
}

// CommandShadows reports the ring entries the embedding shell now ships a
// command for (see WithReservedNames): each is skipped at dispatch and named
// here so the operator can rename it. Without a reserved-name predicate it
// reports nothing, because it cannot know.
func (c *Catalog) CommandShadows() map[string]string {
	out := map[string]string{}
	if c.cfg.reservedName == nil {
		return out
	}
	cmds, _ := c.Commands()
	for _, r := range cmds {
		for _, n := range r.Names() {
			if holder, taken := c.cfg.reservedName(n); taken {
				out[r.Name] = holder
				break
			}
		}
	}
	return out
}

// --- writes ----------------------------------------------------------------

// SaveCommand validates and writes a command into the local store as
// canonical YAML.
func (c *Catalog) SaveCommand(r Command) error {
	r.applyDefaults()
	if err := r.Validate(c.cfg.reservedName); err != nil {
		return err
	}
	r.Kind = KindCommand
	data, err := Marshal(r)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirCommands), r.Name, data)
}

// MaterializeCommand copies a command into the local store if needed and
// returns the path an editor should open.
func (c *Catalog) MaterializeCommand(name string) (string, error) {
	r, ok := c.Command(name)
	if !ok {
		return "", fmt.Errorf("fleet: no registered command %q", name)
	}
	if r.Ring != ringLocal() {
		if err := c.SaveCommand(r); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirCommands), r.Name)
}

// RemoveCommand deletes a command from the local store.
func (c *Catalog) RemoveCommand(name string) error {
	return removeEntry(c.nounDir(dirCommands), dirCommands, name)
}

// VerifyCommand checks that a registered command is usable on this host:
// the record validates, its name is not shadowed by something bashy now
// ships, an exec program is present, a download record carries a digest for
// this platform and provisions (network on a cache miss — this is the one
// verb that fetches on purpose), and a script body passes the wired probe.
func (c *Catalog) VerifyCommand(ctx context.Context, name string) Check {
	chk := Check{Kind: KindCommand, Name: name}
	r, ok := c.Command(name)
	if !ok {
		chk.Reason = "not in the ring"
		return chk
	}
	chk.Name = r.Name
	if err := r.Validate(nil); err != nil {
		chk.Reason = err.Error()
		return chk
	}
	if holder, taken := c.shadowedBy(r); taken {
		chk.Reason = "shadowed: " + holder + " now owns this name; rename the record (commands set " + r.Name + " --set name=…)"
		return chk
	}
	switch r.Mode() {
	case atlas.RegisteredExec:
		p, present := r.Executable()
		if !present {
			chk.Reason = "not installed: " + r.Exec[0] + " is not on PATH"
			return chk
		}
		chk.OK, chk.Reason, chk.Detail = true, "exec", p
	case atlas.RegisteredDownload:
		p, err := r.Ensure(ctx)
		if err != nil {
			chk.Reason = err.Error()
			return chk
		}
		chk.OK, chk.Reason, chk.Detail = true, "download (provisioned)", p
	case atlas.RegisteredScript:
		if c.cfg.commandProbe == nil {
			chk.OK, chk.Reason, chk.Warn = true, "script", "no syntax probe wired in this build; the body was not parsed"
			return chk
		}
		reason, ok := c.cfg.commandProbe(r)
		if !ok {
			chk.Reason = "script: " + reason
			return chk
		}
		chk.OK, chk.Reason = true, "script"
	}
	return chk
}

func (c *Catalog) shadowedBy(r Command) (string, bool) {
	if c.cfg.reservedName == nil {
		return "", false
	}
	for _, n := range r.Names() {
		if holder, taken := c.cfg.reservedName(n); taken {
			return holder, true
		}
	}
	return "", false
}
