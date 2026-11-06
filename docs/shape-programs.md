# Native shaping MVP

An app's trusted registry record can opt into native shaping through its nested
`shaping` value. Existing shaped apps default to `kind: web`.

```yaml
shaping:
  kind: cli
  build: [go, build, -o, '{output}', ./cmd/example]
  test: [go, test, -json, ./...]
```

The managed source must contain complete local Git history. The current local
clone transport cannot serve a shallow boundary consistently across candidate
creation, verification and activation. A shallow source is refused before a
candidate or tracking item is created. Shaping never fetches upstream history
implicitly. The operator may explicitly fetch full history from the source's
trusted upstream (`git fetch --unshallow`), preserving its selected commit/tree,
and retry. A refused attempt remains a failure; this does not substitute a
source snapshot or change an installed artifact's identity.

Use the existing app `--set` interface to update this registry policy. The
candidate cannot supply or replace it. Build and test are required argv arrays;
`{output}` is the binary destination (or a container build's image-ID receipt),
`{artifact}` is the resulting executable path or immutable image ID, and
`{revision}` is the exact candidate source revision. Commands have time, process,
memory and output bounds and run with private working/home directories and a
scrubbed environment. The default verifier shares one owned private home/temp
and Go cache root outside the source across baseline listing, build and tests,
and removes it after the run. Windows profile/app-data/temp paths use that same
owned runtime. A source-contained OS temp root is refused; source dependency
manifests are still checked without cache-directory exclusions. These are
ordinary process guards, not a filesystem or network sandbox.

CLI and service source history and activation use one `host` principal. Candidate
visibility, actor attribution and tracking remain tied to the requesting caller.
Remote host mutations require an operator session. A preview runs the verified
artifact without selecting it as the host version. CLI previews accept arguments
and default to `--help`; their captured stdout, stderr and exit status are bounded.
The existing operator-authenticated REST preview route accepts repeated literal
arguments, for example
`GET /shape/APP/preview/CANDIDATE/?arg=shape&arg=--help`.
Omitting `arg` keeps the `--help` default. Arguments are passed directly as argv,
without shell parsing; the route refuses more than 128 arguments, more than
32 KiB total argument bytes, or NUL bytes. Service previews use their side-port
URL and refuse CLI arguments.


Nested Go modules may use one relative global directory option, for example
`[go, -C, net/server, test, -json, ./...]` or the equivalent `bashy go` form.
The directory must remain inside both exact source clones, including after
symlink resolution; absolute paths, volumes and parent traversal are refused.
Baseline listing and candidate accounting use the same declared directory and
selection. Container build recipes/ignore files and all existing dependency
manifests are frozen alongside native tests and checker sources.

Both declared commands must succeed. Supported Go checks use direct
`go test -json` or `bashy go test -json`. The product wrapper
`scripts/go-product.sh` supports builds only and cannot serve as a test gate. Selected
checks are listed from the trusted base; every selected baseline check must
complete successfully. Missing, skipped and failed checks refuse verification.
Packages reported as `[no test files]` by that same baseline listing are recorded
separately. Only those exact package-level no-test results are accepted from the
candidate; a skipped named test or newly appearing skipped package still fails.
Existing tests, fixtures, build scripts, dependency manifests and trust adapters
are frozen; adding native checker/test files is also refused in this MVP. A fixed
external checker must resolve outside the candidate repository. Unsupported
mutable gate layouts fail explicitly. Accounting checks observed Go test events;
it does not prove arbitrary adversarial code executed a particular function.

The seal binds the exact source and base, host principal, verifier build,
dependencies, registry policy and final artifact digest. Activation rechecks these
bindings. The installed CLI release remains intact; selected versions live in
immutable side slots. A source/artifact transition journal recovers an interrupted
swap before later host mutations. Status reports an unresolved transition rather
than claiming a coherent active version. Normal undo/restore runs the required
checks again. Restoring `release` uses the exact retained installed bytes and
requires a proven source stamp. Unknown or mismatched installed-source identity
refuses normal activation/restore. The product's independent recovery entry can
still run the retained release without making a source-provenance claim.

For `kind: service`, also set `service: UNIT`. The unit must already be a disposable
container-backed process unit using direct `podman run` or `bashy oci run` argv.
An explicit `--name` must equal the unit name. Its settings provide an immutable
`image: sha256:...` and existing absolute
`data_dir`; the image carries `org.opencontainers.image.revision`. Native units
are not migrated. A supported run template is:

```yaml
exec: [podman, run, --rm, --name, example,
       -p, '127.0.0.1:{port}:8080',
       -v, '{settings.data_dir}:/data', '{settings.image}']
```

The narrow option grammar also permits `--init`, `--hostname`, `--publish` and
`--volume`; every publish uses a managed port and loopback or managed bind.
Extra mounts, compact option forms, network options, environment injection,
secrets, dependencies, privileged options and image command overrides are refused.
A declared HTTP or TCP health probe is required. Verification first checks a
unique temporary unit on a side port with a private copy of data; it does not
mutate the original unit. Failed cleanup preserves the unit/data for retry.
Runtime control first loads current declarations. Stop waits for the unit to
stop, then reconciles a remaining container through the same declared backend.
Cleanup requires its exact name, immutable image and sole declared data mount;
it removes only the inspected full container ID. A mismatch preserves the unit
and data. Image transitions complete stop before changing the declaration.
Approval changes only the original unit's image setting, restarts it and requires
health; failure restores the previous unit. The original data directory is kept.
