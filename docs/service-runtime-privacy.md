# Managed service runtime privacy

Bashy-owned service launchers pin every standard OTLP endpoint (including the
signal-specific overrides) to the local OTLP/HTTP proxy at
`http://127.0.0.1:31415`. They never inherit an arbitrary
`OTEL_EXPORTER_OTLP_ENDPOINT`. The table records each upstream control used by
the generated configuration.

| Service | Tracking/update control | OpenTelemetry control | Upstream evidence |
| --- | --- | --- | --- |
| Loom (Gitea) | `[cron.update_checker] ENABLED = false` | No native exporter in the pinned server; endpoint env is still contained | [Gitea update-check configuration](https://docs.gitea.com/administration/config-cheat-sheet/#cron---check-for-new-gitea-versions-cronupdate_checker) |
| act_runner | No updater or analytics setting exists in its config; child and job endpoint env is contained | No native exporter; endpoint env is still contained | [act_runner configuration example](https://gitea.com/gitea/act_runner/src/branch/main/internal/pkg/config/config.example.yaml) |
| SeaweedFS | `-master.telemetry=false` | No native OTLP exporter; endpoint env is still contained | [SeaweedFS server flags](https://github.com/seaweedfs/seaweedfs/blob/master/weed/command/server.go) |
| Kopia | `--no-check-for-updates`; no anonymous usage-report feature exists in the server CLI | `--otlp-trace`, endpoint pinned locally | [Kopia global flags](https://kopia.io/docs/reference/command-line/flags/) |
| Zot | Optional extensions are emitted as an empty object, leaving sync, CVE feeds, events, and remote metrics off | No native OTLP exporter; endpoint env is still contained | [Zot configuration reference](https://github.com/project-zot/zot/blob/main/examples/README.md) |
| Ollama | Managed server does not start the desktop updater; `OLLAMA_NO_CLOUD=1` disables cloud inference and web search | No native OTLP exporter; endpoint env is still contained | [Ollama cloud opt-out](https://github.com/ollama/ollama/blob/main/docs/faq.mdx#how-can-i-disable-ollama-cloud), [desktop updater source](https://github.com/ollama/ollama/blob/main/app/updater/updater.go) |
| VictoriaLogs | No `-pushmetrics.url` is supplied | Native OTLP is an ingestion interface, not an exporter; child endpoint env is contained | [Victoria push-metrics flags](https://docs.victoriametrics.com/victoriametrics/#push-metrics) |
| VictoriaMetrics | No `-pushmetrics.url`; `-selfScrapeInterval=0` | Native OTLP is an ingestion interface, not an exporter; child endpoint env is contained | [Victoria push-metrics flags](https://docs.victoriametrics.com/victoriametrics/#push-metrics) |
| VictoriaTraces | No `-pushmetrics.url` is supplied | Native OTLP is an ingestion interface, not an exporter; child endpoint env is contained | [Victoria push-metrics flags](https://docs.victoriametrics.com/victoriametrics/#push-metrics) |
| Headlamp v0.27.0 | The pinned web/server release contains no analytics reporter or analytics-disable flag | The pinned release predates Headlamp's native telemetry configuration, so no unsupported flag is injected | [v0.27.0 server configuration](https://github.com/kubernetes-sigs/headlamp/blob/v0.27.0/backend/pkg/config/config.go), [current telemetry controls](https://headlamp.dev/docs/latest/development/telemetry/) |

Kopia v0.23.1's `--otlp-trace` implementation uses the Go OTLP/gRPC exporter.
The endpoint remains pinned to bashy's loopback collector; a collector transport
failure is local and cannot fall back to a vendor destination.
