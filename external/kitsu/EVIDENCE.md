# Kitsu / Zou external API client — Sprint 382, Story 354

The client connects only to a caller-configured, user-managed Zou service. It does not install, start, download, embed, or manage a Kitsu/Zou server. Validation here uses loopback HTTP fixtures; real-account validation belongs to Sprint 465. The implementation uses only Go standard-library HTTP code and does not copy Zou source.

## Immutable upstream route evidence

Source revision: [`cgwire/zou@474d46b402b2d35f0738ddf863e7d0e2179870a4`](https://github.com/cgwire/zou/tree/474d46b402b2d35f0738ddf863e7d0e2179870a4). The `/api` deployment prefix and bearer token form are described in [CGWire's authentication guide](https://dev.kitsu.cloud/guides/authentication); the pinned source below supplies the route suffix and HTTP method. All listed reads are `GET` methods in the linked resource classes.

| Supported operation | Route below `/api` | Pinned route and handler evidence |
| --- | --- | --- |
| List projects, optional `name` | `/data/projects/all` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/projects/__init__.py#L50), [GET handler and `name` parameter](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/projects/resources.py#L75-L98) |
| List project episodes | `/data/projects/{project_id}/episodes` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/__init__.py#L86), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/resources.py#L543-L558) |
| List project sequences | `/data/projects/{project_id}/sequences` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/__init__.py#L85), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/resources.py#L506-L521) |
| List project shots | `/data/projects/{project_id}/shots` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/__init__.py#L83), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/resources.py#L467-L481) |
| List project tasks, optional `page`, `episode_id`, `task_type_id` | `/data/projects/{project_id}/tasks` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/__init__.py#L71), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/resources.py#L759-L790), [query parser](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/mixin.py#L111-L166) |
| List shot versions | `/data/shots/{shot_id}/versions` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/__init__.py#L58), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/resources.py#L910-L925) |
| List shot previews | `/data/shots/{shot_id}/preview-files` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/__init__.py#L57), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/shots/resources.py#L279) |
| List task previews | `/data/tasks/{task_id}/previews` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/__init__.py#L49), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/resources.py#L127-L138) |
| List task comments | `/data/tasks/{task_id}/comments` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/__init__.py#L47-L48), [GET handler](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/resources.py#L139-L155) |
| Delete one task comment | `/data/tasks/{task_id}/comments/{comment_id}` | [route](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/__init__.py#L47-L48), [DELETE handler returning 204](https://github.com/cgwire/zou/blob/474d46b402b2d35f0738ddf863e7d0e2179870a4/zou/app/blueprints/tasks/resources.py#L192-L218) |

## Contract and limits

A caller supplies a scoped bearer token at construction. The client keeps it only in memory, sends it in the Authorization header, and never puts it in effects or errors. Remote base URLs require HTTPS; plaintext HTTP is allowed only for loopback fixtures. Invalid UUID path arguments, unknown read operations, and unsupported review actions fail before any request. The copied HTTP client applies a same-origin redirect policy even when a client is injected. Every request has a maximum 15-second context deadline and responses are capped at 4 MiB.

The one write, `DeleteComment`, requires exactly one explicit `WithDryRun()` or `WithExecute()` option. Zero or conflicting options return `ErrExecutionNotExplicit`. Dry-run returns the DELETE effect without network access. An executed 204 sets `Executed` to true.

The source inspected did not justify generic `/data/versions` or `/data/reviews` routes, review mutations, comment creation bodies, or generic project/episode/sequence/shot/task mutation payloads. They are absent or return typed `ErrUnsupported`; no route is guessed. Reads return bounded JSON so callers can decode shapes according to their Zou deployment version. Project task pagination is server-defined; a caller can request a particular `page` but this client does not claim to retrieve all pages automatically.

## Verification

`go test ./external/kitsu` and `go vet ./external/kitsu` pass against in-process fixtures. Tests cover every allowlisted path, query encoding, token header, loopback/HTTPS validation, cross-origin redirects with an injected client, invalid operations and IDs, response bound, timeout, explicit write modes, dry-run zero calls, and executed 204.
