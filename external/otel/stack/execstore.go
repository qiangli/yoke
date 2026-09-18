package stack

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// These are pinned community releases whose tagged sources provide the wire
	// paths used by service.go: native OTLP ingestion plus LogsQL/PromQL queries.
	victoriaLogsVersion    = "v1.51.0"
	victoriaMetricsVersion = "v1.147.0"
	victoriaTracesVersion  = "v0.9.4"
)

var (
	victoriaLogsSpec = victoriaSpec(
		"victoria-logs", "VictoriaMetrics/VictoriaLogs", victoriaLogsVersion,
	)
	victoriaMetricsSpec = victoriaSpec(
		"victoria-metrics", "VictoriaMetrics/VictoriaMetrics", victoriaMetricsVersion,
	)
	victoriaTracesSpec = victoriaSpec(
		"victoria-traces", "VictoriaMetrics/VictoriaTraces", victoriaTracesVersion,
	)
)

// argvBuilder maps a store's stack configuration to its native command line.
type argvBuilder func(port int, dataDir, pathPrefix string) []string

// execStore runs one Victoria store as a managed subprocess. Keeping this type
// free of Victoria imports is essential: any two linked store libraries collide
// while registering their process-global flags during init.
type execStore struct {
	name       string
	spec       binmgr.GitHubSpec
	buildArgv  argvBuilder
	port       int
	dataDir    string
	pathPrefix string

	mu          sync.Mutex
	proc        *binmgr.Process
	watchCancel context.CancelFunc
	starting    bool
}

var (
	_ Component          = (*execStore)(nil)
	_ PrefixConfigurable = (*execStore)(nil)
)

func NewVictoriaLogsComponent(port int, dataDir string) *execStore {
	return newExecStore("victoria-logs", victoriaLogsSpec, victoriaLogsArgs, port, dataDir)
}

func NewVictoriaMetricsComponent(port int, dataDir string) *execStore {
	return newExecStore("victoria-metrics", victoriaMetricsSpec, victoriaMetricsArgs, port, dataDir)
}

func NewVictoriaTracesComponent(port int, dataDir string) *execStore {
	return newExecStore("victoria-traces", victoriaTracesSpec, victoriaTracesArgs, port, dataDir)
}

func newExecStore(name string, spec binmgr.GitHubSpec, buildArgv argvBuilder, port int, dataDir string) *execStore {
	return &execStore{
		name: name, spec: spec, buildArgv: buildArgv, port: port, dataDir: dataDir,
	}
}

func (s *execStore) Name() string { return s.name }

func (s *execStore) SetPathPrefix(prefix string) {
	s.mu.Lock()
	s.pathPrefix = strings.TrimSuffix(prefix, "/")
	s.mu.Unlock()
}

func (s *execStore) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.proc != nil || s.starting {
		s.mu.Unlock()
		return fmt.Errorf("%s: already started", s.name)
	}
	s.starting = true
	pathPrefix := s.pathPrefix
	s.mu.Unlock()

	started := false
	defer func() {
		if !started {
			s.mu.Lock()
			s.starting = false
			s.mu.Unlock()
		}
	}()

	if !IsPortAvailable(s.port) {
		return fmt.Errorf("%s: port %d already in use", s.name, s.port)
	}
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return fmt.Errorf("%s: create data directory: %w", s.name, err)
	}

	tool, err := binmgr.ResolveGitHub(ctx, s.spec)
	if err != nil {
		return fmt.Errorf("%s: resolve binary: %w", s.name, err)
	}
	bin, err := binmgr.Ensure(ctx, tool)
	if err != nil {
		return fmt.Errorf("%s: fetch binary: %w", s.name, err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	proc, err := binmgr.Launch(watchCtx, bin, binmgr.RunSpec{
		Args:       s.buildArgv(s.port, s.dataDir, pathPrefix),
		HealthURL:  s.healthURL(pathPrefix),
		HealthWait: 30 * time.Second,
	})
	if err != nil {
		cancel()
		return fmt.Errorf("%s: launch: %w", s.name, err)
	}

	s.mu.Lock()
	s.proc = proc
	s.watchCancel = cancel
	s.starting = false
	s.mu.Unlock()
	started = true

	slog.Info(s.name+": started", "port", s.port, "dataDir", s.dataDir, "version", tool.Version)
	go s.watch(watchCtx)
	return nil
}

func (s *execStore) Stop(_ context.Context) error {
	return s.stopProcess()
}

// Healthy probes the store rather than merely checking that it was launched.
func (s *execStore) Healthy() bool {
	s.mu.Lock()
	running := s.proc != nil
	prefix := s.pathPrefix
	s.mu.Unlock()
	if !running {
		return false
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(s.healthURL(prefix))
	ok := err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300
	if err == nil {
		_ = resp.Body.Close()
	}
	return ok
}

func (s *execStore) HTTPHandler() http.Handler { return nil }
func (s *execStore) Port() int                 { return s.port }

// watch ties the subprocess to the component context and continuously exposes
// an unexpected exit through Healthy. binmgr.Process owns termination and wait,
// so the watcher deliberately probes HTTP instead of racing Process.Stop with a
// second call to Cmd.Wait.
func (s *execStore) watch(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	wasHealthy := true
	for {
		select {
		case <-ctx.Done():
			_ = s.stopProcess()
			return
		case <-ticker.C:
			ok := s.Healthy()
			if wasHealthy && !ok {
				slog.Error(s.name + ": subprocess is unhealthy")
			}
			wasHealthy = ok
		}
	}
}

func (s *execStore) stopProcess() error {
	s.mu.Lock()
	proc := s.proc
	cancel := s.watchCancel
	s.proc = nil
	s.watchCancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if proc == nil {
		return nil
	}
	err := proc.Stop(10 * time.Second)
	slog.Info(s.name + ": stopped")
	return err
}

func (s *execStore) healthURL(prefix string) string {
	return "http://127.0.0.1:" + strconv.Itoa(s.port) + prefix + "/health"
}

func victoriaLogsArgs(port int, dataDir, pathPrefix string) []string {
	return victoriaArgs(port, dataDir, pathPrefix)
}

func victoriaMetricsArgs(port int, dataDir, pathPrefix string) []string {
	return victoriaArgs(port, dataDir, pathPrefix)
}

func victoriaTracesArgs(port int, dataDir, pathPrefix string) []string {
	return victoriaArgs(port, dataDir, pathPrefix)
}

func victoriaArgs(port int, dataDir, pathPrefix string) []string {
	args := []string{
		"--storageDataPath=" + filepath.Join(dataDir, "data"),
		"--httpListenAddr=127.0.0.1:" + strconv.Itoa(port),
	}
	if pathPrefix != "" {
		args = append(args, "--http.pathPrefix="+pathPrefix)
	}
	return args
}

func victoriaSpec(name, repo, version string) binmgr.GitHubSpec {
	return binmgr.GitHubSpec{
		Name:       name,
		Repo:       repo,
		Version:    version,
		Member:     victoriaArchiveMember(name),
		AssetMatch: victoriaAssetMatch(name),
	}
}

// Community archives are named <product>-<os>-<arch>-<version>. Enterprise
// and cluster archives carry the same platform tokens and must be rejected.
func victoriaAssetMatch(product string) func(string, string, string) bool {
	return func(assetName, goos, goarch string) bool {
		n := strings.ToLower(assetName)
		if strings.Contains(n, "enterprise") || strings.Contains(n, "cluster") {
			return false
		}
		return strings.HasPrefix(n, product+"-"+goos+"-"+goarch+"-")
	}
}

func victoriaArchiveMember(product string) string {
	if runtime.GOOS == "windows" {
		return product + "-windows-" + runtime.GOARCH + "-prod"
	}
	return product + "-prod"
}
