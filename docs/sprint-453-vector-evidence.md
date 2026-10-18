# Sprint 453 vector evidence — story 7e9e0ba8dc34 / #2265

Worker: `claude-opus5.5-w450` (registered holder; author and committer), weave
yoke#450, branch `agent/weave-issue-450` on yoke main `d9714fd5` (fetched from
`/tmp/s453-memory-publish` `refs/remotes/origin/main`, fast-forward from
`cd966ae0`). This is the package (core) phase. The manager's whole-package,
crossvet, independent-process and remote CLI/MCP gates are outstanding, and so
is the constructor/wiring phase (`cmds/db`, atlas rows). This record does not
claim story acceptance.

## What was added

- `pkg/dbvec`: the vector category over `pkg/dbstore/sqlitedb` and
  sqlite-vec `vec0` (`pkg/dbstore/vecdb`, CGO-free through
  `modernc.org/sqlite/vec`). Every operation registers with
  `dblifecycle.RegisterCategoryOp`. There is no second registry, lock
  manager, claim or owner path. Operations:
  - `vec.collection.add|rm|list`
  - `vec.put`, `vec.get`, `vec.rm`, `vec.batch`, `vec.search`
  - `vec.export`, `vec.import`
- Collections are explicit. `collection add --collection N --dimension D
  --metric cosine|l2|l1 --model ID` is exclusive and needs `--yes`. It records
  dimension, metric, model identity and dtype `f32` in `bashy_vec_collections`
  (created by the first add, never earlier). It then creates a STRICT item
  table `bashy_vec_m_<name>` (`id`, `revision > 0`, metadata `doc` with a JSON
  object CHECK) and a vec0 table `bashy_vec_v_<name>__e`
  (`id text primary key, emb float[D] distance_metric=M`). Names are
  `[a-z][a-z0-9_]{0,62}` with no `__`, so a collection name can never collide
  with another collection's vec0 shadow tables (`…__e_info`, `…__e_chunks`,
  and so on). The dimension limit is vec0's 8192. The metrics are the ones the
  probe below showed vec0 accepts (hamming is refused).
- Vectors are always supplied by the caller. A vector is either the tagged
  lossless form from contract §8.3, `{"t":"vec","dtype":"f32","b64":…}`
  (little-endian, bit-exact), or a JSON number array where each number is
  rounded to the nearest float32, the collection's declared dtype. The package
  refuses all of these with `invalid_input`:
  - a wrong dimension
  - a value outside float32
  - NaN or ±Inf
  - a non-number element (including a numeric string)
  - an f64 tag
  - bad base64, or a blob length that is not a multiple of 4
  - an all-zero vector in a cosine collection
  - an explicit `--model` that differs from the collection's model

  vec0 itself silently stores NaN and zero vectors and then ranks them with a
  NULL distance, so these checks are in dbvec and not left to the engine.
- Items: each item has an ID (generated as 32 hex characters inside `Run`
  when omitted), a per-item revision that every write increments, the
  embedding, and metadata (a JSON object canonicalized by the db doc rules:
  numbers verbatim, duplicate keys refused, `{}` when absent, at most 64 KiB).
  `--if-revision` and `--if-absent` are preconditions (`conflict` and `exists`).
  `batch` takes up to 1000 put/rm ops in one IMMEDIATE transaction, so either
  all of them apply or none do. `rm` deletes the vec0 row and the item row in
  one transaction.
- Search ranks exactly. There is no ANN index. Search uses sqlite-vec's
  `vec_distance_<metric>` over the vec0 embeddings, joined to the items that
  the metadata filter admits. Results are ordered by `(distance, id)`, so
  equal distances order deterministically. A `bashy-cursor-v1` cursor is
  bound to the target and the query (collection, query vector bits, filter,
  params, include_vectors). It resumes exactly after the last hit, using that
  hit's float64 distance bits and id. Defaults: `--limit` 10 (maximum 1000),
  `--max-bytes` 256 KiB (maximum 8 MiB). A hit that alone exceeds
  `--max-bytes` is `limit_exceeded`. Vectors are returned only with
  `--include-vectors`; `get` always returns the item's vector.
- Metadata filters use the db doc grammar itself, not a copy. The manager
  approved Q1: `pkg/dbdoc/export.go` adds `dbdoc.CompileFilter` and
  `dbdoc.CanonicalObject` as wrappers that do not change behavior, pinned by
  `TestExportedFilterAndObjectMatchDocGrammar`. Values and `--params` are
  always bound. A malformed filter is refused at admission (`Access`), before
  the target is touched.
- Interchange (`bashy-db-export-v1`, format `1.0`, category `vectors`,
  engine `sqlite`):
  - `manifest.json` carries `schema_def.vec_format` `1`, the linked
    `sqlite_vec_version` and each collection's name, dimension, metric, model
    and dtype. `snapshot` is `{"consistent":true,"method":"read-txn"}`.
  - There is one `records-NNNNNN.jsonl` per collection. Each line is
    `{"id","revision","vector":<tagged f32>,"metadata":{…}}`, in ascending id
    order. `blobs` and `derived` are empty, because vec0 holds the source
    embeddings and no derived index.
  - Import validates first: the manifest, the version, every record (tagged
    vector required, collection dimension, finite values, metadata object,
    strictly ascending ids, so duplicates are refused with memory bounded by
    one record), and each file's count, bytes and sha256. All of this happens
    before anything changes. Then it loads in one transaction and verifies the
    checksum again inside it.
  - Merge needs each exported collection to exist with the same
    dimension, metric and model. Otherwise it fails with `conflict`.
  - `--on-conflict error|skip|replace`: a replaced item's revision moves past
    both revisions.
  - `--replace` is exclusive. It removes every vector collection of the
    target and recreates the exported ones. Other tables are never touched.
- Effects, from `Spec`, the same declaration used by CLI, `--explain`, MCP
  and owner IPC:
  - reads: `read`, hold `none`
  - put/rm/batch and merge import: `write`, `shared-writer`
  - collection add/rm and `import --replace`: `destructive`, `exclusive`
  - memory-mode specs drop `fs.*` tokens, except where export or import reads
    or writes a directory.

## Embedding provider (Q2, approved and implemented)

The manager approved the Q2 proposal in mb 6969, so it is implemented in
`pkg/dbvec/ops.go` (`embedInput`, `embedURL`, `embed`). It reuses no provider
registry and adds none. `put` and `search` accept `text` only together with
an explicit `endpoint`: the OpenAI-compatible base URL, for example
`http://127.0.0.1:11434/v1`. The client POSTs `{model, input,
encoding_format:"float"}` to `<endpoint>/embeddings`.
- There is no default URL and no fallback. Text without an endpoint is
  `invalid_input`. A supplied vector given with `endpoint` or `api_key_env`
  is refused, so a supplied vector never reaches the network.
- The model is always the collection's persisted model. A different
  `model` is refused before the call.
- The credential follows the existing env-var convention, as
  `OPENAI_API_KEY` does in `pkg/broker`. `api_key_env` names a variable in
  the process running the operation (the owner, for owner sessions). Its
  value is sent only as `Authorization: Bearer` and never appears in an
  input, an error or the request hash. An unset or empty variable is
  `invalid_input`, never an anonymous retry.
- The endpoint is refused when it has userinfo, a query or a fragment, so a
  credential cannot ride in the URL. Redirects are never followed
  (`http.ErrUseLastResponse`). A 3xx answer is `backend_error`, so a bearer
  key cannot leave the named host.
- The text is limited to 64 KiB and checked before any HTTP. The response is
  read through a 4 MiB `LimitReader` (`limit_exceeded` above that). The call
  uses the caller's context under a 60 s cap, so cancellation is `cancelled`
  and the deadline is `timeout`.
- The reply must contain exactly one `data[].embedding` float array. It goes
  through the same `parseVector` as a supplied vector: the dimension, finite
  float32 values and the cosine zero vector are checked.
- Inside the owner's `Run`, the collection is read in a short read
  transaction that is closed before the HTTP call. No database lock is held
  across the network, and the write path checks the model again. The
  logical request (text, endpoint, env name) is what dblifecycle hashes for
  request-id replay; nondeterminism stays inside `Run`.

## sqlite-vec probe (vec0 v0.1.9 through modernc, Dragon)

| Probe | Result |
|---|---|
| `distance_metric=l2 / cosine / l1` | accepted |
| `distance_metric=hamming` (float column) | refused by the vec0 constructor |
| `float[9000]` | refused: "maximum 8192" |
| NaN or zero vector insert | accepted silently; KNN ranks them with a NULL distance (so dbvec refuses them) |
| `vec_distance_l1` versus vec0 KNN distance | equal (4 and 4); search uses the scalar functions only, and their expected values per metric are pinned in `TestSearchMetricsFiltersAndPages` |
| vec0 insert inside `begin … rollback` | rolled back |

## Tests

The command is exactly the focused local gate, run on Dragon with short
in-process tests only and its exit status taken directly:

```
CGO_ENABLED=0 GOMAXPROCS=2 GOFLAGS=-p=2 go test -short -count=1 ./pkg/dbvec   # exit 0, 45.5s (with Q2 tests)
CGO_ENABLED=0 GOMAXPROCS=2 GOFLAGS=-p=2 go test -short -count=1 ./pkg/dbdoc    # exit 0, 37.2s (whole package)
CGO_ENABLED=0 go vet ./pkg/dbvec ./pkg/dbdoc   # exit 0
```

Each persistent subtest is slow on this host (seconds per subtest). The
sibling `pkg/dbdoc` persistent subtests are equally slow on the same host, so
the cause is the environment and not dbvec.

| Test | Covers |
|---|---|
| `TestVecCollectionsAndItemsBothModes` | both modes; nothing pre-created; `--yes` for add/rm; name, dimension, metric and model validation; tagged and array vectors; bit-exact f32 (subnormal, 24-bit mantissa, −0); every incompatible-vector refusal; model mismatch; metadata strictness; if-absent and if-revision conflicts; update bumps the revision; deletion removes the item from get and search; an unknown `provider` field refused; list reports dimension, metric, model and count; collection rm, then re-add with a new definition |
| `TestSearchMetricsFiltersAndPages` | both modes; l2, l1 and cosine distances and order; metadata filter; bound `in` param with an SQL-looking value; bad field refused; wrong query dimension, zero cosine query and model mismatch refused; cursor pages through equal distances; cursor bound to its query; include_vectors; max_bytes truncation and an oversized hit; limit maximum |
| `TestBatchIsAtomic` | both modes; a failing batch applies nothing; results per op; bad op; batch size maximum |
| `TestPersistentSurvivesRestartMemoryEndsWithOwner` | fresh service and handle on the same file: vector bits, metadata, deletion and the persisted dimension, metric and model survive; memory ends with its MemSpace |
| `TestConcurrentRevisionUpdatesLoseNothing` | both modes; 4 writers × 8 revision-guarded increments, so exactly 32 applied and revision 33 |
| `TestExclusiveOpsRespectNativeLock` | collection rm is `busy` under another native exclusive lock; the holder's handle still writes |
| `TestReadHandleRefusesWrites` | a write on a read handle is `read_only` or `busy` and nothing is written |
| `TestSpecDeclaresEffects` | mutation class and hold per op; import access depends on `--replace`; no `fs.*` effect in memory mode; malformed filter refused at admission |
| `TestExportImportRoundTripsAcrossModes` | memory→persistent, persistent→memory, persistent→persistent, memory→memory: collections, IDs, revisions, metadata (big and decimal numbers, nested) and vector bits identical; search and dimension enforcement work after import; `--replace` needs `--yes` |
| `TestImportMergeConflictPolicies` | both modes; error (nothing applied), skip, replace (revision past both), merge keeps items absent from the export, `--replace` empties first, replace plus on-conflict refused, merge into a different model or a missing collection is `conflict` |
| `TestImportValidatesBeforeChanging` | schema v2, format 2.0, foreign schema, documents category, vec_format, bad metric, escaping path, no manifest, tampered checksum, untagged vector, wrong dimension and missing final newline are each refused with its code, and the target is unchanged; export over an existing or relative directory refused; cancelled import and export are `cancelled` |
| `TestEmbedExplicitEndpoint` | both modes; text put and text search against a fake OpenAI-compatible endpoint; the request carries the collection's persisted model and the text; the bearer key comes from `api_key_env` and is sent only when named; the stored vector is bit-exact from the reply |
| `TestEmbedRefusals` | no endpoint, text plus vector, endpoint with a supplied vector, userinfo, query, non-http scheme, empty or invalid key env, another model, oversize text and a missing collection are each refused, with zero requests reaching the endpoint |
| `TestEmbedBadReplies` | wrong dimension and non-finite values are `invalid_input`; malformed JSON, two items, a base64 embedding and a non-200 status are `backend_error`; an oversize response is `limit_exceeded`; nothing is written |
| `TestEmbedRefusesRedirect` | a 307 to another host is not followed (that host gets zero calls); `backend_error`, and the key is not in the error |
| `TestEmbedCancel` | a hung endpoint plus caller cancellation gives `cancelled` promptly |
| `TestSuppliedVectorZeroNetwork` | supplied-vector put and search make zero calls; with the HTTP client replaced by a failing transport, a put still succeeds |
| `TestOwnerSessionSharesMemoryCollection` | in-process `dblifecycle.Serve` plus `Call` (the owner IPC path): two agents share one memory collection; contested revision is `conflict`; request-id replay applies once and reuse with other input is `conflict`; hold makes the other agent `claim_held` for search and put, a wrong epoch is `stale_epoch`, and the holder runs an exclusive add with its epoch, then releases; owner export, then a persistent import is bit-exact with revisions preserved; stop gives `lifetime_ended` |

These independent-process tests are written. They skip under `-short` and
were not run on Dragon. They are for the manager's gate:

- `TestIndependentProcessesShareOwnerMemoryCollection`: a re-exec'd owner
  process and three concurrent client processes (2 writers × 15 puts plus a
  searching reader). Same-id `if_absent` contention between two processes
  lets exactly one win. After the owner process is killed (crash), a client
  gets `lifetime_ended`.
- `TestIndependentProcessesSharePersistentCollection`: three direct
  processes on one WAL file, with 40 puts and no lost writes. A deletion in
  one process is seen by the next. An exclusive collection rm from another
  process is `busy` while the native exclusive lock is held, and the
  collection is unchanged.

## Not done in this phase

- `cmds/db` dispatch, flags, MCP schema and atlas rows. These are shared
  files left for the manager-coordinated phase 2 mount.
- Independent-process, whole-package, crossvet and remote gates (manager).
