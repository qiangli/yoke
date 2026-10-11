# bashy service: unit registry and provider contract (Sprint 418 S15)

**Status:** S15 contract, filed 2026-10-10 PDT. Implemented here:
`pkg/fleet` (the `service` kind, its seven embedded units, the CRUD and
desired-state verbs) and `pkg/service` (the runtime provider contract and the
internal provider). The monitor (S16), built-in unit replacement (S17) and
outpost integration (S18) build on this contract and are not implemented yet.

**Design authority:** the S14 design of record, `dhnt/docs/bashy-service-design.md`,
and the Sprint 418 plan, `dhnt/docs/sprint-418-master-execution-plan.md`.
This file records what S15 settles inside them: the schema, the rings, the
port rule, the verbs and the provider interface that S16 and S18 code against.

## Roles: the inetd/systemd split

| Role | Unix analogue | Owner |
|---|---|---|
| Unit definitions and per-unit verbs | unit files + `systemctl` | `bashy service` (`fleet.NewServiceCmd`) |
| Service monitor | `inetd` | `bashy supervisord`, a bashy command (S16) |
| Init | `systemd` | `outpost supervisord`. It starts and watches only the outpost daemon and `bashy supervisord`. |

The init bootstrap is **not** a runtime provider. Init supervises the
monitor, and the monitor supervises units. A container runs
`bashy supervisord --pid1` with no outpost.

## Seven extension kinds

The extension contract is **resource / command / service / model / tool /
agent / skill**. `service` is registered in the fleet kind table
(`pkg/fleet/kinds.go`) like the others. It therefore inherits the generic
`show` / `add` / `set` / `rm` / `edit` / `schema` / `retire` verbs,
name-collision checks, `verify` enumeration, claim guards and ring assembly.
The S1 seven-kind round-trip matrix adds its service row from these verbs.
`add -f FILE|-` is opt-in per kind (`typedRecord.FileFlag`), so S1 can turn
it on for every kind.

## Unit schema

`bashy service schema` prints every path. The fields are:

| Field | Meaning |
|---|---|
| `name`, `kind: service`, `description` | Identity. |
| `provider` | `process` (default): a foreground child the monitor owns. `internal`: a capability registered in-process by its owning subsystem. |
| `exec` | Process only. The foreground argv. Placeholders: `{port}` / `{port.NAME}`, `{bind}` / `{bind.NAME}`, `{settings.KEY}`, `{download}`. An unresolved placeholder is refused when the unit is written. |
| `download` | Process only. A pinned release (`github` or `url`, `version`, per-platform `sha256`) that binmgr provisions. `{download}` is the cached path. It reuses `CommandDownload`, so pins are verified the same way for commands and services. |
| `workdir` | Process only. |
| `capability`, `owner` | Internal only. `capability` is a registered id (`meet.events`, `outpost.files`, …). `owner` is `outpost` or the name of the unit whose process hosts the capability, and that unit is an implied dependency. |
| `env`, `secrets[] {env, ref}` | Environment, plus vault references resolved by the monitor. Secrets are never stored inline. |
| `settings` | String map visible to the provider. `exec` reads it through `{settings.KEY}`, and an internal capability receives it as `Config.Settings`. It holds per-service parameters (act_runner labels and lane, sandbox limits, loom root URL, sdlc interval) without adding a schema field per service. |
| `ports[] {name, port, bind, protocol}` | Declared listeners. The first is the primary. `bind` is an address or `loopback` (default) / `lan`. |
| `health` | At most one of `http` (path), `tcp`, `exec`. Also `port`, `interval`, `timeout`, `failures`, `start_period`. With no probe, health means the process is alive. |
| `restart` | `policy` (`always` / `on-failure` / `no`), `backoff {min, max, healthy_after}`, `start_limit {burst, interval}`. |
| `stop {signal, timeout}` | TERM by default, with a grace period before KILL. |
| `requires` | Units that must be active first (systemd Requires+After). |
| `expose.app {name, port, root_url, require_login, elevation, trust_cloud_identity}` | Tunnel app route and its access policy. |
| `expose.mesh[] {name, port}` | Mesh service names. |
| `upgrade` | Behaviour after a bashy upgrade: `restart` (default) or `keep`. |
| `enabled` | Desired state. It is always serialized, so a `false` override survives the sparse overlay. |
| `retired` | The common registry lifecycle. A retired unit holds no port. |

Changes from the S14 example, each needed for coverage:

- **`ports[]` replaces the single `port`.** loom has two listeners (31880
  and 31881), and Zot / SeaweedFS / Kopia need named listeners for mesh
  names.
- **`provider` / `capability` / `owner` are new.** They implement the
  rod-not-fish internal provider.
- **`settings`, `secrets`, `download`, `stop` and `upgrade` are new.** They
  carry the parameters, secrets, version pins and upgrade behaviour that are
  configured in agent.json today.
- **`mesh` is a list of `{name, port}`.**

Unknown fields are refused (`KnownFields`), so an old `bashy_services` /
`supervised` shape cannot be loaded by accident. No converter exists.

## Rings and the built-in seeds

Units resolve through the fleet rings in this order: **embedded seeds →
shared (`$BASHY_SERVICES_PATH`) → org cache → local
(`$BASHY_SERVICES_DIR`, default `~/.config/bashy/services`)**.

- Built-ins are compiled in (`pkg/fleet/baseline/services/*.yaml`) and never
  copied to disk.
- They are mechanism, so they survive `BASHY_FLEET_SEEDS=off`.
- `set` on a seed writes a sparse local overlay (`overlay: true` plus only
  the changed paths), and `rm` of the overlay restores the seed.
- `add -f` of a seed's own YAML writes a full local copy, which is still the
  identity.

| Unit | Provider | Exec / capability | Ports | Health | Exposure |
|---|---|---|---|---|---|
| `apps` | process | `bashy apps serve --port {port} --bind {bind}` | 22749 | http `/healthz` | app `apps`, login |
| `meet` | process | `bashy meet serve --port {port} --bind {bind}` | 8637 | http `/healthz` | app `meet`, login, cloud identity |
| `loom` | process | `bashy loom serve --port {port} --addr {bind} --root-url {settings.root_url}` | 31880 http, 31881 proxy | http `/api/healthz` | app `loom`, login; mesh `git` |
| `sdlc` | process | `bashy sdlc service run --interval {settings.interval}` | none | process alive | none |
| `schedule` | process | `bashy schedule daemon` | none | process alive | none |
| `llm` | process | `bashy llm serve --port {port} --bind {bind}` | 24556 loopback | tcp | none (the mesh door is S5/S13) |
| `events` | internal | `meet.events`, hosted by `meet` | meet's listener | capability health | rides meet's route |

Seeds ship **disabled**. Turning a daemon on is an operator decision
(`bashy service enable`), and S18 removes the agent.json `*_enabled` keys
that used to make it.

**Events (a design boundary the plan left open), settled here:** `events` is
not a second process. S3's SSE route `GET /api/events` is served on meet's
listener, and the `events` unit is an *internal* unit that enables or
disables that capability inside meet. This means:

- there is no duplicate meet process or second listener;
- the unit declares no port of its own;
- `requires` is implied by `owner: meet`.

## Desired state and port conflicts

- `enable` / `disable` persist `enabled` through the same overlay path as
  `set`, so every rule a record must pass still applies. A running monitor
  reconciles the change live (S16). The verbs themselves never start or stop
  anything.
- **Port rule.** An *enabled* unit may not declare a listener that another
  enabled, unretired unit holds on the same protocol with an overlapping
  bind. `lan`, `0.0.0.0` and `::` overlap everything, and the loopback
  spellings are one address.
- The rule runs on `add`, `set` and `enable`. A refusal names the holder:
  `fleet: service "chat2": port 8637/tcp is held by service "meet" (bind loopback); disable "meet" or change the port`.
- A *disabled* unit holds nothing. Two units may therefore declare one port
  as alternatives, for example the seeded `llm` door and a third-party
  gateway, but only one of them can be enabled.
- Conflicts with processes outside the registry (foreign listeners) are
  detected at start time by the monitor (S16), not by the registry.

## Verbs

| Verb | Story | Notes |
|---|---|---|
| `service list [--json] [--retired]` | S15 | `bashy-registry-list-v1` envelope. Each row: name, provider, enabled, ports, requires (including the implied owner), app, mesh, ring. |
| `service show NAME [--yaml\|--json\|--field P]` | S15 | `show --yaml \| add -f -` is the identity. |
| `service add -f FILE\|-` / `add NAME --exec … --port N` / `set` / `rm` / `edit` / `schema` / `retire` / `unretire` | S15 | Generic fleet verbs. |
| `service enable\|disable NAME` | S15 | Desired state only. |
| `service start\|stop\|restart\|status\|logs\|run`, `supervisord [--pid1]` | S16 | Over the same records. `status --json` is `service.Status`. |

**MCP:** the tree is one cobra command (`fleet.NewServiceCmd`). bashy mounts
it as the `service` verb, the same way it mounts `fleet.NewCommandsCmd`, so
`bashy mcp` reaches every verb through the registry's `run_tool` / direct
tools with no MCP-specific code. Mounting it in `bashy/internal/agentos` is
the bashy-side integration step (one line beside `commands`). It sits
outside this repository and is the remaining part of the "every verb is an
MCP tool" acceptance.

## The provider contract (S15 / S16 / S18)

`pkg/service` defines the contract. Its rules:

1. **The declaration selects the provider, and nothing else does.**
   `Providers.For(unit)` dispatches on `unit.ProviderName()`. The monitor,
   exposure reconciler, outpost toggles and admin UI never branch on a
   unit's name. Adding a daemon means adding a record, never a provider and
   never a conditional.
2. **States are a closed set:** `inactive`, `starting`, `active`,
   `degraded`, `failed`, `stopping`. `State.Exposed()` (active or degraded)
   is the only test the exposure reconciler uses: everything else is
   withdrawn from the tunnel and the mesh. Nothing parses status text.
3. **Every transition is an `Event{Unit, From, To, Reason, At}`.** It is
   published on the room timeline as `service.<state>` (S3), and `failed`
   goes to the notify sinks (S7).
4. **`Provider` covers mechanism only:**
   `Name`, `Activate(ctx, unit, Runtime)`, `Configure(ctx, unit, Runtime)`
   (returns `ErrRestartRequired` when a change cannot be applied live),
   `Deactivate`, `Health` and `Status`. Policy is the monitor's, read
   identically for both providers: restart and backoff, start limits,
   `requires` ordering, the declared http/tcp/exec probe, and exposure.
   `Runtime` carries the values the monitor resolves once: the environment
   with secrets resolved, the provisioned download path, and `Emit`.
5. **The process provider is S16's.** It owns a foreground child in its own
   process group, so `stop` is deterministic. svcd's detached pidfile model
   retires.
6. **The internal provider is implemented here (`service.Internal`) and
   wired by S18.**
   - The owning subsystem keeps its implementation and registers it as a
     `Capability` (`Activate` / `Configure` / `Deactivate` / `Health`) in a
     per-process `Registry`.
   - `NewInternal(self, reg)` serves only units whose `owner` equals
     `self`. Other units return `ErrNotOwner`, because the hosting process
     applies them.
   - `Status.PID` is always 0: no fake child, no wrapper executable.
   - `Configure` is always live.
   - A capability is on exactly when its unit is enabled and activated.
     There is no parallel feature flag.
7. **Status IPC is S16's,** over a local socket the monitor serves. Its
   payload is `[]service.Status`. With no monitor running, `service status`
   reports desired state with `state: inactive` and does not guess.

Consumers code against `pkg/fleet.Service` (the declaration) and
`pkg/service` (the runtime). Neither is extended per service. A capability
that is missing is added as a reusable field, setting or placeholder.

## Retained service and feature coverage

This is the complete current inventory, taken on 2026-10-10 from the outpost,
bashy, yoke and coreutils trees.

- **Each row** names its new home: a unit (seeded, or an operator/recipe
  unit), an internal capability, or a retained owner outside the unit
  model.
- **Each row** also names the existing tests that pin its behaviour today.
  Those tests are the regression floor S17/S18 must keep green or port.
- **Columns:** "S17"/"S18" mark the story that moves the row; "recipe" means
  a unit YAML in `bashy/examples/fleet/services/` with the same schema
  rather than a seed.

Paths: `O` = outpost, `Y` = yoke, `B` = bashy, `C` = coreutils.

| Current surface | New home | Settings / exposure carried by the unit | Existing evidence |
|---|---|---|---|
| Apps console (`bashy_apps_*`, svcd) | seed `apps` (S17) | `--pair` and `--launcher` in exec. Port 22749 (the legacy 8639 is gone). `bind: lan`. App with login, elevation and cloud identity. SSO secret through `secrets`. | `O/cmd/outpost/bashy_apps_test.go` (12 tests); `Y/pkg/webconsole/service_test.go`, `e2e_test.go`; `adminui/builtins_test.go` TestBuiltinsToggleBashyAppsREST |
| meet (`meet_enabled`, `meet_port`) | seed `meet` (S17) | Port 8637. App with login and cloud identity. Today's bug (a `meet_port` override never reaches `--port`) disappears because exec expands `{port}`. | `O/cmd/outpost/meet_test.go` (7); `Y/pkg/meet/service_test.go`, `service_json_stopped_test.go`, `serve_test.go` |
| loom / Gitea (`loom_enabled`, `loom_port`) | seed `loom` (S17) | Gitea pinned (v28.1.0) by loom's binmgr spec. Ports 31880 and 31881. `settings.root_url` from cloudbox. Mesh `git`. App with login. `LOOM_*` env. | `Y/external/loom/loom_test.go` (~40, e.g. TestStartRefusesWhenTheGiteaPortIsHeldByAForeignProcess); `admincore/builtins_loom_optin_test.go` |
| SDLC loop | seed `sdlc` (S17) | `settings.interval`. `--runs-dir`, `--config`, `--cwd` and `--sandbox` through an exec override. | `Y/pkg/sdlc/service_test.go` |
| schedule daemon | seed `schedule` (S17). The service code moves above coreutils (import rule). | `--interval` through exec. The kernel lease stays inside the daemon. | `C/pkg/schedule/service_concurrency_unix_test.go`, `schedule_test.go` |
| LLM door (`llm up/down`, `EnsureUp`) | seed `llm` (S17) | Port 24556. Bind loopback or lan. `--policy`, `--max-in-flight`, `--no-engine` and `--no-cli` through exec. Owner socket unchanged. | `Y/pkg/broker/broker_test.go`, `stop_test.go`, `sticky_live_test.go` (live) |
| events (S3, new) | seed `events`: internal `meet.events` on meet (S3/S17) | No port. Rides meet's route. | S3 acceptance (contiguous Seq, SSE resume, resync) |
| Zot registry | seed `zot` (S17) | Port 5000. Data dir in `settings`. Mesh `registry`. Gains restart, which it lacks today. Launcher parity with `bashy zot serve`. | `Y/external/zot/zot_test.go`; `Y/pkg/fleet/service_test.go` |
| SeaweedFS S3 | seed `seaweedfs` (S17) | Port 8333. Data dir in `settings`. Mesh `s3`. Launcher parity with `bashy seaweedfs serve`. | `Y/external/seaweedfs/seaweedfs_test.go`; `Y/pkg/fleet/service_test.go` |
| Kopia backup | seed `kopia` (S17) | Port 51515. Data dir in `settings`. Mesh `backup`. Launcher parity with `bashy kopia serve`. | `Y/external/kopia/kopia_test.go`; `Y/pkg/fleet/service_test.go` |
| act_runner (host and sandbox lanes) | recipe unit, no ports or exposure (consumer only) (S18) | `settings`: instance (default local loom), labels (default `host:host`), sandbox, sandbox_image, docker_host. Registration token in `secrets`. `requires: [loom]` when the forge is local. `mesh_consumes` stays a mesh setting. | `Y/external/actrunner/actrunner_test.go` (TestSandboxLabel, TestRegisterOptionsSandboxLabels) |
| Headlamp port-forward | recipe unit `exec: [bashy, kubectl, port-forward, …]` with `bind: loopback`. `requires` names the control-plane unit. Mesh `headlamp`. (S18) | Port 18466. **Today it is configured but never started** (`O/internal/agent/headlamp` has no importer), so the unit is its first working lifecycle. | `O/internal/agent/headlamp/{headlamp,deploy,posture}_test.go`; TestSetBuiltinsHeadlampParity |
| Custom supervised programs (`supervised[]`) | operator units (`add -f`) | path/args → `exec`; `dir` → `workdir`; `env`; `log_path` → the monitor's log dir. `enabled`. | `O/internal/agent/supervisor/manager_test.go`; `conf/file_test.go` TestSupervisedPrograms* (ported to unit tests by S16) |
| Custom bashy services (`bashy_services[]`) | operator units | `command`/`args` → `exec`. app_name/app_port, require_login, elevation_required, trust_cloud_identity → `expose.app`. mesh_service → `expose.mesh`. root_url → `settings`. sso_secret and secrets_env → `secrets`. | `O/cmd/outpost/bashy_service_supervisor_test.go`; `admincore/builtins_test.go` TestSetBuiltinsPersistsGenericBashyServices |
| outpost files (File Browser) | internal `outpost.files` (S18) | `settings`: allow_write, scope. App with login. | `O/cmd/outpost/files_archive_formats_test.go` |
| outpost podman proxy | internal `outpost.podman` (S18) | App with role admin and login. Socket detection stays in the capability. | `builtin_apps_test.go` TestDetectPodman*; `live_podman_test.go`; `localsock/live_test.go`; `O/scripts/qa-poller-podman.sh` |
| outpost sandbox / container proxy and prewarm | internal `outpost.sandbox` (S18) | `settings`: memory, cpus, pids, max containers, allowed images, scratch dir, prewarm images. | `O/internal/agent/sandbox/{filter,service,prewarm}_test.go` |
| outpost Ollama proxy, pool, warm serving, sharding | internal `outpost.ollama`, `outpost.ollama-pool`, `outpost.warm`, `outpost.shard` (S18) | `settings`: warm budget, shard role, peers, node bytes and binaries. | `ollama/watcher_test.go`; `warm/warm_test.go`; `shard/*_test.go` TestShardChannelsOverRealMesh; `conf/shard_config_test.go` |
| LAN inference proxy | internal `outpost.lan-inference` (S18) | Port 11436, `bind: lan` (so it is checked for port conflicts). | `conf/file_test.go` TestLANInference* |
| otel / ycode surfaces | internal `outpost.otel`, `outpost.ycode` (S18) | `settings`: pool, share. | covered via outpost short suite |
| AppRegistry, mesh forwarder and `mesh_services` | **retained owner: outpost.** It is the exposure reconciler fed by `expose` and `State.Exposed()`, not a unit. | Mesh pairing, rendezvous, `mesh_consumes`. Cloud identity and SSO secrets per app. | `internal/agent/apps_test.go`; TestRegisterBashyServiceAppsRejectsTrustedIdentityWithoutSecret; `mesh/mdns_test.go` |
| `builtins set`, MCP `outpost_set_builtins`, admin UI toggles | **retained surfaces, re-pointed** to `bashy service enable\|disable\|set` (S18) | No restart message. Live through the monitor. | `admincore/builtins_test.go`; `adminui/builtins_test.go` |
| bashy binary pin and upgrade (`bashy_version`, `ReconcileExisting`) | **retained owner: outpost init** | After an upgrade, init restarts `bashy supervisord`, which applies each unit's `upgrade` policy. | TestSetBuiltinsPersistsBashyVersionPin |
| apphealth | **folded into unit health.** The package is unwired today (no importer) and retires with S18. | Declared `health` probes. | `apphealth_test.go` (retires) |
| outpost backup scheduler, mirror, peerplane, recipebuilder, LAN SSH | **retained owner: outpost daemon** (core daemon functions, not toggled services today). Promoting one to an internal unit is a reusable S18 change, not new machinery. | none | `mirror/supervisor_test.go`; outpost short suite |

**Removed outright, with no reader, converter or alias:**

- **agent.json keys:** `bashy_services`, `supervised`, `meet_enabled`,
  `meet_port`, `loom_enabled`, `loom_port`, `bashy_apps_enabled`,
  `bashy_apps_port`, and the per-service `*_enabled` / `*_port` / `actrunner_*`
  / `headlamp_*` / `sandbox_*` keys that the units above replace.
- **Lifecycle code:** the outpost 30 s poller (`superviseBashyService`), and
  the private start/status/stop copies in `Y/pkg/svcd`, `Y/pkg/meet/service.go`,
  `Y/pkg/sdlc/service.go`, `C/pkg/schedule/service.go`, loom's
  `StartDaemon`/`StopDaemon` and the broker's `llm up/down`.
- **Other:** the "stopped" / "not running" status-text wire format.

S17 removes the bashy-side items and S18 the outpost-side ones.

## External service seeds: launcher parity, binary CLI mismatches, and schema gaps (Sprint 418 S17)

The seeds for Zot, SeaweedFS, and Kopia (`pkg/fleet/baseline/services/{zot,seaweedfs,kopia}.yaml`) provide generic process-provider service units preserving the options and defaults from `yoke/external/{zot,seaweedfs,kopia}` and Outpost startup:
- **Zot (`zot`):** Port 5000, loopback bind, health probe `http: /v2/` (start period 30s), mesh service `registry`, data dir in `settings.data_dir`.
- **SeaweedFS (`seaweedfs`):** Port 8333, loopback bind, health probe `http: /` (start period 45s), mesh service `s3`, data dir in `settings.data_dir`.
- **Kopia (`kopia`):** Port 51515, loopback bind, health probe `http: /` (start period 30s), mesh service `backup`, data dir in `settings.data_dir`.

### Documented binary CLI mismatches & schema gaps:

1. **Raw binary CLI vs Pinned Launcher Parity:**
   - **Zot:** The upstream raw `zot` binary only accepts a JSON configuration file path (`zot serve <config.json>`); it does not accept CLI flags `--port`, `--addr`, or `--data`. The `bashy zot serve` launcher generates and provisions the required configuration on first run.
   - **Kopia:** The upstream raw `kopia` binary requires repository creation (`repository create filesystem`), configuration persistence (`--config-file`), and repository password management (`KOPIA_PASSWORD`) before `server start`. The `bashy kopia serve` launcher performs first-run repository initialization and password generation.
   - **SeaweedFS:** Upstream `weed` binary uses Go `flag` syntax (`-flag=val`), e.g., `-dir=... -ip.bind=... -s3 -s3.port=...`. The `bashy seaweedfs serve` launcher wraps this with standard POSIX/GNU flags (`--port`, `--addr`, `--data`).
   - Consequently, the unit seeds invoke the pinned launcher commands (`bashy {zot,seaweedfs,kopia} serve --port {port} --addr {bind} --data={settings.data_dir}`), maintaining lifecycle and provisioning parity without requiring name-specific supervisor or outpost branches.

2. **Launcher Flag Inconsistencies:**
   - Version flags: Zot uses `--zot-version`, while SeaweedFS and Kopia use `--version`.
   - Bind flags: Zot, SeaweedFS, and Kopia launchers use `--addr`, whereas `apps` and `llm` use `--bind`.
   - The YAML seeds faithfully use the actual launcher flags supported by each binary rather than inventing nonexistent flags.

3. **Dynamic binmgr Resolution vs S15 Static `CommandDownload` Digests:**
   - S15 `CommandDownload` requires a static version (strictly rejecting `"latest"`) and a map of static 64-hex SHA256 digests per platform.
   - `yoke/external/{zot,seaweedfs,kopia}` uses binmgr dynamic GitHub release resolution with `DefaultVersion = "latest"` and matches release assets at runtime.
   - Version and digest provisioning are handled by the launcher via binmgr and can be overridden via `settings` and environment variables (`ZOT_VERSION`, `SEAWEEDFS_VERSION`, `KOPIA_VERSION`). Operators wishing to pin static digests via S15's `{download}` can supply the per-platform SHA256 map in custom overlays.

## S15 acceptance evidence

Run with `go test ./pkg/fleet ./pkg/service`. Unset any ambient
`BASHY_MINT_SPRINT` first, because it leaks into the existing CLI goldens.

| Acceptance | Test |
|---|---|
| Seven seeded built-ins, embedded and never copied, survive seeds-off | `TestBuiltinServiceSeedsAreEmbeddedAndValid` |
| `show --yaml \| add -f -` is the identity for a built-in and a custom unit | `TestServiceShowYAMLAddFileIsIdentity` |
| A conflicting port is refused at add/set/enable, naming the holder; a disabled alternative is allowed | `TestServicePortConflictNamesHolder` |
| A built-in, an internal capability and a fresh custom unit use the same verbs; an operator override is a sparse overlay that changes the argv | `TestServiceSameVerbsForBuiltinInternalAndCustom` |
| The schema refuses malformed units (provider mix, inline secrets, unknown listeners, unresolved placeholders, old keys) | `TestServiceValidateRefusesMalformedUnits` |
| Argument templating | `TestServiceExpandExec` |
| Provider dispatch uses the provider field, not the name | `TestProvidersDispatchOnProviderField` |
| Internal lifecycle with no PID, structured events, live configure | `TestInternalProviderLifecycleHasNoPID` |
| Foreign owner refused; a failed capability is reported and not exposed | `TestInternalProviderRefusesForeignOwnerAndReportsFailure` |

**Remaining for "every verb is an MCP tool":** bashy mounts
`fleet.NewServiceCmd()` as its `service` verb, after which `bashy mcp`
reaches it through the existing dispatch. That change is in the bashy
repository and is not made here.
