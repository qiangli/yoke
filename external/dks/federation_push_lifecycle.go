// Push-mode join/leave/status flows plus the minimal Karmada apiserver
// client (Secret + Cluster create/get/list/delete over HTTPS, auth from a
// kubeconfig). Companion to federation_push.go, which owns flags, satellite
// calls, naming and the baseline record.

package dks

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v3"
)

// kubeUser is one kubeconfig user entry: token or client certificate.
type kubeUser struct {
	Token          string `yaml:"token"`
	ClientCertData string `yaml:"client-certificate-data"`
	ClientKeyData  string `yaml:"client-key-data"`
	ClientCertFile string `yaml:"client-certificate"`
	ClientKeyFile  string `yaml:"client-key"`
}

// kubeClusterEntry is one kubeconfig cluster entry.
type kubeClusterEntry struct {
	Server   string `yaml:"server"`
	CAData   string `yaml:"certificate-authority-data"`
	CAFile   string `yaml:"certificate-authority"`
	Insecure bool   `yaml:"insecure-skip-tls-verify"`
}

// kubeconfigFile is the subset of a kubeconfig this tool reads: the
// current context's cluster server + CA and its user's credentials.
type kubeconfigFile struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string           `yaml:"name"`
		Cluster kubeClusterEntry `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string   `yaml:"name"`
		User kubeUser `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// karmadaCreds are the resolved dial parameters for a Karmada apiserver.
type karmadaCreds struct {
	server string
	caPool *x509.CertPool
	cert   *tls.Certificate
	token  string
	caPEM  []byte // the CA bundle as PEM, when the kubeconfig carries one
}

// parseKubeconfig decodes kubeconfig YAML into dial parameters for the
// current context. It supports token and client-certificate users and a
// CA bundle, file, or (explicitly marked) insecure cluster.
func parseKubeconfig(raw []byte) (karmadaCreds, error) {
	var kc kubeconfigFile
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		return karmadaCreds{}, fmt.Errorf("dks federation --mode push: decode kubeconfig: %w", err)
	}
	wantCtx := kc.CurrentContext
	ctxName, clusterName, userName := "", "", ""
	if len(kc.Contexts) == 0 && len(kc.Clusters) == 1 && len(kc.Users) == 1 {
		clusterName, userName = kc.Clusters[0].Name, kc.Users[0].Name
	} else {
		for _, c := range kc.Contexts {
			if wantCtx == "" || c.Name == wantCtx {
				ctxName, clusterName, userName = c.Name, c.Context.Cluster, c.Context.User
				break
			}
		}
		if ctxName == "" {
			return karmadaCreds{}, fmt.Errorf("dks federation --mode push: kubeconfig has no current context %q", wantCtx)
		}
	}
	var cl *kubeClusterEntry
	for i := range kc.Clusters {
		if kc.Clusters[i].Name == clusterName {
			cl = &kc.Clusters[i].Cluster
			break
		}
	}
	if cl == nil {
		return karmadaCreds{}, fmt.Errorf("dks federation --mode push: kubeconfig has no cluster %q", clusterName)
	}
	var u *kubeUser
	for i := range kc.Users {
		if kc.Users[i].Name == userName {
			u = &kc.Users[i].User
			break
		}
	}
	if u == nil {
		return karmadaCreds{}, fmt.Errorf("dks federation --mode push: kubeconfig has no user %q", userName)
	}
	server := strings.TrimSpace(cl.Server)
	if server == "" {
		return karmadaCreds{}, fmt.Errorf("dks federation --mode push: kubeconfig cluster %q has no server", clusterName)
	}
	var creds karmadaCreds
	creds.server = strings.TrimRight(server, "/")
	switch {
	case cl.Insecure:
		creds.caPool = nil // nil pool + InsecureSkipVerify is set by the caller
	case cl.CAData != "":
		der, err := base64.StdEncoding.DecodeString(cl.CAData)
		if err != nil {
			return creds, fmt.Errorf("dks federation --mode push: decode cluster CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(der) {
			return creds, fmt.Errorf("dks federation --mode push: cluster CA did not parse as PEM")
		}
		creds.caPool = pool
		creds.caPEM = der
	case cl.CAFile != "":
		pem, err := os.ReadFile(cl.CAFile)
		if err != nil {
			return creds, fmt.Errorf("dks federation --mode push: read cluster CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return creds, fmt.Errorf("dks federation --mode push: cluster CA file did not parse as PEM")
		}
		creds.caPEM = pem
		creds.caPool = pool
	}
	switch {
	case u.Token != "":
		creds.token = u.Token
	case u.ClientCertData != "" && u.ClientKeyData != "":
		certPEM, err := base64.StdEncoding.DecodeString(u.ClientCertData)
		if err != nil {
			return creds, fmt.Errorf("dks federation --mode push: decode client certificate: %w", err)
		}
		keyPEM, err := base64.StdEncoding.DecodeString(u.ClientKeyData)
		if err != nil {
			return creds, fmt.Errorf("dks federation --mode push: decode client key: %w", err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return creds, fmt.Errorf("dks federation --mode push: load client certificate: %w", err)
		}
		creds.cert = &cert
	case u.ClientCertFile != "" && u.ClientKeyFile != "":
		cert, err := tls.LoadX509KeyPair(u.ClientCertFile, u.ClientKeyFile)
		if err != nil {
			return creds, fmt.Errorf("dks federation --mode push: load client certificate files: %w", err)
		}
		creds.cert = &cert
	default:
		return creds, fmt.Errorf("dks federation --mode push: kubeconfig user %q has neither token nor client certificate", userName)
	}
	return creds, nil
}

// karmadaClient speaks the Karmada apiserver HTTPS API with credentials
// from a kubeconfig. Only the objects push registration needs: a Secret
// holding the member kubeconfig and the Cluster object itself.
type karmadaClient struct {
	server string
	token  string // Karmada credential; never printed
	http   *http.Client
}

// newKarmadaClient builds the client. Plain-HTTP servers (tests) dial as-is;
// HTTPS verifies against the kubeconfig CA, or the platform roots when the
// kubeconfig carries none.
func newKarmadaClient(creds karmadaCreds, insecure bool) *karmadaClient {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12} //nolint:gosec // min version pinned
	if insecure {
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // tests only: httptest TLS servers
	} else if creds.caPool != nil {
		tlsCfg.RootCAs = creds.caPool
	}
	if creds.cert != nil {
		tlsCfg.Certificates = []tls.Certificate{*creds.cert}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsCfg
	return &karmadaClient{
		server: creds.server,
		token:  creds.token,
		http:   &http.Client{Timeout: 20 * time.Second, Transport: tr},
	}
}

// karmadaInsecureTLS forces InsecureSkipVerify on Karmada apiserver dials.
// Tests set it for httptest.NewTLSServer; production code leaves it false.
var karmadaInsecureTLS = false

// do performs one Karmada API call. wantStatus lists acceptable 2xx codes;
// a 404 returns (nil, ErrKarmadaNotFound) so callers can tolerate absence.
func (c *karmadaClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = strings.NewReader(string(raw))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dks federation --mode push: karmada %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, pushMaxBody))
	if err != nil {
		return nil, fmt.Errorf("dks federation --mode push: karmada %s %s: read: %w", method, path, err)
	}
	switch {
	case resp.StatusCode/100 == 2:
		return raw, nil
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrKarmadaNotFound
	case resp.StatusCode == http.StatusConflict:
		return nil, ErrKarmadaExists
	default:
		return nil, fmt.Errorf("dks federation --mode push: karmada %s %s: %d %s", method, path, resp.StatusCode, pushTrimBody(raw))
	}
}

// ErrKarmadaNotFound marks a tolerated 404 from the Karmada apiserver;
// ErrKarmadaExists marks a 409 (a pre-existing object this tool refuses to
// adopt).
var (
	ErrKarmadaNotFound = karmadaStatusError("not found")
	ErrKarmadaExists   = karmadaStatusError("already exists")
)

type karmadaStatusError string

func (e karmadaStatusError) Error() string {
	return "dks federation --mode push: karmada object " + string(e)
}

// memberSecretName is the karmada-system Secret holding a member kubeconfig.
func memberSecretName(clusterName string) string { return clusterName + "-kubeconfig" }

// createMemberSecret stores the member credential the Push Cluster's
// secretRef points at. Karmada builds its member client from the keys
// "token" and "caBundle" (what karmadactl join writes); a Secret holding
// only a kubeconfig left the Cluster at StatusCollectionFailed "missing a
// non-empty value for token" (S490.8). The kubeconfig is kept alongside.
func (c *karmadaClient) createMemberSecret(ctx context.Context, clusterName string, memberKubeconfig []byte) (string, error) {
	name := memberSecretName(clusterName)
	creds, err := parseKubeconfig(memberKubeconfig)
	if err != nil {
		return "", err
	}
	if creds.token == "" {
		return "", fmt.Errorf("dks federation --mode push: member kubeconfig for %s has no bearer token", clusterName)
	}
	data := map[string]string{
		pushKubeconfigKey: base64.StdEncoding.EncodeToString(memberKubeconfig),
		"token":           base64.StdEncoding.EncodeToString([]byte(creds.token)),
	}
	if len(creds.caPEM) > 0 {
		data["caBundle"] = base64.StdEncoding.EncodeToString(creds.caPEM)
	}
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      name,
			"namespace": pushKarmadaNS,
			"labels":    map[string]string{"dhnt.io/federation-push": "true"},
		},
		"type": "Opaque",
		"data": data,
	}
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/namespaces/"+pushKarmadaNS+"/secrets", obj); err != nil {
		return "", err
	}
	return "Secret/" + pushKarmadaNS + "/" + name, nil
}

// createCluster registers the member in Push mode. A 409 means a
// pre-existing Cluster the tool must not adopt — the caller turns it into a
// refusal naming the object.
func (c *karmadaClient) createCluster(ctx context.Context, clusterName, memberServer string) (string, error) {
	obj := map[string]any{
		"apiVersion": pushClusterAPIVer,
		"kind":       "Cluster",
		"metadata": map[string]any{
			"name":   clusterName,
			"labels": map[string]string{"dhnt.io/federation-push": "true"},
		},
		"spec": map[string]any{
			"syncMode":    pushSyncMode,
			"apiEndpoint": memberServer,
			"secretRef": map[string]any{
				"namespace": pushKarmadaNS,
				"name":      memberSecretName(clusterName),
			},
		},
	}
	if _, err := c.do(ctx, http.MethodPost, "/apis/"+pushClusterAPIVer+"/clusters", obj); err != nil {
		return "", err
	}
	return "clusters." + pushClusterAPIVer + "/" + clusterName, nil
}

// karmadaCluster is the list/status subset push mode reads.
type karmadaCluster struct {
	Name     string
	SyncMode string
	Ready    string // "True" / "False" / "Unknown"
}

func clusterReadyOf(raw map[string]any) string {
	status, _ := raw["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	ready := "Unknown"
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" {
			if s, _ := m["status"].(string); s != "" {
				ready = s
			}
		}
	}
	return ready
}

func parseClusterList(raw []byte) ([]karmadaCluster, error) {
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("dks federation --mode push: decode cluster list: %w", err)
	}
	out := make([]karmadaCluster, 0, len(env.Items))
	for _, it := range env.Items {
		meta, _ := it["metadata"].(map[string]any)
		spec, _ := it["spec"].(map[string]any)
		name, _ := meta["name"].(string)
		mode, _ := spec["syncMode"].(string)
		out = append(out, karmadaCluster{Name: name, SyncMode: mode, Ready: clusterReadyOf(it)})
	}
	return out, nil
}

// listClusters returns every registered member Cluster.
func (c *karmadaClient) listClusters(ctx context.Context) ([]karmadaCluster, error) {
	raw, err := c.do(ctx, http.MethodGet, "/apis/"+pushClusterAPIVer+"/clusters", nil)
	if err != nil {
		return nil, err
	}
	return parseClusterList(raw)
}

// deleteCluster unregisters the member; absence is success (idempotent).
func (c *karmadaClient) deleteCluster(ctx context.Context, clusterName string) (bool, error) {
	_, err := c.do(ctx, http.MethodDelete, "/apis/"+pushClusterAPIVer+"/clusters/"+clusterName, nil)
	if err == ErrKarmadaNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// deleteMemberSecret removes the member kubeconfig Secret; absence is success.
func (c *karmadaClient) deleteMemberSecret(ctx context.Context, clusterName string) (bool, error) {
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+pushKarmadaNS+"/secrets/"+memberSecretName(clusterName), nil)
	if err == ErrKarmadaNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// decodeKubeconfigResponse accepts the two plausible kubeconfig response
// shapes — raw kubeconfig YAML, or a JSON envelope carrying it under
// "kubeconfig" (or "data") — and returns the YAML bytes.
func decodeKubeconfigResponse(raw []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("dks federation --mode push: empty kubeconfig response")
	}
	if strings.HasPrefix(trimmed, "{") {
		var env map[string]any
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, fmt.Errorf("dks federation --mode push: decode kubeconfig envelope: %w", err)
		}
		for _, key := range []string{"kubeconfig", "data", "kubeconfigYaml", "kubeconfig_yaml"} {
			if s, _ := env[key].(string); strings.TrimSpace(s) != "" {
				return []byte(s), nil
			}
		}
		return nil, fmt.Errorf("dks federation --mode push: kubeconfig envelope carries no kubeconfig field")
	}
	return []byte(trimmed + "\n"), nil
}

// normalizeResourceRefs renders the member-credential "created" refs —
// strings or {kind,namespace,name}-ish objects — as stable one-line refs for
// the baseline record.
func normalizeResourceRefs(v any) []string {
	var out []string
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			out = append(out, normalizeOneRef(e))
		}
	case []string:
		out = append(out, t...)
	case string:
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func normalizeOneRef(e any) string {
	if s, ok := e.(string); ok {
		return s
	}
	m, ok := e.(map[string]any)
	if !ok {
		return "unknown"
	}
	kind, _ := m["kind"].(string)
	ns, _ := m["namespace"].(string)
	name, _ := m["name"].(string)
	if api, _ := m["apiVersion"].(string); api != "" && kind != "" {
		kind = api + "/" + kind
	}
	ref := kind
	if ns != "" {
		ref += "/" + ns
	}
	if name != "" {
		ref += "/" + name
	}
	if ref == "" {
		if s, _ := m["ref"].(string); s != "" {
			return s
		}
		return "unknown"
	}
	return ref
}

// karmadaReadyOf interprets the host readiness probe. Accepted shapes:
// {"ready":true}, {"phase":"Ready"}, {"status":"Ready"},
// {"condition":"Ready"} (case-insensitive), or {"available":true}.
func karmadaReadyOf(raw []byte) bool {
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		return false
	}
	if b, ok := env["ready"].(bool); ok {
		return b
	}
	if b, ok := env["available"].(bool); ok {
		return b
	}
	for _, key := range []string{"phase", "status", "condition", "state"} {
		if s, ok := env[key].(string); ok && strings.EqualFold(strings.TrimSpace(s), "ready") {
			return true
		}
	}
	return false
}

// waitKarmadaReady polls GET status until Ready or the deadline. When the
// satellite has no status endpoint yet (404/405), readiness falls back to
// the in-contract kubeconfig endpoint: a 200 there means the control plane
// materialised its admin kubeconfig.
func waitKarmadaReady(ctx context.Context, host *pushSatellite, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	statusOK := true
	for {
		raw, status, err := host.do(ctx, http.MethodGet, pushPathStatus, nil)
		if err == nil {
			if karmadaReadyOf(raw) {
				return nil
			}
		} else if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			statusOK = false
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("dks federation --mode push: timed out after %s waiting for Karmada Ready on host %s (GET %s)", wait, pushHostOf(host.baseURL), pushPathStatus)
		}
		if err := pushSleep(ctx, pushStatusInterval); err != nil {
			return err
		}
	}
	if !statusOK {
		for {
			if _, _, err := host.do(ctx, http.MethodGet, pushPathKubeconfig, nil); err == nil {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("dks federation --mode push: timed out after %s waiting for Karmada Ready on host %s (GET %s; no status endpoint yet)", wait, pushHostOf(host.baseURL), pushPathKubeconfig)
			}
			if err := pushSleep(ctx, pushStatusInterval); err != nil {
				return err
			}
		}
	}
	return nil
}

func pushSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pushJoin implements `federation join --mode push`.
func pushJoin(ctx context.Context, out io.Writer, o pushOptions) error {
	ownerToken, tokenSource, err := pushResolveToken(o.tokenOverride)
	if err != nil {
		return err
	}
	name := o.name
	if name == "" {
		name = defaultClusterName(o.memberSatellite)
	}
	if o.dryRun {
		pushPrintJoinPlan(out, o, name, tokenSource)
		return nil
	}
	host, err := pushDial(o.hostSatellite, ownerToken, o.scopes, o.ttl)
	if err != nil {
		return err
	}
	member, err := pushDial(o.memberSatellite, ownerToken, o.scopes, o.ttl)
	if err != nil {
		return err
	}
	ownerToken = ""

	// 1. Probe before installing: a Karmada this tool did not install must
	// never be recorded as installed-by-us (leave --uninstall relies on
	// it). Present-but-not-Ready means someone else's install is still
	// converging: skip our POST and wait for it instead of reinstalling.
	installedByUs := false
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	probeRaw, _, probeErr := host.do(probeCtx, http.MethodGet, pushPathStatus, nil)
	cancel()
	if probeErr == nil {
		if karmadaReadyOf(probeRaw) {
			fmt.Fprintf(out, "federation push: Karmada already Ready on host %s (pre-existing; not installed by this tool)\n", pushHostOf(host.baseURL))
		} else {
			fmt.Fprintf(out, "federation push: Karmada converging on host %s (pre-existing install; waiting)\n", pushHostOf(host.baseURL))
		}
	}
	if probeErr != nil {
		// Nothing installed (404/405) or the probe failed another way:
		// ensure via install, which is idempotent server-side. A 409
		// marks a pre-existing control plane this tool must not claim.
		_, installStatus, installErr := host.do(ctx, http.MethodPost, pushPathInstall, map[string]any{})
		if installErr != nil {
			if installStatus == http.StatusConflict {
				installedByUs = false
			} else {
				return fmt.Errorf("dks federation --mode push: ensure Karmada on host %s: %v", pushHostOf(host.baseURL), installErr)
			}
		} else {
			installedByUs = true
		}
	}
	if err := waitKarmadaReady(ctx, host, o.wait); err != nil {
		return err
	}

	// 2. Host Karmada admin kubeconfig (server https://karmada.<sat-host>).
	rawCfg, _, err := host.do(ctx, http.MethodGet, pushPathKubeconfig, nil)
	if err != nil {
		return fmt.Errorf("dks federation --mode push: get host Karmada kubeconfig: %v", err)
	}
	cfgYAML, err := decodeKubeconfigResponse(rawCfg)
	if err != nil {
		return err
	}
	creds, err := parseKubeconfig(cfgYAML)
	if err != nil {
		return err
	}
	karmada := newKarmadaClient(creds, karmadaInsecureTLS)

	// 3. Member credential (SA + binding + token Secret live on the member).
	rawCred, _, err := member.do(ctx, http.MethodPost, pushPathMemberCred, map[string]any{})
	if err != nil {
		return fmt.Errorf("dks federation --mode push: mint member credential on %s: %v", pushHostOf(member.baseURL), err)
	}
	var credEnv struct {
		Kubeconfig string `json:"kubeconfig"`
		Data       string `json:"data"`
		Created    any    `json:"created"`
	}
	if err := json.Unmarshal(rawCred, &credEnv); err != nil {
		return fmt.Errorf("dks federation --mode push: decode member credential: %w", err)
	}
	memberCfg := credEnv.Kubeconfig
	if memberCfg == "" {
		memberCfg = credEnv.Data
	}
	if strings.TrimSpace(memberCfg) == "" {
		return fmt.Errorf("dks federation --mode push: member credential on %s returned no kubeconfig", pushHostOf(member.baseURL))
	}
	memberCreds, err := parseKubeconfig([]byte(memberCfg))
	if err != nil {
		return fmt.Errorf("dks federation --mode push: decode member kubeconfig: %v", err)
	}
	memberRefs := normalizeResourceRefs(credEnv.Created)
	if len(memberRefs) == 0 {
		// The contract names the three objects; a server that omits the
		// listing still created them, so record the canonical refs.
		memberRefs = []string{
			"ServiceAccount/default/" + pushMemberSA,
			"ClusterRoleBinding//" + pushMemberSA,
			"Secret/default/" + pushMemberSA + "-token",
		}
	}

	// 4. Register the Push Cluster (secret first: the Cluster points at it).
	if _, err := karmada.do(ctx, http.MethodGet, "/apis/"+pushClusterAPIVer+"/clusters/"+name, nil); err == nil {
		return fmt.Errorf("dks federation --mode push: Karmada Cluster %q already exists on host %s and is not recorded here; refusing to adopt it (leave --mode push only removes what it created)", name, pushHostOf(host.baseURL))
	} else if err != nil && err != ErrKarmadaNotFound {
		return err
	}
	secretRef, err := karmada.createMemberSecret(ctx, name, []byte(memberCfg))
	if err != nil {
		if err == ErrKarmadaExists {
			return fmt.Errorf("dks federation --mode push: %s already exists on host %s and is not recorded here; refusing to adopt it", secretRef, pushHostOf(host.baseURL))
		}
		return fmt.Errorf("dks federation --mode push: store member secret on host %s: %v", pushHostOf(host.baseURL), err)
	}
	clusterRef, err := karmada.createCluster(ctx, name, memberCreds.server)
	if err != nil {
		_, _ = karmada.deleteMemberSecret(ctx, name) // roll back the orphaned secret
		if err == ErrKarmadaExists {
			return fmt.Errorf("dks federation --mode push: Karmada Cluster %q already exists on host %s and is not recorded here; refusing to adopt it (orphaned member secret rolled back)", name, pushHostOf(host.baseURL))
		}
		return fmt.Errorf("dks federation --mode push: register Cluster %q on host %s: %v (orphaned member secret rolled back)", name, pushHostOf(host.baseURL), err)
	}

	rec := pushBaseline{
		Version:          pushRecordVersion,
		HostSatellite:    strings.TrimRight(strings.TrimSpace(o.hostSatellite), "/"),
		MemberSatellite:  strings.TrimRight(strings.TrimSpace(o.memberSatellite), "/"),
		ClusterName:      name,
		KarmadaInstalled: installedByUs,
		HostCreated:      []string{secretRef, clusterRef},
		MemberCreated:    memberRefs,
		MemberServer:     memberCreds.server,
		KarmadaServer:    creds.server,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}
	path, err := pushWriteBaseline(rec)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "federation push: member %q joined (Push) via host %s\n", name, pushHostOf(host.baseURL))
	fmt.Fprintf(out, "  karmada control plane: %s (installed by this tool: %v)\n", creds.server, installedByUs)
	fmt.Fprintf(out, "  member apiserver:      %s\n", memberCreds.server)
	fmt.Fprintf(out, "  host resources:        %d recorded\n", len(rec.HostCreated))
	fmt.Fprintf(out, "  member resources:      %d recorded\n", len(rec.MemberCreated))
	fmt.Fprintf(out, "  baseline:              %s\n", path)
	return nil
}

// pushLeave implements `federation leave --mode push`: it removes exactly
// the baseline's objects and then the baseline itself. Karmada stays unless
// --uninstall is given and the baseline says this tool installed it.
func pushLeave(ctx context.Context, out io.Writer, o pushOptions) error {
	rec, path, err := pushFindBaseline(o)
	if err != nil {
		return err
	}
	name := rec.ClusterName
	if o.dryRun {
		pushPrintLeavePlan(out, o, rec, path)
		return nil
	}
	ownerToken, _, err := pushResolveToken(o.tokenOverride)
	if err != nil {
		return err
	}
	host, err := pushDial(rec.HostSatellite, ownerToken, o.scopes, o.ttl)
	if err != nil {
		return err
	}
	member, err := pushDial(rec.MemberSatellite, ownerToken, o.scopes, o.ttl)
	if err != nil {
		return err
	}
	ownerToken = ""

	rawCfg, _, err := host.do(ctx, http.MethodGet, pushPathKubeconfig, nil)
	if err != nil {
		return fmt.Errorf("dks federation --mode push: get host Karmada kubeconfig: %v", err)
	}
	cfgYAML, err := decodeKubeconfigResponse(rawCfg)
	if err != nil {
		return err
	}
	creds, err := parseKubeconfig(cfgYAML)
	if err != nil {
		return err
	}
	karmada := newKarmadaClient(creds, karmadaInsecureTLS)

	removed := 0
	if gone, err := karmada.deleteCluster(ctx, name); err != nil {
		return fmt.Errorf("dks federation --mode push: unregister Cluster %q: %v", name, err)
	} else if gone {
		removed++
		fmt.Fprintf(out, "federation push: Cluster %q unregistered\n", name)
	} else {
		fmt.Fprintf(out, "federation push: Cluster %q already absent\n", name)
	}
	if gone, err := karmada.deleteMemberSecret(ctx, name); err != nil {
		return fmt.Errorf("dks federation --mode push: delete member secret for %q: %v", name, err)
	} else if gone {
		removed++
		fmt.Fprintf(out, "federation push: member secret %q deleted\n", memberSecretName(name))
	} else {
		fmt.Fprintf(out, "federation push: member secret %q already absent\n", memberSecretName(name))
	}
	if _, status, err := member.do(ctx, http.MethodDelete, pushPathMemberCred, nil); err != nil {
		if status == http.StatusNotFound {
			fmt.Fprintf(out, "federation push: member credential already absent\n")
		} else {
			return fmt.Errorf("dks federation --mode push: revoke member credential on %s: %v", pushHostOf(member.baseURL), err)
		}
	} else {
		removed++
		fmt.Fprintf(out, "federation push: member credential revoked (%d resources)\n", len(rec.MemberCreated))
	}

	uninstalled := false
	if o.uninstall {
		if !rec.KarmadaInstalled {
			fmt.Fprintf(out, "federation push: Karmada left in place (pre-existing; not installed by this tool)\n")
		} else if ok, note := uninstallKarmada(ctx, host); ok {
			uninstalled = true
			fmt.Fprintf(out, "federation push: Karmada uninstalled (%s)\n", note)
		} else {
			fmt.Fprintf(out, "federation push: Karmada left in place (%s)\n", note)
		}
	} else {
		fmt.Fprintf(out, "federation push: Karmada left in place (pass --uninstall to remove it)\n")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("dks federation --mode push: remove baseline %s: %w", path, err)
	}
	fmt.Fprintf(out, "federation push: member %q removed (%d objects); baseline cleared\n", name, removed)
	_ = uninstalled
	return nil
}

// uninstallKarmada best-effort removes the builtin this tool installed. The
// uninstall endpoint is a client convention: 404/405 means the satellite
// does not implement it yet, which is a warning, not a failure — Karmada
// staying installed is the safe default.
func uninstallKarmada(ctx context.Context, host *pushSatellite) (bool, string) {
	_, status, err := host.do(ctx, http.MethodDelete, pushPathInstall, nil)
	if err == nil {
		return true, "DELETE " + pushPathInstall
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return false, "satellite has no uninstall endpoint"
	}
	return false, strings.TrimSpace(err.Error())
}

// pushMemberSnapshot is one status row.
type pushMemberSnapshot struct {
	Name     string `json:"name"`
	Ready    string `json:"ready"`
	SyncMode string `json:"syncMode,omitempty"`
	Managed  bool   `json:"managedByThisTool"`
}

// pushStatus implements `federation status --mode push`: members and
// readiness from the host Karmada apiserver, starred where a baseline shows
// this tool created them.
func pushStatus(ctx context.Context, out io.Writer, o pushOptions) error {
	ownerToken, tokenSource, err := pushResolveToken(o.tokenOverride)
	if err != nil {
		return err
	}
	if o.dryRun {
		fmt.Fprintf(out, "federation push status plan (dry-run, zero network calls):\n")
		fmt.Fprintf(out, "  1. exchange owner Bearer (%s [redacted]) on host %s for scope %s\n", pushDryRunToken(tokenSource), pushHostOf(o.hostSatellite), strings.Join(o.scopes, ","))
		fmt.Fprintf(out, "  2. GET %s (Karmada admin kubeconfig)\n", pushPathKubeconfig)
		fmt.Fprintf(out, "  3. list Karmada Clusters (Push members) with Ready conditions\n")
		return nil
	}
	host, err := pushDial(o.hostSatellite, ownerToken, o.scopes, o.ttl)
	if err != nil {
		return err
	}
	ownerToken = ""
	rawCfg, _, err := host.do(ctx, http.MethodGet, pushPathKubeconfig, nil)
	if err != nil {
		return fmt.Errorf("dks federation --mode push: get host Karmada kubeconfig: %v", err)
	}
	cfgYAML, err := decodeKubeconfigResponse(rawCfg)
	if err != nil {
		return err
	}
	creds, err := parseKubeconfig(cfgYAML)
	if err != nil {
		return err
	}
	karmada := newKarmadaClient(creds, karmadaInsecureTLS)
	clusters, err := karmada.listClusters(ctx)
	if err != nil {
		return err
	}
	managed := map[string]bool{}
	hostDir := filepath.Join(federationRecordDir(), pushSanitize(pushHostOf(o.hostSatellite)))
	if entries, err := os.ReadDir(hostDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				managed[strings.TrimSuffix(e.Name(), ".json")] = true
			}
		}
	}
	snaps := make([]pushMemberSnapshot, 0, len(clusters))
	for _, c := range clusters {
		snaps = append(snaps, pushMemberSnapshot{Name: c.Name, Ready: c.Ready, SyncMode: c.SyncMode, Managed: managed[c.Name]})
	}
	if o.asJSON {
		raw, err := json.MarshalIndent(map[string]any{"host": pushHostOf(o.hostSatellite), "members": snaps}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", raw)
		return nil
	}
	fmt.Fprintf(out, "federation push members on host %s (%d):\n", pushHostOf(o.hostSatellite), len(snaps))
	if len(snaps) == 0 {
		fmt.Fprintf(out, "  (none)\n")
		return nil
	}
	for _, s := range snaps {
		mark := ""
		if s.Managed {
			mark = " *"
		}
		fmt.Fprintf(out, "  %s ready=%s syncMode=%s%s\n", s.Name, s.Ready, s.SyncMode, mark)
	}
	fmt.Fprintf(out, "(* recorded by this tool)\n")
	return nil
}

// pushPrintJoinPlan renders the join dry-run: every step, zero network,
// every secret redacted.
func pushPrintJoinPlan(out io.Writer, o pushOptions, name, tokenSource string) {
	scope := strings.Join(o.scopes, ",")
	fmt.Fprintf(out, "federation push join plan (dry-run, zero network calls):\n")
	fmt.Fprintf(out, "  host satellite:    %s\n", o.hostSatellite)
	fmt.Fprintf(out, "  member satellite:  %s\n", o.memberSatellite)
	fmt.Fprintf(out, "  cluster name:      %s\n", name)
	fmt.Fprintf(out, "  owner Bearer:      %s\n", pushDryRunToken(tokenSource))
	fmt.Fprintf(out, "  1. exchange owner Bearer on host %s for scope %s (ttl %s)\n", pushHostOf(o.hostSatellite), scope, o.ttl)
	fmt.Fprintf(out, "  2. exchange owner Bearer on member %s for scope %s (ttl %s)\n", pushHostOf(o.memberSatellite), scope, o.ttl)
	fmt.Fprintf(out, "  3. POST %s%s (ensure karmada builtin); wait Ready up to %s\n", o.hostSatellite, pushPathInstall, o.wait)
	fmt.Fprintf(out, "  4. GET %s%s (Karmada admin kubeconfig, server https://karmada.<sat-host>)\n", o.hostSatellite, pushPathKubeconfig)
	fmt.Fprintf(out, "  5. POST %s%s (ServiceAccount %s + cluster-admin binding + token Secret)\n", o.memberSatellite, pushPathMemberCred, pushMemberSA)
	fmt.Fprintf(out, "  6. register Karmada Cluster %q (Push) + Secret %s/%s on the host control plane\n", name, pushKarmadaNS, memberSecretName(name))
	fmt.Fprintf(out, "  7. write baseline %s\n", pushRecordPath(o.hostSatellite, name))
}

// pushPrintLeavePlan renders the leave dry-run from the baseline: exactly
// the recorded objects, nothing else.
func pushPrintLeavePlan(out io.Writer, o pushOptions, rec pushBaseline, path string) {
	fmt.Fprintf(out, "federation push leave plan (dry-run, zero network calls):\n")
	fmt.Fprintf(out, "  member: %q on host %s\n", rec.ClusterName, pushHostOf(rec.HostSatellite))
	fmt.Fprintf(out, "  1. unregister Karmada Cluster %q on host %s\n", rec.ClusterName, pushHostOf(rec.HostSatellite))
	for _, r := range rec.HostCreated {
		fmt.Fprintf(out, "     - host: %s\n", r)
	}
	fmt.Fprintf(out, "  2. DELETE %s%s on member %s (revoke %d recorded resources)\n", rec.MemberSatellite, pushPathMemberCred, pushHostOf(rec.MemberSatellite), len(rec.MemberCreated))
	for _, r := range rec.MemberCreated {
		fmt.Fprintf(out, "     - member: %s\n", r)
	}
	if o.uninstall && rec.KarmadaInstalled {
		fmt.Fprintf(out, "  3. uninstall Karmada on host %s (installed by this tool)\n", pushHostOf(rec.HostSatellite))
	} else {
		fmt.Fprintf(out, "  3. Karmada stays installed%s\n", map[bool]string{true: " (pre-existing)", false: ""}[!rec.KarmadaInstalled])
	}
	fmt.Fprintf(out, "  4. remove baseline %s\n", path)
}
