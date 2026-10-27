package loom

// Dropping privileges — why loom does this itself.
//
// Gitea refuses to run as root (its own mustNotRunAsRoot), and loom used to
// honour that by refusing too: ErrRefuseRoot, "run bashy as a regular user".
// That is fine on a laptop and wrong on a product satellite. The satellite-local
// outpost runs AS ROOT — it drives podman, systemd and DKS — so when its service
// monitor starts the enabled loom unit, loom inherits euid 0 and exits. Measured
// live on a fresh v0.18.41 satellite (cloudbox docs/s515-1-live-round4.md): loom
// never served on any product satellite at all.
//
// The fix keeps the rule "bashy is all you need": loom drops the privilege
// itself. When it is root on Linux it provisions a dedicated unprivileged system
// account, hands that account its data dir, and starts Gitea under that uid/gid
// (SysProcAttr.Credential) with HOME/USER and app.ini's RUN_USER agreeing. Gitea
// therefore never sees uid 0 and its own guard is satisfied honestly — loom does
// NOT set Gitea's undocumented run-as-root override.
//
// Scope, deliberately narrow:
//
//   - Linux only. A root bashy on macOS still gets ErrRefuseRoot: there is no
//     satellite there, `useradd` does not exist, and inventing a mechanism for a
//     case nobody hits is how you get an untested one. Windows is unchanged
//     (no euid, no refusal to begin with).
//   - Non-root is untouched on every OS — the home-outpost path keeps running
//     Gitea as the invoking user, with no account, no chown and no RUN_USER key.
//   - The front proxy stays in the parent process (it only binds a loopback
//     port and forwards), so only Gitea and the `gitea admin` CLI calls drop.
//
// Two ownership problems the live fault would have hit next, both handled here:
//
//   - The data dir defaults to ~/.agents/bashy/loom, which under a root outpost
//     is /root/.agents/bashy/loom — on the satellite's persistent volume, which
//     is why it must stay there. /root is 0700, so the loom account cannot even
//     TRAVERSE to its own data dir. ensureTraversable adds the search bit (and
//     only the search bit: 0700 -> 0701, never read or write) to each ancestor.
//   - The gitea binary itself lives in root's binmgr cache (~/.cache/bashy/bin),
//     with the same traversal problem.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// DefaultSystemUser is the dedicated unprivileged account loom starts Gitea
// under when it is root. Override with $LOOM_SYSTEM_USER for a host whose
// operator pre-created a differently named account.
const DefaultSystemUser = "loom"

func systemUserName() string {
	if n := strings.TrimSpace(os.Getenv("LOOM_SYSTEM_USER")); n != "" {
		return n
	}
	return DefaultSystemUser
}

// privDropGOOS is runtime.GOOS, injectable so the decision can be tested for
// every platform from one host (the whole point of shouldDropPrivileges being
// pure).
var privDropGOOS = runtime.GOOS

// shouldDropPrivileges reports whether loom must start Gitea under a dedicated
// account instead of refusing to run: only on Linux, only when root.
func shouldDropPrivileges(goos string, uid int) bool {
	return goos == "linux" && uid == 0
}

// privDrop is the credential Gitea is started under. A nil *privDrop means "run
// as the invoking user" — every method below is a no-op on nil, so the non-root
// path needs no branches at the call sites.
type privDrop struct {
	User string // account name; also app.ini RUN_USER and $USER
	UID  int
	GID  int
	Home string // $HOME for Gitea — the data dir, which this account owns
}

// runUser is the name app.ini's RUN_USER must carry, or "" when loom runs as the
// invoking user and Gitea's own default (the current user) is correct.
func (d *privDrop) runUser() string {
	if d == nil {
		return ""
	}
	return d.User
}

// env is the environment Gitea needs to agree with the credential it runs under.
// Gitea resolves "the current user" from $USER before os/user, and writes git's
// global config into $HOME — left pointing at /root both would contradict the
// uid it actually has. Appended last, so it wins over the inherited values.
func (d *privDrop) env() []string {
	if d == nil {
		return nil
	}
	return []string{"HOME=" + d.Home, "USER=" + d.User, "LOGNAME=" + d.User}
}

// ensurePrivDrop resolves the credential for this start: nil when loom runs as
// the invoking user, otherwise the dedicated account (provisioned if absent).
//
// Called late — after the ports are proven free and the gitea binary is in hand
// — so a start that is going to be refused anyway never creates an OS account.
func ensurePrivDrop(ctx context.Context, dataDir string, uid int) (*privDrop, error) {
	if !shouldDropPrivileges(privDropGOOS, uid) {
		return nil, nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("loom: data dir: %w", err)
	}
	return ensureAccount(ctx, systemUserName(), dataDir)
}

// ensureAccount is the account seam — tests inject a fake instead of touching
// the host's /etc/passwd.
var ensureAccount = ensureSystemAccount

// ensureSystemAccount resolves the named account, creating it as a system
// account on first run. Idempotent: an account left behind by an earlier start
// (or pre-created by an operator) is reused as-is.
func ensureSystemAccount(ctx context.Context, name, home string) (*privDrop, error) {
	if u, err := user.Lookup(name); err == nil {
		return dropFromUser(u, home)
	}
	if err := createSystemAccount(ctx, name, home); err != nil {
		return nil, err
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("loom: created account %q but cannot resolve it: %w", name, err)
	}
	return dropFromUser(u, home)
}

func dropFromUser(u *user.User, home string) (*privDrop, error) {
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil, fmt.Errorf("loom: account %q has non-numeric uid %q: %w", u.Username, u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, fmt.Errorf("loom: account %q has non-numeric gid %q: %w", u.Username, u.Gid, err)
	}
	if uid == 0 || gid == 0 {
		return nil, fmt.Errorf("loom: account %q is privileged (uid %d gid %d); loom needs an unprivileged account to run Gitea under", u.Username, uid, gid)
	}
	return &privDrop{User: u.Username, UID: uid, GID: gid, Home: home}, nil
}

// createSystemAccount creates the unprivileged account. There is no bashy/yoke
// helper for this (checked: nothing in yoke or coreutils creates OS accounts),
// so this is the minimal correct mechanism — the distro's own tool, in the two
// spellings a Linux host actually ships:
//
//	shadow-utils:  groupadd --system / useradd --system
//	busybox:       addgroup -S      / adduser -S
//
// A dedicated group of the same name is created first so the gid is ours and not
// whatever the distro's useradd defaults picked. No login shell, no home
// creation: the account's home IS the loom data dir, which already exists and is
// handed over by privDrop.own. Both tools exit non-zero when the name is already
// taken, which is why the caller re-looks-up rather than trusting the exit code.
func createSystemAccount(ctx context.Context, name, home string) error {
	shell := nologinShell()
	groupAttempts := [][]string{
		{"groupadd", "--system", name},
		{"addgroup", "-S", name},
	}
	userAttempts := [][]string{
		{"useradd", "--system", "--gid", name, "--no-create-home", "--home-dir", home, "--shell", shell, name},
		{"adduser", "-S", "-D", "-H", "-h", home, "-s", shell, "-G", name, name},
	}
	var errs []error
	ran := false
	for i, gargs := range groupAttempts {
		if _, err := exec.LookPath(gargs[0]); err != nil {
			continue
		}
		ran = true
		// A pre-existing group is fine; only the user creation below is fatal.
		_ = runQuiet(ctx, gargs)
		if err := runQuiet(ctx, userAttempts[i]); err != nil {
			errs = append(errs, err)
			continue
		}
		return nil
	}
	if !ran {
		return fmt.Errorf("loom: cannot create the unprivileged account %q: neither useradd nor adduser is available; create it yourself (e.g. useradd --system %s) or run bashy as a regular user", name, name)
	}
	return fmt.Errorf("loom: create account %q: %w", name, errors.Join(errs...))
}

func runQuiet(ctx context.Context, args []string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// nologinShell is the first non-login shell this host actually has. /bin/false
// is the last resort: it exists everywhere.
func nologinShell() string {
	for _, s := range []string{"/usr/sbin/nologin", "/sbin/nologin", "/usr/bin/nologin"} {
		if st, err := os.Stat(s); err == nil && !st.IsDir() {
			return s
		}
	}
	return "/bin/false"
}

// own hands the data dir to the account: everything under it is chowned, and
// every ancestor is made traversable. Idempotent and cheap on a warm data dir —
// an entry already owned by the account is skipped, so the walk is stat-only
// after the first start. It must run AFTER the config writers, because app.ini
// and custom/ are written by the root parent.
//
// A root-owned tree left behind by a failed root start (the live fault's own
// leftovers) is exactly what this converges.
func (d *privDrop) own(dataDir string) error {
	if d == nil {
		return nil
	}
	if err := ensureTraversable(dataDir); err != nil {
		return err
	}
	return filepath.WalkDir(dataDir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return d.chown(path)
	})
}

// allowExec makes the managed gitea binary reachable by the account: it lives in
// the root parent's binmgr cache, so the path to it needs the search bit and the
// binary itself needs to be readable + executable by others. Ownership is NOT
// changed — the cache belongs to whoever manages the binaries.
func (d *privDrop) allowExec(bin string) error {
	if d == nil || bin == "" {
		return nil
	}
	if err := ensureTraversable(filepath.Dir(bin)); err != nil {
		return err
	}
	st, err := os.Stat(bin)
	if err != nil {
		return err
	}
	if mode := st.Mode().Perm(); mode&0o005 != 0o005 {
		return os.Chmod(bin, mode|0o005)
	}
	return nil
}

// chown sets ownership unless it is already right (so a warm tree costs one
// lstat per entry and no write). Symlinks are chowned without following, which
// is what Gitea's hook trees want.
func (d *privDrop) chown(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if uid, gid, ok := fileOwner(fi); ok && uid == d.UID && gid == d.GID {
		return nil
	}
	if err := os.Lchown(path, d.UID, d.GID); err != nil {
		return fmt.Errorf("loom: chown %s to %s: %w", path, d.User, err)
	}
	return nil
}

// ensureTraversable adds the others-search bit to dir and every ancestor up to
// the filesystem root, stopping early at the first one that already has it (the
// chain above a world-searchable dir is world-searchable by construction on a
// sane host, and / is 0755 everywhere).
//
// Only +x is ever added — never +r, never +w. /root goes 0700 -> 0701: the loom
// account can walk THROUGH it to the data dir it owns, and still cannot list or
// read anything else in root's home.
func ensureTraversable(dir string) error {
	for p := filepath.Clean(dir); ; p = filepath.Dir(p) {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		mode := st.Mode().Perm()
		if mode&0o001 != 0 {
			return nil // this one and, by construction, the chain above it
		}
		if err := os.Chmod(p, mode|0o001); err != nil {
			return fmt.Errorf("loom: make %s traversable: %w", p, err)
		}
		if parent := filepath.Dir(p); parent == p {
			return nil
		}
	}
}
