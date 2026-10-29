package loom

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Legacy push-mirror inventory/removal is kept for safe migration to backups.
// Creating new destructive mirrors is disabled; replica has a separate contract.

// defaultMirrorInterval matches `replica add`: Gitea's own default periodic
// push. sync_on_commit is what keeps a mirror current; the interval is the
// backstop for a commit whose hook push failed.
const defaultMirrorInterval = 8 * time.Hour

// MirrorOptions configures MirrorAdd/List/Rm.
type MirrorOptions struct {
	Primary      string        // source Gitea base URL (the loom that owns the repo)
	Token        string        // source Gitea API token
	Repo         string        // owner/name on the source; required for add and rm
	Remote       string        // target repo URL — must ALREADY exist; never provisioned
	Username     string        // account the mirror push authenticates as on the target
	Password     string        // target push token; read from a file or stdin, never argv
	SyncOnCommit bool          // push on every commit to the source
	Interval     time.Duration // periodic push; 0 = defaultMirrorInterval
}

// readMirrorToken reads the target push token from a file (mode 0600) or, for
// "-", from stdin. There is deliberately no --token/env form: a secret on the
// command line lands in the shell history and in every process listing on the
// host, and one in the environment is readable from /proc and echoed by any
// `env` dump an agent makes.
func readMirrorToken(path string, stdin io.Reader) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("loom: --token-file is required (a path, or - to read the token from stdin); the token is never taken on the command line")
	}
	var r io.Reader
	if path == "-" {
		if stdin == nil {
			return "", fmt.Errorf("loom: --token-file - needs the token on stdin")
		}
		r = stdin
	} else {
		fi, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("loom: token file %q unreadable: %v", path, err)
		}
		if fi.IsDir() {
			return "", fmt.Errorf("loom: token file %q is a directory, want a file", path)
		}
		if err := checkTokenFileMode(path, fi.Mode()); err != nil {
			return "", err
		}
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("loom: token file %q unreadable: %v", path, err)
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return "", fmt.Errorf("loom: reading the token from %s: %v", path, err)
	}
	// First line only: a token file commonly carries a trailing newline, and
	// sometimes a comment line after the secret.
	first, _, _ := strings.Cut(string(b), "\n")
	tok := strings.TrimSpace(first)
	if tok == "" {
		return "", fmt.Errorf("loom: token file %s is empty", path)
	}
	return tok, nil
}

// checkTokenFileMode refuses a group- or world-readable token file: taking the
// token off the command line is pointless if any account on the host can read
// it off disk. Windows file modes are synthesized by the Go runtime (0666 for
// every regular file), so there is nothing to check there.
func checkTokenFileMode(path string, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := mode.Perm(); perm&0o077 != 0 {
		return fmt.Errorf("loom: token file %q is mode %04o — it must not be readable by group or others: chmod 600 %s", path, perm, path)
	}
	return nil
}

// mirrorAddr normalizes a target repo URL for comparison: credentials dropped
// (Gitea answers with the sanitized address, and an operator types the bare
// URL), scheme and host lowercased, trailing "/" and ".git" dropped — so
// `rm --remote https://github.com/acme/app` matches a mirror recorded as
// https://github.com/acme/app.git.
func mirrorAddr(raw string) string {
	s := strings.TrimSpace(raw)
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		u.User = nil
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		s = u.String()
	}
	return strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
}

// validateMirrorRemote accepts an http(s) owner/name URL without credentials.
func validateMirrorRemote(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("loom: --remote is required (the EXISTING target repo URL, e.g. https://github.com/acme/app.git)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("loom: --remote %q must be an http(s) repo URL, e.g. https://github.com/acme/app.git", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("loom: --remote must not carry credentials — pass them with --username and --token-file")
	}
	if _, err := repoSuffix(raw); err != nil {
		return "", fmt.Errorf("loom: --remote %q is not an owner/name repo URL", raw)
	}
	return raw, nil
}

// mirrorHint explains the 401 Gitea answers for a refused push-mirror address.
// A genuinely bad credential lands here too, so the text names both causes.
func mirrorHint(addr string) string {
	return localNetworkHint(addr) + fmt.Sprintf(`

If the target is on the internet the address is not the problem: github.com
stays on loom's allow-list automatically, any OTHER external host must be
named in %s once that env is set. Then the 401
is the credential — check --username and that the token in --token-file can
push to the target repo.`, MirrorAllowedDomainsEnv)
}

// MirrorAdd refuses destructive mirror creation and directs callers to backup.
func MirrorAdd(ctx context.Context, o MirrorOptions) ([]string, error) {
	return nil, fmt.Errorf("loom: mirror add disabled: Gitea force-syncs and prunes; use loom backup add (local or github)")
}

// MirrorList inventories existing destructive mirrors for migration.
func MirrorList(ctx context.Context, o MirrorOptions) ([]string, error) {
	p := newGiteaAPI(o.Primary, o.Token)
	repos, err := listRepos(ctx, p, o.Repo)
	if err != nil {
		return nil, err
	}
	want := ""
	if strings.TrimSpace(o.Remote) != "" {
		want = mirrorAddr(o.Remote)
	}
	var lines []string
	for _, ri := range repos {
		ms, err := listMirrors(ctx, p, ri.FullName)
		if err != nil {
			return lines, err
		}
		for _, m := range ms {
			if want != "" && mirrorAddr(m.RemoteAddress) != want {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s -> %s sync_on_commit=%t interval=%s error=%q",
				ri.FullName, m.RemoteAddress, m.SyncOnCommit, m.Interval, m.LastError))
		}
	}
	return lines, nil
}

// MirrorRm deletes o.Repo's push mirror to o.Remote. Idempotent: a missing
// mirror reports so and succeeds.
func MirrorRm(ctx context.Context, o MirrorOptions) ([]string, error) {
	if strings.TrimSpace(o.Repo) == "" {
		return nil, fmt.Errorf("loom: mirror rm needs --repo owner/name")
	}
	if strings.TrimSpace(o.Remote) == "" {
		return nil, fmt.Errorf("loom: mirror rm needs --remote (the target URL to stop mirroring to)")
	}
	p := newGiteaAPI(o.Primary, o.Token)
	ms, err := listMirrors(ctx, p, o.Repo)
	if err != nil {
		return nil, fmt.Errorf("%s: list mirrors: %w", o.Repo, err)
	}
	want := mirrorAddr(o.Remote)
	var lines []string
	for _, m := range ms {
		if mirrorAddr(m.RemoteAddress) != want {
			continue
		}
		if _, err := p.do(ctx, "DELETE", "/repos/"+o.Repo+"/push_mirrors/"+url.PathEscape(m.RemoteName), nil, nil); err != nil {
			return lines, err
		}
		lines = append(lines, fmt.Sprintf("%s -> %s: removed", o.Repo, m.RemoteAddress))
	}
	if len(lines) == 0 {
		lines = append(lines, fmt.Sprintf("%s: no push mirror to %s", o.Repo, strings.TrimSpace(o.Remote)))
	}
	return lines, nil
}

const mirrorLong = `Legacy Gitea push mirrors: list and remove only.

mirror add is disabled: Gitea FORCE-syncs and prunes, so target-only refs are
DELETED and divergent commits are OVERWRITTEN. Use bashy loom backup add for
never-delete local or GitHub backups. A GitHub backup target ALREADY EXISTS;
provide its credential through a persistent mode-0600 --token-file.

Before migration, use mirror list --repo owner/name, then mirror rm --repo
owner/name --remote URL. rm removes the mirror configuration, not remote data.
Existing mirrors are NOT automatically disabled; remove them before backup add.`

func newMirrorCmd() *cobra.Command {
	var o MirrorOptions
	var data, tokenFile string
	root := &cobra.Command{
		Use:   "mirror",
		Short: "List/remove legacy destructive mirrors; use backup for new targets",
		Long:  mirrorLong,
	}
	// Resolve the SOURCE loom the same way `replica` does: --primary, else the
	// local loom's recorded URL.
	prep := func() error {
		if o.Primary == "" {
			lo := Options{DataDir: data}
			lo.defaults()
			st, err := readState(lo.DataDir)
			if err != nil {
				return fmt.Errorf("loom: no --primary and no local loom state: %w", err)
			}
			o.Primary = st.URL
		}
		return nil
	}
	emit := func(cmd *cobra.Command, lines []string, err error) error {
		for _, l := range lines {
			fmt.Fprintln(cmd.OutOrStdout(), l)
		}
		return err
	}
	// SilenceUsage: a failure here is a runtime one (a forge said no), not a
	// mis-typed command, and cobra's usage block would scroll the explanation
	// — the 401 hint — off the operator's screen.
	mk := func(use, short string, fn func(context.Context, MirrorOptions) ([]string, error)) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, SilenceUsage: true,
			RunE: func(c *cobra.Command, _ []string) error {
				if err := prep(); err != nil {
					return err
				}
				lines, err := fn(c.Context(), o)
				return emit(c, lines, err)
			}}
	}
	add := mk("add", "Disabled: use backup add", func(ctx context.Context, mo MirrorOptions) ([]string, error) {
		return MirrorAdd(ctx, mo)
	})
	add.Long = mirrorLong
	add.RunE = func(c *cobra.Command, _ []string) error {
		_, err := MirrorAdd(c.Context(), o)
		return err
	}

	list := mk("list", "List push mirrors", MirrorList)
	rm := mk("rm", "Remove a push mirror", MirrorRm)

	for _, c := range []*cobra.Command{add, list, rm} {
		c.Flags().StringVar(&o.Primary, "primary", "", "source Gitea base URL (default: the local loom)")
		c.Flags().StringVar(&data, "data", "", "local loom work dir (default ~/.agents/bashy/loom)")
		c.Flags().StringVar(&o.Token, "token", os.Getenv("LOOM_TOKEN"), "source Gitea API token ($LOOM_TOKEN)")
		c.Flags().StringVar(&o.Repo, "repo", "", "source repo owner/name")
		c.Flags().StringVar(&o.Remote, "remote", "", "target repo URL (must already exist)")
	}
	add.Flags().StringVar(&o.Username, "username", "", "account the mirror push authenticates as on the target")
	add.Flags().StringVar(&tokenFile, "token-file", "", "file holding the target push token (mode 0600), or - for stdin")
	add.Flags().BoolVar(&o.SyncOnCommit, "sync-on-commit", false, "push on every commit to the source (recommended; otherwise only --interval and an explicit add sync)")
	add.Flags().DurationVar(&o.Interval, "interval", defaultMirrorInterval, "periodic push interval (0 = the 8h default)")
	root.AddCommand(add, list, rm)
	return root
}
