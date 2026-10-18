# adminclients evidence — story 66494446f180 (sprint 457, seq 2293)

Admission and verification record for the Microsoft 365 CLI (`m365`) and GAM
(`gam`). Source/doc inspection, fixture tests and live support are distinct:
a registered command or a passing fake is NOT live support. Every operation
mapping in `ops.go` comes from vendor documentation and was not executed
against a real tenant.

## Admission (reviewed 2026-10-18)

| | m365 | gam |
|---|---|---|
| Source | `pnp/cli-microsoft365` | `GAM-team/GAM` |
| Licence (client, GitHub SPDX) | MIT | Apache-2.0 |
| Release | v11.11.0 | v7.48.30 |
| Immutable commit | `628d601cdbe6c1d38ba86b36fc06520428a20fcb` | `e8a3bc8061b0c5633438121424b8994c3706543f` |
| Artifact | npm `@pnp/cli-microsoft365@11.11.0`, shasum `78b944f232cc66618e7db7cce73b785cd6b5bbcc`, integrity `sha512-GXdN3Cw4lOEFixVfI6YZlGM9OzAlMbjGItT8yMn8FznlG6+aWGNI6xjgNJRbigykeSE6YI3KRai+fijMt24DMg==` | GitHub release asset SHA-256 per platform in `clients.go` (e.g. `gam-7.48.30-windows-x86_64.zip` `78468701…adef6`; 8 assets recorded) |
| Runtime | Node (host-provided); not pinned here | self-contained PyInstaller-style archive |
| Mode | short-lived client (supply-chain Rule 2) | short-lived client (Rule 2) |
| Resolution | host-only (PATH); no download | host-only (PATH); `.tar.xz` is not extractable by binmgr |

Pinned dependencies/runtimes: only the release/commit/artifact above are
pinned. Locked transitive dependency licences, SBOM and vulnerability audit
of the shipped artifacts are NOT done (gap). go.mod/go.sum untouched.

## Overlap with gws (external/gws, merged)

gws admits Drive, Gmail, Calendar, Sheets, Docs, Chat, Slides, Tasks and
Contacts and explicitly leaves Admin Directory (`admin:users|groups|orgunits|
mobile|chromeos`) UNCLAIMED, handing it to this story (`gws.Deferred`).
GAM is therefore retained only for `admin:users`, `admin:groups`,
`admin:orgunits` (list/info/create/delete). Every GAM op row names the gws
deferral key it fills (`TestOverlapWithGwsAndReversibility` fails if a row
fills something gws does not defer, or duplicates a gws service). GAM
commands on gws-covered services are refused with "use gws"
(`ModeOverlap`, `TestModeAdmission`).

GAM reuses the gws credential selection: `GAM_DOMAIN` + `gws.ResolveCredentials()`
(`GWS_ACCOUNT` + exactly one gws credential file; ambiguity fails as in
gws). The gws service-account key path is handed to GAM through the child
environment (`OAUTHSERVICEFILE`, scratch `GAMCFGDIR`); no key is copied or
stored, no second store exists (`TestResolveScopeGAMReusesGwsCredentials`).

## Acceptance matrix

| Requirement | Evidence |
|---|---|
| Explicit tenant/domain | `ResolveScope`: `M365_TENANT` (GUID or DNS domain; `common`/`organizations` rejected), `GAM_DOMAIN`; no ambient fallback (`TestResolveScopeM365`, `…GAM…`) |
| Least-scope auth | each op declares one permission; `Request.Validate` requires it in the declared scopes and rejects a read that declares any write permission (`TestValidateLeastScopeAndSandbox`) |
| Structured results | `ParseRecords` (m365 JSON, gam CSV→JSON) keeps values verbatim (int64 precision, nulls); `gws.EmitPage` loom-v2 envelope |
| Pagination | `ListPage` over `gws.Paginate`/`ResumeOffset`; token bound to (target, op, query), cross-op/tampered tokens fail `cursor-mismatch`; total bounded by `gws.MaxTotal` (`TestParseAndPagination`) |
| Reversible test operations | every create has a delete `Reverse`; mutations need a declared test tenant/domain (`*_SANDBOX=1`); in-memory fake tenant create→replay-refused→reverse (`TestReversibleRoundTripWithLedger`) |
| Bounded output / typed errors / redaction | `Run`: stdout bounded while reading, 60s deadline, no retry; stderr redacted with canary secrets; typed codes from `gws` (`TestRunBoundsAndRedaction`, fake CLI is the test binary itself) |
| Denied modes | bulk (`csv/loop/batch`), raw `m365 request`/`cli`, interactive `login`/`oauth` refused unsupported exit 2 |
| Registry + atlas | `external/registry/adminclients.go` (+test), `pkg/atlas/platform.go` rows `m365`, `gam` |

## Atlas external-count delta

+2 externals (`m365`, `gam`): one registry entry and one `pkg/atlas/platform.go`
row each; no other new commands.

## Unsupported admin operations

`ops.go` `UnsupportedAdmin` is the machine-readable list:
- GAM mobile devices and ChromeOS devices (gws defers `admin:mobile|chromeos`; command mapping not reviewed) — unclaimed.
- GAM `csv/loop/batch/tbatch/multiprocess` bulk fan-out; OAuth user credentials (only service-account domain-wide delegation maps onto gws credentials).
- m365 raw `request`/`cli` access; license assignment (billing); privileged role assignment; SharePoint/Teams/Exchange admin; device-code login.

## Named unverified gaps (move to Sprint 465)

- Live read, reversible write with cleanup, and denied/expired-auth against a real Microsoft 365 test tenant and a real Google Workspace test domain; no tenant or credential exists here.
- Live confirmation of the `Argv` command mapping and the stderr→typed-code phrases against the pinned binaries; GAM env override names (`OAUTHSERVICEFILE`, `GAMCFGDIR`) are doc-derived.
- m365/GAM telemetry and auto-update: only gws's shared opt-outs apply; no vendor switch verified and no no-outbound-connection demonstration.
- m365 Graph permission names and GAM scope strings are doc-derived, not checked against a live consent screen.
- m365 `entra … list` pagination happens inside the CLI; provider `@odata.nextLink`/GAM page tokens are not exposed, so cursors are offset tokens over the bounded result (may duplicate/omit on a mutating directory).
- Locked transitive dependency licences, SBOM and audit; no sha256 pins in `pkg/binmgr/pins.go`; Node runtime licence/version for m365.
- Durable cross-process idempotency ledger (in-memory only); m365 `user-add` replay behaviour (UPN conflict) not observed live.
- Bashy-built pipeline migration for the clients (Rule 2 SHA-256-pinned upstream exception only).
