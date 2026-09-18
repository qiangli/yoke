// Package secrets is the `bashy secret ...` front door to cloudbox's
// AES-encrypted API-key vault. It replaces a plaintext shell rc file of API
// keys/tokens (the historical `~/.novigensrc` pattern) with a single
//
//	eval "$(bashy secret env)"
//
// line: the secrets live encrypted in cloudbox (one place to rotate /
// revoke / audit), and the rc file holds no secret material at all.
//
// It is part of the AgentOS hub (consumed by bashy as `bashy secret`),
// stdlib + cobra only — no new dependency. The on-the-wire contract is
// cloudbox's Bearer /api/v1/secrets surface (secrets:read for env/ls/get,
// secrets:write for set/import/rm).
package secrets

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// NewSecretsCmd returns the `secrets` cobra command tree — the
// host-agnostic entry point a front end mounts (e.g. `bashy secret`).
func NewSecretsCmd() *cobra.Command { return newSecretsCmd() }

func newSecretsCmd() *cobra.Command {
	var cfg Config
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Cloudbox-managed API keys/tokens for your shell (replaces a plaintext rc file)",
		Long: `secrets fetches your API keys/tokens from cloudbox's encrypted vault
instead of keeping them in a plaintext shell rc file. Put your keys in
cloudbox once (bashy secret import ~/.novigensrc), then replace the rc
file body with a single line:

  eval "$(bashy secret env)"

Every new shell pulls the current values over an authenticated, audited,
revocable token; the rc file itself holds no secret material. For resilience
'env' caches the rendered exports on disk (owner-only, mode 0600) and falls
back to that cache when cloudbox is unreachable, so opening a shell never
blocks or breaks — so the decrypted values DO persist there, at the same
0600 protection an rc file would have. Pass --no-cache to keep nothing on
disk (at the cost of the offline fallback).

To enter a secret interactively without exposing it to an agent or shell
history, compose the separate ask command with set:

  bashy ask --name OPENAI_API_KEY --stdout | bashy secret set openai`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.PersistentFlags().StringVar(&cfg.URL, "url", "", "cloudbox base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	cmd.PersistentFlags().StringVar(&cfg.Token, "token", "", "Bearer token (default $BASHY_SECRETS_TOKEN, ~/.config/bashy/secrets-token, or $BASHY_API_KEY)")

	cmd.AddCommand(newEnvCmd(&cfg))
	cmd.AddCommand(newLsCmd(&cfg))
	cmd.AddCommand(newGetCmd(&cfg))
	cmd.AddCommand(newCheckCmd(&cfg))
	cmd.AddCommand(newSetCmd(&cfg))
	cmd.AddCommand(newImportCmd(&cfg))
	cmd.AddCommand(newRmCmd(&cfg))
	return cmd
}

// --- env ---------------------------------------------------------------

func newEnvCmd(cfg *Config) *cobra.Command {
	var refresh, noCache bool
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Resolve a local binding template into 'export NAME=VALUE' lines for eval",
		Long: `Resolve a LOCAL binding template into shell export statements, for:

  eval "$(bashy secret env)"

Each line is LOCAL_NAME=RHS. An RHS beginning with '@' is a cloudbox secret
REFERENCE (@name, or @{name}) resolved from the vault; any other RHS is a
literal value. The template contains NO secrets, so it is safe to commit:

  # ~/.config/bashy/secrets.map
  OPENAI_API_KEY=@host-a-aider-openai
  DEEPSEEK_API_KEY=@host-a-aider-deepseek
  ANTHROPIC_API_KEY=@host-b-opencode-anthropic
  EDITOR=vim                      # a bare literal, not a reference

env fetches each referenced value from cloudbox and emits
'export OPENAI_API_KEY=<real value>'. The TOOL owns the env-var name and
casing; cloudbox only resolves @ref -> value. Only the references you list
are materialized — nothing else in the vault leaks into your shell.

Default template: $XDG_CONFIG_HOME/bashy/secrets.map (else
~/.config/bashy/secrets.map); pass a path to override. The last successful
render is cached (mode 0600) and reused only when cloudbox is unreachable,
so env never breaks shell startup — it always exits 0 and prints a leading
comment on degraded paths.`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(c *cobra.Command, args []string) error {
			tmpl := ""
			if len(args) == 1 {
				tmpl = args[0]
			}
			return runEnv(c.OutOrStdout(), c.ErrOrStderr(), *cfg, tmpl, noCache)
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "fetch fresh (the default; retained for compatibility)")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "do not read or write the cache")
	return cmd
}

// binding is one template line: a local env-var NAME mapped to either a
// cloudbox secret REFERENCE ($ref / ${ref}, resolved from the vault) or a
// LITERAL value (bare RHS, passed through verbatim).
type binding struct {
	local   string
	ref     string // vault reference name, when isRef
	literal string // literal value, when !isRef
	isRef   bool
}

func runEnv(out, errOut io.Writer, cfg Config, tmplPath string, noCache bool) error {
	// env must never break shell startup: on any error fall back to cache,
	// and if even that fails emit a harmless comment and exit 0.
	usingDefault := tmplPath == ""
	if usingDefault {
		tmplPath = defaultTemplatePath()
	}

	// Cache only the default-template render (a custom path is an ad-hoc
	// invocation; caching it under the shared key would poison the default).
	cachePath := ""
	if usingDefault {
		cachePath = cacheFile()
	}
	bindings, terr := readTemplate(tmplPath)
	if terr != nil {
		// No template yet (or unreadable) — guide the user, don't break.
		fmt.Fprintf(errOut, "bashy secret: %v\n", terr)
		fmt.Fprintf(out, "# bashy secret: no binding template (%v)\n# create %s with lines like ENV_NAME=@<cloudbox secret name>\n", terr, tmplPath)
		return nil
	}
	if len(bindings) == 0 {
		fmt.Fprintf(out, "# bashy secret: template %s has no ENV_NAME=ref bindings\n", tmplPath)
		return nil
	}

	client, err := cfg.Resolve()
	if err == nil {
		var items []Item
		items, err = client.List()
		if err == nil {
			rendered, missing := renderEnv(bindings, items)
			for _, m := range missing {
				fmt.Fprintf(errOut, "bashy secret: %q -> %q not found in vault; skipped\n", m.local, m.ref)
			}
			if cachePath != "" && !noCache {
				_ = writeCache(cachePath, rendered)
			}
			_, _ = out.Write(rendered)
			return nil
		}
	}

	// Degraded: try any cache regardless of age (default template only).
	if cachePath != "" && !noCache {
		if data, e := os.ReadFile(cachePath); e == nil {
			fmt.Fprintf(errOut, "bashy secret: cloudbox unreachable (%v); using cached values\n", err)
			fmt.Fprintf(out, "# bashy secret: served from cache (cloudbox unreachable: %v)\n", err)
			_, _ = out.Write(data)
			return nil
		}
	}
	fmt.Fprintf(errOut, "bashy secret: %v\n", err)
	fmt.Fprintf(out, "# bashy secret unavailable: %v\n", err)
	return nil
}

// readTemplate parses a binding template: 'LOCAL_NAME=RHS' lines. A RHS that
// starts with '@' is a cloudbox secret REFERENCE (@name or @{name}) resolved
// from the vault; any other RHS is a LITERAL value passed through verbatim.
// Comments (#) and blank lines are skipped; the local name must be a valid
// env identifier; ref names may contain dashes (e.g. host-app-provider).
func readTemplate(path string) ([]binding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []binding
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		local := strings.TrimSpace(line[:eq])
		if !validName(local) {
			continue
		}
		// Detect the '@' ref sigil on the RAW RHS (before unquoting), so a
		// quoted literal like '@foo' stays a literal.
		raw := strings.TrimSpace(line[eq+1:])
		if strings.HasPrefix(raw, "@") {
			ref := strings.TrimSpace(stripInlineComment(raw))
			ref = strings.TrimPrefix(ref, "@")
			ref = strings.TrimSuffix(strings.TrimPrefix(ref, "{"), "}") // @{name}
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			out = append(out, binding{local: local, ref: ref, isRef: true})
			continue
		}
		out = append(out, binding{local: local, literal: unquote(stripInlineComment(raw)), isRef: false})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// renderEnv resolves each binding against the fetched vault items and emits
// deterministic, safely-quoted export lines. Literal bindings pass through;
// references absent from the vault are returned in `missing` (and omitted)
// rather than failing the whole render.
func renderEnv(bindings []binding, items []Item) (out []byte, missing []binding) {
	byName := make(map[string]string, len(items))
	for _, it := range items {
		byName[it.Name] = it.Value
	}
	sorted := append([]binding(nil), bindings...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].local < sorted[j].local })
	var b strings.Builder
	for _, bd := range sorted {
		val := bd.literal
		if bd.isRef {
			v, ok := byName[bd.ref]
			if !ok {
				missing = append(missing, bd)
				continue
			}
			val = v
		}
		fmt.Fprintf(&b, "export %s=%s\n", bd.local, shellSingleQuote(val))
	}
	return []byte(b.String()), missing
}

// defaultTemplatePath is the XDG-aware default binding template:
// $XDG_CONFIG_HOME/bashy/secrets.map, else ~/.config/bashy/secrets.map.
func defaultTemplatePath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "bashy", "secrets.map")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "secrets.map"
	}
	return filepath.Join(home, ".config", "bashy", "secrets.map")
}

// shellSingleQuote wraps s in single quotes, the only POSIX-safe way to
// quote arbitrary content: 'it'\”s' for an embedded single quote.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- ls ----------------------------------------------------------------

func newLsCmd(cfg *Config) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List secret names (values are never printed)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			items, err := client.List()
			if err != nil {
				return err
			}
			for _, it := range items {
				fmt.Fprintln(c.OutOrStdout(), it.Name)
			}
			return nil
		},
	}
}

// --- get ---------------------------------------------------------------

func newGetCmd(cfg *Config) *cobra.Command {
	return &cobra.Command{
		Use:   "get NAME",
		Short: "Print one secret value (for KEY=$(bashy secret get NAME))",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			val, err := client.Get(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), val)
			return nil
		},
	}
}

// --- set ---------------------------------------------------------------

func newSetCmd(cfg *Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set NAME [VALUE]",
		Short: "Set one secret; with no VALUE, read it from stdin (keeps it out of shell history)",
		Long: `Set one secret in the cloudbox vault. With no VALUE, read it from
stdin so the value stays out of shell history.

To enter a secret interactively on a channel an agent cannot read:

  bashy ask --name OPENAI_API_KEY --stdout | bashy secret set openai`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			var value string
			if len(args) == 2 {
				value = args[1]
			} else {
				v, rerr := readSecretValue(c, args[0])
				if rerr != nil {
					return rerr
				}
				value = v
			}
			if value == "" {
				return emptyValueError(c, args[0])
			}
			if err := client.Put([]Item{{Name: args[0], Value: value}}); err != nil {
				return err
			}
			if err := invalidateCache(); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "set %s\n", args[0])
			return nil
		},
	}
	return cmd
}

// emptyValueError explains an empty value in terms of WHY it was empty.
//
// The bare "refusing to store an empty value" is accurate and useless in the one
// situation that produces it most often. Inside an agentic session, stdin is a
// pipe the harness owns and it is usually already at EOF, so readSecretValue's
// non-terminal branch reads nothing and returns instantly. What the operator sees
// is an immediate, unexplained refusal from the vault — so it reads as a bug in
// `secrets`, or in their token, rather than as "there was no way to ask you".
//
// That misdiagnosis is exactly what sends people to a scratch file in /tmp. Naming
// the command that CAN reach them turns a dead end into a next step.
func emptyValueError(c *cobra.Command, name string) error {
	if f, ok := c.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		return fmt.Errorf("refusing to store an empty value for %s", name)
	}
	return fmt.Errorf(
		"refusing to store an empty value for %s: nothing arrived on stdin.\n"+
			"If you are inside an agent session, stdin belongs to the agent, not to you —\n"+
			"use `bashy ask` to be prompted directly, then store the result:\n"+
			"    bashy ask --name %s --stdout | bashy secret set %s", name, name, name)
}

// readSecretValue gets the value for `secrets set NAME` when none was given on the
// command line.
//
// It used to be a bare io.ReadAll(stdin). At a terminal that BLOCKS WITH NO PROMPT —
// silently, forever, until you happen to know you are supposed to type a secret and
// press Ctrl-D. It is indistinguishable from a hang, and it was reported as one.
//
// Reading from stdin is the RIGHT default (it keeps the key out of shell history,
// which is the entire point of this command). The bug was never the read. It was that
// the command asked for something without saying so.
//
//	A prompt is not decoration. Without it, waiting looks exactly like broken.
//
// At a TTY: prompt on stderr (so `secrets set X < file` and pipelines are unaffected)
// and read one line with echo DISABLED — it is a secret being typed in a terminal.
// Not at a TTY: unchanged, read the stream to EOF.
func readSecretValue(c *cobra.Command, name string) (string, error) {
	in := c.InOrStdin()

	f, ok := in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		b, err := io.ReadAll(in)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}

	fmt.Fprintf(c.ErrOrStderr(), "Value for %s (input hidden): ", name)
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(c.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// --- import ------------------------------------------------------------

func newImportCmd(cfg *Config) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "import [FILE]",
		Short: "Import 'export NAME=VALUE' lines from an rc file (default stdin) into the vault",
		Long: `Parse a shell rc file (or stdin) and upsert every 'export NAME=VALUE'
(or bare 'NAME=VALUE') line into the cloudbox vault. Commented (#) and
blank lines are skipped; surrounding single/double quotes are stripped;
values are stored verbatim (no shell expansion). Idempotent — re-running
overwrites in place.`,
		Args: cobra.RangeArgs(0, 1),
		RunE: func(c *cobra.Command, args []string) error {
			var r io.Reader = c.InOrStdin()
			if len(args) == 1 {
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()
				r = f
			}
			items, err := parseEnvFile(r)
			if err != nil {
				return err
			}
			if len(items) == 0 {
				return fmt.Errorf("no NAME=VALUE assignments found")
			}
			if dryRun {
				for _, it := range items {
					fmt.Fprintln(c.OutOrStdout(), it.Name)
				}
				fmt.Fprintf(c.OutOrStdout(), "# %d secret(s) would be imported (dry run)\n", len(items))
				return nil
			}
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			if err := client.Put(items); err != nil {
				return err
			}
			if err := invalidateCache(); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "imported %d secret(s)\n", len(items))
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the names that would be imported, don't write")
	return cmd
}

// parseEnvFile extracts NAME=VALUE assignments from a shell-style rc file.
func parseEnvFile(r io.Reader) ([]Item, error) {
	var items []Item
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		line = strings.TrimSpace(line)
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:eq])
		if !validName(name) {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		val = stripInlineComment(val)
		val = unquote(val)
		items = append(items, Item{Name: name, Value: val})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// validName accepts a POSIX-ish env var name (letters, digits, underscore;
// not starting with a digit) so we don't try to import malformed lines.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// stripInlineComment drops a trailing " # ..." comment from an UNQUOTED
// value. Quoted values keep '#' verbatim.
func stripInlineComment(s string) string {
	if strings.HasPrefix(s, "'") || strings.HasPrefix(s, `"`) {
		return s
	}
	if i := strings.Index(s, " #"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// unquote strips a single matching pair of surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// --- rm ----------------------------------------------------------------

func newRmCmd(cfg *Config) *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME",
		Short: "Delete one secret from the vault",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			client, err := cfg.Resolve()
			if err != nil {
				return err
			}
			if err := client.Delete(args[0]); err != nil {
				return err
			}
			if err := invalidateCache(); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "deleted %s\n", args[0])
			return nil
		},
	}
}

// --- cache -------------------------------------------------------------

func cacheFile() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "bashy", "secrets-env.sh")
}

func writeCache(path string, data []byte) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func invalidateCache() error {
	path := cacheFile()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("invalidate secrets render cache: %w", err)
	}
	return nil
}
