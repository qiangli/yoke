package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ref"
)

const recipeOutputLimit = 4 << 20

type recipeRunner func(context.Context, string, ...string) ([]byte, error)

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("recipe output exceeds %d bytes", b.limit)
	}
	return b.Buffer.Write(p)
}

func runRecipe(ctx context.Context, command string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	out := &boundedOutput{limit: recipeOutputLimit}
	errOut := &boundedOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", command, err, strings.TrimSpace(errOut.String()))
	}
	return out.Bytes(), nil
}

type platformRecord struct {
	id, name, kind, typ string
	attrs               map[string]any
}

func recipeNode(scheme, collection string, r platformRecord) Node {
	return recipeScopedNode(scheme, "", "", collection, r)
}

func recipeScopedNode(scheme, scopeKind, scopeValue, collection string, r platformRecord) Node {
	target := collection + "/" + r.id
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           (ref.Locator{Scheme: scheme, Target: target, ScopeKind: scopeKind, ScopeValue: scopeValue}).String(),
		Path:          target,
		Name:          r.name,
		Kind:          r.kind,
		Type:          r.typ,
		Attrs:         r.attrs,
	}
}

func recipeRoot(loc ref.Locator, scheme, collection string, count int) Node {
	n := uint64(count)
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           loc.String(),
		Path:          collection,
		Name:          collection,
		Kind:          "directory",
		Type:          scheme + "/collection",
		Children:      &n,
		Attrs:         map[string]any{},
	}
}

func recipeRows(rows any) (io.ReadCloser, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	switch v := rows.(type) {
	case []Node:
		for _, row := range v {
			if err := enc.Encode(row); err != nil {
				return nil, err
			}
		}
	case []Edge:
		for _, row := range v {
			if err := enc.Encode(row); err != nil {
				return nil, err
			}
		}
	default:
		return nil, ErrUnsupported
	}
	return io.NopCloser(bytes.NewReader(b.Bytes())), nil
}

func platformRecipe(scheme, collection string, snapshot func(context.Context) ([]platformRecord, error)) Recipe {
	return scopedPlatformRecipe(scheme, "", "", collection, snapshot)
}

func scopedPlatformRecipe(scheme, scopeKind, scopeValue, collection string, snapshot func(context.Context) ([]platformRecord, error)) Recipe {
	valid := func(loc ref.Locator) (string, error) {
		if loc.Scheme != scheme || loc.ScopeKind != scopeKind || loc.ScopeValue != scopeValue || len(loc.Inner) != 0 {
			return "", ErrUnsupported
		}
		switch {
		case loc.Target == "", loc.Target == collection:
			return "", nil
		case strings.HasPrefix(loc.Target, collection+"/"):
			id := strings.TrimPrefix(loc.Target, collection+"/")
			if id != "" && !strings.Contains(id, "..") {
				return id, nil
			}
		}
		return "", os.ErrNotExist
	}
	nodes := func(ctx context.Context, loc ref.Locator) ([]Node, error) {
		id, err := valid(loc)
		if err != nil {
			return nil, err
		}
		if id != "" {
			return []Node{}, nil
		}
		records, err := snapshot(ctx)
		if err != nil {
			return nil, err
		}
		rows := make([]Node, 0, len(records))
		for _, record := range records {
			rows = append(rows, recipeScopedNode(scheme, scopeKind, scopeValue, collection, record))
		}
		return rows, nil
	}
	return Recipe{
		StatNode: func(ctx context.Context, loc ref.Locator) (Node, error) {
			id, err := valid(loc)
			if err != nil {
				return Node{}, err
			}
			records, err := snapshot(ctx)
			if err != nil {
				return Node{}, err
			}
			if id == "" {
				return recipeRoot(loc, scheme, collection, len(records)), nil
			}
			for _, record := range records {
				if record.id == id {
					return recipeScopedNode(scheme, scopeKind, scopeValue, collection, record), nil
				}
			}
			return Node{}, os.ErrNotExist
		},
		ListJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			rows, err := nodes(ctx, loc)
			if err != nil {
				return nil, err
			}
			return recipeRows(rows)
		},
		EdgesJSONL: func(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
			rows, err := nodes(ctx, loc)
			if err != nil {
				return nil, err
			}
			edges := make([]Edge, 0, len(rows))
			for _, row := range rows {
				edges = append(edges, Edge{SchemaVersion: EdgeSchema, Src: loc.String(), Rel: "contains", Dst: row.Ref, Attrs: map[string]any{}})
			}
			return recipeRows(edges)
		},
	}
}

func jsonString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		if len(x) > 0 {
			return jsonString(x[0])
		}
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := jsonString(m[key]); s != "" {
			return s
		}
	}
	return ""
}

func parseOCI(data []byte) ([]platformRecord, error) {
	var items []map[string]any
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("oci recipe: %w", err)
	}
	rows := make([]platformRecord, 0, len(items))
	for _, item := range items {
		id := firstString(item, "Id", "ID", "id")
		if id == "" || strings.ContainsAny(id, "/!:") {
			return nil, fmt.Errorf("oci recipe: missing or invalid container id")
		}
		name := firstString(item, "Names", "Name", "names")
		if name == "" {
			name = id
		}
		rows = append(rows, platformRecord{id: id, name: name, kind: "container", typ: "oci/container", attrs: map[string]any{
			"image": firstString(item, "Image", "image"),
			"state": firstString(item, "State", "Status", "state"),
		}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	return rows, nil
}

func parseK8sPods(data []byte) ([]platformRecord, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
				UID       string `json:"uid"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("k8s recipe: %w", err)
	}
	rows := make([]platformRecord, 0, len(list.Items))
	for _, item := range list.Items {
		ns, name := item.Metadata.Namespace, item.Metadata.Name
		if ns == "" {
			ns = "default"
		}
		if name == "" || strings.ContainsAny(ns+name, "/!:") {
			return nil, fmt.Errorf("k8s recipe: missing or invalid pod identity")
		}
		rows = append(rows, platformRecord{id: ns + "/" + name, name: name, kind: "pod", typ: "k8s/pod", attrs: map[string]any{
			"namespace": ns, "uid": item.Metadata.UID, "phase": item.Status.Phase,
		}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	return rows, nil
}

func parseK8sNodes(data []byte) ([]platformRecord, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				UID    string            `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				NodeInfo struct {
					KubeletVersion string `json:"kubeletVersion"`
				} `json:"nodeInfo"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("k8s recipe: %w", err)
	}
	rows := make([]platformRecord, 0, len(list.Items))
	for _, item := range list.Items {
		name := item.Metadata.Name
		if name == "" || strings.ContainsAny(name, "/!:") {
			return nil, fmt.Errorf("k8s recipe: missing or invalid node name")
		}
		ready := "Unknown"
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status
				break
			}
		}
		seen := map[string]bool{}
		var roles []string
		addRole := func(r string) {
			if r != "" && !seen[r] {
				seen[r] = true
				roles = append(roles, r)
			}
		}
		for k := range item.Metadata.Labels {
			if r, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok {
				addRole(r)
			}
		}
		addRole(item.Metadata.Labels["kubernetes.io/role"])
		sort.Strings(roles)
		rows = append(rows, platformRecord{id: name, name: name, kind: "node", typ: "k8s/node", attrs: map[string]any{
			"uid": item.Metadata.UID, "ready": ready,
			"kubeletVersion": item.Status.NodeInfo.KubeletVersion,
			"roles":          strings.Join(roles, ","),
		}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	return rows, nil
}

// NewOCIRecipe lists local containers with a read-only CLI snapshot.
func NewOCIRecipe(run recipeRunner) Recipe {
	if run == nil {
		run = runRecipe
	}
	return platformRecipe("oci", "containers", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "podman", "ps", "--all", "--format", "json")
		if err != nil {
			return nil, err
		}
		return parseOCI(data)
	})
}

// NewK8sRecipe lists pods from the selected kubectl context without mutation.
func NewK8sRecipe(run recipeRunner) Recipe {
	if run == nil {
		run = runRecipe
	}
	return platformRecipe("k8s", "pods", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "kubectl", "get", "pods", "--all-namespaces", "-o", "json")
		if err != nil {
			return nil, err
		}
		return parseK8sPods(data)
	})
}

// ValidK8sContext reports whether name is acceptable as a literal kubectl
// --context value: non-empty, bounded, no leading dash, no control or space.
func ValidK8sContext(name string) bool {
	if name == "" || len(name) > 253 || name[0] == '-' {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || r == '=' {
			return false
		}
	}
	return true
}

func parseCloudDroplets(data []byte) ([]platformRecord, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var items []map[string]any
	if err := dec.Decode(&items); err != nil {
		return nil, fmt.Errorf("cloud recipe: %w", err)
	}
	if items == nil {
		return nil, fmt.Errorf("cloud recipe: null droplet list")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("unexpected trailing data")
		}
		return nil, fmt.Errorf("cloud recipe: %w", err)
	}
	rows := make([]platformRecord, 0, len(items))
	for _, item := range items {
		id, err := parseCloudDropletID(firstNonNil(item, "id", "ID", "Id"))
		if err != nil {
			return nil, err
		}
		name := firstString(item, "name", "Name")
		if name == "" {
			name = id
		}
		rows = append(rows, platformRecord{id: id, name: name, kind: "droplet", typ: "cloud/droplet", attrs: map[string]any{
			"status": firstString(item, "status", "Status"),
		}})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	return rows, nil
}

// parseCloudDropletID resolves a droplet id to its canonical uint64 form.
// Numbers arrive as json.Number (the decoder uses UseNumber, so large ids
// keep full precision); quoted digits are accepted too. Null, missing,
// non-numeric, negative, fractional, and overflowing ids are rejected before
// any row is emitted.
func parseCloudDropletID(v any) (string, error) {
	var s string
	switch t := v.(type) {
	case nil:
		return "", fmt.Errorf("cloud recipe: missing droplet id")
	case json.Number:
		s = t.String()
	case string:
		s = t
	default:
		return "", fmt.Errorf("cloud recipe: invalid droplet id of type %T", v)
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return "", fmt.Errorf("cloud recipe: invalid droplet id %q", s)
	}
	return strconv.FormatUint(n, 10), nil
}

func firstNonNil(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := m[key]; ok && v != nil {
			return v
		}
	}
	return nil
}

// ValidCloudContext reports whether name is acceptable as a literal doctl
// --context value: non-empty, bounded, no leading dash, no control or space.
func ValidCloudContext(name string) bool {
	if name == "" || len(name) > 253 || name[0] == '-' {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || r == '=' {
			return false
		}
	}
	return true
}

// NewCloudDropletsRecipe lists droplets from exactly the named doctl context.
// The context is passed as a literal --context argument; there is no ambient
// DIGITALOCEAN_CONTEXT fallback, nothing is ever written to the cloud, and an
// invalid name is refused before any command runs.
func NewCloudDropletsRecipe(cloudContext string, run recipeRunner) (Recipe, error) {
	if !ValidCloudContext(cloudContext) {
		return Recipe{}, fmt.Errorf("cloud recipe: invalid or missing @cloud context %q", cloudContext)
	}
	if run == nil {
		run = runRecipe
	}
	return scopedPlatformRecipe("cloud", "cloud", cloudContext, "droplets", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "doctl", "--context", cloudContext, "compute", "droplet", "list", "--output", "json")
		if err != nil {
			return nil, err
		}
		return parseCloudDroplets(data)
	}), nil
}

// ValidSandboxTarget reports whether name is acceptable as an explicit
// outpost SSH target for a sandbox scope: non-empty, bounded, no leading
// dash, no control, space or '=' that could smuggle extra flags.
func ValidSandboxTarget(name string) bool {
	if name == "" || len(name) > 253 || name[0] == '-' {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || r == '=' {
			return false
		}
	}
	return true
}

// NewOCISandboxRecipe lists containers on a Podman-backed outpost peer. The
// scope value is the peer id spelled on the locator (it names every emitted
// ref, so pulled rows round-trip); the SSH target is the single saved
// outpost target that peer resolved to (it travels as literal argv in
// outpost ssh exec <target> -- podman ps ...). There is no ambient host
// fallback, only the read-only ps verb is ever issued, and an invalid
// scope or target is refused before any command runs.
func NewOCISandboxRecipe(scopeValue, sandboxTarget string, run recipeRunner) (Recipe, error) {
	if !ValidSandboxTarget(scopeValue) {
		return Recipe{}, fmt.Errorf("sandbox recipe: invalid or missing @sandbox scope %q", scopeValue)
	}
	if !ValidSandboxTarget(sandboxTarget) {
		return Recipe{}, fmt.Errorf("sandbox recipe: invalid @sandbox target %q", sandboxTarget)
	}
	if run == nil {
		run = runRecipe
	}
	return scopedPlatformRecipe("oci", "sandbox", scopeValue, "containers", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "outpost", "ssh", "exec", sandboxTarget, "--", "podman", "ps", "--all", "--format", "json")
		if err != nil {
			return nil, err
		}
		return parseOCI(data)
	}), nil
}

// NewK8sScopedRecipe lists pods from exactly the named kubectl context. The
// context is passed as a literal --context argument; there is no ambient
// fallback, and an invalid name is refused before any command runs.
func NewK8sScopedRecipe(kubeContext string, run recipeRunner) (Recipe, error) {
	if !ValidK8sContext(kubeContext) {
		return Recipe{}, fmt.Errorf("k8s recipe: invalid or missing @k8s context %q", kubeContext)
	}
	if run == nil {
		run = runRecipe
	}
	return scopedPlatformRecipe("k8s", "k8s", kubeContext, "pods", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "kubectl", "--context", kubeContext, "get", "pods", "--all-namespaces", "-o", "json")
		if err != nil {
			return nil, err
		}
		return parseK8sPods(data)
	}), nil
}

// NewK8sNodesRecipe lists nodes from the selected kubectl context without
// mutation. Nodes are cluster-scoped, so there is no --all-namespaces flag.
func NewK8sNodesRecipe(run recipeRunner) Recipe {
	if run == nil {
		run = runRecipe
	}
	return platformRecipe("k8s", "nodes", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "kubectl", "get", "nodes", "-o", "json")
		if err != nil {
			return nil, err
		}
		return parseK8sNodes(data)
	})
}

// NewK8sNodesScopedRecipe lists nodes from exactly the named kubectl
// context. The context is passed as a literal --context argument; there is
// no ambient fallback, and an invalid name is refused before any command
// runs.
func NewK8sNodesScopedRecipe(kubeContext string, run recipeRunner) (Recipe, error) {
	if !ValidK8sContext(kubeContext) {
		return Recipe{}, fmt.Errorf("k8s recipe: invalid or missing @k8s context %q", kubeContext)
	}
	if run == nil {
		run = runRecipe
	}
	return scopedPlatformRecipe("k8s", "k8s", kubeContext, "nodes", func(ctx context.Context) ([]platformRecord, error) {
		data, err := run(ctx, "kubectl", "--context", kubeContext, "get", "nodes", "-o", "json")
		if err != nil {
			return nil, err
		}
		return parseK8sNodes(data)
	}), nil
}
