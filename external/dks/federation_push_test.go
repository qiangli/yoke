package dks

// httptest fakes for the push-mode satellite federation lifecycle: every
// step (exchange, install, status, kubeconfigs, member credential,
// Karmada apiserver) runs against local fakes. No live satellites, no real
// hostnames or secrets anywhere in this file.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSatellite is one satellite's API surface for push tests.
type fakeSatellite struct {
	mu sync.Mutex

	ownerToken     string
	exchangedToken string

	// Karmada kubeconfig served to the host role.
	karmadaServer  string // set after the karmada fake starts
	installHits    int
	installReady   bool // status reports Ready without an install POST
	convergeAfter  int  // status reports ready:false until this many status hits, then true (someone else's converging install)
	statusHits     int
	kubeconfigHits int
	uninstallHits  int
	memberServer   string // served inside the member credential kubeconfig
	credHits       int
	credRevoked    bool
	credCreated    []any

	hits []string // "METHOD path" log for dry-run zero-call assertions
}

func (f *fakeSatellite) log(r *http.Request) {
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
}

func (f *fakeSatellite) hitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

const (
	testOwnerToken     = "test-owner-bearer-aaa"
	testExchangedToken = "test-exchanged-bearer-bbb"
	testKarmadaToken   = "test-karmada-admin-token-ccc"
	testMemberToken    = "test-member-sa-token-ddd"
)

// testMemberCAData is a throwaway self-signed CA (base64 PEM), as the
// real member credential embeds the member apiserver CA.
var testMemberCAData = func() string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-member-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}()

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func memberKubeconfigYAML(server string) string {
	return "apiVersion: v1\nkind: Config\ncurrent-context: member\n" +
		"clusters:\n- name: member\n  cluster:\n    server: " + server + "\n    certificate-authority-data: " + testMemberCAData + "\n" +
		"users:\n- name: member-sa\n  user:\n    token: " + testMemberToken + "\n" +
		"contexts:\n- name: member\n  context:\n    cluster: member\n    user: member-sa\n"
}

func karmadaKubeconfigYAML(server string) string {
	return "apiVersion: v1\nkind: Config\ncurrent-context: karmada\n" +
		"clusters:\n- name: karmada\n  cluster:\n    server: " + server + "\n    insecure-skip-tls-verify: true\n" +
		"users:\n- name: karmada-admin\n  user:\n    token: " + testKarmadaToken + "\n" +
		"contexts:\n- name: karmada\n  context:\n    cluster: karmada\n    user: karmada-admin\n"
}

func (f *fakeSatellite) serveHost(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(r)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokens/exchange":
		if r.Header.Get("Authorization") != "Bearer "+f.ownerToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": f.exchangedToken, "scopes": []string{FederationPushScope}})
	case r.Method == http.MethodGet && r.URL.Path == pushPathStatus:
		f.statusHits++
		w.Header().Set("Content-Type", "application/json")
		// A converging install reports through the status endpoint from
		// the first probe, ready:false until convergeAfter hits.
		if f.convergeAfter > 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"ready": f.statusHits >= f.convergeAfter})
			return
		}
		// No install yet and no pre-existing control plane: the satellite
		// has nothing to report, like a real host before its first install.
		if !f.installReady && f.installHits == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": true})
	case r.Method == http.MethodPost && r.URL.Path == pushPathInstall:
		if f.installReady {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "already installed"})
			return
		}
		f.installHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"installed": true})
	case r.Method == http.MethodGet && r.URL.Path == pushPathKubeconfig:
		if r.Header.Get("Authorization") != "Bearer "+f.exchangedToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.kubeconfigHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"kubeconfig": karmadaKubeconfigYAML(f.karmadaServer)})
	case r.Method == http.MethodDelete && r.URL.Path == pushPathInstall:
		f.uninstallHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"uninstalled": true})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeSatellite) serveMember(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(r)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tokens/exchange":
		if r.Header.Get("Authorization") != "Bearer "+f.ownerToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": f.exchangedToken, "scopes": []string{FederationPushScope}})
	case r.Method == http.MethodPost && r.URL.Path == pushPathMemberCred:
		if r.Header.Get("Authorization") != "Bearer "+f.exchangedToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.credHits++
		f.credCreated = []any{
			"ServiceAccount/karmada-system/" + pushMemberSA,
			map[string]any{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding", "name": pushMemberSA},
			"Secret/karmada-system/" + pushMemberSA + "-token",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"kubeconfig": memberKubeconfigYAML(f.memberServer),
			"created":    f.credCreated,
		})
	case r.Method == http.MethodDelete && r.URL.Path == pushPathMemberCred:
		if r.Header.Get("Authorization") != "Bearer "+f.exchangedToken {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		f.credRevoked = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"revoked": true})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// fakeKarmada is the Karmada apiserver surface push registration needs.
type fakeKarmada struct {
	mu            sync.Mutex
	token         string
	secrets       map[string]map[string]any
	clusters      map[string]map[string]any
	hits          []string
	preexisting   map[string]bool // cluster names that exist before join
	deleteHits    int
	clustersAfter map[string]bool
}

func newFakeKarmada() *fakeKarmada {
	return &fakeKarmada{
		token:       testKarmadaToken,
		secrets:     map[string]map[string]any{},
		clusters:    map[string]map[string]any{},
		preexisting: map[string]bool{},
	}
}

func (f *fakeKarmada) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = append(f.hits, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	const clusterPrefix = "/apis/" + pushClusterAPIVer + "/clusters"
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/"+pushKarmadaNS+"/secrets":
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		f.secrets[name] = obj
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == http.MethodPost && r.URL.Path == clusterPrefix:
		var obj map[string]any
		_ = json.NewDecoder(r.Body).Decode(&obj)
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		if _, ok := f.clusters[name]; ok || f.preexisting[name] {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "already exists"})
			return
		}
		f.clusters[name] = obj
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj)
	case r.Method == http.MethodGet && r.URL.Path == clusterPrefix:
		items := []any{}
		for name := range f.preexisting {
			items = append(items, map[string]any{
				"metadata": map[string]any{"name": name},
				"spec":     map[string]any{"syncMode": "Push"},
				"status":   map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
			})
		}
		for _, obj := range f.clusters {
			items = append(items, withReady(obj))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	case (r.Method == http.MethodGet || r.Method == http.MethodDelete) && strings.HasPrefix(r.URL.Path, clusterPrefix+"/"):
		name := strings.TrimPrefix(r.URL.Path, clusterPrefix+"/")
		_, mine := f.clusters[name]
		_, pre := f.preexisting[name]
		if !mine && !pre {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			f.deleteHits++
			delete(f.clusters, name)
			delete(f.preexisting, name)
		}
		obj := f.clusters[name]
		if obj == nil {
			obj = map[string]any{"metadata": map[string]any{"name": name}}
		}
		_ = json.NewEncoder(w).Encode(withReady(obj))
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/"+pushKarmadaNS+"/secrets/"):
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/"+pushKarmadaNS+"/secrets/")
		if _, ok := f.secrets[name]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(f.secrets, name)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func withReady(obj map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range obj {
		out[k] = v
	}
	out["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	return out
}

// pushFixture wires host + member satellites and the Karmada apiserver.
type pushFixture struct {
	host    *fakeSatellite
	member  *fakeSatellite
	karmada *fakeKarmada
	hostSrv *httptest.Server
	membSrv *httptest.Server
	karmSrv *httptest.Server
}

func setupPushFixture(t *testing.T) *pushFixture {
	t.Helper()
	fx := &pushFixture{
		member:  &fakeSatellite{ownerToken: testOwnerToken, exchangedToken: testExchangedToken},
		karmada: newFakeKarmada(),
	}
	fx.karmSrv = httptest.NewServer(http.HandlerFunc(fx.karmada.serve))
	t.Cleanup(fx.karmSrv.Close)
	fx.host = &fakeSatellite{ownerToken: testOwnerToken, exchangedToken: testExchangedToken, karmadaServer: fx.karmSrv.URL}
	fx.hostSrv = httptest.NewServer(http.HandlerFunc(fx.host.serveHost))
	t.Cleanup(fx.hostSrv.Close)
	// The member server is the k8s.<sat-host> URL the credential carries;
	// any https URL works here since registration only records it.
	fx.member.memberServer = "https://k8s.example.invalid"
	fx.membSrv = httptest.NewServer(http.HandlerFunc(fx.member.serveMember))
	t.Cleanup(fx.membSrv.Close)

	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	oldInterval := pushStatusInterval
	pushStatusInterval = 5 * time.Millisecond
	t.Cleanup(func() { pushStatusInterval = oldInterval })
	return fx
}

func (fx *pushFixture) joinArgs(extra ...string) []string {
	return append([]string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL,
		"--member-satellite", fx.membSrv.URL,
		"--name", "alien",
		"--token", testOwnerToken,
		"--wait", "5s",
	}, extra...)
}

func runPushCmd(t *testing.T, sub string, args []string) (string, error) {
	t.Helper()
	cmd := NewFederationCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(append([]string{sub}, args...))
	err := cmd.Execute()
	return outBuf.String() + errBuf.String(), err
}

func mustNotLeak(t *testing.T, out string, values ...string) {
	t.Helper()
	for _, v := range values {
		if v != "" && strings.Contains(out, v) {
			t.Fatalf("output leaks a secret value:\n%s", out)
		}
	}
}

func TestPushJoinFullLifecycle(t *testing.T) {
	fx := setupPushFixture(t)
	out, err := runPushCmd(t, "join", fx.joinArgs())
	if err != nil {
		t.Fatalf("join failed: %v (%s)", err, out)
	}
	if !strings.Contains(out, `"alien"`) || !strings.Contains(out, "joined") {
		t.Fatalf("join output missing confirmation: %q", out)
	}
	mustNotLeak(t, out, testOwnerToken, testExchangedToken, testKarmadaToken, testMemberToken)

	// Baseline record: refs only, never secrets.
	rec, path, err := pushReadBaseline(fx.hostSrv.URL, "alien")
	if err != nil {
		t.Fatalf("baseline missing: %v", err)
	}
	if !rec.KarmadaInstalled {
		t.Fatalf("baseline KarmadaInstalled = false, want true (this join installed it)")
	}
	if len(rec.HostCreated) != 2 {
		t.Fatalf("hostCreated = %v, want the member secret + Cluster refs", rec.HostCreated)
	}
	if len(rec.MemberCreated) != 3 {
		t.Fatalf("memberCreated = %v, want the 3 member refs", rec.MemberCreated)
	}
	raw, _ := os.ReadFile(path)
	mustNotLeak(t, string(raw), testOwnerToken, testExchangedToken, testKarmadaToken, testMemberToken)
	if fi, _ := os.Stat(path); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("baseline mode = %v, want 0600", fi.Mode().Perm())
	}

	// Karmada side: Push Cluster with secretRef + apiEndpoint.
	fx.karmada.mu.Lock()
	defer fx.karmada.mu.Unlock()
	obj, ok := fx.karmada.clusters["alien"]
	if !ok {
		t.Fatalf("karmada clusters = %v, want alien registered", fx.karmada.clusters)
	}
	spec, _ := obj["spec"].(map[string]any)
	if spec["syncMode"] != "Push" {
		t.Fatalf("cluster syncMode = %v, want Push", spec["syncMode"])
	}
	if spec["apiEndpoint"] != "https://k8s.example.invalid" {
		t.Fatalf("cluster apiEndpoint = %v, want the member k8s server", spec["apiEndpoint"])
	}
	ref, _ := spec["secretRef"].(map[string]any)
	if ref["name"] != "alien-kubeconfig" || ref["namespace"] != pushKarmadaNS {
		t.Fatalf("cluster secretRef = %v, want karmada-system/alien-kubeconfig", ref)
	}
	sec, ok := fx.karmada.secrets["alien-kubeconfig"]
	if !ok {
		t.Fatalf("member secret missing: %v", fx.karmada.secrets)
	}
	// Karmada's Push client reads "token" and "caBundle" from the secretRef
	// (S490.8: a kubeconfig-only Secret left the Cluster StatusCollectionFailed).
	data, _ := sec["data"].(map[string]any)
	for _, key := range []string{"token", "caBundle"} {
		v, _ := data[key].(string)
		if raw, err := base64.StdEncoding.DecodeString(v); err != nil || len(raw) == 0 {
			t.Fatalf("member secret lacks a non-empty %q (keys %v)", key, keysOf(data))
		}
	}
	if fx.host.installHits != 1 {
		t.Fatalf("install hits = %d, want 1", fx.host.installHits)
	}
	if fx.member.credHits != 1 {
		t.Fatalf("member-credential hits = %d, want 1", fx.member.credHits)
	}
}

func TestPushJoinSkipsInstallWhenAlreadyReady(t *testing.T) {
	fx := setupPushFixture(t)
	fx.host.installReady = true
	out, err := runPushCmd(t, "join", fx.joinArgs())
	if err != nil {
		t.Fatalf("join failed: %v (%s)", err, out)
	}
	if fx.host.installHits != 0 {
		t.Fatalf("install hits = %d, want 0 (Karmada was already Ready)", fx.host.installHits)
	}
	rec, _, err := pushReadBaseline(fx.hostSrv.URL, "alien")
	if err != nil {
		t.Fatalf("baseline missing: %v", err)
	}
	if rec.KarmadaInstalled {
		t.Fatalf("baseline KarmadaInstalled = true, want false (pre-existing control plane)")
	}
}

func TestPushJoinAdoptsConvergingKarmada(t *testing.T) {
	fx := setupPushFixture(t)
	fx.host.convergeAfter = 3 // present but not Ready at first probe
	out, err := runPushCmd(t, "join", fx.joinArgs())
	if err != nil {
		t.Fatalf("join failed: %v (%s)", err, out)
	}
	if fx.host.installHits != 0 {
		t.Fatalf("install hits = %d, want 0 (someone else's install was converging)", fx.host.installHits)
	}
	rec, _, err := pushReadBaseline(fx.hostSrv.URL, "alien")
	if err != nil {
		t.Fatalf("baseline missing: %v", err)
	}
	if rec.KarmadaInstalled {
		t.Fatalf("baseline KarmadaInstalled = true, want false (adopted a converging control plane)")
	}
	if !strings.Contains(out, "converging") {
		t.Fatalf("join output should name the converging plane: %q", out)
	}
}

func TestPushJoinRefusesPreexistingCluster(t *testing.T) {
	fx := setupPushFixture(t)
	fx.karmada.preexisting["alien"] = true
	out, err := runPushCmd(t, "join", fx.joinArgs())
	if err == nil {
		t.Fatalf("join adopted a pre-existing Cluster, want refusal (out=%s)", out)
	}
	if !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), "refusing to adopt") {
		t.Fatalf("err = %q, want the no-adopt refusal", err.Error())
	}
	// No baseline may record objects this tool does not own.
	if _, _, rerr := pushReadBaseline(fx.hostSrv.URL, "alien"); rerr == nil {
		t.Fatalf("baseline written for a refused join")
	}
}

func TestPushJoinDryRunMakesZeroCalls(t *testing.T) {
	fx := setupPushFixture(t)
	out, err := runPushCmd(t, "join", fx.joinArgs("--dry-run"))
	if err != nil {
		t.Fatalf("dry-run failed: %v (%s)", err, out)
	}
	if got := fx.host.hitCount() + fx.member.hitCount(); got != 0 {
		t.Fatalf("dry-run made %d satellite calls, want zero", got)
	}
	for _, want := range []string{"exchange", "ensure karmada", "dhnt-karmada-member", "register Karmada Cluster", "baseline"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Fatalf("dry-run plan missing step %q:\n%s", want, out)
		}
	}
	mustNotLeak(t, out, testOwnerToken, testExchangedToken, testKarmadaToken, testMemberToken)
	if strings.Contains(out, "--token "+testOwnerToken) {
		t.Fatalf("dry-run echoes the token flag value:\n%s", out)
	}
}

func TestPushLeaveRemovesExactlyTheBaseline(t *testing.T) {
	fx := setupPushFixture(t)
	if out, err := runPushCmd(t, "join", fx.joinArgs()); err != nil {
		t.Fatalf("join failed: %v (%s)", err, out)
	}
	out, err := runPushCmd(t, "leave", []string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL,
		"--name", "alien",
		"--token", testOwnerToken,
	})
	if err != nil {
		t.Fatalf("leave failed: %v (%s)", err, out)
	}
	if !fx.member.credRevoked {
		t.Fatalf("member credential was not revoked")
	}
	fx.karmada.mu.Lock()
	_, clusterLeft := fx.karmada.clusters["alien"]
	_, preLeft := fx.karmada.preexisting["alien"]
	_, secretLeft := fx.karmada.secrets["alien-kubeconfig"]
	fx.karmada.mu.Unlock()
	if clusterLeft || preLeft || secretLeft {
		t.Fatalf("karmada objects left behind (cluster=%v secret=%v)", clusterLeft || preLeft, secretLeft)
	}
	if _, _, err := pushReadBaseline(fx.hostSrv.URL, "alien"); !os.IsNotExist(err) {
		t.Fatalf("baseline not cleared: %v", err)
	}
	// Karmada itself stays: no uninstall call without --uninstall.
	if fx.host.uninstallHits != 0 {
		t.Fatalf("uninstall hits = %d, want 0 without --uninstall", fx.host.uninstallHits)
	}
	if !strings.Contains(out, "Karmada left in place") {
		t.Fatalf("leave output must say Karmada stays: %q", out)
	}
	mustNotLeak(t, out, testOwnerToken, testExchangedToken, testKarmadaToken, testMemberToken)
}

func TestPushLeaveUninstallOnlyWhenToolInstalled(t *testing.T) {
	fx := setupPushFixture(t)
	if _, err := runPushCmd(t, "join", fx.joinArgs()); err != nil {
		t.Fatalf("join failed: %v", err)
	}
	out, err := runPushCmd(t, "leave", []string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL, "--name", "alien",
		"--token", testOwnerToken, "--uninstall",
	})
	if err != nil {
		t.Fatalf("leave --uninstall failed: %v (%s)", err, out)
	}
	if fx.host.uninstallHits != 1 {
		t.Fatalf("uninstall hits = %d, want 1 (this tool installed Karmada)", fx.host.uninstallHits)
	}

	// Second pair: pre-existing Karmada must survive --uninstall.
	fx2 := setupPushFixture(t)
	fx2.host.installReady = true
	if _, err := runPushCmd(t, "join", []string{
		"--mode", "push",
		"--host-satellite", fx2.hostSrv.URL, "--member-satellite", fx2.membSrv.URL,
		"--name", "alien", "--token", testOwnerToken, "--wait", "5s",
	}); err != nil {
		t.Fatalf("join (pre-existing) failed: %v", err)
	}
	out, err = runPushCmd(t, "leave", []string{
		"--mode", "push",
		"--host-satellite", fx2.hostSrv.URL, "--name", "alien",
		"--token", testOwnerToken, "--uninstall",
	})
	if err != nil {
		t.Fatalf("leave --uninstall (pre-existing) failed: %v (%s)", err, out)
	}
	if fx2.host.uninstallHits != 0 {
		t.Fatalf("uninstall hits = %d, want 0 for pre-existing Karmada", fx2.host.uninstallHits)
	}
}

func TestPushLeaveRefusesWithoutBaseline(t *testing.T) {
	fx := setupPushFixture(t)
	// Even with a live-looking member, no baseline means no removals.
	fx.karmada.preexisting["alien"] = true
	_, err := runPushCmd(t, "leave", []string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL, "--member-satellite", fx.membSrv.URL,
		"--token", testOwnerToken,
	})
	if err == nil {
		t.Fatal("leave without a baseline succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "no baseline") {
		t.Fatalf("err = %q, want the no-baseline refusal", err.Error())
	}
	if got := fx.host.hitCount() + fx.member.hitCount(); got != 0 {
		t.Fatalf("refused leave made %d satellite calls, want zero", got)
	}
	fx.karmada.mu.Lock()
	defer fx.karmada.mu.Unlock()
	if !fx.karmada.preexisting["alien"] {
		t.Fatalf("refused leave deleted a pre-existing Cluster")
	}
}

func TestPushStatusListsMembersAndReadiness(t *testing.T) {
	fx := setupPushFixture(t)
	if _, err := runPushCmd(t, "join", fx.joinArgs()); err != nil {
		t.Fatalf("join failed: %v", err)
	}
	out, err := runPushCmd(t, "status", []string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL,
		"--token", testOwnerToken,
	})
	if err != nil {
		t.Fatalf("status failed: %v (%s)", err, out)
	}
	for _, want := range []string{"alien", "True", "Push"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
	mustNotLeak(t, out, testOwnerToken, testExchangedToken, testKarmadaToken, testMemberToken)

	// JSON shape carries the managed marker.
	out, err = runPushCmd(t, "status", []string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL,
		"--token", testOwnerToken, "--json",
	})
	if err != nil {
		t.Fatalf("status --json failed: %v (%s)", err, out)
	}
	var env struct {
		Members []pushMemberSnapshot `json:"members"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("status --json did not parse: %v (%s)", err, out)
	}
	if len(env.Members) != 1 || env.Members[0].Name != "alien" || !env.Members[0].Managed {
		t.Fatalf("status members = %+v, want the managed alien member", env.Members)
	}
}

func TestPushRequiresSatellitesAndToken(t *testing.T) {
	fx := setupPushFixture(t)
	// Missing --host-satellite.
	if _, err := runPushCmd(t, "join", []string{"--mode", "push", "--member-satellite", fx.membSrv.URL, "--token", testOwnerToken}); err == nil {
		t.Fatal("join without --host-satellite succeeded, want an error")
	}
	// Missing member selector on join.
	if _, err := runPushCmd(t, "join", []string{"--mode", "push", "--host-satellite", fx.hostSrv.URL, "--token", testOwnerToken}); err == nil {
		t.Fatal("join without member succeeded, want an error")
	}
	// Missing token everywhere.
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	if _, err := runPushCmd(t, "join", []string{"--mode", "push", "--host-satellite", fx.hostSrv.URL, "--member-satellite", fx.membSrv.URL, "--token", ""}); err == nil {
		t.Fatal("join without a token succeeded, want an error")
	}
	// Pull mode is untouched: no --mode flag still execs outpost.
	calls, _, _, err := runFederationCmd(t, []string{"status"})
	if err != nil {
		t.Fatalf("pull status failed: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("pull status outpost calls = %v, want the passthrough", calls)
	}
}

func TestPushDefaultNameFromMemberHost(t *testing.T) {
	if got := defaultClusterName("https://member-7.example.invalid"); got != "member-7" {
		t.Fatalf("defaultClusterName = %q, want member-7", got)
	}
	if got := defaultClusterName("https://127.0.0.1:43123"); got == "" || !strings.Contains(got, "43123") {
		t.Fatalf("defaultClusterName = %q, want the port to disambiguate loopback members", got)
	}
	// Join without --name records under the derived name.
	fx := setupPushFixture(t)
	out, err := runPushCmd(t, "join", []string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL, "--member-satellite", fx.membSrv.URL,
		"--token", testOwnerToken, "--wait", "5s",
	})
	if err != nil {
		t.Fatalf("join failed: %v (%s)", err, out)
	}
	want := defaultClusterName(fx.membSrv.URL)
	if _, _, err := pushReadBaseline(fx.hostSrv.URL, want); err != nil {
		t.Fatalf("baseline under derived name %q missing: %v", want, err)
	}
	// Leave via --member-satellite resolves the same derived name.
	out, err = runPushCmd(t, "leave", []string{
		"--mode", "push",
		"--host-satellite", fx.hostSrv.URL, "--member-satellite", fx.membSrv.URL,
		"--token", testOwnerToken,
	})
	if err != nil {
		t.Fatalf("leave by member satellite failed: %v (%s)", err, out)
	}
	if _, _, err := pushReadBaseline(fx.hostSrv.URL, want); !os.IsNotExist(err) {
		t.Fatalf("baseline under derived name %q not cleared: %v", want, err)
	}
}

func TestPushExchangeRefusalStaysSecretFree(t *testing.T) {
	fx := setupPushFixture(t)
	fx.host.ownerToken = "something-else"
	out, err := runPushCmd(t, "join", fx.joinArgs())
	if err == nil {
		t.Fatal("join with a bad owner token succeeded, want refusal")
	}
	mustNotLeak(t, out+err.Error(), testOwnerToken, testExchangedToken)
}

func TestPushContextCancellation(t *testing.T) {
	fx := setupPushFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := pushOptions{
		hostSatellite:   fx.hostSrv.URL,
		memberSatellite: fx.membSrv.URL,
		name:            "alien",
		scopes:          []string{FederationPushScope},
		ttl:             pushExchangeTTL,
		wait:            5 * time.Second,
	}
	t.Setenv("BASHY_FLEET_TOKEN", testOwnerToken)
	owner, _, _ := pushResolveToken("")
	o.tokenOverride = owner
	if err := pushJoin(ctx, io.Discard, o); err == nil {
		t.Fatal("cancelled join succeeded, want ctx error")
	}
}
