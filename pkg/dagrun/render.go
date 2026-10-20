package dagrun

import (
	"errors"
	"fmt"
	"hash/fnv"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/dag"
)

// Object is one rendered Kubernetes object in plain unstructured form, ready
// for server-side apply. This package links no Kubernetes client: only the
// cluster transport does.
type Object = map[string]any

// ExecutorClass is the kind of executor a manifest is rendered for.
type ExecutorClass struct {
	OS    string // executor GOOS: linux (rendered); darwin / windows are native, Stage B
	Image string // bashy-built, signed image a Linux executor runs the manifest in
}

// ErrUnsupportedExecutor: the manifest is valid but this renderer cannot run
// it on the requested executor class yet (native macOS/Windows, services,
// credentials, plane egress). Nothing is rendered; there is no fallback.
var ErrUnsupportedExecutor = errors.New("dagrun: unsupported executor")

// Labels and well-known keys on every rendered object. ps, logs, stop and
// cleanup select by these labels only.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelWorkspace = "dhnt.io/workspace"
	LabelRun       = "dhnt.io/run"
	LabelCommand   = "dhnt.io/command"
	ManagedByBashy = "bashy"
	AnnotManifest  = "dhnt.io/manifest"
	AnnotDigest    = "dhnt.io/command-digest"
	// AnnotAttachPort carries the session server's published attach port on
	// interactive and long-running Pods. `dks attach` and `dks logs` read it
	// back to build the tunnel URL; one-shot Jobs never carry it.
	AnnotAttachPort = "dhnt.io/attach-port"
	annotArtifactNS = "artifact.dhnt.io/" // + <goos>-<goarch>

	labelNodeOS      = "kubernetes.io/os"
	labelNodeArch    = "kubernetes.io/arch"
	labelNodeHost    = "outpost.dhnt.io/host"
	labelNodeBackend = "outpost.dhnt.io/backend"

	// HomeHostRoot is the executor directory under which each workspace's
	// home lives (HostPath; executor-affine, like the Linux home contract).
	HomeHostRoot = "/var/lib/bashy/dks/home"
	// HomeMountPath is where the workspace home appears in the container; it
	// is also $HOME and what a logical `~` cwd resolves against.
	HomeMountPath = "/home/bashy"
)

// Render turns a run manifest into the Kubernetes objects one executor class
// runs it with. Pure: no I/O, no clock; the same bytes give the same
// objects. Linux only for now:
//
//	one-shot                  -> batch/v1 Job, backoffLimit 0
//	interactive, long-running -> v1 Pod, restartPolicy Never
//
// plus a deny-all-egress NetworkPolicy when the manifest says egress none.
func Render(manifest []byte, class ExecutorClass) ([]Object, error) {
	m, err := ParseManifest(manifest)
	if err != nil {
		return nil, err
	}
	switch class.OS {
	case "linux":
	case "darwin", "windows":
		return nil, fmt.Errorf("%w: native %s executor (Stage B)", ErrUnsupportedExecutor, class.OS)
	default:
		return nil, fmt.Errorf("%w: executor class %q", ErrUnsupportedExecutor, class.OS)
	}
	if strings.TrimSpace(class.Image) == "" {
		return nil, fmt.Errorf("dagrun: linux executor class has no image")
	}
	return renderLinux(m, class)
}

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsupportedExecutor, fmt.Sprintf(format, a...))
}

func badManifest(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, a...))
}

func renderLinux(m *Manifest, class ExecutorClass) ([]Object, error) {
	t := m.Task
	switch t.Venue {
	case dag.VenueCluster:
	case dag.VenueNative:
		return nil, unsupported("venue native renders to the vk-native backend (Stage B)")
	default:
		return nil, badManifest("venue %q is not a cluster venue", t.Venue)
	}
	if len(t.Secrets) > 0 {
		return nil, unsupported("credentials %v are not wired into cluster dispatch yet", t.Secrets)
	}
	if m.Meta.Egress == EgressPlane {
		return nil, unsupported("egress plane has no cluster policy yet")
	}

	nodeSelector, affinity, err := linuxPlacement(m)
	if err != nil {
		return nil, err
	}
	resources, err := linuxResources(t)
	if err != nil {
		return nil, err
	}
	workDir, err := containerCwd(m.Meta.Cwd)
	if err != nil {
		return nil, err
	}

	env := []any{map[string]any{"name": "HOME", "value": HomeMountPath}}
	for _, kv := range sortedEnv(t.Env) {
		k, v, _ := strings.Cut(kv, "=")
		if k == "HOME" {
			continue // home is the workspace home, never the client's
		}
		env = append(env, map[string]any{"name": k, "value": v})
	}

	// Session kinds run under the hidden session server: the executor's
	// vknode publishes the container port that sets hostPort as a transient
	// app, and the verbs reach the server through the matrix tunnel at it.
	attachPort := 0
	if m.Meta.RunKind == RunInteractive || m.Meta.RunKind == RunLongRunning {
		attachPort = sessionAttachPort(m.Meta.RunID)
	}
	body := strings.TrimSuffix(t.Body, "\n")
	command := []any{"bashy", "-c", body}
	if attachPort != 0 {
		command = []any{"bashy", "dks", "session-server",
			"--session", m.Meta.RunID,
			"--home", HomeMountPath,
			"--port", strconv.Itoa(attachPort),
			"--", "bashy", "-c", body}
	}
	container := map[string]any{
		"name":         "run",
		"image":        class.Image,
		"command":      command,
		"workingDir":   workDir,
		"env":          env,
		"volumeMounts": []any{map[string]any{"name": "home", "mountPath": HomeMountPath}},
	}
	if len(resources) > 0 {
		container["resources"] = resources
	}
	if attachPort != 0 {
		container["ports"] = []any{map[string]any{
			"name": "attach", "containerPort": attachPort, "hostPort": attachPort,
		}}
	}
	if m.Meta.RunKind == RunInteractive {
		container["stdin"] = true
		container["tty"] = true
	}

	podSpec := map[string]any{
		"restartPolicy":                "Never",
		"automountServiceAccountToken": false,
		"nodeSelector":                 nodeSelector,
		"affinity":                     affinity,
		"tolerations": []any{map[string]any{
			"key": "virtual-kubelet.io/provider", "operator": "Equal", "value": "outpost", "effect": "NoSchedule",
		}},
		"containers": []any{container},
		"volumes": []any{map[string]any{
			"name":     "home",
			"hostPath": map[string]any{"path": HomeHostRoot + "/" + m.Meta.Workspace, "type": "DirectoryOrCreate"},
		}},
	}

	name := "bashy-run-" + strings.ToLower(m.Meta.RunID)
	labels := renderLabels(m)
	meta := func(withAnnotations bool) map[string]any {
		md := map[string]any{"name": name, "labels": copyMap(labels)}
		if withAnnotations {
			md["annotations"] = renderAnnotations(m)
		}
		return md
	}

	var workload Object
	switch m.Meta.RunKind {
	case RunOneShot:
		spec := map[string]any{
			"backoffLimit": int64(0),
			"template":     map[string]any{"metadata": map[string]any{"labels": copyMap(labels)}, "spec": podSpec},
		}
		if t.Timeout > 0 {
			spec["activeDeadlineSeconds"] = int64(t.Timeout.Seconds())
		}
		workload = Object{"apiVersion": "batch/v1", "kind": "Job", "metadata": meta(true), "spec": spec}
	case RunInteractive, RunLongRunning:
		if t.Timeout > 0 {
			podSpec["activeDeadlineSeconds"] = int64(t.Timeout.Seconds())
		}
		md := meta(true)
		md["annotations"].(map[string]any)[AnnotAttachPort] = strconv.Itoa(attachPort)
		workload = Object{"apiVersion": "v1", "kind": "Pod", "metadata": md, "spec": podSpec}
	default:
		return nil, unsupported("run-kind %s is not rendered yet", m.Meta.RunKind)
	}

	objs := []Object{workload}
	if m.Meta.Egress == EgressNone {
		objs = append(objs, Object{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "NetworkPolicy",
			"metadata":   meta(false),
			"spec": map[string]any{
				"podSelector": map[string]any{"matchLabels": map[string]any{LabelRun: m.Meta.RunID}},
				"policyTypes": []any{"Egress"},
				"egress":      []any{},
			},
		})
	}
	return objs, nil
}

// linuxPlacement maps Match: onto node labels with the fail-closed selector
// semantics of the DKS node selector: os is linux, arch is pinned (or limited
// to the arches that have an artifact), host and backend are copied.
func linuxPlacement(m *Manifest) (map[string]any, map[string]any, error) {
	sel := map[string]any{labelNodeOS: "linux"}
	for k, v := range m.Task.Match {
		switch k {
		case "os", labelNodeOS:
			if v != "linux" {
				return nil, nil, unsupported("match os %s on a linux executor", v)
			}
		case "arch", labelNodeArch:
			sel[labelNodeArch] = v
		case labelNodeBackend:
			if v == "vk-native" {
				return nil, nil, unsupported("backend vk-native is the native executor (Stage B)")
			}
			sel[k] = v
		default:
			if !strings.Contains(k, "/") {
				return nil, nil, badManifest("match key %q has no node label", k)
			}
			sel[k] = v
		}
	}

	var arches []string
	for platform := range m.Meta.Artifact {
		if o, a, _ := strings.Cut(platform, "/"); o == "linux" {
			arches = append(arches, a)
		}
	}
	sort.Strings(arches)
	if len(m.Meta.Artifact) > 0 && len(arches) == 0 {
		return nil, nil, badManifest("command %s has no linux artifact", m.Meta.Command.Name)
	}
	// The native-process backend ignores the image and runs the node's own
	// binaries, so a container workload requires a node whose backend is not
	// vk-native; nodes without the label (plain kubelets) stay eligible.
	exprs := []any{map[string]any{"key": labelNodeBackend, "operator": "NotIn", "values": []any{"vk-native"}}}
	if pinned, ok := sel[labelNodeArch].(string); ok {
		if len(m.Meta.Artifact) > 0 && m.Meta.Artifact["linux/"+pinned] == "" {
			return nil, nil, badManifest("command %s has no linux/%s artifact", m.Meta.Command.Name, pinned)
		}
	} else {
		switch len(arches) {
		case 0: // a shipped command: identity is the bashy release itself
		case 1:
			sel[labelNodeArch] = arches[0]
		default:
			values := make([]any, len(arches))
			for i, a := range arches {
				values[i] = a
			}
			exprs = append(exprs, map[string]any{"key": labelNodeArch, "operator": "In", "values": values})
		}
	}
	affinity := map[string]any{"nodeAffinity": map[string]any{
		"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
			"nodeSelectorTerms": []any{map[string]any{"matchExpressions": exprs}},
		},
	}}
	return sel, affinity, nil
}

// linuxResources maps the dag request lines onto container resources.
// Extended resources (GPU, VRAM) go in limits too, as Kubernetes requires.
func linuxResources(t *dag.Task) (map[string]any, error) {
	requests := map[string]any{}
	limits := map[string]any{}
	if t.CPUPerTask > 0 {
		requests["cpu"] = strconv.Itoa(t.CPUPerTask)
	}
	if t.MemPerTask > 0 {
		requests["memory"] = quantity(t.MemPerTask)
	}
	if a := t.Accelerator; a.Kind != "" {
		if a.Kind != "gpu" || a.Family != "cuda" {
			return nil, unsupported("accelerator %s/%s has no linux device plugin mapping", a.Kind, a.Family)
		}
		gpus := strconv.Itoa(a.Count)
		requests["nvidia.com/gpu"], limits["nvidia.com/gpu"] = gpus, gpus
		if a.MemoryBytes > 0 {
			// The node-level per-device check against dhnt.io/vram-device-max
			// is DKS placement's; the request is the aggregate it reserves.
			vram := quantity(a.MemoryBytes * uint64(a.Count))
			requests["dhnt.io/vram"], limits["dhnt.io/vram"] = vram, vram
		}
	}
	out := map[string]any{}
	if len(requests) > 0 {
		out["requests"] = requests
	}
	if len(limits) > 0 {
		out["limits"] = limits
	}
	return out, nil
}

// sessionAttachPortRange bounds the deterministic per-session attach port:
// 40000-49999, above the ephemeral range and clear of well-known services.
const (
	sessionAttachPortBase  = 40000
	sessionAttachPortCount = 10000
)

// sessionAttachPort maps a run id onto a deterministic attach port in
// 40000-49999. Render stays pure: the same manifest always yields the same
// port with no allocation and no I/O. The limit is documented, not solved:
// two sessions of one host that hash to the same port collide, and moving
// one of them is placement retry — out of scope for this slice.
func sessionAttachPort(runID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(runID))
	return sessionAttachPortBase + int(h.Sum32()%sessionAttachPortCount)
}

// quantity spells a byte count as a Kubernetes binary quantity.
func quantity(n uint64) string {
	for _, u := range []struct {
		suffix string
		bytes  uint64
	}{{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if n >= u.bytes && n%u.bytes == 0 {
			return strconv.FormatUint(n/u.bytes, 10) + u.suffix
		}
	}
	return strconv.FormatUint(n, 10)
}

// containerCwd resolves the logical cwd: `~` is the workspace home, an
// absolute path stays as is, and a `~` path may not climb out of home.
func containerCwd(cwd string) (string, error) {
	switch {
	case cwd == "" || cwd == "~":
		return HomeMountPath, nil
	case strings.HasPrefix(cwd, "~/"):
		p := path.Join(HomeMountPath, cwd[2:])
		if p != HomeMountPath && !strings.HasPrefix(p, HomeMountPath+"/") {
			return "", badManifest("cwd %q leaves the workspace home", cwd)
		}
		return p, nil
	case path.IsAbs(cwd):
		return path.Clean(cwd), nil
	}
	return "", badManifest("cwd %q is neither ~-relative nor absolute", cwd)
}

func renderLabels(m *Manifest) map[string]any {
	return map[string]any{
		LabelManagedBy: ManagedByBashy,
		LabelWorkspace: m.Meta.Workspace,
		LabelRun:       m.Meta.RunID,
		LabelCommand:   commandLabel(m.Meta.Command.Name),
	}
}

func renderAnnotations(m *Manifest) map[string]any {
	a := map[string]any{
		AnnotManifest: string(m.Text),
		AnnotDigest:   m.Meta.Command.Digest,
	}
	for platform, d := range m.Meta.Artifact {
		a[annotArtifactNS+strings.ReplaceAll(platform, "/", "-")] = d
	}
	return a
}

// commandLabel folds a command name into a label value: characters outside
// the label alphabet become '-', at most 63, alphanumeric at both ends.
func commandLabel(name string) string {
	b := []byte(name)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			b[i] = '-'
		}
	}
	s := string(b)
	if len(s) > 63 {
		s = s[:63]
	}
	s = strings.TrimFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	if s == "" {
		return "unnamed"
	}
	return s
}

func sortedEnv(env []string) []string {
	out := append([]string(nil), env...)
	sort.Strings(out)
	return out
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
