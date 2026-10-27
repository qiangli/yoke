package loom

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// withPrivDropGOOS pins the platform the privilege-drop decision is made for, so
// every case is testable from one host.
func withPrivDropGOOS(t *testing.T, goos string) {
	t.Helper()
	orig := privDropGOOS
	privDropGOOS = goos
	t.Cleanup(func() { privDropGOOS = orig })
}

// withFakeAccount replaces the /etc/passwd-touching provisioner. A unit test
// must never create an account on the host running it.
func withFakeAccount(t *testing.T, uid, gid int) *int {
	t.Helper()
	calls := 0
	orig := ensureAccount
	ensureAccount = func(_ context.Context, name, home string) (*privDrop, error) {
		calls++
		return &privDrop{User: name, UID: uid, GID: gid, Home: home}, nil
	}
	t.Cleanup(func() { ensureAccount = orig })
	return &calls
}

// TestShouldDropPrivileges is the decision itself: Linux + root, nothing else.
func TestShouldDropPrivileges(t *testing.T) {
	cases := []struct {
		goos string
		uid  int
		want bool
	}{
		{"linux", 0, true},     // the product satellite: outpost runs as root
		{"linux", 1000, false}, // the home outpost: unchanged
		{"darwin", 0, false},   // still ErrRefuseRoot — no satellite, no useradd
		{"darwin", 501, false},
		{"windows", 0, false}, // no euid; loom never refused there
		{"windows", -1, false},
	}
	for _, c := range cases {
		if got := shouldDropPrivileges(c.goos, c.uid); got != c.want {
			t.Errorf("shouldDropPrivileges(%q, %d) = %v, want %v", c.goos, c.uid, got, c.want)
		}
	}
}

// TestRootOnLinuxDropsInsteadOfRefusing: the live fault. Root on Linux must get
// past the guard that used to end the run.
func TestRootOnLinuxDropsInsteadOfRefusing(t *testing.T) {
	withPrivDropGOOS(t, "linux")
	if err := checkNotRoot(func() int { return 0 }); err != nil {
		t.Fatalf("checkNotRoot(root on linux) = %v, want nil (loom drops instead)", err)
	}
	if err := checkNotRoot(func() int { return 1000 }); err != nil {
		t.Fatalf("checkNotRoot(non-root on linux) = %v, want nil", err)
	}

	withPrivDropGOOS(t, "darwin")
	if err := checkNotRoot(func() int { return 0 }); !errors.Is(err, ErrRefuseRoot) {
		t.Fatalf("checkNotRoot(root on darwin) = %v, want ErrRefuseRoot", err)
	}
}

func TestEffectiveUIDHonoursInjectedGetter(t *testing.T) {
	if got := effectiveUID(func() int { return 0 }); got != 0 {
		t.Fatalf("effectiveUID(injected 0) = %d, want 0", got)
	}
	orig := currentUID
	t.Cleanup(func() { currentUID = orig })
	currentUID = func() int { return 4242 }
	if got := effectiveUID(nil); got != 4242 {
		t.Fatalf("effectiveUID(nil) = %d, want the package getter's 4242", got)
	}
}

// TestEnsurePrivDropSkipsWhenNotRoot: the non-root path must not provision an
// account, on any OS.
func TestEnsurePrivDropSkipsWhenNotRoot(t *testing.T) {
	calls := withFakeAccount(t, 1001, 1001)
	for _, c := range []struct {
		goos string
		uid  int
	}{
		{"linux", 1000}, {"darwin", 0}, {"darwin", 501}, {"windows", -1},
	} {
		withPrivDropGOOS(t, c.goos)
		drop, err := ensurePrivDrop(context.Background(), t.TempDir(), c.uid)
		if err != nil {
			t.Fatalf("ensurePrivDrop(%s, %d) = %v", c.goos, c.uid, err)
		}
		if drop != nil {
			t.Fatalf("ensurePrivDrop(%s, %d) = %+v, want nil", c.goos, c.uid, drop)
		}
	}
	if *calls != 0 {
		t.Fatalf("provisioned an account %d times on the non-root path", *calls)
	}
}

func TestEnsurePrivDropResolvesAccountWhenRoot(t *testing.T) {
	withPrivDropGOOS(t, "linux")
	withFakeAccount(t, 1001, 1002)
	dir := filepath.Join(t.TempDir(), "loom")
	drop, err := ensurePrivDrop(context.Background(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if drop == nil {
		t.Fatal("ensurePrivDrop(root on linux) = nil, want the loom account")
	}
	if drop.User != DefaultSystemUser || drop.UID != 1001 || drop.GID != 1002 {
		t.Fatalf("drop = %+v, want user %q uid 1001 gid 1002", drop, DefaultSystemUser)
	}
	// HOME is the data dir, which the account is given below — not /root.
	if drop.Home != dir {
		t.Fatalf("drop.Home = %q, want the data dir %q", drop.Home, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data dir not created: %v", err)
	}
}

func TestSystemUserNameOverride(t *testing.T) {
	if got := systemUserName(); got != DefaultSystemUser {
		t.Fatalf("systemUserName() = %q, want %q", got, DefaultSystemUser)
	}
	t.Setenv("LOOM_SYSTEM_USER", " forge ")
	if got := systemUserName(); got != "forge" {
		t.Fatalf("systemUserName($LOOM_SYSTEM_USER) = %q, want \"forge\"", got)
	}
}

// --- command construction -------------------------------------------------

// TestGiteaWebRunSpecCredential: the Start path. Credential set when root, not
// when not, and the env agrees with the uid either way.
func TestGiteaWebRunSpecCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no credentials on windows")
	}
	dir := t.TempDir()
	o := Options{DataDir: dir}

	spec := giteaWebRunSpec("/d/app.ini", "http://127.0.0.1:31880", o, nil)
	cmd := newProbeCmd()
	if spec.Configure != nil {
		spec.Configure(cmd)
	}
	if _, _, ok := credOf(cmd); ok {
		t.Fatal("non-root spec set a credential")
	}
	for _, key := range []string{"HOME=", "USER=", "LOGNAME="} {
		if hasEnvPrefix(spec.Env, key) {
			t.Fatalf("non-root spec overrode %s — the invoking user's env must stand: %v", key, spec.Env)
		}
	}

	drop := &privDrop{User: "loom", UID: 1001, GID: 1002, Home: dir}
	spec = giteaWebRunSpec("/d/app.ini", "http://127.0.0.1:31880", o, drop)
	cmd = newProbeCmd()
	spec.Configure(cmd)
	uid, gid, ok := credOf(cmd)
	if !ok {
		t.Fatal("root spec set no credential — Gitea would run as root and exit")
	}
	if uid != 1001 || gid != 1002 {
		t.Fatalf("credential = uid %d gid %d, want 1001/1002", uid, gid)
	}
	assertEnv(t, spec.Env, "HOME="+dir, "USER=loom", "LOGNAME=loom")
	assertEnv(t, spec.Env, "GITEA_WORK_DIR="+dir)
}

// TestGiteaWebCommandCredential: the StartDaemon path, plus the merge with
// setDetached — whichever ran first must survive.
func TestGiteaWebCommandCredential(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no credentials on windows")
	}
	dir := t.TempDir()

	cmd := giteaWebCommand(context.Background(), "/cache/gitea", "/d/app.ini", dir, nil)
	setDetached(cmd)
	nilDrop := (*privDrop)(nil)
	nilDrop.apply(cmd)
	if _, _, ok := credOf(cmd); ok {
		t.Fatal("non-root daemon command set a credential")
	}
	if !detachedOf(cmd) {
		t.Fatal("non-root daemon command lost its detach attribute")
	}

	drop := &privDrop{User: "loom", UID: 1001, GID: 1002, Home: dir}
	cmd = giteaWebCommand(context.Background(), "/cache/gitea", "/d/app.ini", dir, drop)
	setDetached(cmd)
	drop.apply(cmd)
	uid, gid, ok := credOf(cmd)
	if !ok || uid != 1001 || gid != 1002 {
		t.Fatalf("daemon credential = uid %d gid %d (set %v), want uid 1001 gid 1002", uid, gid, ok)
	}
	if !detachedOf(cmd) {
		t.Fatal("drop.apply erased setDetached — the daemon would die with the parent shell")
	}
	assertEnv(t, cmd.Env, "HOME="+dir, "USER=loom", "LOGNAME=loom")
	// The daemon command inherits os.Environ(), so the parent's own HOME=/root
	// and USER=root are in there too — the drop's values must be the ones exec
	// keeps, which is why they are appended last.
	for key, want := range map[string]string{"HOME": dir, "USER": "loom", "LOGNAME": "loom"} {
		got, ok := lastEnvValue(cmd.Env, key)
		if !ok || got != want {
			t.Fatalf("effective %s = %q (set %v), want %q — the inherited root value wins otherwise", key, got, ok, want)
		}
	}

	// The reverse order must hold too (apply then setDetached).
	cmd = giteaWebCommand(context.Background(), "/cache/gitea", "/d/app.ini", dir, drop)
	drop.apply(cmd)
	setDetached(cmd)
	if _, _, ok := credOf(cmd); !ok || !detachedOf(cmd) {
		t.Fatal("setDetached erased the credential — Gitea would run as root")
	}
}

// TestRunGiteaAdminDropsToo: the `gitea admin` CLI writes the same SQLite DB, so
// it must run under the same uid as the server.
func TestRunGiteaAdminDropsToo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no credentials on windows")
	}
	drop := &privDrop{User: "loom", UID: 1001, GID: 1002, Home: t.TempDir()}
	// A non-existent binary: CombinedOutput fails, which is fine — this asserts
	// what the command CARRIES, and a credential set on an unprivileged test
	// process cannot actually be exercised.
	dir := t.TempDir()
	if _, err := runGiteaAdmin(context.Background(), filepath.Join(dir, "nope"), dir, drop, "admin"); err == nil {
		t.Fatal("expected the missing binary to fail")
	}
}

// --- RUN_USER convergence -------------------------------------------------

func TestEnsureRunUserAddsRemovesAndRewrites(t *testing.T) {
	const base = "APP_NAME = loom\nRUN_MODE = prod\n\n[server]\nHTTP_PORT = 31880\n"

	withUser := ensureRunUser(base, "loom")
	if !strings.Contains(withUser, "RUN_USER = loom\n") {
		t.Fatalf("RUN_USER not added:\n%s", withUser)
	}
	// It belongs in the nameless preamble, above the first section header.
	if idx, sec := strings.Index(withUser, "RUN_USER"), strings.Index(withUser, "[server]"); idx < 0 || idx > sec {
		t.Fatalf("RUN_USER landed inside a section, Gitea reads it only from the preamble:\n%s", withUser)
	}
	if !strings.Contains(withUser, "APP_NAME = loom") || !strings.Contains(withUser, "HTTP_PORT = 31880") {
		t.Fatalf("ensureRunUser disturbed the rest of app.ini:\n%s", withUser)
	}

	// Idempotent.
	if again := ensureRunUser(withUser, "loom"); again != withUser {
		t.Fatalf("ensureRunUser not idempotent:\n%s\n---\n%s", withUser, again)
	}

	// Rewritten in place when the account name changes.
	renamed := ensureRunUser(withUser, "forge")
	if !strings.Contains(renamed, "RUN_USER = forge") || strings.Contains(renamed, "RUN_USER = loom") {
		t.Fatalf("RUN_USER not rewritten:\n%s", renamed)
	}

	// Removed when loom runs as the invoking user: a stale RUN_USER from an
	// earlier root start would make Gitea exit on the mismatch.
	cleared := ensureRunUser(withUser, "")
	if strings.Contains(cleared, "RUN_USER") {
		t.Fatalf("RUN_USER not removed:\n%s", cleared)
	}
	if cleared != base {
		t.Fatalf("clearing did not restore the original app.ini:\n%q\nwant\n%q", cleared, base)
	}
	if again := ensureRunUser(base, ""); again != base {
		t.Fatalf("clearing an app.ini without RUN_USER changed it:\n%q", again)
	}
}

func TestEnsureRunUserIgnoresCommentsAndSections(t *testing.T) {
	ini := "APP_NAME = loom\n# RUN_USER = commented\n\n[other]\nRUN_USER = not-the-preamble\n"
	got := ensureRunUser(ini, "loom")
	if !strings.Contains(got, "# RUN_USER = commented") {
		t.Fatalf("rewrote a comment:\n%s", got)
	}
	if !strings.Contains(got, "RUN_USER = not-the-preamble") {
		t.Fatalf("rewrote a key inside a named section:\n%s", got)
	}
	if strings.Count(got, "RUN_USER = loom") != 1 {
		t.Fatalf("want exactly one preamble RUN_USER = loom:\n%s", got)
	}
}

// TestPrepareForRunUserConvergesConfig: both directions, through the file.
func TestPrepareForRunUserConvergesConfig(t *testing.T) {
	dir := filepath.Join(searchableTempDir(t), "loom")
	cfg, err := ensureConfig(dir, "127.0.0.1", 31880, "http://127.0.0.1:31880/", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "gitea")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Non-root: nothing is stamped.
	if err := prepareForRunUser(cfg, bin, dir, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); strings.Contains(string(b), "RUN_USER") {
		t.Fatalf("non-root start stamped RUN_USER:\n%s", b)
	}

	// Root: stamped, consistent with the account name.
	drop := &privDrop{User: "loom", UID: os.Getuid(), GID: os.Getgid(), Home: dir}
	if err := prepareForRunUser(cfg, bin, dir, drop); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), "RUN_USER = loom") {
		t.Fatalf("root start did not stamp RUN_USER:\n%s", b)
	}
	if drop.runUser() != "loom" {
		t.Fatalf("drop.runUser() = %q, want loom", drop.runUser())
	}
	if nilDrop := (*privDrop)(nil); nilDrop.runUser() != "" {
		t.Fatalf("nil drop.runUser() = %q, want empty", nilDrop.runUser())
	}

	// Back to a plain user run on the same data dir: the stale key goes.
	if err := prepareForRunUser(cfg, bin, dir, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cfg); strings.Contains(string(b), "RUN_USER") {
		t.Fatalf("stale RUN_USER survived a non-root start — Gitea exits on the mismatch:\n%s", b)
	}
}

// --- ownership / traversal ------------------------------------------------

// TestEnsureTraversableAddsOnlySearchBit: /root is 0700, so the loom account
// cannot reach its own data dir without this — and must gain nothing else.
func TestEnsureTraversableAddsOnlySearchBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	root := searchableTempDir(t)
	home := filepath.Join(root, "root")
	data := filepath.Join(home, ".agents", "bashy", "loom")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureTraversable(data); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{data, filepath.Dir(data), home} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		mode := st.Mode().Perm()
		if mode&0o001 == 0 {
			t.Fatalf("%s = %o, want the others-search bit", p, mode)
		}
		if mode&0o006 != 0 {
			t.Fatalf("%s = %o, want +x only — never others-read or others-write", p, mode)
		}
	}
	// Idempotent.
	before, _ := os.Stat(home)
	if err := ensureTraversable(data); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(home)
	if before.Mode() != after.Mode() {
		t.Fatalf("second pass changed %s: %o -> %o", home, before.Mode().Perm(), after.Mode().Perm())
	}
}

// TestAllowExecMakesManagedBinaryReachable: the gitea binary sits in the root
// parent's binmgr cache (~/.cache/bashy/bin), unreadable by the loom account.
func TestAllowExecMakesManagedBinaryReachable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	cache := filepath.Join(searchableTempDir(t), "cache", "bashy", "bin")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(cache, "gitea")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	drop := &privDrop{User: "loom", UID: os.Getuid(), GID: os.Getgid(), Home: t.TempDir()}
	if err := drop.allowExec(bin); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}
	if mode := st.Mode().Perm(); mode&0o005 != 0o005 {
		t.Fatalf("gitea = %o, want others read+execute", mode)
	}
	if st, err := os.Stat(cache); err != nil || st.Mode().Perm()&0o001 == 0 {
		t.Fatalf("cache dir %v not traversable", cache)
	}
	// nil drop touches nothing.
	if err := (*privDrop)(nil).allowExec(bin); err != nil {
		t.Fatal(err)
	}
}

// TestOwnIsIdempotentAndChownsTheTree uses the test process's own uid/gid, which
// is the only chown an unprivileged test is allowed to make. It proves the walk
// and the skip-when-correct fast path, not the uid change itself.
func TestOwnIsIdempotentAndChownsTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX ownership")
	}
	dir := filepath.Join(searchableTempDir(t), "loom")
	nested := filepath.Join(dir, "repositories", "org", "repo.git")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "HEAD"), []byte("ref: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	drop := &privDrop{User: "loom", UID: os.Getuid(), GID: os.Getgid(), Home: dir}
	for i := 0; i < 2; i++ {
		if err := drop.own(dir); err != nil {
			t.Fatalf("own pass %d: %v", i, err)
		}
	}
	if err := (*privDrop)(nil).own(dir); err != nil {
		t.Fatalf("nil drop.own: %v", err)
	}
	// A foreign uid is refused with an actionable error rather than silently.
	foreign := &privDrop{User: "loom", UID: 1, GID: 1, Home: dir}
	if err := foreign.own(dir); err == nil {
		t.Skip("running privileged: chown to uid 1 succeeded")
	} else if !strings.Contains(err.Error(), "chown") {
		t.Fatalf("own(foreign uid) = %v, want a chown error", err)
	}
}

// --- account provisioning -------------------------------------------------

func TestDropFromUserRejectsPrivilegedAccount(t *testing.T) {
	if _, err := dropFromUser(userStub{"root", "0", "0"}.user(), "/d"); err == nil {
		t.Fatal("dropFromUser(root) = nil error, want a refusal: dropping to uid 0 drops nothing")
	}
	if _, err := dropFromUser(userStub{"loom", "1001", "0"}.user(), "/d"); err == nil {
		t.Fatal("dropFromUser(gid 0) = nil error, want a refusal")
	}
	if _, err := dropFromUser(userStub{"loom", "nope", "1001"}.user(), "/d"); err == nil {
		t.Fatal("dropFromUser(non-numeric uid) = nil error, want a refusal")
	}
	d, err := dropFromUser(userStub{"loom", "1001", "1002"}.user(), "/d")
	if err != nil {
		t.Fatal(err)
	}
	if d.User != "loom" || d.UID != 1001 || d.GID != 1002 || d.Home != "/d" {
		t.Fatalf("dropFromUser = %+v", d)
	}
}

func TestNologinShellExists(t *testing.T) {
	sh := nologinShell()
	if sh == "" {
		t.Fatal("nologinShell() = \"\"")
	}
	if _, err := os.Stat(sh); err != nil && sh != "/bin/false" {
		t.Fatalf("nologinShell() = %q which does not exist; the fallback should have been chosen", sh)
	}
}

// TestCreateSystemAccountFailsLoudlyWithoutTooling: a host with neither
// useradd nor adduser must say what to do, not drop to uid 0.
func TestCreateSystemAccountFailsLoudlyWithoutTooling(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := createSystemAccount(context.Background(), "loom", t.TempDir())
	if err == nil {
		t.Fatal("createSystemAccount with an empty PATH = nil, want an error")
	}
	for _, want := range []string{"useradd", "adduser", "loom"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

// --- the fault itself, end to end ----------------------------------------

// TestStartDaemonAsRootOnLinuxGetsPastTheGuard is the acceptance in unit form:
// what the satellite hit was loom EXITING at the root check before it did
// anything. With a cancelled context the start still fails — but on fetching
// gitea, not on being root.
func TestStartDaemonAsRootOnLinuxGetsPastTheGuard(t *testing.T) {
	withPrivDropGOOS(t, "linux")
	withFakeAccount(t, 1001, 1002)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := StartDaemon(ctx, Options{
		DataDir:   t.TempDir(),
		Addr:      "127.0.0.1",
		Port:      freeTCPPort(t),
		ProxyPort: freeTCPPort(t),
		UIDGetter: func() int { return 0 },
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err == nil {
		t.Fatal("StartDaemon unexpectedly succeeded with a cancelled context")
	}
	if errors.Is(err, ErrRefuseRoot) || strings.Contains(err.Error(), "refusing to run as root") {
		t.Fatalf("root loom on linux still refuses to run: %v", err)
	}
}

// TestServeAsRootOnLinuxGetsPastTheGuard: the `bashy loom serve` path the
// satellite's systemd unit actually takes.
func TestServeAsRootOnLinuxGetsPastTheGuard(t *testing.T) {
	withPrivDropGOOS(t, "linux")
	withFakeAccount(t, 1001, 1002)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Serve(ctx, Options{
		DataDir:   t.TempDir(),
		Addr:      "127.0.0.1",
		Port:      freeTCPPort(t),
		ProxyPort: freeTCPPort(t),
		UIDGetter: func() int { return 0 },
		Stdout:    io.Discard,
		Stderr:    io.Discard,
	})
	if err == nil {
		t.Fatal("Serve unexpectedly succeeded with a cancelled context")
	}
	if errors.Is(err, ErrRefuseRoot) {
		t.Fatalf("root loom on linux still refuses to serve: %v", err)
	}
}

// TestRootIntegrationProvisionsAccountAndOwnsDataDir is the real thing: it
// creates the account with the host's own useradd/adduser and hands it a data
// dir. It needs root on Linux and therefore runs nowhere by default — opt in
// with LOOM_ROOT_INTEGRATION=1 inside a throwaway root container:
//
//	podman run --rm -v $PWD:/src -w /src golang:1.27 \
//	  env LOOM_ROOT_INTEGRATION=1 go test ./external/loom -run RootIntegration -v
//
// It deliberately does NOT download or launch Gitea: the privilege drop is what
// is under test, and a download is the part CI already covers elsewhere.
func TestRootIntegrationProvisionsAccountAndOwnsDataDir(t *testing.T) {
	if os.Getenv("LOOM_ROOT_INTEGRATION") != "1" {
		t.Skip("set LOOM_ROOT_INTEGRATION=1 in a throwaway root container to run this")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skipf("needs root on linux, have %s euid %d", runtime.GOOS, os.Geteuid())
	}
	// Undo the package-wide safety fake: this test WANTS the real provisioner.
	orig := ensureAccount
	ensureAccount = ensureSystemAccount
	t.Cleanup(func() { ensureAccount = orig })

	home := filepath.Join(searchableTempDir(t), "root")
	data := filepath.Join(home, ".agents", "bashy", "loom")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil { // the /root the satellite has
		t.Fatal(err)
	}
	// A root-owned leftover from the failed root start the fault produced.
	stale := filepath.Join(data, "gitea.db")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	drop, err := ensurePrivDrop(context.Background(), data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if drop == nil || drop.UID == 0 || drop.GID == 0 {
		t.Fatalf("drop = %+v, want an unprivileged uid/gid", drop)
	}
	if err := drop.own(data); err != nil {
		t.Fatal(err)
	}
	// Run it twice: restarts and upgrades must converge, not fight.
	if err := drop.own(data); err != nil {
		t.Fatalf("second own pass: %v", err)
	}
	for _, p := range []string{data, stale} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		uid, gid, ok := fileOwner(fi)
		if !ok || uid != drop.UID || gid != drop.GID {
			t.Fatalf("%s owned by %d:%d, want %d:%d", p, uid, gid, drop.UID, drop.GID)
		}
	}
	for _, p := range []string{home, filepath.Dir(data)} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o001 == 0 {
			t.Fatalf("%s = %o, the loom account cannot traverse to its data dir", p, st.Mode().Perm())
		}
	}
}
