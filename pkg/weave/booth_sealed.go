package weave

// Sealed booths: the sandbox tier of a booth (band-ladder design §12, "Two
// tiers"). A sealed attempt runs the agent CLI inside a podman container whose
// only host mount is the per-attempt workspace and whose only egress is the
// allowlist: the bound model's provider endpoints and the loom arena.
//
// Egress enforcement is two layers, both podman-native:
//
//  1. The agent container sits on its own `podman network create --internal`
//     network. An internal network has no gateway, so a direct connection to
//     any address outside it fails with "network unreachable" — including a
//     process that ignores proxy variables.
//  2. The only other member of that network is a proxy container (tinyproxy)
//     that is ALSO attached to the ordinary egress network. It runs
//     FilterDefaultDeny with one anchored host pattern per allowlist entry and
//     one ConnectPort per allowlisted port, so everything not named is refused
//     with 403 before a connection is made.
//
// The agent is pointed at the proxy with HTTP(S)_PROXY. Loom on the host's
// loopback is reached as host.containers.internal: git gets an insteadOf
// rewrite and a container-side copy of the booth credential store, so the
// workspace's recorded origin keeps working unchanged.
//
// Nothing else crosses from the host: no $HOME mount, no --env-host, podman's
// own http-proxy passthrough off. The agent's login material (what the
// standard booth seeded into booth-N-home / booth-N-agent) is COPIED into the
// container with `podman cp` before it starts, and environment values travel
// in podman's process environment (`--env NAME`), never in its argv.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/podman"
)

const (
	boothSealedWorkdir            = "/workspace"
	boothSealedHome               = "/home/booth"
	boothSealedHostAlias          = "host.containers.internal"
	boothSealedProxyPort          = 3128
	boothSealedDefaultProxyImage  = "localhost/bashy-sealed-proxy:1"
	boothSealedProxyContainerfile = "FROM docker.io/library/alpine:3.20\nRUN apk add --no-cache tinyproxy\n"
	boothSealedProxyScript        = `printf '%s\n' "$BASHY_SEALED_PROXY_CONF" >/tmp/tinyproxy.conf && printf '%s\n' "$BASHY_SEALED_PROXY_FILTER" >/tmp/filter && exec tinyproxy -d -c /tmp/tinyproxy.conf`
)

// boothSealedRecord is what a sealed attempt leaves on its run item.
type boothSealedRecord struct {
	Sealed      bool     `json:"sealed"`
	Image       string   `json:"image"`
	ImageDigest string   `json:"image_digest,omitempty"`
	Allowlist   []string `json:"allowlist"`
	Network     string   `json:"network,omitempty"`
	ProxyImage  string   `json:"proxy_image,omitempty"`
}

// boothSealedSpec is the fully resolved container plan; the argv builders are
// pure functions of it.
type boothSealedSpec struct {
	Run           int64
	Container     string
	Proxy         string
	Network       string
	EgressNetwork string
	Image         string
	ProxyImage    string
	ProxyAddr     string // ip:port of the proxy on the internal network
	Workspace     string // host path, bind-mounted rw at boothSealedWorkdir
	LoomURL       string // booth fork URL as recorded on the host
	Env           []string
	Argv          []string
	Allow         []string
	TTY           bool
	MemLimitBytes int64
}

// boothSealedInput is what boothSealedLaunch needs from a weave start.
type boothSealedInput struct {
	QueueDir       string
	Run            int64
	Workspace      string
	Image          string
	ProxyImage     string
	Argv           []string // host argv, bound to Workspace
	Env            []string // booth child env (scrubbed + seeded)
	LoomURL        string
	CredentialFile string
	Allow          []string // --allow-host entries
	AllowEnv       string   // BASHY_SEALED_ALLOW
	ModelBaseURL   string
	TTY            bool
	MemLimitBytes  int64
}

type boothSealedLaunched struct {
	Argv    []string
	Env     []string
	Record  *boothSealedRecord
	Cleanup func()
}

type boothSealedPathMap struct{ host, container string }

// boothSealedNames scopes container/network names by queue dir so two repos'
// run 7 never collide on one podman engine.
func boothSealedNames(queueDir string, run int64) (container, proxy, network string) {
	sum := sha256.Sum256([]byte(queueDir))
	container = fmt.Sprintf("bashy-sealed-%s-%d", hex.EncodeToString(sum[:4]), run)
	return container, container + "-proxy", container + "-net"
}

var boothSealedHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

func boothSealedIsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// boothSealedEndpoint normalizes a URL, host or host:port to host:port. A
// loopback host means "this machine", which a container reaches through the
// podman host alias.
func boothSealedEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	host, port := raw, ""
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", fmt.Errorf("sealed allowlist entry %q: %w", raw, err)
		}
		host, port = u.Hostname(), u.Port()
		if port == "" {
			port = "443"
			if u.Scheme == "http" {
				port = "80"
			}
		}
	} else if h, p, err := net.SplitHostPort(raw); err == nil {
		host, port = h, p
	}
	if port == "" {
		port = "443"
	}
	host = strings.ToLower(host)
	if boothSealedIsLoopback(host) {
		host = boothSealedHostAlias
	}
	n, err := strconv.Atoi(port)
	if !boothSealedHostRE.MatchString(host) || err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid sealed allowlist entry %q (want host, host:port or URL)", raw)
	}
	return host + ":" + port, nil
}

// boothSealedAllowlist is the complete egress allowlist: the model-provider
// endpoints (the bound model's base_url, --allow-host, BASHY_SEALED_ALLOW) and
// the loom address. A seal with no provider endpoint would starve the agent,
// so it is refused with the knobs that fix it.
func boothSealedAllowlist(flag []string, envAllow, modelBaseURL, loomURL string) ([]string, error) {
	var raw []string
	split := func(s string) []string {
		return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	}
	for _, f := range flag {
		raw = append(raw, split(f)...)
	}
	raw = append(raw, split(envAllow)...)
	if strings.TrimSpace(modelBaseURL) != "" {
		raw = append(raw, modelBaseURL)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("sealed booth has no model-provider endpoint: the bound model declares no base_url; pass --allow-host HOST or set BASHY_SEALED_ALLOW")
	}
	if u, err := url.Parse(loomURL); err != nil || u.Host == "" {
		return nil, fmt.Errorf("sealed booth needs the loom address (the booth fork URL); got %q", loomURL)
	}
	raw = append(raw, loomURL)
	seen := map[string]bool{}
	var out []string
	for _, r := range raw {
		ep, err := boothSealedEndpoint(r)
		if err != nil {
			return nil, err
		}
		if !seen[ep] {
			seen[ep] = true
			out = append(out, ep)
		}
	}
	sort.Strings(out)
	return out, nil
}

func boothSealedPathMaps(workspace, queueDir string, run int64) []boothSealedPathMap {
	id := strconv.FormatInt(run, 10)
	maps := []boothSealedPathMap{
		{filepath.Join(queueDir, "booth-"+id+"-agent"), boothSealedHome + "/.booth-agent"},
		{filepath.Join(queueDir, "booth-"+id+"-home"), boothSealedHome},
		{filepath.Clean(workspace), boothSealedWorkdir},
	}
	sort.SliceStable(maps, func(i, j int) bool { return len(maps[i].host) > len(maps[j].host) })
	return maps
}

func boothSealedMapPath(s string, maps []boothSealedPathMap) (string, bool) {
	for _, m := range maps {
		if s == m.host || strings.HasPrefix(s, m.host+"/") {
			return m.container + s[len(m.host):], true
		}
	}
	return s, false
}

// boothSealedDropEnv names host-shaped variables that must not cross into the
// container: its own HOME/PATH/PWD/proxy are set by the seal, and the rest
// describe the host session.
func boothSealedDropEnv(key string) bool {
	switch strings.ToUpper(key) {
	case "HOME", "PATH", "PWD", "OLDPWD", "SHELL", "TMPDIR", "TMP", "TEMP", "USER", "LOGNAME", "DISPLAY",
		"CONTAINER_HOST", "DOCKER_HOST", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_USE_ENV_PROXY":
		return true
	}
	for _, p := range []string{"SSH_", "XPC_", "__CF", "XDG_", "TERM_SESSION", "GIT_CONFIG_"} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// boothSealedTranslateEnv rewrites the booth env for the container: booth
// paths move to their container locations, any other host path is dropped,
// and a repeated key keeps its first position with its last value.
func boothSealedTranslateEnv(env []string, maps []boothSealedPathMap) []string {
	var out []string
	index := map[string]int{}
	for _, kv := range env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" || boothSealedDropEnv(key) {
			continue
		}
		if mapped, ok := boothSealedMapPath(val, maps); ok {
			val = mapped
		} else if filepath.IsAbs(val) {
			continue
		}
		if i, ok := index[key]; ok {
			out[i] = key + "=" + val
			continue
		}
		index[key] = len(out)
		out = append(out, key+"="+val)
	}
	return out
}

// boothSealedTranslateArgv moves workspace/booth paths to their container
// locations. A host-absolute binary path becomes its base name, resolved on
// the image's PATH: a host binary is the wrong OS for the container anyway.
func boothSealedTranslateArgv(argv []string, maps []boothSealedPathMap) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if i == 0 {
			if mapped, ok := boothSealedMapPath(a, maps); ok {
				out[i] = mapped
			} else if filepath.IsAbs(a) {
				out[i] = filepath.Base(a)
			} else {
				out[i] = a
			}
			continue
		}
		for _, m := range maps {
			a = strings.ReplaceAll(a, m.host, m.container)
		}
		out[i] = a
	}
	return out
}

func boothSealedAliasHost(u *url.URL) (string, bool) {
	if !boothSealedIsLoopback(u.Hostname()) {
		return u.Host, false
	}
	if p := u.Port(); p != "" {
		return boothSealedHostAlias + ":" + p, true
	}
	return boothSealedHostAlias, true
}

// boothSealedGitEnv is the command-scope git config (GIT_CONFIG_COUNT) that
// lets the workspace's host-recorded loom origin work from inside the
// container: loopback rewritten to the host alias, and the credential helper
// list reset to the container-side store.
func boothSealedGitEnv(forkURL string) []string {
	type kv struct{ k, v string }
	var cfg []kv
	if u, err := url.Parse(forkURL); err == nil && u.Host != "" {
		if alias, ok := boothSealedAliasHost(u); ok {
			if u.User != nil && u.User.Username() != "" {
				user := url.User(u.User.Username()).String()
				cfg = append(cfg, kv{"url." + u.Scheme + "://" + user + "@" + alias + "/.insteadOf", u.Scheme + "://" + user + "@" + u.Host + "/"})
			}
			cfg = append(cfg, kv{"url." + u.Scheme + "://" + alias + "/.insteadOf", u.Scheme + "://" + u.Host + "/"})
		}
	}
	cfg = append(cfg, kv{"credential.helper", ""}, kv{"credential.helper", "store --file=" + boothSealedHome + "/.git-credentials"})
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(cfg))}
	for i, c := range cfg {
		out = append(out, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, c.k), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, c.v))
	}
	return out
}

// boothSealedCredentials rewrites a git credential store for the container.
func boothSealedCredentials(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		u, err := url.Parse(strings.TrimSpace(line))
		if err != nil || u.Host == "" {
			continue
		}
		if alias, ok := boothSealedAliasHost(u); ok {
			u.Host = alias
			lines[i] = u.String()
		}
	}
	return strings.Join(lines, "\n")
}

func boothSealedNetworkArgs(spec boothSealedSpec) []string {
	return []string{"network", "create", "--internal", "--label=bashy.sealed=1", spec.Network}
}

// boothSealedProxyConfig renders tinyproxy's config and host filter. Hosts
// are validated to [a-z0-9.-], so escaping the dots makes each pattern exact.
func boothSealedProxyConfig(allow []string) (conf, filter string) {
	var hosts, ports []string
	seenHost, seenPort := map[string]bool{}, map[string]bool{}
	for _, ep := range allow {
		host, port, err := net.SplitHostPort(ep)
		if err != nil {
			continue
		}
		if !seenHost[host] {
			seenHost[host] = true
			hosts = append(hosts, "^"+strings.ReplaceAll(host, ".", `\.`)+"$")
		}
		if !seenPort[port] {
			seenPort[port] = true
			ports = append(ports, port)
		}
	}
	sort.Strings(hosts)
	sort.Slice(ports, func(i, j int) bool {
		a, _ := strconv.Atoi(ports[i])
		b, _ := strconv.Atoi(ports[j])
		return a < b
	})
	lines := []string{
		"User nobody", "Group nobody",
		"Port " + strconv.Itoa(boothSealedProxyPort),
		"Timeout 600", "MaxClients 64", "LogLevel Notice",
		"FilterDefaultDeny Yes", `Filter "/tmp/filter"`,
	}
	for _, p := range ports {
		lines = append(lines, "ConnectPort "+p)
	}
	return strings.Join(lines, "\n"), strings.Join(hosts, "\n")
}

func boothSealedProxyEnv(spec boothSealedSpec) []string {
	conf, filter := boothSealedProxyConfig(spec.Allow)
	return []string{"BASHY_SEALED_PROXY_CONF=" + conf, "BASHY_SEALED_PROXY_FILTER=" + filter}
}

func boothSealedProxyArgs(spec boothSealedSpec) []string {
	return []string{
		"run", "--detach", "--name=" + spec.Proxy,
		"--label=bashy.sealed=1", "--label=bashy.sealed.run=" + strconv.FormatInt(spec.Run, 10),
		"--network=" + spec.EgressNetwork, "--network=" + spec.Network,
		"--pull=never", "--http-proxy=false",
		"--env=BASHY_SEALED_PROXY_CONF", "--env=BASHY_SEALED_PROXY_FILTER",
		spec.ProxyImage, "sh", "-c", boothSealedProxyScript,
	}
}

// boothSealedCreateArgs is the agent container: internal network only, the
// workspace as its only mount, constants inline, agent env by NAME only.
func boothSealedCreateArgs(spec boothSealedSpec) []string {
	args := []string{
		"create", "--name=" + spec.Container,
		"--label=bashy.sealed=1", "--label=bashy.sealed.run=" + strconv.FormatInt(spec.Run, 10),
		"--network=" + spec.Network, "--http-proxy=false", "--pull=never",
		"--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--volume", filepath.Clean(spec.Workspace) + ":" + boothSealedWorkdir + ":rw",
		"--workdir=" + boothSealedWorkdir,
		"--interactive",
	}
	if spec.TTY {
		args = append(args, "--tty")
	}
	if spec.MemLimitBytes > 0 {
		args = append(args, "--memory="+strconv.FormatInt(spec.MemLimitBytes, 10)+"b")
	}
	proxy := "http://" + spec.ProxyAddr
	inline := []string{
		"HOME=" + boothSealedHome, "PWD=" + boothSealedWorkdir,
		"HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy, "https_proxy=" + proxy, "http_proxy=" + proxy,
		"NODE_USE_ENV_PROXY=1",
	}
	inline = append(inline, boothSealedGitEnv(spec.LoomURL)...)
	for _, kv := range inline {
		args = append(args, "--env="+kv)
	}
	for _, kv := range spec.Env {
		key, _, _ := strings.Cut(kv, "=")
		args = append(args, "--env="+key)
	}
	args = append(args, spec.Image)
	return append(args, spec.Argv...)
}

// boothSealedPodmanEnv is the podman client's own environment with the agent
// env overlaid, so `--env NAME` picks the value up without it touching argv.
func boothSealedPodmanEnv(base, agentEnv []string) []string {
	out := append([]string(nil), base...)
	index := map[string]int{}
	for i, kv := range out {
		key, _, _ := strings.Cut(kv, "=")
		index[key] = i
	}
	for _, kv := range agentEnv {
		key, _, _ := strings.Cut(kv, "=")
		if i, ok := index[key]; ok {
			out[i] = kv
			continue
		}
		index[key] = len(out)
		out = append(out, kv)
	}
	return out
}

func boothSealedPodman(ctx context.Context, bin string, env []string, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// boothSealedEnsureProxyImage builds the default proxy image once. An operator
// override (BASHY_SEALED_PROXY_IMAGE) must already exist.
func boothSealedEnsureProxyImage(ctx context.Context, bin, image string) error {
	if _, err := boothSealedPodman(ctx, bin, os.Environ(), "", "image", "exists", image); err == nil {
		return nil
	}
	if image != boothSealedDefaultProxyImage {
		return fmt.Errorf("sealed proxy image %q not found", image)
	}
	buildCtx, err := os.MkdirTemp("", "bashy-sealed-proxy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(buildCtx)
	_, err = boothSealedPodman(ctx, bin, os.Environ(), boothSealedProxyContainerfile, "build", "--quiet", "--tag", image, "--file", "-", buildCtx)
	return err
}

// boothSealedImageUnavailable tells an operator how to supply the image that
// executes a sealed agent. Podman machines run Linux containers, so a host CLI
// executable cannot be copied into the image as a substitute.
func boothSealedImageUnavailable(image string, err error) error {
	return fmt.Errorf("sealed booth image %q is not available: build an image containing a Linux build of the agent CLI (or set BASHY_SEALED_IMAGE): %w", image, err)
}

// boothSealedStage assembles the container's home: the standard booth's
// seeded login material plus a container-side git credential store.
func boothSealedStage(in boothSealedInput) (string, error) {
	id := strconv.FormatInt(in.Run, 10)
	stage := filepath.Join(in.QueueDir, "booth-"+id+"-sealed-home")
	if err := os.RemoveAll(stage); err != nil {
		return "", err
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return "", err
	}
	if err := boothSealedCopyTree(filepath.Join(in.QueueDir, "booth-"+id+"-home"), stage); err != nil {
		return "", err
	}
	if err := boothSealedCopyTree(filepath.Join(in.QueueDir, "booth-"+id+"-agent"), filepath.Join(stage, ".booth-agent")); err != nil {
		return "", err
	}
	if in.CredentialFile != "" {
		b, err := os.ReadFile(in.CredentialFile)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err == nil {
			if err := os.WriteFile(filepath.Join(stage, ".git-credentials"), []byte(boothSealedCredentials(string(b))), 0o600); err != nil {
				return "", err
			}
		}
	}
	return stage, nil
}

// boothSealedCopyTree copies regular files and directories; symlinks are not
// followed, so a link cannot pull an arbitrary host file into the seal.
func boothSealedCopyTree(src, dst string) error {
	if _, err := os.Lstat(src); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0o600)
		}
		return nil
	})
}

// boothSealedLaunch creates the sealed network, proxy and (not yet started)
// agent container, and returns the argv that attaches to it.
func boothSealedLaunch(ctx context.Context, bin string, in boothSealedInput) (*boothSealedLaunched, error) {
	allow, err := boothSealedAllowlist(in.Allow, in.AllowEnv, in.ModelBaseURL, in.LoomURL)
	if err != nil {
		return nil, err
	}
	base := os.Environ()
	digest, err := boothSealedPodman(ctx, bin, base, "", "image", "inspect", "--format", "{{.Id}}", in.Image)
	if err != nil {
		return nil, boothSealedImageUnavailable(in.Image, err)
	}
	if !strings.HasPrefix(digest, "sha256:") {
		digest = "sha256:" + digest
	}
	if in.ProxyImage == "" {
		in.ProxyImage = boothSealedDefaultProxyImage
	}
	if err := boothSealedEnsureProxyImage(ctx, bin, in.ProxyImage); err != nil {
		return nil, err
	}
	egress := os.Getenv("BASHY_SEALED_EGRESS_NETWORK")
	if egress == "" {
		egress = "podman"
	}
	maps := boothSealedPathMaps(in.Workspace, in.QueueDir, in.Run)
	spec := boothSealedSpec{
		Run: in.Run, EgressNetwork: egress, Image: in.Image, ProxyImage: in.ProxyImage,
		Workspace: in.Workspace, LoomURL: in.LoomURL, Allow: allow, TTY: in.TTY, MemLimitBytes: in.MemLimitBytes,
		Env:  boothSealedTranslateEnv(in.Env, maps),
		Argv: boothSealedTranslateArgv(in.Argv, maps),
	}
	spec.Container, spec.Proxy, spec.Network = boothSealedNames(in.QueueDir, in.Run)
	cleanup := func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = boothSealedPodman(cctx, bin, base, "", "rm", "--force", "--ignore", spec.Container, spec.Proxy)
		_, _ = boothSealedPodman(cctx, bin, base, "", "network", "rm", "--force", spec.Network)
	}
	cleanup() // leftovers of an earlier attempt at this run (--resume)
	fail := func(err error) (*boothSealedLaunched, error) {
		cleanup()
		return nil, err
	}
	if _, err := boothSealedPodman(ctx, bin, base, "", boothSealedNetworkArgs(spec)...); err != nil {
		return fail(err)
	}
	if _, err := boothSealedPodman(ctx, bin, boothSealedPodmanEnv(base, boothSealedProxyEnv(spec)), "", boothSealedProxyArgs(spec)...); err != nil {
		return fail(err)
	}
	ip, err := boothSealedPodman(ctx, bin, base, "", "inspect", "--format", `{{(index .NetworkSettings.Networks "`+spec.Network+`").IPAddress}}`, spec.Proxy)
	if err != nil || net.ParseIP(ip) == nil {
		return fail(fmt.Errorf("sealed proxy has no address on %s: %q %v", spec.Network, ip, err))
	}
	spec.ProxyAddr = net.JoinHostPort(ip, strconv.Itoa(boothSealedProxyPort))
	ready := false
	for i := 0; i < 50 && !ready; i++ {
		_, err := boothSealedPodman(ctx, bin, base, "", "exec", spec.Proxy, "nc", "-z", "127.0.0.1", strconv.Itoa(boothSealedProxyPort))
		ready = err == nil
		if !ready {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !ready {
		logs, _ := boothSealedPodman(ctx, bin, base, "", "logs", spec.Proxy)
		return fail(fmt.Errorf("sealed proxy did not start: %s", logs))
	}
	stage, err := boothSealedStage(in)
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(stage)
	if _, err := boothSealedPodman(ctx, bin, boothSealedPodmanEnv(base, spec.Env), "", boothSealedCreateArgs(spec)...); err != nil {
		return fail(err)
	}
	if _, err := boothSealedPodman(ctx, bin, base, "", "cp", stage+"/.", spec.Container+":"+boothSealedHome); err != nil {
		return fail(err)
	}
	return &boothSealedLaunched{
		Argv:    []string{bin, "start", "--attach", "--interactive", spec.Container},
		Env:     base,
		Record:  &boothSealedRecord{Sealed: true, Image: in.Image, ImageDigest: digest, Allowlist: allow, Network: spec.Network, ProxyImage: in.ProxyImage},
		Cleanup: cleanup,
	}, nil
}

func boothSealedRecordRun(queueDir string, run int64, rec *boothSealedRecord) error {
	return withWeaveQueueLock(queueDir, func(q *weaveQueue) error {
		it := findWeaveItem(q, run)
		if it == nil {
			return fmt.Errorf("run #%d not in queue", run)
		}
		it.Sealed = rec
		return nil
	})
}

// boothSealedImageFor is the default agent image for a tool; the operator
// builds it with the agent CLI installed, or names another via
// BASHY_SEALED_IMAGE.
func boothSealedImageFor(tool string) string {
	if img := os.Getenv("BASHY_SEALED_IMAGE"); img != "" {
		return img
	}
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(tool)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return "localhost/bashy-sealed-" + b.String() + ":latest"
}

// boothSealedStart is weave start's one call into the sealed tier: it turns
// the booth's host argv/env into a sealed container, records the seal on the
// run item, and returns the podman argv/env to spawn plus its cleanup.
func boothSealedStart(ctx context.Context, dir string, it *weaveItem, l *weaveAgentLaunch, displayTool, workspace string, toolArgs, env []string, boothFork, boothCred string, opts weaveStartOptions) ([]string, []string, func(), error) {
	if it == nil || it.ArenaSprint == 0 {
		return nil, nil, nil, fmt.Errorf("--sealed runs a booth in the sandbox tier and requires --arena SPRINT")
	}
	bin, err := podman.Resolve()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--sealed needs podman: %w", err)
	}
	tool := displayTool
	modelBaseURL := ""
	if l != nil {
		if l.ToolName != "" {
			tool = l.ToolName
		}
		if l.ModelName != "" {
			if m, ok := fleetCatalog().Model(l.ModelName); ok {
				modelBaseURL = m.BaseURL
			}
		}
	}
	memLimit, err := parseWeaveMemLimit(opts.memLimit)
	if err != nil {
		return nil, nil, nil, err
	}
	if boothFork == "" {
		boothFork = it.BoothForkURL
	}
	launched, err := boothSealedLaunch(ctx, bin, boothSealedInput{
		QueueDir: dir, Run: it.ID, Workspace: workspace,
		Image: boothSealedImageFor(tool), ProxyImage: os.Getenv("BASHY_SEALED_PROXY_IMAGE"),
		Argv: toolArgs, Env: env, LoomURL: boothFork, CredentialFile: boothCred,
		Allow: opts.sealedAllow, AllowEnv: os.Getenv("BASHY_SEALED_ALLOW"), ModelBaseURL: modelBaseURL,
		TTY: opts.ptyMode() != "never", MemLimitBytes: memLimit,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := boothSealedRecordRun(dir, it.ID, launched.Record); err != nil {
		launched.Cleanup()
		return nil, nil, nil, err
	}
	it.Sealed = launched.Record
	return launched.Argv, launched.Env, launched.Cleanup, nil
}
