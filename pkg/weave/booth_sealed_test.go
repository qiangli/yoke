package weave

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/external/podman"
)

func sealedTestSpec(t *testing.T) boothSealedSpec {
	t.Helper()
	container, proxy, network := boothSealedNames("/q/dir", 7)
	return boothSealedSpec{
		Run:           7,
		Container:     container,
		Proxy:         proxy,
		Network:       network,
		EgressNetwork: "podman",
		Image:         "localhost/agent-image:1",
		ProxyImage:    boothSealedDefaultProxyImage,
		ProxyAddr:     "10.89.0.2:3128",
		Workspace:     "/booths/ws-7",
		Env:           []string{"AGENT_A_API_KEY=sk-very-secret", "TERM=xterm"},
		Argv:          []string{"agent-a", "--cwd", boothSealedWorkdir, "-p", "do it"},
		Allow:         []string{"api.provider.test:443", "host.containers.internal:3000"},
		TTY:           true,
	}
}

func TestSealedCreateArgsMountOnlyWorkspace(t *testing.T) {
	spec := sealedTestSpec(t)
	args := boothSealedCreateArgs(spec)
	home, _ := os.UserHomeDir()
	var mounts []string
	for i, a := range args {
		if home != "" && strings.Contains(a, home) {
			t.Errorf("argv leaks the host home directory: %q", a)
		}
		if a == "-v" || a == "--volume" || a == "--mount" {
			mounts = append(mounts, args[i+1])
		}
		if strings.HasPrefix(a, "--volume=") || strings.HasPrefix(a, "--mount=") || strings.HasPrefix(a, "-v=") {
			mounts = append(mounts, a)
		}
		if strings.Contains(a, "sk-very-secret") {
			t.Errorf("secret env value in podman argv: %q", a)
		}
		if a == "--env-host" || strings.HasPrefix(a, "--env-host=") {
			t.Errorf("sealed create must not inherit the host env: %v", args)
		}
	}
	if want := []string{"/booths/ws-7:" + boothSealedWorkdir + ":rw"}; !reflect.DeepEqual(mounts, want) {
		t.Fatalf("mounts = %v, want %v", mounts, want)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--network=" + spec.Network,
		"--http-proxy=false",
		"--pull=never",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--workdir=" + boothSealedWorkdir,
		"--env=HOME=" + boothSealedHome,
		"--env=HTTPS_PROXY=http://10.89.0.2:3128",
		"--env=AGENT_A_API_KEY",
		"--env=TERM",
		"--tty",
		"--interactive",
		"--name=" + spec.Container,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("create args missing %q: %s", want, joined)
		}
	}
	tail := args[len(args)-len(spec.Argv)-1:]
	if tail[0] != spec.Image || !reflect.DeepEqual(tail[1:], spec.Argv) {
		t.Fatalf("image+argv tail = %v", tail)
	}
}

func TestSealedPodmanEnvCarriesValuesOutOfArgv(t *testing.T) {
	spec := sealedTestSpec(t)
	env := boothSealedPodmanEnv([]string{"HOME=/host/home", "PATH=/usr/bin", "AGENT_A_API_KEY=stale"}, spec.Env)
	got := strings.Join(env, "\n")
	for _, want := range []string{"HOME=/host/home", "PATH=/usr/bin", "AGENT_A_API_KEY=sk-very-secret", "TERM=xterm"} {
		if !strings.Contains(got, want) {
			t.Errorf("podman env missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "stale") {
		t.Fatalf("agent env value did not override the launcher's: %s", got)
	}
}

func TestSealedNetworkIsInternal(t *testing.T) {
	spec := sealedTestSpec(t)
	got := boothSealedNetworkArgs(spec)
	want := []string{"network", "create", "--internal", "--label=bashy.sealed=1", spec.Network}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("network args = %v, want %v", got, want)
	}
}

func TestSealedProxyArgsBridgeBothNetworks(t *testing.T) {
	spec := sealedTestSpec(t)
	args := boothSealedProxyArgs(spec)
	joined := strings.Join(args, " ")
	for _, want := range []string{"run", "--detach", "--name=" + spec.Proxy, "--network=podman", "--network=" + spec.Network, "--env=BASHY_SEALED_PROXY_CONF", "--env=BASHY_SEALED_PROXY_FILTER", spec.ProxyImage} {
		if !strings.Contains(joined, want) {
			t.Errorf("proxy args missing %q: %s", want, joined)
		}
	}
	for _, a := range args {
		if a == "-v" || strings.HasPrefix(a, "--volume") || strings.HasPrefix(a, "--mount") {
			t.Fatalf("proxy must not mount anything: %v", args)
		}
	}
}

func TestSealedProxyConfigDefaultDeny(t *testing.T) {
	conf, filter := boothSealedProxyConfig([]string{"api.provider.test:443", "host.containers.internal:3000", "alt.provider.test:443"})
	for _, want := range []string{"FilterDefaultDeny Yes", "Filter \"/tmp/filter\"", "ConnectPort 443", "ConnectPort 3000", "Port 3128"} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf missing %q:\n%s", want, conf)
		}
	}
	if strings.Count(conf, "ConnectPort 443") != 1 {
		t.Errorf("duplicate ConnectPort:\n%s", conf)
	}
	want := `^alt\.provider\.test$` + "\n" + `^api\.provider\.test$` + "\n" + `^host\.containers\.internal$`
	if filter != want {
		t.Fatalf("filter = %q, want %q", filter, want)
	}
}

func TestSealedAllowlistFromEnvFlagAndModel(t *testing.T) {
	t.Setenv("BASHY_SEALED_ALLOW", "api.provider.test, https://alt.provider.test:8443/v1 ")
	got, err := boothSealedAllowlist([]string{"flag.provider.test"}, os.Getenv("BASHY_SEALED_ALLOW"), "https://model.provider.test/v1", "http://127.0.0.1:3000/arena/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alt.provider.test:8443", "api.provider.test:443", "flag.provider.test:443", "host.containers.internal:3000", "model.provider.test:443"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowlist = %v, want %v", got, want)
	}
}

func TestSealedAllowlistRequiresProviderEndpoint(t *testing.T) {
	if _, err := boothSealedAllowlist(nil, "", "", "http://127.0.0.1:3000/a/b.git"); err == nil || !strings.Contains(err.Error(), "BASHY_SEALED_ALLOW") {
		t.Fatalf("expected a refusal naming BASHY_SEALED_ALLOW, got %v", err)
	}
	if _, err := boothSealedAllowlist([]string{"bad host!"}, "", "", "http://127.0.0.1:3000/a/b.git"); err == nil {
		t.Fatal("invalid host accepted")
	}
	if _, err := boothSealedAllowlist([]string{"api.provider.test"}, "", "", ""); err == nil {
		t.Fatal("missing loom address accepted")
	}
}

func TestSealedTranslateEnvAndArgv(t *testing.T) {
	maps := boothSealedPathMaps("/booths/ws-7", "/q/dir", 7)
	env := boothSealedTranslateEnv([]string{
		"HOME=/q/dir/booth-7-home",
		"BASHY_HOME=/q/dir/booth-7-home",
		"PATH=/opt/host/bin:/usr/bin",
		"PWD=/booths/ws-7",
		"AGENT_A_CONFIG_DIR=/q/dir/booth-7-agent/agent_a_config_dir",
		"TMPDIR=/var/folders/xx",
		"SOME_HOST_PATH=/Users/someone/private",
		"HTTPS_PROXY=http://host-proxy:8080",
		"AGENT_A_API_KEY=sk-1",
		"TERM=xterm-256color",
	}, maps)
	want := []string{
		"BASHY_HOME=" + boothSealedHome,
		"AGENT_A_CONFIG_DIR=" + boothSealedHome + "/.booth-agent/agent_a_config_dir",
		"AGENT_A_API_KEY=sk-1",
		"TERM=xterm-256color",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v\nwant %v", env, want)
	}
	argv := boothSealedTranslateArgv([]string{"/opt/host/bin/agent-a", "--cwd=/booths/ws-7", "--events", "/booths/ws-7/.git/events.jsonl", "fix /booths/ws-7/x.go"}, maps)
	wantArgv := []string{"agent-a", "--cwd=" + boothSealedWorkdir, "--events", boothSealedWorkdir + "/.git/events.jsonl", "fix " + boothSealedWorkdir + "/x.go"}
	if !reflect.DeepEqual(argv, wantArgv) {
		t.Fatalf("argv = %v\nwant %v", argv, wantArgv)
	}
}

func TestSealedGitReachesLoomThroughHostAlias(t *testing.T) {
	env := boothSealedGitEnv("http://booth-4-7@127.0.0.1:3000/sprint-4/repo.git")
	want := []string{
		"GIT_CONFIG_COUNT=4",
		"GIT_CONFIG_KEY_0=url.http://booth-4-7@host.containers.internal:3000/.insteadOf",
		"GIT_CONFIG_VALUE_0=http://booth-4-7@127.0.0.1:3000/",
		"GIT_CONFIG_KEY_1=url.http://host.containers.internal:3000/.insteadOf",
		"GIT_CONFIG_VALUE_1=http://127.0.0.1:3000/",
		"GIT_CONFIG_KEY_2=credential.helper",
		"GIT_CONFIG_VALUE_2=",
		"GIT_CONFIG_KEY_3=credential.helper",
		"GIT_CONFIG_VALUE_3=store --file=" + boothSealedHome + "/.git-credentials",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("git env = %v\nwant %v", env, want)
	}
	if got := boothSealedCredentials("http://booth-4-7:pw@127.0.0.1:3000\n"); got != "http://booth-4-7:pw@host.containers.internal:3000\n" {
		t.Fatalf("credentials = %q", got)
	}
}

func TestSealedNamesAreQueueScoped(t *testing.T) {
	a, ap, an := boothSealedNames("/q/one", 7)
	b, _, _ := boothSealedNames("/q/two", 7)
	if a == b {
		t.Fatal("two queues' run 7 share one container name")
	}
	if ap != a+"-proxy" || an != a+"-net" || !strings.HasPrefix(a, "bashy-sealed-") {
		t.Fatalf("names = %q %q %q", a, ap, an)
	}
}

func TestSealedRecordOnRunItem(t *testing.T) {
	dir := t.TempDir()
	if err := saveWeaveQueue(dir, &weaveQueue{Items: []*weaveItem{{ID: 3, Title: "x"}}}); err != nil {
		t.Fatal(err)
	}
	rec := &boothSealedRecord{Sealed: true, Image: "localhost/agent-image:1", ImageDigest: "sha256:abc", Allowlist: []string{"api.provider.test:443"}}
	if err := boothSealedRecordRun(dir, 3, rec); err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 3)
	if it == nil || it.Sealed == nil || !it.Sealed.Sealed || it.Sealed.ImageDigest != "sha256:abc" || !reflect.DeepEqual(it.Sealed.Allowlist, rec.Allowlist) {
		t.Fatalf("run item record = %+v", it)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "queue.json"))
	if !strings.Contains(string(raw), `"sealed": true`) && !strings.Contains(string(raw), `"sealed":true`) {
		t.Fatalf("queue.json lacks sealed=true: %s", raw)
	}
}

func TestSealedStartRequiresArena(t *testing.T) {
	_, _, _, err := boothSealedStart(context.Background(), t.TempDir(), &weaveItem{ID: 1}, nil, "agent-a", t.TempDir(), []string{"agent-a"}, nil, "", "", weaveStartOptions{sealed: true})
	if err == nil || !strings.Contains(err.Error(), "--arena") {
		t.Fatalf("expected --arena refusal, got %v", err)
	}
}

// TestSealedLiveEgress starts a real sealed container and proves the egress
// allowlist: the loom stand-in (an HTTP server on host loopback) is reachable
// through the proxy, while an external host is refused by the proxy and
// unreachable directly, and that the workspace is the container's only mount.
// Opt in with BASHY_SEALED_LIVE=1; needs a running podman and network access to
// build the proxy image once. TestMain fences HOME, so on a podman-machine host
// point podman at the VM with CONTAINER_HOST and CONTAINER_SSHKEY.
func TestSealedLiveEgress(t *testing.T) {
	if os.Getenv("BASHY_SEALED_LIVE") != "1" {
		t.Skip("set BASHY_SEALED_LIVE=1 to run the live sealed-booth smoke")
	}
	bin, err := podman.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "loom-ok") })}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	loom := fmt.Sprintf("http://booth-1-1@%s/sprint-1/repo.git", ln.Addr())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	proxyImage := os.Getenv("BASHY_SEALED_PROXY_IMAGE")
	if proxyImage == "" {
		proxyImage = boothSealedDefaultProxyImage
	}
	if err := boothSealedEnsureProxyImage(ctx, bin, proxyImage); err != nil {
		t.Fatal(err)
	}
	queue := t.TempDir()
	workspace := t.TempDir()
	// Login material the standard booth would have seeded, and its git store.
	if err := os.MkdirAll(filepath.Join(queue, "booth-1-home", ".agent-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queue, "booth-1-home", ".agent-a", "auth.json"), []byte(`{"login":"seeded"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(queue, "booth-1.credentials")
	if err := os.WriteFile(cred, []byte(fmt.Sprintf("http://booth-1-1:pw@%s\n", ln.Addr())), 0o600); err != nil {
		t.Fatal(err)
	}
	external := os.Getenv("BASHY_SEALED_LIVE_EXTERNAL")
	if external == "" {
		external = "example.com"
	}
	script := fmt.Sprintf(`set -u
echo written > %[1]s/from-sealed
wget -q -T 10 -O- http://host.containers.internal:%[2]d/ && echo " LOOM_REACHABLE"
if wget -q -T 10 -O- http://%[3]s/ >/dev/null 2>&1; then echo EXTERNAL_VIA_PROXY_REACHABLE; else echo EXTERNAL_VIA_PROXY_REFUSED; fi
if env -u http_proxy -u https_proxy -u HTTP_PROXY -u HTTPS_PROXY wget -q -T 10 -O- http://%[3]s/ >/dev/null 2>&1; then echo EXTERNAL_DIRECT_REACHABLE; else echo EXTERNAL_DIRECT_REFUSED; fi
cat "$HOME/.agent-a/auth.json"; echo
grep -q '@host.containers.internal:%[2]d' "$HOME/.git-credentials" && echo GIT_CREDENTIALS_REWRITTEN
`, boothSealedWorkdir, ln.Addr().(*net.TCPAddr).Port, external)
	in := boothSealedInput{
		QueueDir:       queue,
		Run:            1,
		Workspace:      workspace,
		Image:          proxyImage, // any image with a shell + wget stands in for an agent image
		ProxyImage:     proxyImage,
		Argv:           []string{"sh", "-c", script},
		Env:            []string{"AGENT_A_API_KEY=sk-live-test"},
		LoomURL:        loom,
		CredentialFile: cred,
		Allow:          []string{"api.provider.test"},
	}
	l, err := boothSealedLaunch(ctx, bin, in)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Cleanup()
	container, _, _ := boothSealedNames(queue, 1)
	mounts, err := exec.CommandContext(ctx, bin, "inspect", "--format", "{{range .Mounts}}{{.Destination}};{{end}}", container).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect mounts: %v: %s", err, mounts)
	}
	if got := strings.TrimSpace(string(mounts)); got != boothSealedWorkdir+";" {
		t.Errorf("sealed container mounts = %q, want only %s", got, boothSealedWorkdir)
	}
	cmd := exec.CommandContext(ctx, l.Argv[0], l.Argv[1:]...)
	cmd.Env = l.Env
	out, err := cmd.CombinedOutput()
	t.Logf("record: %+v", *l.Record)
	t.Logf("sealed container output:\n%s", out)
	if err != nil {
		t.Fatalf("sealed run: %v", err)
	}
	for _, want := range []string{"loom-ok LOOM_REACHABLE", "EXTERNAL_VIA_PROXY_REFUSED", "EXTERNAL_DIRECT_REFUSED", `{"login":"seeded"}`, "GIT_CREDENTIALS_REWRITTEN"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q", want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(workspace, "from-sealed")); err != nil || strings.TrimSpace(string(b)) != "written" {
		t.Errorf("workspace mount not writable from the container: %v %q", err, b)
	}
	if !l.Record.Sealed || l.Record.ImageDigest == "" || len(l.Record.Allowlist) == 0 {
		t.Errorf("incomplete record: %+v", *l.Record)
	}
	// Control: the external host IS reachable without the seal, so the refusal
	// above is the seal's doing and not a dead network.
	ctl := exec.CommandContext(ctx, bin, "run", "--rm", "--network=podman", "--pull=never", proxyImage, "wget", "-q", "-T", "10", "-O", "/dev/null", "http://"+external+"/")
	if out, err := ctl.CombinedOutput(); err != nil {
		t.Errorf("control: %s is not reachable even unsealed (%v: %s); the refusal proves nothing", external, err, out)
	}
}
