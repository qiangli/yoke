package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/oci/bindings/containers"
	"github.com/qiangli/yoke/pkg/oci/handlers"
	"github.com/qiangli/yoke/pkg/oci/nettypes"
	ociSpec "github.com/qiangli/yoke/pkg/oci/spec"
	"github.com/qiangli/yoke/pkg/oci/specgen"
)

// ContainerConfig holds the configuration for creating a container.
type ContainerConfig struct {
	Name       string            // container name
	Image      string            // container image
	Env        map[string]string // environment variables
	Mounts     []Mount           // volume mounts
	Ports      []PortMapping     // port mappings (host -> container)
	WorkDir    string            // working directory inside container
	Network    string            // network name (empty = default bridge)
	ReadOnly   bool              // read-only root filesystem
	CapAdd     []string          // capabilities to add (e.g. "NET_ADMIN", "SYS_ADMIN"). Conflicts with Privileged.
	CapDrop    []string          // capabilities to drop (default: ["ALL"] unless KeepCaps or Privileged is set). Conflicts with Privileged.
	KeepCaps   bool              // skip the default "drop ALL" — use whatever CapDrop says verbatim (empty means drop nothing). Set this for docker-compatible workloads (e.g. postgres) that need Linux caps to start.
	Privileged bool              // grant all caps, all devices, and disable seccomp/SELinux/AppArmor. Overrides CapAdd/CapDrop/KeepCaps.
	// CgroupNS sets the container's cgroup namespace mode. One of
	// "private" (default; private cgroup namespace per container),
	// "host" (share the host VM's cgroup namespace — required for
	// k3s-agent / kubelet-in-container so cpuset and other v2
	// controllers propagate from the VM root), or "" (engine default).
	// Mirrors `podman run --cgroupns=host`.
	CgroupNS string
	Tmpfs    []string          // tmpfs mounts (e.g., /tmp, /var/tmp)
	Init     bool              // use init for signal handling
	Labels   map[string]string // container labels for tracking
	Command  []string          // override command (default: image CMD)
	Resources
}

// PortMapping maps a host port to a container port.
type PortMapping struct {
	HostPort      uint16 // port on the host
	ContainerPort uint16 // port inside the container
	Protocol      string // "tcp" (default) or "udp"
}

// Mount describes a bind mount from host to container.
type Mount struct {
	Source   string // host path
	Target   string // container path
	ReadOnly bool   // read-only mount
}

// Resources holds resource limits for a container.
type Resources struct {
	CPUs   string // CPU limit (e.g., "2.0")
	Memory string // memory limit (e.g., "4g")
}

// Container represents a running or created container.
type Container struct {
	ID     string
	Name   string
	engine *Engine
}

// NewContainer creates a Container handle for an existing container by ID or name.
func NewContainer(engine *Engine, idOrName string) *Container {
	return &Container{ID: idOrName, engine: engine}
}

// ContainerInfo holds inspection data from podman.
type ContainerInfo struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	State  string `json:"State"`
	Image  string `json:"Image"`
	Status string `json:"Status"`
}

// CreateContainer creates a new container from the given config via REST API.
func (e *Engine) CreateContainer(ctx context.Context, cfg *ContainerConfig) (*Container, error) {
	sg := specgen.NewSpecGenerator(cfg.Image, false)
	sg.Name = cfg.Name
	sg.Command = cfg.Command
	sg.Env = cfg.Env
	sg.Labels = cfg.Labels

	if cfg.Init {
		initTrue := true
		sg.Init = &initTrue
	}
	if cfg.WorkDir != "" {
		sg.WorkDir = cfg.WorkDir
	}
	if cfg.ReadOnly {
		readOnly := true
		sg.ReadOnlyFilesystem = &readOnly
	}
	switch cfg.CgroupNS {
	case "", "default":
		// engine default — typically Private
	case "host":
		sg.CgroupNS = specgen.Namespace{NSMode: specgen.Host}
	case "private":
		sg.CgroupNS = specgen.Namespace{NSMode: specgen.Private}
	case "none":
		sg.CgroupNS = specgen.Namespace{NSMode: specgen.None}
	default:
		return nil, fmt.Errorf("CgroupNS=%q unsupported (expect host|private|none|default)", cfg.CgroupNS)
	}

	if cfg.Privileged {
		priv := true
		sg.Privileged = &priv
		// Upstream specgen documents CapAdd/CapDrop as conflicting with
		// Privileged — privileged already grants all caps, so leave them unset.
		if e.inProcess {
			// In-process libpod runs as the invoking UID, so "privileged"
			// here is rootless-privileged: all caps inside the user
			// namespace, not real host root. Kernel module loading,
			// cgroup hierarchy creation, and host-netns iptables rules
			// will still fail — workloads like k3s won't come up healthy.
			// Point users at a rootful socket as the escape hatch.
			slog.Warn("container: --privileged via in-process libpod is rootless-privileged (no host root caps); for k3s-style workloads, run podman rootful and set CONTAINER_HOST=unix:///run/podman/podman.sock", "name", cfg.Name, "image", cfg.Image)
		}
	} else {
		capDrop := cfg.CapDrop
		if !cfg.KeepCaps && len(capDrop) == 0 {
			capDrop = []string{"ALL"}
		}
		sg.CapDrop = capDrop
		sg.CapAdd = cfg.CapAdd
	}

	for _, p := range cfg.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "tcp"
		}
		sg.PortMappings = append(sg.PortMappings, nettypes.PortMapping{
			HostPort:      p.HostPort,
			ContainerPort: p.ContainerPort,
			Protocol:      proto,
		})
	}

	if cfg.Network != "" {
		sg.Networks = map[string]nettypes.PerNetworkOptions{
			cfg.Network: {},
		}
	}

	for _, m := range cfg.Mounts {
		opts := []string{"bind"}
		if m.ReadOnly {
			opts = append(opts, "ro")
		} else {
			opts = append(opts, "rw")
		}
		sg.Mounts = append(sg.Mounts, ociSpec.Mount{
			Type:        "bind",
			Source:      m.Source,
			Destination: m.Target,
			Options:     opts,
		})
	}

	for _, t := range cfg.Tmpfs {
		sg.Mounts = append(sg.Mounts, ociSpec.Mount{
			Type:        "tmpfs",
			Destination: t,
			Options:     []string{"rw", "nosuid", "nodev"},
		})
	}

	resp, err := containers.CreateWithSpec(e.connCtx, sg, nil)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	name := cfg.Name
	if name == "" && len(resp.ID) >= 12 {
		name = resp.ID[:12]
	}

	return &Container{
		ID:     resp.ID,
		Name:   name,
		engine: e,
	}, nil
}

// Start starts the container via REST API.
func (c *Container) Start(ctx context.Context) error {
	return containers.Start(c.engine.connCtx, c.ID, nil)
}

// Stop gracefully stops the container with the given timeout via REST API.
func (c *Container) Stop(ctx context.Context, timeout time.Duration) error {
	secs := uint(timeout.Seconds())
	opts := new(containers.StopOptions).WithTimeout(secs)
	return containers.Stop(c.engine.connCtx, c.ID, opts)
}

// Remove removes the container via REST API.
func (c *Container) Remove(ctx context.Context, force bool) error {
	opts := new(containers.RemoveOptions).WithForce(force)
	_, err := containers.Remove(c.engine.connCtx, c.ID, opts)
	return err
}

// Exec runs a command inside the container via REST API and returns the output.
func (c *Container) Exec(ctx context.Context, command string, workDir string) (*ExecResult, error) {
	// Create exec session.
	execCfg := &handlers.ExecCreateConfig{}
	execCfg.Cmd = []string{"sh", "-c", command}
	execCfg.AttachStdout = true
	execCfg.AttachStderr = true
	if workDir != "" {
		execCfg.WorkingDir = workDir
	}

	// Span around the whole round-trip. Two layers of instrumentation:
	//   - startExecSpan: the unified ycode.exec.* signal
	//     shared with bash/toolexec/sandbox.
	//   - RecordContainerExec: feeds the container-package legacy
	//     counters (creates was wired; execs and failures were
	//     orphaned before this commit).
	spanCtx, finish := startExecSpan(ctx, execScopeContainer, "sh", []string{"-c", command})
	start := time.Now()
	var (
		execErr  error
		exitCode int
	)
	defer func() {
		dur := float64(time.Since(start).Milliseconds())
		RecordContainerExec(spanCtx, execErr == nil && exitCode == 0, dur)
		finish(exitCode, execErr)
	}()

	sessionID, err := containers.ExecCreate(c.engine.connCtx, c.ID, execCfg)
	if err != nil {
		execErr = err
		exitCode = 1
		return nil, fmt.Errorf("exec create: %w", err)
	}

	// Attach and capture output.
	var stdout, stderr bytes.Buffer
	attachOpts := new(containers.ExecStartAndAttachOptions).
		WithOutputStream(&stdout).
		WithErrorStream(&stderr).
		WithAttachOutput(true).
		WithAttachError(true)

	if err := containers.ExecStartAndAttach(c.engine.connCtx, sessionID, attachOpts); err != nil {
		execErr = err
		exitCode = 1
		return &ExecResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String() + "\n" + err.Error(),
			ExitCode: 1,
		}, nil
	}

	// Get exit code.
	inspect, err := containers.ExecInspect(c.engine.connCtx, sessionID, nil)
	if err != nil {
		execErr = err
	} else {
		exitCode = inspect.ExitCode
	}

	return &ExecResult{
		Stdout:   strings.TrimRight(stdout.String(), "\n"),
		Stderr:   strings.TrimRight(stderr.String(), "\n"),
		ExitCode: exitCode,
	}, nil
}

// ExecResult holds the result of a command execution inside a container.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// IsRunning returns true if the container is currently running via REST API.
func (c *Container) IsRunning(ctx context.Context) bool {
	data, err := containers.Inspect(c.engine.connCtx, c.ID, nil)
	if err != nil {
		return false
	}
	return data.State != nil && data.State.Running
}

// CopyTo copies a host path into the container via REST API (tar streaming).
func (c *Container) CopyTo(ctx context.Context, hostPath, containerPath string) error {
	// Create a tar archive from the host path.
	var buf bytes.Buffer
	if err := tarPath(hostPath, &buf); err != nil {
		return fmt.Errorf("create tar archive: %w", err)
	}

	copyFunc, err := containers.CopyFromArchive(c.engine.connCtx, c.ID, containerPath, &buf)
	if err != nil {
		return fmt.Errorf("copy to container: %w", err)
	}
	return copyFunc()
}

// CopyFrom copies a path from the container to the host via REST API (tar streaming).
func (c *Container) CopyFrom(ctx context.Context, containerPath, hostPath string) error {
	var buf bytes.Buffer
	copyFunc, err := containers.CopyToArchive(c.engine.connCtx, c.ID, containerPath, &buf)
	if err != nil {
		return fmt.Errorf("copy from container: %w", err)
	}
	if err := copyFunc(); err != nil {
		return fmt.Errorf("copy from container stream: %w", err)
	}

	// Extract tar archive to host path.
	return untarToPath(&buf, hostPath)
}

// ListContainers lists containers matching the given filters via REST API.
func (e *Engine) ListContainers(ctx context.Context, filters map[string]string) ([]ContainerInfo, error) {
	opts := new(containers.ListOptions).WithAll(true)
	if len(filters) > 0 {
		filterMap := make(map[string][]string, len(filters))
		for k, v := range filters {
			filterMap[k] = []string{v}
		}
		opts = opts.WithFilters(filterMap)
	}

	listed, err := containers.List(e.connCtx, opts)
	if err != nil {
		return nil, err
	}

	var infos []ContainerInfo
	for _, c := range listed {
		name := ""
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		infos = append(infos, ContainerInfo{
			ID:    c.ID,
			Name:  name,
			State: c.State,
			Image: c.Image,
		})
	}
	return infos, nil
}

// InspectContainer returns detailed information about a container via REST API.
func (e *Engine) InspectContainer(ctx context.Context, idOrName string) (json.RawMessage, error) {
	data, err := containers.Inspect(e.connCtx, idOrName, nil)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// ContainerLogs retrieves logs from a container via REST API.
func (e *Engine) ContainerLogs(ctx context.Context, idOrName string, follow bool, tail string) (string, error) {
	opts := new(containers.LogOptions).
		WithStdout(true).
		WithStderr(true).
		WithFollow(follow)
	if tail != "" {
		opts = opts.WithTail(tail)
	}

	var stdout, stderr bytes.Buffer
	stdoutCh := make(chan string, 1024)
	stderrCh := make(chan string, 1024)

	go func() {
		for line := range stdoutCh {
			stdout.WriteString(line)
			stdout.WriteByte('\n')
		}
	}()
	go func() {
		for line := range stderrCh {
			stderr.WriteString(line)
			stderr.WriteByte('\n')
		}
	}()

	if err := containers.Logs(e.connCtx, idOrName, opts, stdoutCh, stderrCh); err != nil {
		return "", fmt.Errorf("container logs: %w", err)
	}

	result := stdout.String()
	if s := stderr.String(); s != "" {
		if result != "" {
			result += s
		} else {
			result = s
		}
	}
	return strings.TrimRight(result, "\n"), nil
}

// RunResult is the outcome of a one-shot container invocation: separate
// stdout/stderr streams and the process exit code. ExitCode is -1 when
// the container failed to start or was killed by a signal before producing
// a status code.
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// RunOneShot creates a container from cfg, starts it, streams its
// stdout/stderr into separate buffers, waits for it to exit, removes it,
// and returns the captured output plus the exit code. This is the
// embedded-engine equivalent of `podman run --rm <image> <cmd>` and is
// what the sandbox shell builtin and sandbox_exec MCP tool use to execute
// untrusted code without shelling out to a `podman` binary.
//
// The container is always removed on return, even on error paths.
func (e *Engine) RunOneShot(ctx context.Context, cfg *ContainerConfig) (*RunResult, error) {
	ctr, err := e.CreateContainer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	// Always remove, even if the caller cancels. Use a fresh context so
	// cleanup still runs when ctx has been cancelled (otherwise the user
	// gets dangling exited containers).
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = ctr.Remove(cleanupCtx, true)
	}()

	if err := ctr.Start(ctx); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	// Stream logs with follow=true while the container runs. The libpod
	// HTTP API multiplexes stdout/stderr into separate frames; the
	// bindings demux them into two channels. Logs blocks until the
	// container exits, draining both channels.
	follow := true
	stdoutFlag := true
	stderrFlag := true
	opts := &containers.LogOptions{Follow: &follow, Stdout: &stdoutFlag, Stderr: &stderrFlag}

	stdoutCh := make(chan string, 1024)
	stderrCh := make(chan string, 1024)

	var stdoutBuf, stderrBuf bytes.Buffer
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() {
		for s := range stdoutCh {
			stdoutBuf.WriteString(s)
		}
		close(stdoutDone)
	}()
	go func() {
		for s := range stderrCh {
			stderrBuf.WriteString(s)
		}
		close(stderrDone)
	}()

	logsErr := containers.Logs(e.connCtx, ctr.ID, opts, stdoutCh, stderrCh)
	close(stdoutCh)
	close(stderrCh)
	<-stdoutDone
	<-stderrDone

	result := &RunResult{
		Stdout:   strings.TrimRight(stdoutBuf.String(), "\n"),
		Stderr:   strings.TrimRight(stderrBuf.String(), "\n"),
		ExitCode: -1,
	}

	// Get exit code via Wait (returns when the container is in a
	// terminal state). Wait against the connection context, not ctx —
	// if the caller cancelled, the container is still in libpod and
	// has a real exit code we want to surface.
	code, waitErr := containers.Wait(e.connCtx, ctr.ID, nil)
	if waitErr == nil {
		result.ExitCode = int(code)
	}

	// Logs errors are usually downstream of the container exiting
	// abnormally (e.g. context cancelled mid-stream). Surface them only
	// when we don't have a clean exit code to report.
	if logsErr != nil && waitErr != nil {
		return result, fmt.Errorf("logs: %w (wait: %v)", logsErr, waitErr)
	}
	return result, nil
}

// --- tar helpers ---

// tarPath creates a tar archive from a host path.
func tarPath(hostPath string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	info, err := os.Stat(hostPath)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		// Single file.
		return tarFile(tw, hostPath, info.Name())
	}

	// Directory — walk and add all files.
	base := filepath.Base(hostPath)
	return filepath.Walk(hostPath, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(hostPath, path)
		name := filepath.Join(base, rel)
		if fi.IsDir() {
			return tw.WriteHeader(&tar.Header{
				Name:     name + "/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			})
		}
		return tarFile(tw, path, name)
	})
}

func tarFile(tw *tar.Writer, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return err
	}

	hdr := &tar.Header{
		Name: name,
		Size: info.Size(),
		Mode: int64(info.Mode()),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// untarToPath extracts a tar archive to a host directory.
func untarToPath(r io.Reader, hostPath string) error {
	tr := tar.NewReader(r)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(hostPath, filepath.Base(hdr.Name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o755) //nolint:errcheck
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
