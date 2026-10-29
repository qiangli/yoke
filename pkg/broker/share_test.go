package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// exitError models a non-zero exit from the outpost CLI whose stderr
// carries the remote command's own error text (e.g. cobra usage).
type exitError struct{ msg string }

func (e exitError) Error() string { return e.msg }

// fakeOutpost stands in for the outpost CLI: canned answers by verb, every
// call recorded with its stdin.
type fakeOutpost struct {
	mu       sync.Mutex
	services string // `mesh service ls` output
	apps     string // `apps list --json` output
	consumes string // `mesh consume ls` output
	calls    [][]string
	stdin    []string
	sshErr   error
}

func (f *fakeOutpost) run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.stdin = append(f.stdin, in)
	switch strings.Join(args[:min(len(args), 3)], " ") {
	case "status --json":
		return []byte(`{"config":{"agent_name":"Dooor","cloudbox_url":"https://cloudbox.example/"}}`), nil
	case "mesh service ls":
		return []byte(f.services), nil
	case "mesh service add":
		f.services += args[3] + " -> " + args[4] + "\n"
		return []byte("persistently exposing\n"), nil
	case "mesh status --json":
		return []byte(`{"status":{"peer_id":"PEERID-DOOR"}}`), nil
	case "apps list --json":
		if f.apps == "" {
			return []byte(`{"apps":[]}`), nil
		}
		return []byte(f.apps), nil
	case "apps add llm":
		f.apps = fmt.Sprintf(`{"apps":[{"name":"llm","host":"127.0.0.1","port":%d,"enabled":true,"require_login":false}]}`, door.Port())
		return nil, nil
	case "mesh consume ls":
		return []byte(f.consumes), nil
	case "mesh consume add":
		f.consumes += fmt.Sprintf("%s <- peer %s -> %s\n", args[3], args[4], args[6])
		return []byte(fmt.Sprintf("persistently consuming %s from peer %s -> %s\n", args[3], args[4], args[6])), nil
	}
	if args[0] == "ssh" {
		return nil, f.sshErr
	}
	return nil, fmt.Errorf("fake outpost: unexpected %v", args)
}

func (f *fakeOutpost) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			n++
		}
	}
	return n
}

func useFakeOutpost(t *testing.T) *fakeOutpost {
	t.Helper()
	f := &fakeOutpost{}
	prevRun, prevWSL := outpostRun, inWSL
	outpostRun = f.run
	inWSL = func() bool { return false }
	t.Cleanup(func() { outpostRun, inWSL = prevRun, prevWSL })
	return f
}

func runShareLLM(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestShareExposesMintsPushesAndRotates(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(door.PortEnv, "")
	f := useFakeOutpost(t)

	out, err := runShareLLM(t, "", "share", "Peer", "--via", "both")
	if err != nil {
		t.Fatalf("share: %v\n%s", err, out)
	}
	if f.count("mesh service add llm 127.0.0.1:24556") != 1 || f.count("apps add llm --url http://127.0.0.1:24556") != 1 {
		t.Fatalf("door not exposed: %v", f.calls)
	}
	last := f.calls[len(f.calls)-1]
	if strings.Join(last, " ") != "ssh peer bashy llm door import" {
		t.Fatalf("grant not pushed over outpost ssh: %v", last)
	}
	var g DoorGrant
	if err := json.Unmarshal([]byte(f.stdin[len(f.stdin)-1]), &g); err != nil {
		t.Fatalf("pushed grant: %v", err)
	}
	if g.validate() != nil || g.Door != "dooor" || g.Peer != "peer" || g.MeshPeerID != "PEERID-DOOR" || g.MeshService != "llm" ||
		g.CloudboxURL != "https://cloudbox.example/matrix/h/Dooor/app/llm" {
		t.Fatalf("grant = %+v", g)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), g.Credential) {
			t.Fatalf("credential on an argv: %v", c)
		}
	}
	if strings.Contains(out, g.Credential) {
		t.Fatalf("share printed the credential:\n%s", out)
	}
	path, _ := door.PeersPath()
	ps, _ := LoadPeerShares(path)
	if rec := ps.Peers["peer"]; rec.Share != ShareAllow || rec.Hash != hashCredential(g.Credential) {
		t.Fatalf("peer record = %+v", rec)
	}

	// Rerun: exposure reused, credential rotated.
	if out, err := runShareLLM(t, "", "share", "peer", "--via", "both"); err != nil {
		t.Fatalf("share again: %v\n%s", err, out)
	}
	if f.count("mesh service add") != 1 || f.count("apps add") != 1 {
		t.Fatalf("rerun re-exposed the door: %v", f.calls)
	}
	var g2 DoorGrant
	_ = json.Unmarshal([]byte(f.stdin[len(f.stdin)-1]), &g2)
	if g2.Credential == "" || g2.Credential == g.Credential {
		t.Fatal("rerun did not rotate the credential")
	}
	ps, _ = LoadPeerShares(path)
	if ps.Peers["peer"].Hash != hashCredential(g2.Credential) {
		t.Fatal("rotated credential not stored")
	}
}

func TestShareMeshReusesAnExistingDoorService(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(door.PortEnv, "")
	f := useFakeOutpost(t)
	f.services = "git -> 127.0.0.1:3000\nllm-door -> 127.0.0.1:24556\n"
	out, err := runShareLLM(t, "", "share", "peer", "--print")
	if err != nil {
		t.Fatalf("share: %v\n%s", err, out)
	}
	if f.count("mesh service add") != 0 || f.count("apps") != 0 || f.count("ssh") != 0 {
		t.Fatalf("unexpected outpost calls: %v", f.calls)
	}
	var g DoorGrant
	if err := json.Unmarshal([]byte(out), &g); err != nil || g.MeshService != "llm-door" || g.CloudboxURL != "" {
		t.Fatalf("--print grant = %+v (%v)", g, err)
	}
}

// A stale hand-wired llm app (host carrying an embedded port, e.g.
// "127.0.0.1:17788" with the door's port) must not count as present:
// share re-adds it against the door's real loopback address.
func TestShareCloudboxReaddsAppWithAnEmbeddedHostPort(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(door.PortEnv, "")
	f := useFakeOutpost(t)
	f.apps = fmt.Sprintf(`{"apps":[{"name":"llm","host":"127.0.0.1:17788","port":%d,"enabled":true,"require_login":false}]}`, door.Port())
	out, err := runShareLLM(t, "", "share", "peer", "--via", "cloudbox")
	if err != nil {
		t.Fatalf("share: %v\n%s", err, out)
	}
	if f.count("apps add llm --url http://127.0.0.1:24556") != 1 {
		t.Fatalf("stale app kept as present: %v", f.calls)
	}
}

// A correctly wired app (plain loopback host, no embedded port) is
// reused as-is: share must not re-add it.
func TestShareCloudboxReusesAppOnPlainLoopbackHost(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(door.PortEnv, "")
	f := useFakeOutpost(t)
	f.apps = fmt.Sprintf(`{"apps":[{"name":"llm","host":"localhost","port":%d,"enabled":true,"require_login":false}]}`, door.Port())
	out, err := runShareLLM(t, "", "share", "peer", "--via", "cloudbox")
	if err != nil {
		t.Fatalf("share: %v\n%s", err, out)
	}
	if f.count("apps add") != 0 {
		t.Fatalf("healthy app re-added: %v", f.calls)
	}
}

// The live elephant error (10-29): the old bashy names its command path
// without the "bashy " prefix, so the matcher must not require it.
func TestSharePushOverOlderPeerBashyLiveText(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(door.PortEnv, "")
	f := useFakeOutpost(t)
	f.sshErr = exitError{msg: `bashy llm: unknown command "door" for "llm"` + "\n\nDid you mean this?\n\tdown"}
	_, err := runShareLLM(t, "", "share", "peer", "--via", "mesh")
	if err == nil || !strings.Contains(err.Error(), "older than this door") {
		t.Fatalf("err = %v", err)
	}
}

// A peer whose PATH bashy predates `llm door import` answers with
// cobra's unknown-subcommand error; share must translate it instead of
// surfacing the raw text.
func TestSharePushOverOlderPeerBashyNamesUpgrade(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(door.PortEnv, "")
	f := useFakeOutpost(t)
	f.sshErr = exitError{msg: `Error: unknown command "import" for "bashy llm door"` + "\n" + `Run 'bashy llm door --help' for usage.`}
	_, err := runShareLLM(t, "", "share", "peer", "--via", "mesh")
	if err == nil || !strings.Contains(err.Error(), "the bashy on peer is older than this door; upgrade it") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("raw cobra error surfaced: %v", err)
	}
}

func TestShareFailedPushIsAnError(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	f := useFakeOutpost(t)
	f.sshErr = fmt.Errorf("unreachable")
	if _, err := runShareLLM(t, "", "share", "peer"); err == nil || !strings.Contains(err.Error(), "--print") {
		t.Fatalf("err = %v", err)
	}
	if _, err := runShareLLM(t, "", "share", "peer", "--via", "wan"); err == nil {
		t.Fatal("unknown route accepted")
	}
}

func testGrant() DoorGrant {
	return DoorGrant{Schema: GrantSchemaVersion, Door: "dooor", Peer: "peer", MeshPeerID: "PEERID-DOOR", MeshService: "llm",
		CloudboxURL: "https://cloudbox.example/matrix/h/dooor/app/llm", Credential: "bpeer_" + strings.Repeat("ab", 32), Issued: time.Now().UTC()}
}

func TestDoorImportStoresOwnerOnlyAndRecordsToolcmdPeer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	f := useFakeOutpost(t)
	g := testGrant()
	data, _ := json.Marshal(g)
	out, err := runShareLLM(t, string(data), "door", "import")
	if err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	if strings.Contains(out, g.Credential) {
		t.Fatal("import printed the credential")
	}
	path := filepath.Join(home, "llm", "doors", "dooor.json")
	got, err := LoadDoorGrant("dooor")
	if err != nil || got.Credential != g.Credential {
		t.Fatalf("stored grant = %+v (%v)", got, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("grant mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
			t.Fatalf("doors dir mode %v", fi.Mode().Perm())
		}
	}
	if f.count("mesh consume add llm PEERID-DOOR --local 127.0.0.1:") != 1 {
		t.Fatalf("no mesh forward opened: %v", f.calls)
	}
	pd, err := toolcmd.LoadPeer("dooor")
	if err != nil || pd.Credential != g.Credential || !strings.HasPrefix(pd.URL, "http://127.0.0.1:") {
		t.Fatalf("toolcmd peer = %+v (%v)", pd, err)
	}

	for _, bad := range []string{"not json", `{"schema_version":"x"}`, strings.Replace(string(data), "dooor", "../evil", 1)} {
		if _, err := runShareLLM(t, bad, "door", "import"); err == nil {
			t.Fatalf("import accepted %q", bad)
		}
	}
}

func TestEnvFromRendersMeshAndCloudbox(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(SessionEnv, "")
	f := useFakeOutpost(t)
	g := testGrant()
	if err := SaveDoorGrant(g); err != nil {
		t.Fatal(err)
	}
	f.consumes = "git <- peer OTHER -> 127.0.0.1:3000\nllm <- peer PEERID-DOOR -> 127.0.0.1:40123\n"
	owner, _ := door.Token()

	out, err := runShareLLM(t, "", "env", "--from", "dooor")
	if err != nil {
		t.Fatalf("env --from: %v\n%s", err, out)
	}
	for _, want := range []string{
		"OPENAI_BASE_URL=http://127.0.0.1:40123/v1\n",
		"OPENAI_API_KEY=" + g.Credential + "\n",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:40123/anthropic\n",
		"ANTHROPIC_API_KEY=" + g.Credential + "\n",
		"OLLAMA_HOST=http://127.0.0.1:40123/k/" + g.Credential + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("mesh env missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, owner) {
		t.Fatal("peer env carries the owner token")
	}
	if f.count("mesh consume add") != 0 {
		t.Fatal("existing forward not reused")
	}

	out, err = runShareLLM(t, "", "env", "--from", "dooor", "--via", "cloudbox", "--json")
	if err != nil {
		t.Fatalf("env --via cloudbox: %v\n%s", err, out)
	}
	var env struct {
		SchemaVersion string            `json:"schema_version"`
		BaseURL       string            `json:"base_url"`
		Via           string            `json:"via"`
		Env           map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	kbase := g.CloudboxURL + "/k/" + g.Credential
	if env.SchemaVersion != EnvSchemaVersion || env.BaseURL != g.CloudboxURL || env.Via != "cloudbox" || env.Env["OPENAI_BASE_URL"] != kbase+"/v1" ||
		env.Env["ANTHROPIC_BASE_URL"] != kbase+"/anthropic" || env.Env["OPENAI_API_KEY"] != g.Credential || env.Env["OLLAMA_HOST"] != kbase {
		t.Fatalf("cloudbox env = %+v", env)
	}

	out, err = runShareLLM(t, "", "env", "--from", "dooor", "--client", "openclaw")
	if err != nil || !strings.Contains(out, `"baseUrl": "http://127.0.0.1:40123/v1"`) || !strings.Contains(out, g.Credential) {
		t.Fatalf("openclaw --from: %v\n%s", err, out)
	}
}

func TestEnvFromWithoutGrantNamesShare(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	useFakeOutpost(t)
	_, err := runShareLLM(t, "", "env", "--from", "dooor")
	if err == nil || !strings.Contains(err.Error(), "bashy llm share") {
		t.Fatalf("err = %v", err)
	}
	g := testGrant()
	g.CloudboxURL = ""
	if err := SaveDoorGrant(g); err != nil {
		t.Fatal(err)
	}
	if _, err := runShareLLM(t, "", "env", "--from", "dooor", "--via", "cloudbox"); err == nil {
		t.Fatal("cloudbox route without a cloudbox grant accepted")
	}
	if _, err := runShareLLM(t, "", "env", "--from", "dooor", "--sticky", "k"); err == nil {
		t.Fatal("--sticky with --from accepted")
	}
	if _, err := runShareLLM(t, "", "env", "--via", "cloudbox"); err == nil {
		t.Fatal("--via cloudbox without --from accepted")
	}
}

func TestEnvFromWSLUsesWindowsGrant(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	useFakeOutpost(t)
	inWSL = func() bool { return true }
	prevI, prevG := wslInterop, wslGateway
	t.Cleanup(func() { wslInterop, wslGateway = prevI, prevG })
	wslGateway = func() (string, error) { return "172.20.0.1", nil }
	var got []string
	wslInterop = func(_ context.Context, args ...string) ([]byte, error) {
		got = args
		return json.Marshal(map[string]any{"schema_version": EnvSchemaVersion, "base_url": "http://172.20.0.1:40123", "via": "mesh",
			"env": peerEnvVars("http://172.20.0.1:40123", "bpeer_x", false)})
	}
	out, err := runShareLLM(t, "", "env", "--from", "dooor")
	if err != nil {
		t.Fatalf("wsl env: %v\n%s", err, out)
	}
	if strings.Join(got, " ") != "llm env --from dooor --json --bind-for-wsl 172.20.0.1" {
		t.Fatalf("interop args = %v", got)
	}
	if !strings.Contains(out, "OPENAI_BASE_URL=http://172.20.0.1:40123/v1\n") {
		t.Fatalf("wsl env:\n%s", out)
	}

	wslInterop = func(context.Context, ...string) ([]byte, error) { return nil, errWSLInterop }
	if _, err := runShareLLM(t, "", "env", "--from", "dooor"); err == nil || !strings.Contains(err.Error(), "--via cloudbox") {
		t.Fatalf("interop off: %v", err)
	}
}

func TestEnvFromBindForWSL(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	f := useFakeOutpost(t)
	if err := SaveDoorGrant(testGrant()); err != nil {
		t.Fatal(err)
	}
	// A forward exists on another address; the requested bind gets its own.
	f.consumes = "llm <- peer PEERID-DOOR -> 192.0.2.1:40123\n"
	out, err := runShareLLM(t, "", "env", "--from", "dooor", "--bind-for-wsl", "127.0.0.1")
	if err != nil {
		t.Fatalf("env --bind-for-wsl: %v\n%s", err, out)
	}
	if f.count("mesh consume add llm PEERID-DOOR --local 127.0.0.1:") != 1 || strings.Contains(out, "192.0.2.1") {
		t.Fatalf("forward not bound as asked: %v\n%s", f.calls, out)
	}
}

func TestDefaultGateway(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\n" +
		"eth0\t0014A8C0\t00000000\t0001\n" +
		"eth0\t00000000\t0100A8C0\t0003\n"
	if gw, err := defaultGateway(table); err != nil || gw != "192.168.0.1" {
		t.Fatalf("gateway = %q (%v)", gw, err)
	}
	if _, err := defaultGateway("Iface\tDestination\tGateway\n"); err == nil {
		t.Fatal("no default route accepted")
	}
}

func TestPeerMayListModelsOnly(t *testing.T) {
	h, _, peers := peerHarness(t)
	cred, _ := MintPeerCredential(peers, "p1", ShareAllow, time.Now())
	req, _ := http.NewRequest("GET", h.server.URL+"/k/"+cred+"/v1/models", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("shared peer GET /v1/models: %d", resp.StatusCode)
	}
	for _, p := range []string{"/v1/sticky", "/api/pull", "/v1/models"} {
		if code, _ := peerPost(t, h, p, cred, nil); code != http.StatusForbidden {
			t.Fatalf("peer POST %s: %d", p, code)
		}
	}
	if peerRouteAllowed("GET", "/v1/sticky") || !peerRouteAllowed("GET", "/openai/v1/models") {
		t.Fatal("peer route gate")
	}
}

func TestMeshForwardStateField(t *testing.T) {
	g := testGrant()
	cases := []struct {
		name, consumes string
		wantAdds       int
	}{
		{"live reused", "llm <- peer PEERID-DOOR -> 127.0.0.1:40123 [live]\n", 0},
		{"stateless reused", "llm <- peer PEERID-DOOR -> 127.0.0.1:40123\n", 0},
		{"down not reused", "llm <- peer PEERID-DOOR -> 127.0.0.1:40123 [down]\n", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := useFakeOutpost(t)
			f.consumes = c.consumes
			if _, err := ensureMeshForward(context.Background(), g, "127.0.0.1"); err != nil {
				t.Fatal(err)
			}
			if n := f.count("mesh consume add"); n != c.wantAdds {
				t.Fatalf("adds = %d, want %d", n, c.wantAdds)
			}
		})
	}
}

func TestMeshForwardInWSLWithoutOutpostUsesWindows(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	lin := useFakeOutpost(t)
	win := &fakeOutpost{}
	prevWin, prevWSL, prevPath := outpostWinRun, inWSL, outpostOnPath
	outpostWinRun, inWSL, outpostOnPath = win.run, func() bool { return true }, func() bool { return false }
	t.Cleanup(func() { outpostWinRun, inWSL, outpostOnPath = prevWin, prevWSL, prevPath })

	if _, err := ensureMeshForward(context.Background(), testGrant(), "172.20.0.1"); err != nil {
		t.Fatal(err)
	}
	if len(lin.calls) != 0 {
		t.Fatalf("Linux outpost used: %v", lin.calls)
	}
	if win.count("mesh consume add llm PEERID-DOOR --local 172.20.0.1:0") != 1 {
		t.Fatalf("Windows forward not opened on the gateway: %v", win.calls)
	}

	outpostOnPath = func() bool { return true }
	if _, err := ensureMeshForward(context.Background(), testGrant(), "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if lin.count("mesh consume ls") != 1 {
		t.Fatalf("Linux outpost not preferred when present: %v", lin.calls)
	}
}
