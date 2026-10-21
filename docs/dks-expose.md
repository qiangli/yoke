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
bashy dks unexpose <name>     # outpost apps rm + delete labelled cluster objects
bashy dks exposed             # NAME KIND TARGET EXPIRES STATE, from labels
```

- `--ttl` defaults to 1h, max 24h.
- `proxy:egress` and `proxy:git` REQUIRE `--allow`. Egress: `host` or `*.domain[:port]`.
  Git: `owner/repo` or `owner/*`. A bastion is a `tcp://` expose with a fixed target.
- Refused without `--force`: targets that are Outpost built-in/admin-grade apps
  (files, shell/terminal, desktop, ssh, periscope, the admin UI port) and app names
  that collide with built-ins.
- Audit: one line per expose, unexpose and refusal in the dks run journal. Tokens
  never appear in argv, logs, errors or the journal.

## In-cluster relay

`bashy dks relay` is a hidden subcommand that runs in the normal dks run image.
Token comes from env `EXPOSE_TOKEN` (Secret `secretKeyRef`), the upstream app-route
URL from env or flag. Neither is ever in argv or logs.

| Mode | Listens | Behaviour |
|------|---------|-----------|
| `--mode http` | `:8080` | reverse-proxy to the app route URL, adds both headers, optional `--path-prefix` (e.g. `/k/<door-peer>/`) |
| `--mode tcp` | `:<port>` | per connection, WebSocket-dial the app route with both headers, byte-bridge |
| `--mode egress` | `:3128` | HTTP CONNECT proxy; per CONNECT, WebSocket-dial `/app/<app>/?dst=host:port` (S487.3) |

## Cluster objects

Per expose `<name>`, in the workspace namespace: a Secret, a Deployment (1 replica
relay) and a Service `<name>`, all labelled `dks.dhnt.io/expose=<name>` with annotation
`dks.dhnt.io/expires=<RFC3339>`. Pods reach `http://<name>:8080` or `<name>:<port>`.

`bashy dks run` / `--session` injects env for every live expose in the workspace:

- `DKS_<NAME>_URL` (http) or `DKS_<NAME>_ADDR` (tcp)
- egress also: `HTTPS_PROXY`/`HTTP_PROXY=http://<name>:3128`,
  `NO_PROXY=.svc,.cluster.local,localhost`
- plus any `--env K=V` recorded on the expose (stored on the Secret/annotation,
  `{url}` substituted)

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
