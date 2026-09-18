package skills

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/redact"
)

// hostScrubber builds the identity scrubber a record is packed through on
// this machine. A variable so tests can pack hermetically (redact.New) while
// production always scrubs against the live host (redact.FromHost).
var hostScrubber = redact.FromHost

// config is assembled from Options by NewSkillsCmd.
type config struct {
	sources  []Source
	statics  map[string]string
	cfgDir   string
	cacheTTL time.Duration
}

type Option func(*config)

// WithSource appends a skill source. Order matters: later sources
// shadow earlier ones (mount embedded first, local last).
func WithSource(s Source) Option { return func(c *config) { c.sources = append(c.sources, s) } }

// WithHostVersion injects a fact only the host binary knows (its own
// version) as a static probe, e.g. ("bashy", "0.9.1").
func WithHostVersion(name, version string) Option {
	return func(c *config) {
		if version != "" {
			c.statics[name] = version
		}
	}
}

// WithConfigDir overrides the ring-1 directory (default
// ~/.config/bashy/skills): the local skill store + the probe cache.
func WithConfigDir(dir string) Option { return func(c *config) { c.cfgDir = dir } }

// DefaultStoreDir is the local skills store — and, because the craft facts,
// the attest ledger and the space graph all live beside the catalog, the ONE
// place that path is resolved:
//
//	$BASHY_SKILLS_DIR       the specific override, most precise
//	$BASHY_HOME/skills      the whole bashy home relocated (tests, sandboxed runs)
//	~/.config/bashy/skills  the default
//
// Every reader and writer of that store calls this. Until 2026-09-12 the
// space graph was WRITTEN here and READ from ~/.bashy/skills, so `graph space`
// reported an empty store on every host that had one — a plausible answer that
// was not true. Two ladders for one store is how that happens; hence one.
//
// An empty return means no home could be determined. Callers must treat it as
// "no store" rather than as a path.
func DefaultStoreDir() string {
	if dir := strings.TrimSpace(os.Getenv("BASHY_SKILLS_DIR")); dir != "" {
		return dir
	}
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "skills")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".config", "bashy", "skills")
	}
	return ""
}

func defaultConfigDir() string { return DefaultStoreDir() }

// NewSkillsCmd builds the `skills` command tree: probe / list / show.
// Bare `skills` behaves like `skills list` (back-compat with the
// pre-cobra dispatcher).
func NewSkillsCmd(opts ...Option) *cobra.Command {
	cfg := &config{statics: map[string]string{}, cacheTTL: 24 * time.Hour}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.cfgDir == "" {
		cfg.cfgDir = defaultConfigDir()
	}

	root := &cobra.Command{
		Use:           "skill",
		Short:         "workspace skills, gated by this host's space-time coordinate",
		Long:          "skills lists, inspects, and probes the tier-2 workspace skills available\non this host. `list` shows only skills applicable here (env-gated via each\nskill's metadata.requires); `probe` prints the host coordinate the gate\nevaluates against.",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(cmd *cobra.Command, args []string) error { return runList(cmd, cfg, false, false) },
	}
	// Mounted under `bashy`, so cobra's generated `completion` verb documents a
	// `skills` binary that does not exist — and it is the one entry in an
	// agent-facing listing that is not something the verb can do.
	root.CompletionOptions.DisableDefaultCmd = true

	var all, asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "list skills applicable at this coordinate (--all: everything, annotated)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runList(cmd, cfg, all, asJSON) },
	}
	list.Flags().BoolVar(&all, "all", false, "include inapplicable skills, with the failing clause")
	list.Flags().BoolVar(&asJSON, "json", false, "machine-readable listing")

	var refresh, probeJSON bool
	probe := &cobra.Command{
		Use:   "probe",
		Short: "print this host's space-time coordinate (probes + context key)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runProbe(cmd, cfg, refresh, probeJSON) },
	}
	probe.Flags().BoolVar(&refresh, "refresh", false, "re-measure lazy probes (drop the cache)")
	probe.Flags().BoolVar(&probeJSON, "json", false, "machine-readable output")

	var ref, showYAML, showJSON bool
	show := &cobra.Command{
		Use:   "show <name>",
		Short: "print a skill's SKILL.md (--reference: its reference.md; --yaml/--json: its record)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, cfg, args[0], ref, showYAML, showJSON)
		},
	}
	show.Flags().BoolVarP(&ref, "reference", "r", false, "print the deep-companion reference.md")
	show.Flags().BoolVar(&showYAML, "yaml", false, "print the skill's record (the lossless projection of its whole folder) as YAML")
	show.Flags().BoolVar(&showJSON, "json", false, "print the skill's record as JSON")

	var force, addJSON bool
	var create createFlags
	add := &cobra.Command{
		Use:   "add <dir>|<file.yaml>|- | <name> --description TEXT",
		Short: "install a skill folder or record into the host-local store (verified admission)",
		Long:  "add installs a skill folder (SKILL.md + optional reference.md/skill.dhnt),\nor a `kind: skill` record (the projection `show --yaml` prints; a file, or -\nfor stdin) rebuilt into the same folder, into the host-local store after a\nverified-admission gate: frontmatter must parse with name+description,\nmetadata.requires must parse, and a skill.dhnt canonical face must be valid\n(transpilable, content-addressed). Inapplicable-here is reported, not\nrefused — a skill may be installed for a tool you have not provisioned yet.\n\nGiven a name and --description instead of a path, add writes a minimal\nSKILL.md — name, description, metadata.requires if given, and the --body\nverbatim — and installs it through the same gate. No template beyond that.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if create.given(cmd) {
				return runCreate(cmd, cfg, args[0], create, force, addJSON)
			}
			return runAdd(cmd, cfg, args[0], force, addJSON)
		},
	}
	add.Flags().BoolVar(&force, "force", false, "replace an already-installed skill of the same name")
	add.Flags().BoolVar(&addJSON, "json", false, "machine-readable admission report")
	create.bind(add)

	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "remove a skill from the local store",
		Long:  "rm removes a skill from the local store.\n\nOnly the local ring is writable. Removing a skill that also exists in a\nlower ring unshadows the original rather than deleting it; a skill that\nexists ONLY in a lower ring is refused — embedded entries are immutable,\nshadow it with `skill add` instead.",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runRm(cmd, cfg, args[0]) },
	}

	var sf setFlags
	set := &cobra.Command{
		Use:   "set <name>",
		Short: "modify a skill's frontmatter, bindings, files, or body",
		Long:  "set edits an installed skill in place.\n\nA skill from the embedded baseline, a shared dir, or the org ring is copied\ninto the host-local store on first modification: the edit shadows the\noriginal rather than mutating a catalog this host does not own. Frontmatter\nkeys the edit does not name are preserved as written, and so is the body\nunless --body replaces it. The result re-runs the admission gate before it\nis saved; a skill that fails it is left as it was.",
		Example: "  bashy skill set port-check --description \"probe a port\" --requires \"os=linux,darwin\"\n" +
			"  bashy skill set port-check --binding check-tests=\"go test ./...\" --rm-binding step-reada\n" +
			"  bashy skill set port-check --file scripts/run.sh=./run.sh --body ./SKILL-body.md",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runSet(cmd, cfg, args[0], sf) },
	}
	sf.bind(set)

	edit := &cobra.Command{
		Use:   "edit <name>",
		Short: "open a skill's SKILL.md in $EDITOR",
		Long:  "edit opens a skill's SKILL.md in $VISUAL or $EDITOR. A skill from a lower\nring is copied into the local store first, so the editor never opens a file\nthat cannot be saved.",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runEdit(cmd, cfg, args[0]) },
	}

	var verifyJSON bool
	verify := &cobra.Command{
		Use:   "verify <name>",
		Short: "dry gate: structural validity + applicability at this coordinate (exit 0 iff both)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runVerify(cmd, cfg, args[0], verifyJSON) },
	}
	verify.Flags().BoolVar(&verifyJSON, "json", false, "machine-readable report")

	var runJSON, adapt bool
	var repairAgent, target string
	var attempts int
	run := &cobra.Command{
		Use:   "run <name>",
		Short: "execute a dhnt skill and attest it (exit 0 iff the contract held within the cap)",
		Long:  "run executes a skill's canonical face through the in-process userland:\ncontract predicates and step primitives resolve their concrete commands from\nSKILL.md metadata (check-*/step-* keys), a static pre-flight audit refuses to\nstart when the declared effect cap cannot cover what the bindings report, and\nevery run emits a re-checkable attestation stored in the host-local store.\nA host that has learned a fixed version of the skill runs it transparently.\nWith --adapt, a failing run asks the repair agent for corrected steps,\nverifies them under the ORIGINAL contract and effect cap, folds the fix into\na guarded environment arm, and saves it to the host overlay — the next run\n(by any agent on this host) reuses it. Command output streams to stderr;\nthe receipt is stdout.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if target != "" && adapt {
				return fmt.Errorf("skills: --target and --adapt do not combine (adapt repairs canonical steps, not dag targets)")
			}
			if target != "" {
				return runTarget(cmd, cfg, args[0], target, runJSON)
			}
			return runRun(cmd, cfg, args[0], runJSON, adapt, repairAgent, attempts)
		},
	}
	run.Flags().BoolVar(&runJSON, "json", false, "machine-readable receipt")
	run.Flags().BoolVar(&adapt, "adapt", false, "self-heal: repair, verify, and fold a fix on failure")
	run.Flags().StringVar(&repairAgent, "repair-agent", "", "headless agent CLI for repair proposals; the prompt is appended as the last argument (e.g. \"claude -p\")")
	run.Flags().IntVar(&attempts, "attempts", 2, "max repair attempts under --adapt")
	run.Flags().StringVar(&target, "target", "", "execute this dag target from the skill's tasks.md (contracted skills stay attested)")

	var learnJSON bool
	learn := &cobra.Command{
		Use:   "learn <dir>",
		Short: "execution-verified admission: add a skill AND prove its contract holds here",
		Long:  "learn is the writeback gate for skills distilled from experience: the same\nstructural admission as `add`, PLUS the skill must actually run and satisfy\nits contract at this coordinate before it stays in the store. A skill that\nfails the run gate is removed again — the library only accretes procedures\nthat have worked here.",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runLearn(cmd, cfg, args[0], learnJSON) },
	}
	learn.Flags().BoolVar(&learnJSON, "json", false, "machine-readable receipt")

	var promoteOut string
	promote := &cobra.Command{
		Use:   "promote <name>",
		Short: "render the human-review bundle for pushing a learned skill upstream (never commits)",
		Args:  cobra.ExactArgs(1),
		RunE:  func(cmd *cobra.Command, args []string) error { return runPromote(cmd, cfg, args[0], promoteOut) },
	}
	promote.Flags().StringVar(&promoteOut, "out", "", "bundle output directory (default ./promote-<name>)")

	var expTo string
	var expUser, expRepo, expForce, expYAML bool
	export := &cobra.Command{
		Use:   "export <name>",
		Short: "install a catalog skill into agent skill directories (user scope, a dir, or --repo)",
		Long:  "export writes a skill folder where agentic tools read skills:\n  --user  ~/.agents/skills (the vendor-neutral standard) plus each DETECTED\n          vendor root (~/.claude/skills, ~/.copilot/skills)\n  --to    any directory (a workspace, a team catalog checkout)\n  --repo  .agents/skills at the repo root (+ .claude/skills if .claude exists);\n          repo writes are explicit-only — your repository, your call\nEvery export carries an ownership marker; re-exports refresh only folders we\nwrote (--force overrides). Content is the standard portable skill folder.\nWith --yaml (--to only) the skill is written as ONE record document,\n<dir>/<name>.yaml, instead of a folder — the shape `add` imports back.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if expYAML {
				return runExportRecord(cmd, cfg, args[0], expTo, expUser, expRepo, expForce)
			}
			return runExport(cmd, cfg, args[0], expTo, expUser, expRepo, expForce)
		},
	}
	export.Flags().StringVar(&expTo, "to", "", "export into this directory")
	export.Flags().BoolVar(&expUser, "user", false, "install at user scope (detected agent roots)")
	export.Flags().BoolVar(&expRepo, "repo", false, "install at repo scope (explicit consent)")
	export.Flags().BoolVar(&expForce, "force", false, "replace folders not exported by us")
	export.Flags().BoolVar(&expYAML, "yaml", false, "write the record (<dir>/<name>.yaml) instead of the folder; --to only")

	// NOTE: the evidence ledger these runs write is READ by `bashy craft`
	// (coreutils/pkg/craft), not here. skills manages the catalog; craft is
	// what running it accumulates into.
	root.AddCommand(list, probe, show, add, rm, set, edit, verify, run, learn, promote, export, newSyncCmd())
	return root
}

// --- add <name> / rm / set / edit ---------------------------------------

// createFlags are the fields `add <name>` writes into a minimal SKILL.md.
type createFlags struct {
	description, requires, body string
}

func (f *createFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&f.description, "description", "", "what the skill is for (creates <name> from flags instead of a path)")
	c.Flags().StringVar(&f.requires, "requires", "", "metadata.requires gate expression, e.g. \"os=linux,darwin\"")
	c.Flags().StringVar(&f.body, "body", "", "file holding the SKILL.md body (- for stdin); none by default")
}

// given tells `add <name> --description …` from `add <path>`: any create
// flag makes the operand a name, whatever it looks like on disk.
func (f *createFlags) given(cmd *cobra.Command) bool {
	return cmd.Flags().Changed("description") || cmd.Flags().Changed("requires") || cmd.Flags().Changed("body")
}

// runCreate is `add <name> --description …`: a minimal SKILL.md rendered into
// a temp folder and admitted through the same gate as any other add.
func runCreate(cmd *cobra.Command, cfg *config, name string, f createFlags, force, asJSON bool) error {
	if cfg.cfgDir == "" {
		return fmt.Errorf("skills: no host-local store directory configured")
	}
	if err := validSkillName(name); err != nil {
		return err
	}
	if strings.TrimSpace(f.description) == "" {
		return fmt.Errorf("skills: creating %q needs --description (a skill without one fails admission)", name)
	}
	var meta map[string]string
	if f.requires != "" {
		if _, err := ParseRequires(f.requires); err != nil {
			return err
		}
		meta = map[string]string{"requires": f.requires}
	}
	var body string
	if f.body != "" {
		data, err := readSource(f.body, cmd.InOrStdin())
		if err != nil {
			return err
		}
		body = string(data)
	}
	md, err := newSkillMD(name, f.description, meta, body)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "skill-add-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, skillMarker), md, 0o644); err != nil {
		return err
	}
	return addFolder(cmd, cfg, dir, force, asJSON)
}

func runRm(cmd *cobra.Command, cfg *config, name string) error {
	if cfg.cfgDir == "" {
		return fmt.Errorf("skills: no host-local store directory configured")
	}
	if err := validSkillName(name); err != nil {
		return err
	}
	sk, _, ok := cfg.catalog().Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	if sk.Ring != RingLocal {
		if sk.Ring == RingEmbedded {
			return fmt.Errorf("skills: %q is not in the local store — embedded entries are immutable; shadow it with `skill add`", name)
		}
		return fmt.Errorf("skills: %q is not in the local store (it comes from the %s ring, which is read-only); shadow it with `skill add`", name, sk.Ring)
	}
	if err := os.RemoveAll(filepath.Join(cfg.cfgDir, name)); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "removed skill %s\n", name)
	if under, _, ok := cfg.catalog().Get(name); ok {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s now resolves to the %s ring's entry (unshadowed, not deleted)\n", name, under.Ring)
	}
	return nil
}

// materialize copies a skill served from a lower ring into the local store
// (copy-on-write: the original is never touched) and returns its local
// folder and the ring it came from. A skill already local is returned as is.
func materialize(cfg *config, name string) (string, Ring, error) {
	if cfg.cfgDir == "" {
		return "", 0, fmt.Errorf("skills: no host-local store directory configured")
	}
	if err := validSkillName(name); err != nil {
		return "", 0, err
	}
	sk, src, ok := cfg.catalog().Get(name)
	if !ok {
		return "", 0, fmt.Errorf("skills: %q not found", name)
	}
	dir := filepath.Join(cfg.cfgDir, name)
	if sk.Ring == RingLocal {
		return dir, sk.Ring, nil
	}
	files, err := src.Files(name)
	if err != nil {
		return "", 0, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", 0, err
	}
	for _, rel := range files {
		data, ok := src.File(name, rel)
		if !ok {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", 0, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			_ = os.RemoveAll(dir) // never leave a half-copied skill shadowing a whole one
			return "", 0, err
		}
	}
	return dir, sk.Ring, nil
}

// setFlags are the edits `set` applies. Every one is optional; the
// frontmatter keys and files an edit does not name are left as they were.
type setFlags struct {
	description, requires, body string
	bindings, rmBindings        []string
	files, rmFiles              []string
}

func (f *setFlags) bind(c *cobra.Command) {
	c.Flags().StringVar(&f.description, "description", "", "replace the description")
	c.Flags().StringVar(&f.requires, "requires", "", "replace metadata.requires (empty string drops it)")
	c.Flags().StringArrayVar(&f.bindings, "binding", nil, "bind a check/step to a command as check-X=<cmd> or step-X=<cmd> (repeatable)")
	c.Flags().StringArrayVar(&f.rmBindings, "rm-binding", nil, "drop a binding key, e.g. check-tests (repeatable)")
	c.Flags().StringArrayVar(&f.files, "file", nil, "add or replace a file in the folder as <path>=<source> (- reads stdin; repeatable)")
	c.Flags().StringArrayVar(&f.rmFiles, "rm-file", nil, "remove a file from the folder (repeatable)")
	c.Flags().StringVar(&f.body, "body", "", "replace the SKILL.md body from a file (- for stdin)")
}

// bindingKey checks a --binding / --rm-binding key: the spellings the
// executor resolves (run.go CheckBindingKey) — bare `check`, `check-X`,
// `step-X`.
func bindingKey(key string) error {
	if key == "check" {
		return nil
	}
	for _, p := range []string{"check-", "step-"} {
		if rest, ok := strings.CutPrefix(key, p); ok && rest != "" {
			return nil
		}
	}
	return fmt.Errorf("skills: binding key %q must be check, check-<name>, or step-<name>", key)
}

func runSet(cmd *cobra.Command, cfg *config, name string, f setFlags) error {
	// Materialise FIRST: the edit lands on a local copy, never on the ring
	// the skill came from.
	local, from, err := materialize(cfg, name)
	if err != nil {
		return err
	}
	if from != RingLocal {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: copied %s from the %s ring into the local store\n", name, from)
	}

	// Stage the edit beside the store, gate it, and only then swap it in,
	// so a rejected edit leaves the installed skill exactly as it was.
	tmp, err := os.MkdirTemp("", "skill-set-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, name)
	if err := copyDir(local, staged); err != nil {
		return err
	}

	// stdin can be read once; two flags asking for it is a usage error.
	stdinUses := 0
	stdin := func() ([]byte, error) {
		stdinUses++
		if stdinUses > 1 {
			return nil, fmt.Errorf("skills: only one of --body/--file may read stdin (-)")
		}
		return io.ReadAll(cmd.InOrStdin())
	}
	readFrom := func(src string) ([]byte, error) {
		if src == "-" {
			return stdin()
		}
		return os.ReadFile(src)
	}

	mdPath := filepath.Join(staged, skillMarker)
	md, err := os.ReadFile(mdPath)
	if err != nil {
		return err
	}
	edit := FrontmatterEdit{SetMeta: map[string]string{}}
	if cmd.Flags().Changed("description") {
		edit.Description = &f.description
	}
	if cmd.Flags().Changed("requires") {
		if f.requires == "" {
			edit.UnsetMeta = append(edit.UnsetMeta, "requires")
		} else {
			if _, err := ParseRequires(f.requires); err != nil {
				return err
			}
			edit.SetMeta["requires"] = f.requires
		}
	}
	for _, b := range f.bindings {
		key, command, ok := strings.Cut(b, "=")
		if !ok || command == "" {
			return fmt.Errorf("skills: --binding needs <key>=<command>, got %q", b)
		}
		if err := bindingKey(key); err != nil {
			return err
		}
		edit.SetMeta[key] = command
	}
	for _, key := range f.rmBindings {
		if err := bindingKey(key); err != nil {
			return err
		}
		edit.UnsetMeta = append(edit.UnsetMeta, key)
	}
	if md, err = WriteFrontmatter(md, edit); err != nil {
		return err
	}
	if f.body != "" {
		body, err := readFrom(f.body)
		if err != nil {
			return err
		}
		if md, err = replaceBody(md, string(body)); err != nil {
			return err
		}
	}
	if err := os.WriteFile(mdPath, md, 0o644); err != nil {
		return err
	}
	for _, spec := range f.files {
		rel, src, ok := strings.Cut(spec, "=")
		if !ok || rel == "" || src == "" {
			return fmt.Errorf("skills: --file needs <path>=<source>, got %q", spec)
		}
		if err := folderPath(rel); err != nil {
			return err
		}
		data, err := readFrom(src)
		if err != nil {
			return err
		}
		target := filepath.Join(staged, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	}
	for _, rel := range f.rmFiles {
		if err := folderPath(rel); err != nil {
			return err
		}
		target := filepath.Join(staged, filepath.FromSlash(rel))
		if _, err := os.Stat(target); err != nil {
			return fmt.Errorf("skills: %s has no file %s", name, rel)
		}
		if err := os.Remove(target); err != nil {
			return err
		}
	}

	sk, err := loadSkillDir(staged)
	if err != nil {
		return err
	}
	ps, _ := cfg.probes(false)
	a := admit(sk, ps)
	if !a.Valid {
		renderAdmission(cmd.OutOrStdout(), a)
		return fmt.Errorf("skills: %q failed the admission gate — not saved", name)
	}
	if _, err := installSkill(staged, cfg.cfgDir, name, true); err != nil {
		return err
	}
	if !a.Applicable {
		fmt.Fprintf(cmd.ErrOrStderr(), "skills: %s saved but not applicable here (%s)\n", name, a.Failing)
	}
	fmt.Fprintln(cmd.OutOrStdout(), name)
	return nil
}

// folderPath admits a --file / --rm-file path: relative, inside the folder,
// and not the SKILL.md that set itself owns.
func folderPath(rel string) error {
	switch {
	case rel == skillMarker:
		return fmt.Errorf("skills: %s is edited through --description/--requires/--binding/--body, not --file", skillMarker)
	case strings.HasPrefix(rel, "/") || !filepath.IsLocal(filepath.FromSlash(rel)):
		return fmt.Errorf("skills: path %q escapes the skill folder", rel)
	}
	return nil
}

// runEdit opens the materialised SKILL.md in $VISUAL/$EDITOR — the one
// place this package runs another program, and only because an editor is
// what the operator asked for.
func runEdit(cmd *cobra.Command, cfg *config, name string) error {
	dir, from, err := materialize(cfg, name)
	if err != nil {
		return err
	}
	if from != RingLocal {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: copied %s from the %s ring into the local store\n", name, from)
	}
	path := filepath.Join(dir, skillMarker)
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		fmt.Fprintln(cmd.OutOrStdout(), path)
		return fmt.Errorf("skills: neither $VISUAL nor $EDITOR is set; the skill is at the path above")
	}
	ed := exec.Command(editor, path)
	ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
	return ed.Run()
}

func runExport(cmd *cobra.Command, cfg *config, name, to string, user, repo, force bool) error {
	sk, src, ok := cfg.catalog().Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	var roots []string
	if to != "" {
		roots = append(roots, to)
	}
	if user {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		roots = append(roots, userExportRoots(home)...)
	}
	if repo {
		roots = append(roots, repoExportRoots(findRepoRoot(mustGetwd()))...)
	}
	if len(roots) == 0 {
		return fmt.Errorf("skills: pick a target: --user, --repo, or --to DIR")
	}
	var firstErr error
	for _, root := range roots {
		dst, err := ExportTo(sk, src, root, force)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "skills: %v\n", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "exported: %s\n", dst)
	}
	return firstErr
}

// runExportRecord is `export --yaml`: the skill as one record document at
// <to>/<name>.yaml. Agent roots (--user/--repo) read FOLDERS, so a record
// there would be a file no tool opens; only --to takes it.
func runExportRecord(cmd *cobra.Command, cfg *config, name, to string, user, repo, force bool) error {
	if to == "" || user || repo {
		return fmt.Errorf("skills: --yaml writes a record document, which agent roots do not read — pair it with --to DIR only")
	}
	sk, src, ok := cfg.catalog().Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	rec, err := RecordFrom(&sk, src, hostScrubber())
	if err != nil {
		return err
	}
	b, err := MarshalRecord(rec)
	if err != nil {
		return err
	}
	dst := filepath.Join(to, name+".yaml")
	if _, err := os.Stat(dst); err == nil && !force {
		return fmt.Errorf("skills: %s exists — use --force to replace", dst)
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "exported: %s\n", dst)
	return nil
}

func (c *config) probes(refresh bool) (*ProbeSet, *FileCache) {
	var fc *FileCache
	var cache Cache = NopCache()
	if c.cfgDir != "" {
		fc = NewFileCache(c.cfgDir, c.cacheTTL)
		if !refresh {
			cache = fc
		}
	}
	ps := DefaultProbes(cache)
	for name, v := range c.statics {
		ps.SetStatic(name, v)
	}
	return ps, fc
}

func (c *config) catalog() *Catalog {
	cat := &Catalog{Sources: c.sources}
	if c.cfgDir != "" {
		cat.Sources = append(cat.Sources, DirSource(c.cfgDir))
	}
	return cat
}

// NewCatalog assembles the catalog and probe set from the same Options
// NewSkillsCmd consumes.
//
// Exported so a second consumer — pkg/craft, which indexes these skills by
// capability — sees EXACTLY the catalog the skills CLI sees, rather than
// reassembling the source list and drifting from it. Two views of one store
// that disagree about what is in it is a bug that only shows up as a skill
// mysteriously not being found.
func NewCatalog(opts ...Option) (*Catalog, *ProbeSet) {
	cfg := &config{statics: map[string]string{}, cacheTTL: 24 * time.Hour}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.cfgDir == "" {
		cfg.cfgDir = defaultConfigDir()
	}
	ps, _ := cfg.probes(false)
	return cfg.catalog(), ps
}

func runList(cmd *cobra.Command, cfg *config, all, asJSON bool) error {
	ps, _ := cfg.probes(false)
	rows, err := cfg.catalog().List(ps)
	if err != nil {
		return err
	}
	if asJSON {
		type row struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			Ring        string `json:"ring"`
			Applicable  bool   `json:"applicable"`
			Failing     string `json:"failing,omitempty"`
			Unchecked   string `json:"unchecked_compat,omitempty"`
			Shadows     bool   `json:"shadows,omitempty"`
			HasDhnt     bool   `json:"dhnt,omitempty"`
			Identity    string `json:"identity,omitempty"`
			Warning     string `json:"warning,omitempty"`
		}
		out := make([]row, 0, len(rows))
		for _, r := range rows {
			if !all && !r.Verdict.Applicable {
				continue
			}
			var id string
			if r.Dhnt.Valid() {
				id = r.Dhnt.Identity
			}
			out = append(out, row{r.Name, r.Description, r.Ring.String(), r.Verdict.Applicable,
				r.Verdict.Failing, r.Verdict.Unchecked, r.Shadows, r.HasDhnt, id, r.Warning})
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(out)
	}
	for _, r := range rows {
		switch {
		case r.Verdict.Applicable:
			fmt.Fprintln(cmd.OutOrStdout(), r.Name)
		case all:
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t# inapplicable: %s\n", r.Name, r.Verdict.Failing)
		}
	}
	return nil
}

func runProbe(cmd *cobra.Command, cfg *config, refresh, asJSON bool) error {
	ps, fc := cfg.probes(refresh)
	vals := ps.Core()
	if fc != nil && !refresh {
		for k, v := range fc.Entries(ps.PathHash()) {
			if v != "" {
				vals[k] = v
			}
		}
	}
	// The reported coordinate is NOT a hash of everything printed below, and
	// the difference is the point. `probes` is a diagnostic — everything this
	// host can observe, including the clock, the terminal, and whichever lazy
	// probes happen to be cached. A COORDINATE is an address knowledge is filed
	// under, so it may only carry what does not churn: hashing the full map
	// produced a key that changed every hour and on the first cache warm-up, so
	// each observation was written to an address nothing would ever read again.
	key := ps.EnvironmentCoordinate()
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Probes     map[string]string `json:"probes"`
			ContextKey string            `json:"context_key"`
		}{vals, key})
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(cmd.OutOrStdout(), "%s=%s\n", k, vals[k])
	}
	fmt.Fprintf(cmd.OutOrStdout(), "context=%s\n", key)
	return nil
}

func runShow(cmd *cobra.Command, cfg *config, name string, ref, asYAML, asJSON bool) error {
	cat := cfg.catalog()
	sk, src, ok := cat.Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	if asYAML || asJSON {
		if ref {
			return fmt.Errorf("skills: --reference does not combine with --yaml/--json (the record carries every file)")
		}
		return showRecord(cmd.OutOrStdout(), sk, src, asJSON)
	}
	rel := "SKILL.md"
	if ref {
		rel = "reference.md"
	}
	body, ok := src.File(name, rel)
	if !ok {
		return fmt.Errorf("skills: %q has no %s", name, rel)
	}
	// stdout stays byte-identical to the skill content; the verdict is a
	// one-line stderr annotation (the hint-engine idiom), so existing
	// consumers parsing stdout are unaffected.
	fmt.Fprint(cmd.OutOrStdout(), string(body))
	if !ref {
		ps, _ := cfg.probes(false)
		v := verdictOf(sk, ps)
		status := "applicable"
		if !v.Applicable {
			status = "inapplicable: " + v.Failing
		}
		dhnt := "absent"
		if sk.Dhnt.Valid() {
			dhnt = sk.Dhnt.Identity[:13] // content-address prefix
		} else if sk.HasDhnt {
			dhnt = "invalid"
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "skills: %s — ring=%s dhnt=%s %s\n", sk.Name, sk.Ring, dhnt, status)
	}
	return nil
}

// showRecord emits the record projection (`show --yaml` / `--json`). The
// Record struct is YAML-tagged by design (record.go: the YAML emit is the
// canonical, hashable form); the JSON face is that same document re-keyed
// through a generic decode so both spellings carry identical key names — and
// since JSON is YAML, either one feeds `add -` back.
func showRecord(w io.Writer, sk Skill, src Source, asJSON bool) error {
	rec, err := RecordFrom(&sk, src, hostScrubber())
	if err != nil {
		return err
	}
	b, err := MarshalRecord(rec)
	if err != nil {
		return err
	}
	if !asJSON {
		_, err = w.Write(b)
		return err
	}
	var generic map[string]any
	if err := yaml.Unmarshal(b, &generic); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(generic)
}

// readSource loads a record from a path, or from r when the path is "-".
func readSource(path string, r io.Reader) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(r)
	}
	return os.ReadFile(path)
}

// isRecordSource tells `add <file.yaml>|-` from `add <dir>`: stdin or a
// regular file is a record, a directory is a folder. Anything else falls
// through to the folder loader, whose "no SKILL.md" error names the path.
func isRecordSource(arg string) bool {
	if arg == "-" {
		return true
	}
	fi, err := os.Stat(arg)
	return err == nil && fi.Mode().IsRegular()
}

// validSkillName rejects a name that would not be one folder in the store.
// The frontmatter name is authored text, and a record's name becomes a path.
func validSkillName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("skills: empty name")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("skills: name %q must not contain a path separator", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("skills: name %q must not start with a dot", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("skills: name %q must not contain %q", name, "..")
	}
	return nil
}

func runAdd(cmd *cobra.Command, cfg *config, arg string, force, asJSON bool) error {
	if strings.Contains(arg, "://") {
		return fmt.Errorf("skills: url sources are not supported yet — pass a local skill directory")
	}
	if cfg.cfgDir == "" {
		return fmt.Errorf("skills: no host-local store directory configured")
	}
	dir := arg
	if isRecordSource(arg) {
		// A record is rebuilt into a folder first, so it walks through the
		// SAME gate as `add <dir>` — a document must not have a softer
		// admission than the folder it stands for.
		data, err := readSource(arg, cmd.InOrStdin())
		if err != nil {
			return err
		}
		rec, err := ParseRecord(data)
		if err != nil {
			return err
		}
		if err := ValidateRecordShareable(rec, hostScrubber()); err != nil {
			return err
		}
		if err := validSkillName(rec.Name); err != nil {
			return err
		}
		tmp, err := os.MkdirTemp("", "skill-add-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = filepath.Join(tmp, rec.Name)
		if err := WriteFolder(rec, dir); err != nil {
			return err
		}
	}
	return addFolder(cmd, cfg, dir, force, asJSON)
}

// addFolder is the admission gate + install for a skill folder on disk.
func addFolder(cmd *cobra.Command, cfg *config, dir string, force, asJSON bool) error {
	sk, err := loadSkillDir(dir)
	if err != nil {
		return err
	}
	ps, _ := cfg.probes(false)
	a := admit(sk, ps)
	if asJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(a); err != nil {
			return err
		}
	} else {
		renderAdmission(cmd.OutOrStdout(), a)
	}
	if !a.Valid {
		return fmt.Errorf("skills: %q failed the admission gate", sk.Name)
	}
	dst, err := installSkill(dir, cfg.cfgDir, sk.Name, force)
	if err != nil {
		return err
	}
	if !asJSON {
		fmt.Fprintf(cmd.OutOrStdout(), "installed: %s\n", dst)
	}
	if !a.Applicable {
		fmt.Fprintf(cmd.ErrOrStderr(), "skills: %s installed but not applicable here (%s)\n", sk.Name, a.Failing)
	}
	return nil
}

func runVerify(cmd *cobra.Command, cfg *config, name string, asJSON bool) error {
	sk, _, ok := cfg.catalog().Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	ps, _ := cfg.probes(false)
	a := admit(sk, ps)
	if asJSON {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(a); err != nil {
			return err
		}
	} else {
		renderAdmission(cmd.OutOrStdout(), a)
	}
	if !a.Valid || !a.Applicable {
		return fmt.Errorf("skills: %q did not verify (valid=%v applicable=%v)", name, a.Valid, a.Applicable)
	}
	return nil
}

func runRun(cmd *cobra.Command, cfg *config, name string, asJSON, adapt bool, repairAgent string, attempts int) error {
	sk, src, ok := cfg.catalog().Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	ps, _ := cfg.probes(false)

	var rec AttestRecord
	var outcome AdaptOutcome
	var err error
	if adapt {
		var complete Completer
		if repairAgent != "" {
			complete = execCompleter(repairAgent)
		}
		rec, outcome, err = adaptiveRun(cfg, sk, src, ps, mustGetwd(), cmd.ErrOrStderr(), complete, attempts)
	} else {
		rec, _, err = runSkill(cfg, sk, src, ps, mustGetwd(), cmd.ErrOrStderr())
	}
	if err != nil && rec.Name == "" {
		return err
	}
	renderReceipt(cmd, rec, string(outcome), asJSON)
	if err != nil {
		return err
	}
	if !rec.Attest.Valid {
		return fmt.Errorf("skills: %q contract not satisfied", name)
	}
	return nil
}

func runTarget(cmd *cobra.Command, cfg *config, name, target string, asJSON bool) error {
	sk, src, ok := cfg.catalog().Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	ps, _ := cfg.probes(false)
	rec, attested, err := runTargetSkill(cfg, sk, src, ps, target, mustGetwd(), cmd.ErrOrStderr())
	if !attested {
		// Executable-but-uncontracted rung: plain result, no receipt.
		if err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
				"skill": name, "target": target, "ok": true,
			})
		}
		fmt.Fprintf(cmd.OutOrStdout(), "skill: %s\ntarget: %s\nok: true\n", name, target)
		return nil
	}
	if err != nil {
		return err
	}
	renderReceipt(cmd, rec, "target:"+target, asJSON)
	if !rec.Attest.Valid {
		return fmt.Errorf("skills: %q contract not satisfied", name)
	}
	return nil
}

func renderReceipt(cmd *cobra.Command, rec AttestRecord, outcome string, asJSON bool) {
	if asJSON {
		out := struct {
			AttestRecord
			Outcome string `json:"outcome,omitempty"`
		}{rec, outcome}
		_ = json.NewEncoder(cmd.OutOrStdout()).Encode(out)
		return
	}
	a := rec.Attest
	fmt.Fprintf(cmd.OutOrStdout(), "skill: %s\ntier: %s\nvalid: %v\n", rec.Name, rec.Tier, a.Valid)
	if outcome != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "outcome: %s\n", outcome)
	}
	if len(a.Passed) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "passed: %s\n", strings.Join(a.Passed, " "))
	}
	if len(a.Failed) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "failed: %s\n", strings.Join(a.Failed, " "))
	}
	var effs []string
	for _, e := range a.Effects {
		effs = append(effs, e.String())
	}
	fmt.Fprintf(cmd.OutOrStdout(), "effects: %s\ncontext: %s\n", strings.Join(effs, " "), rec.ContextKey)
}

func runLearn(cmd *cobra.Command, cfg *config, dir string, asJSON bool) error {
	if cfg.cfgDir == "" {
		return fmt.Errorf("skills: no host-local store directory configured")
	}
	sk, err := loadSkillDir(dir)
	if err != nil {
		return err
	}
	ps, _ := cfg.probes(false)
	a := admit(sk, ps)
	if !a.Valid {
		if !asJSON {
			renderAdmission(cmd.OutOrStdout(), a)
		}
		return fmt.Errorf("skills: %q failed the admission gate", sk.Name)
	}
	if !a.Applicable {
		return fmt.Errorf("skills: %q is not applicable here (%s) — learn requires proving the contract at this coordinate; use `add` to install without the run gate", sk.Name, a.Failing)
	}
	if _, err := installSkill(dir, cfg.cfgDir, sk.Name, false); err != nil {
		return err
	}
	// The run gate: the contract must actually hold here, or the skill
	// comes back out of the store.
	installed, src, ok := cfg.catalog().Get(sk.Name)
	if !ok {
		return fmt.Errorf("skills: %q vanished after install", sk.Name)
	}
	rec, _, err := runSkill(cfg, installed, src, ps, mustGetwd(), cmd.ErrOrStderr())
	if err != nil || !rec.Attest.Valid {
		_ = os.RemoveAll(filepath.Join(cfg.cfgDir, sk.Name))
		if err != nil {
			return fmt.Errorf("skills: %q not learned: %w", sk.Name, err)
		}
		renderReceipt(cmd, rec, "rejected", asJSON)
		return fmt.Errorf("skills: %q not learned — the contract did not hold here (the failed run is attested)", sk.Name)
	}
	renderReceipt(cmd, rec, "learned", asJSON)
	return nil
}

func runPromote(cmd *cobra.Command, cfg *config, name, out string) error {
	sk, src, ok := cfg.catalog().Get(name)
	if !ok {
		return fmt.Errorf("skills: %q not found", name)
	}
	if out == "" {
		out = "promote-" + name
	}
	dir, err := promoteBundle(cfg, sk, src, out)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "bundle: %s\n", dir)
	fmt.Fprintln(cmd.OutOrStdout(), "review PROMOTION.md, then merge through the catalog's normal change process")
	return nil
}

func mustGetwd() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// Advertised is the L1 surface of one applicable skill — what a host
// injects into an agent's first-hop context (progressive disclosure:
// names + one-liners here; full bodies via `skills show`).
type Advertised struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Ring        string `json:"ring,omitempty"`
	Verified    bool   `json:"verified,omitempty"` // carries a valid canonical face (identity + contract)
}

// Applicable returns the skills applicable at this host's coordinate,
// with descriptions truncated to an L1-sized budget. Errors degrade to
// an empty list — first-hop context must never fail on skills.
func Applicable(opts ...Option) []Advertised {
	cfg := &config{statics: map[string]string{}, cacheTTL: 24 * time.Hour}
	for _, o := range opts {
		o(cfg)
	}
	if cfg.cfgDir == "" {
		cfg.cfgDir = defaultConfigDir()
	}
	ps, _ := cfg.probes(false)
	rows, err := cfg.catalog().List(ps)
	if err != nil {
		return nil
	}
	var out []Advertised
	for _, r := range rows {
		if !r.Verdict.Applicable {
			continue
		}
		out = append(out, Advertised{
			Name:        r.Name,
			Description: truncate(r.Description, 160),
			Ring:        r.Ring.String(),
			Verified:    r.Dhnt.Valid(),
		})
	}
	return out
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n-1])) + "…"
}

// ExitCode maps a NewSkillsCmd Execute error to the repo exit
// convention: 2 for usage errors, 1 otherwise, 0 for nil.
// (Defined in source.go over pkg/assetring.)
