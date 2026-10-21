# dks expose: reach a host service from a cluster workspace

`bashy dks expose` makes ONE service on the Outpost host reachable from pods in a
dks workspace, over the existing matrix-tunnel app route and nothing else.
Default-deny: nothing exposed means nothing reachable. Status: contract (Sprint 487);
the Outpost expose guard and the yoke implementation land in later stories.

## Transport

Only the app route `<satellite>/matrix/h/<host>/app/<app>/`. No STCP visitor, no
new listener on the host.

- HTTP kinds use the Outpost HTTP app proxy.
- TCP kinds use an Outpost `tcp`-scheme app: a WebSocket upgrade on `/app/<app>/`
  is byte-bridged to the target (client side mirrors Outpost's outbound `bridgeTCP`).

## Token guard

An exposed app is registered with
`outpost apps add <app> --target <t> --expose-token-sha256 <hex> --expose-expires <RFC3339>`.

- Every HTTP request and WebSocket upgrade to `/app/<app>/` must carry
  `X-Dhnt-Expose-Token: <token>`. Missing or wrong token: 403. Now past expires: 403
  (TTL cut is immediate, no restart). The guard strips the header before proxying.
- The satellite edge strips `Authorization` and applies CSRF to POST unless it is
  Bearer-shaped, so the relay ALSO sends `Authorization: Bearer <token>`. That is for
  the edge only and is never used for auth.
- Apps without an expose hash behave as today.
- Token = 32 random bytes, hex. Only its sha256 goes to Outpost. No new Bearer scope:
  the token is an app-local credential, like a door peer token.

## CLI

Runs ON the Outpost host (local `outpost apps add`; remote hosts are out of scope).

```
bashy dks expose <http://127.0.0.1:P | tcp://host:port | proxy:egress | proxy:git> \
    --as <name> [--ttl 1h] [--allow a,b] [--env K=V] [--force]
bashy dks unexpose <name>     # outpost apps rm + delete the labelled Secret
bashy dks exposed             # NAME KIND TARGET EXPIRES STATE, from the Secrets
```

- `--ttl` defaults to 1h, max 24h.
- `proxy:egress` and `proxy:git` REQUIRE `--allow`. Egress: `host` or `*.domain[:port]`.
  Git: `owner/repo` or `owner/*`. A bastion is a `tcp://` expose with a fixed target.
- Refused without `--force`: targets that are Outpost built-in/admin-grade apps
  (files, shell/terminal, desktop, ssh, periscope, the admin UI port) and app names
  that collide with built-ins.
- Audit: one line per expose, unexpose and refusal in the dks run journal. Tokens
  never appear in argv, logs, errors or the journal.

## The relay runs IN the run pod (S487.2)

`bashy dks relay` is a hidden subcommand that runs in the normal dks run image. Its
token comes from the environment and from nowhere else; neither the token nor the
app-route URL is ever in argv or logs.

| Mode | Behaviour |
|------|-----------|
| `--mode http` | reverse-proxy to the app route URL, adds both headers, optional `--path-prefix` (e.g. `/k/<door-peer>/`) |
| `--mode tcp` | per connection, WebSocket-dial the app route with both headers, byte-bridge |
| `--mode egress` | HTTP CONNECT proxy; per CONNECT, WebSocket-dial `/app/<app>/?dst=host:port` (S487.3) |

The form the rendered Pod actually runs is the wrapper:

```
bashy dks relay --all -- <the run's own command>
```

It reads the run's relay list from `$DKS_RELAYS` (JSON, no secrets), binds every
relay on `127.0.0.1:<local-port>` BEFORE anything else runs, then runs the command
as a child with the wrapper's stdio, forwards signals to it, and exits with the
child's exit code. Each relay's token arrives as `$DKS_EXPOSE_TOKEN_<NAME>`, via
`valueFrom.secretKeyRef` on the expose's own Secret — never a literal, never argv.
A relay stops accepting at its expiry; a relay already past its expiry at start is
reported and skipped, because the run is not the expose.

**Why in-pod, and not a relay Deployment behind a Service.** Live findings on the
satellite DKS (vk-podman executor, 2026-10-21):

- A relay Deployment needed run-pod placement to be schedulable at all.
- Even running, a pod on a vk-podman node CANNOT use a cluster Service: its DNS is
  podman's resolver (`Lookup db on 10.x.x.x:53: no such host`) and it sits on the
  podman bridge, so a ClusterIP is unroutable (`web:8080` connection refused).
- vk-podman supports exactly ONE container per Pod, so a sidecar is not available
  either.

In-pod is also portable to any Kubernetes and a tighter grant than default-deny
through a Service: only the run that was given the exposes can reach them.

## Cluster objects

Per expose `<name>`, in the workspace namespace: EXACTLY ONE object — the Secret
`dks-expose-<name>`, labelled `dks.dhnt.io/expose=<name>` (plus the bashy
ownership and workspace labels). No Deployment, no Service.

Its `stringData` holds `EXPOSE_TOKEN`; its annotations hold the whole record, so
`exposed` and `dks run` read an expose back from the Secret and no second store
exists anywhere:

| Annotation | Value |
|------------|-------|
| `dks.dhnt.io/expires` | RFC3339 expiry |
| `dks.dhnt.io/expose-kind` | `http`, `tcp`, `egress`, `git` |
| `dks.dhnt.io/expose-target` | the target as typed (proxy kinds: the allow list) |
| `dks.dhnt.io/expose-allow` | the cleaned allow list (proxy kinds) |
| `dks.dhnt.io/expose-app-url` | the app route an in-pod relay dials |
| `dks.dhnt.io/expose-upstream` | a git expose's git host (the `insteadOf` match) |
| `dks.dhnt.io/expose-port` | the expose's own port (a tcp target port; 8080 / 3128) |
| `dks.dhnt.io/expose-env` | the recorded `--env` entries, JSON, `{url}` unsubstituted |

`bashy dks run` / `--session` plans one deterministic LOOPBACK port per live
expose, in expose-name order, and injects both halves off that one plan — the
manifest relay list the pod's wrapper reads, and the environment naming it:

- fixed claims first: `egress` takes `3128`, a `tcp` expose keeps its own target
  port when free (else the next free from `18180`)
- then `http` and `git` fill `18080` upward around them
- `DKS_<NAME>_URL=http://127.0.0.1:<p>` (http, git, egress) or
  `DKS_<NAME>_ADDR=127.0.0.1:<p>` (tcp)
- egress also: `HTTPS_PROXY`/`HTTP_PROXY=http://127.0.0.1:3128`,
  `NO_PROXY=localhost,127.0.0.1,.svc,.cluster.local`
- git also: `GIT_CONFIG_COUNT`/`GIT_CONFIG_KEY_<i>`/`GIT_CONFIG_VALUE_<i>` wiring
  `url.http://127.0.0.1:<p>/.insteadOf <upstream>/`, so a plain `git clone` in the
  pod goes through the relay with NO credential in the pod
- plus any `--env K=V` recorded on the expose (stored on the Secret annotation,
  `{url}` substituted)

A value the dag manifest `Env:` line cannot carry — the comma-separated `NO_PROXY`
bypass list — rides the run Secret under a `dks-expose` ref instead, and the pod
still receives it via `secretKeyRef`.

## Verified against source

Checked read-only in the outpost source (a weave workspace checkout of outpost, since
no `../outpost` sibling exists on this host).

- `cmd/outpost/apps.go`: `appsAddCmd` / `appsRmCmd` default to `runAppUpsertViaMCP` and
  the `outpost_delete_app` MCP call through `dialMCP`, i.e. they talk to the RUNNING
  daemon (`outpost start` required; "not reachable" error otherwise). `--offline`
  edits FileConfig only. Live path: `admincore.Server.UpsertApp` validates, persists,
  then `Apps.Unregister` + `Apps.RegisterFromConfig`; `DeleteApp` persists then
  `Apps.Unregister`. So `apps add`/`rm` without `--offline` make the app live
  immediately; never use `--offline` for expose.
- `cmd/outpost/apps.go` `appsAddCmd` has NO `--expose-token-sha256` / `--expose-expires`
  flags, and `conf.AppConfig` / `AppRegistry` have no expose guard. Nothing named
  "expose" exists in `internal/agent/apps.go`. The guard is entirely Outpost-side
  work still to be done.
- `internal/agent/apps.go` `RegisterFromConfig` / `registerTCP` / `serveTCPBridge`:
  `scheme=tcp` registers name to `host:port`; the `/app/<name>/` handler accepts a
  WebSocket (`websocket.Accept`, binary `NetConn`), dials the target with a 5s timeout
  and copies both ways. It reads no query string, so `?dst=host:port` (egress) is not
  supported: a tcp app has a fixed target.
- `internal/agent/outbound.go` `bridgeTCP`: dial URL is
  `ws(s)://<server>/matrix/h/<host>/app/<name>/` with headers `Authorization: Bearer
  <access token>` and cookie `matrix_elev=<cookie>`, `websocket.MessageBinary`
  `NetConn`, 15s dial timeout. The relay's dial shape is the same minus the elev
  cookie, plus `X-Dhnt-Expose-Token`.
- Built-in app names (`internal/agent/builtin_apps.go`): `podman`, `ollama`, `sandbox`,
  `files`. `admincore/validate.go` `reservedNames` also refuses `api`, `static`,
  `healthz`, `app`, `mcp`, `_periscope`, `periscope`, `cloudbox`, `matrix`, `cloud`,
  `v1`, `health`, `version`, `metrics`, `config`, `overlay`, `u`, etc. Shell, SSH and
  desktop are built-in ROUTES (`/shell`, `/ssh`, desktop), not registry apps.

## Open points

1. **No guard flags yet.** `apps add` lacks `--expose-token-sha256` and
   `--expose-expires`. Proposal: land them (AppConfig fields, guard in the app proxy
   and in `serveTCPBridge`, header stripped) before the yoke expose story is wired;
   until then `dks expose` must fail loudly if `outpost apps add --help` lacks the flag.
2. **Egress `?dst=` unsupported.** `serveTCPBridge` ignores the query and dials a fixed
   address. Proposal: S487.3 adds an Outpost-side `egress` app kind that reads `dst`,
   checks it against an `--allow` list stored on the app, and dials; allowlist is
   enforced on the Outpost host, not only in the relay.
3. **Admin-grade set is not enumerable from Outpost.** `files`/`podman`/`ollama`/
   `sandbox` are constants, but shell/ssh/desktop/admin-UI port are routes. Proposal:
   yoke keeps its own deny list (those four app names, the reserved names above, ports
   of the admin UI and the shell/ssh routes) and refuses targets resolving to them,
   `--force` overriding.
4. **tcp apps ignore `Authorization`.** Fine for the contract (the header exists only
   for the edge), but the guard must read `X-Dhnt-Expose-Token` on the WebSocket
   upgrade request before `websocket.Accept`.
5. **`apps add` needs a running daemon with MCP bearer.** `dks expose` should surface
   the "outpost not reachable" error verbatim rather than falling back to `--offline`.
