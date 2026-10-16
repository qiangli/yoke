package stack

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
)

const (
	// Upstream releases whose tagged sources provide the wire paths used by
	// service.go: native OTLP ingestion plus LogsQL/PromQL queries. These lead
	// the bashy fork builds (builds/victoria-*) at bashy main 0607f515.
	victoriaLogsVersion    = "v1.53.0"
	victoriaMetricsVersion = "v1.154.0"
	victoriaTracesVersion  = "v0.12.0"

	// Bashy-signed fork release tags (builds/victoria-*/tool.env at 0607f515):
	// each is the upstream tag above built by the bashy-build workflow on the
	// qiangli/* mirror and published as raw executables with per-asset
	// SHA256SUMS and <asset>.sigstore.json bundles.
	victoriaLogsBashyTag    = "bashy-v1.53.0-2"
	victoriaMetricsBashyTag = "bashy-v1.154.0-2"
	victoriaTracesBashyTag  = "bashy-v0.12.0-2"
)

var (
	victoriaLogsTool    = victoriaTool("victoria-logs", victoriaLogsBashyTag, victoriaLogsSHAs)
	victoriaMetricsTool = victoriaTool("victoria-metrics", victoriaMetricsBashyTag, victoriaMetricsSHAs)
	victoriaTracesTool  = victoriaTool("victoria-traces", victoriaTracesBashyTag, victoriaTracesSHAs)
)

// SHA256 per asset read from the published SHA256SUMS of each bashy release
// (downloaded via curl -sL, not from the GitHub API asset digest).
var (
	victoriaMetricsSHAs = map[string]string{
		"linux/amd64":   "5cdf3534db2075efdc8a499a4fa630417d9061c0c34728b61944d342721365aa",
		"linux/arm64":   "c6619c2a0c29874209518d8202a143706156dda18e198a50f2169acf1de6af84",
		"darwin/amd64":  "f5e389bd007623821d20b7dac3d63fcb7d5bdaa5a73610dd5e0a8b907ce764ea",
		"darwin/arm64":  "11fa51950cbee34513506b8b16b3bbf22dd46165e8b607516e0ad58f244a85f0",
		"windows/amd64": "4fa0cca9de0eb898d5158d73dbfada99540d2ec20c9bace91020a259020930f3",
	}
	victoriaLogsSHAs = map[string]string{
		"linux/amd64":   "a66427b11d3c2811e06e5c4564bdae76b2644bb5a24123561bf379d4a9190b01",
		"linux/arm64":   "61e8baf9a096c5b6b241adb966893cf6bafc9bcb6aa226be8c5b594fafdaada9",
		"darwin/amd64":  "bb270654af2751a10079d0f23f9200dfbfec2ba275e6b13785ad38bf53b8b085",
		"darwin/arm64":  "7320cdad87fd61c9daa76fc3f28d431814d88defde32d7985ab76d64fac9fbd7",
		"windows/amd64": "7880d57a5e9daa7172905dbb30bced97afd59266ec5b663d89386b8fbe8d4a87",
	}
	victoriaTracesSHAs = map[string]string{
		"linux/amd64":   "ee7650f5fedb345dc8015fe921dd9f65687501988c51e614dd929d333bc3a7f7",
		"linux/arm64":   "4d29cd67c8fbab4091ad0c0cb2dd753ce85aad1890fdb03769c936229a9344d4",
		"darwin/amd64":  "f4850391b00bd080f8f33a8fb0078f20066f36faded2e8dccb4d03bbae35c40d",
		"darwin/arm64":  "aefacedbce9225d469820ed05419e3a6121deeb61b43fc347c19db2ae3d6016d",
		"windows/amd64": "ed3373a067adae2d1a0f4d1c5f3a30798b43d8db1b2a3d134a9200a74ebbf27c",
	}
)

func victoriaTool(tool, bashyTag string, shas map[string]string) binmgr.Tool {
	assets := map[string]binmgr.Asset{}
	for plat, sha := range shas {
		goos, goarch, _ := strings.Cut(plat, "/")
		assetName := tool + "-" + goos + "-" + goarch
		if goos == "windows" {
			assetName += ".exe"
		}
		url := fmt.Sprintf("https://github.com/qiangli/%s/releases/download/%s/%s", tool, bashyTag, assetName)
		assets[plat] = binmgr.Asset{URL: url, SHA256: sha}
	}
	identity := fmt.Sprintf("https://github.com/qiangli/%s/.github/workflows/bashy-build.yml@refs/tags/%s", tool, bashyTag)
	return binmgr.Tool{
		Name:    tool,
		Version: bashyTag,
		Assets:  assets,
		Sigstore: &binmgr.Sigstore{
			Identity: identity,
			Issuer:   binmgr.GitHubActionsIssuer,
		},
	}
}

// argvBuilder maps a store's stack configuration to its native command line.
type argvBuilder func(port int, dataDir, pathPrefix string) []string

// execStore runs one Victoria store as a managed subprocess. Keeping this type
// free of Victoria imports is essential: any two linked store libraries collide
// while registering their process-global flags during init.
type execStore struct {
	name       string
	tool       binmgr.Tool
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
	return newExecStore("victoria-logs", victoriaLogsTool, victoriaLogsArgs, port, dataDir)
}

func NewVictoriaMetricsComponent(port int, dataDir string) *execStore {
	return newExecStore("victoria-metrics", victoriaMetricsTool, victoriaMetricsArgs, port, dataDir)
}

func NewVictoriaTracesComponent(port int, dataDir string) *execStore {
	return newExecStore("victoria-traces", victoriaTracesTool, victoriaTracesArgs, port, dataDir)
}

func newExecStore(name string, tool binmgr.Tool, buildArgv argvBuilder, port int, dataDir string) *execStore {
	return &execStore{
		name: name, tool: tool, buildArgv: buildArgv, port: port, dataDir: dataDir,
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

	bin, err := binmgr.Ensure(ctx, s.tool)
	if err != nil {
		return fmt.Errorf("%s: fetch binary: %w", s.name, err)
	}

	watchCtx, cancel := context.WithCancel(ctx)
	proc, err := binmgr.Launch(watchCtx, bin, binmgr.RunSpec{
		Args:       s.buildArgv(s.port, s.dataDir, pathPrefix),
		Env:        localOTLPEnv(s.name),
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

	slog.Info(s.name+": started", "port", s.port, "dataDir", s.dataDir, "version", s.tool.Version)
	go s.watch(watchCtx)
	return nil
}

const localCollectorEndpoint = "http://127.0.0.1:31415"

func localOTLPEnv(serviceName string) []string {
	return []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + localCollectorEndpoint,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + localCollectorEndpoint + "/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=" + localCollectorEndpoint + "/v1/metrics",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=" + localCollectorEndpoint + "/v1/logs",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL=http/protobuf",
		"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL=http/protobuf",
		"OTEL_EXPORTER_OTLP_LOGS_PROTOCOL=http/protobuf",
		"OTEL_SERVICE_NAME=" + serviceName,
	}
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
	args := victoriaArgs(port, dataDir, pathPrefix)
	// VictoriaMetrics only self-scrapes when this interval is positive and only
	// pushes metrics when -pushmetrics.url is supplied. Keep both outbound paths
	// explicitly inactive. Upstream reference:
	// https://docs.victoriametrics.com/victoriametrics/#push-metrics
	// Flag source at v1.154.0: app/victoria-metrics/self_scraper.go:22
	//   selfScrapeInterval = flag.Duration("selfScrapeInterval", 0, ...)
	// Verified unchanged from v1.147.0.
	return append(args, "-selfScrapeInterval=0")
}

func victoriaTracesArgs(port int, dataDir, pathPrefix string) []string {
	return victoriaArgs(port, dataDir, pathPrefix)
}

// victoriaArgs builds the common flag set for all three Victoria stores.
//
// Flag sources at the pinned tags (all Apache-2.0):
//
//	-storageDataPath: app/vmstorage/main.go:29 (VictoriaMetrics v1.154.0),
//	  app/vlstorage/main.go:47 (VictoriaLogs v1.53.0),
//	  app/vtstorage/main.go:49 (VictoriaTraces v0.12.0)
//	-retentionPeriod: app/vmstorage/main.go:30 (1M default), app/vlstorage/main.go:29
//	  (7d), app/vtstorage/main.go:31 (7d) — defaults are appropriate; not set
//	  explicitly here.
//	-httpListenAddr: app/victoria-metrics/main.go:30, app/victoria-logs/main.go:25,
//	  app/victoria-traces/main.go:26 — flagutil.NewArrayString("httpListenAddr", ...)
//	-http.pathPrefix: lib/httpserver/httpserver.go:50 — flag.String("http.pathPrefix", ...)
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
