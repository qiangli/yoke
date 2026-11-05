# Native shaping MVP

An app's trusted registry record can opt into native shaping through its nested
`shaping` value. Existing shaped apps default to `kind: web`.

```yaml
shaping:
  kind: cli
  build: [go, build, -o, '{output}', ./cmd/example]
  test: [go, test, -json, ./...]
```

Use the existing app `--set` interface to update this registry policy. The
candidate cannot supply or replace it. Build and test are required argv arrays;
`{output}` is the binary destination (or a container build's image-ID receipt),
`{artifact}` is the resulting executable path or immutable image ID, and
`{revision}` is the exact candidate source revision. Commands have time, process,
memory and output bounds and run with private working/home directories and a
scrubbed environment. These are ordinary process guards, not a filesystem or
network sandbox.

CLI and service source history and activation use one `host` principal. Candidate
visibility, actor attribution and tracking remain tied to the requesting caller.
Remote host mutations require an operator session. A preview runs the verified
artifact without selecting it as the host version. CLI previews accept arguments
and default to `--help`; their captured stdout, stderr and exit status are bounded.

Both declared commands must succeed. Supported Go checks use direct
`go test -json` or `bashy go test -json`. The product wrapper
`scripts/go-product.sh` supports builds only and cannot serve as a test gate. Selected
checks are listed from the trusted base; every selected baseline check must
complete successfully. Missing, skipped and failed checks refuse verification.
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
Its settings provide an immutable `image: sha256:...` and existing absolute
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
Approval changes only the original unit's image setting, restarts it and requires
health; failure restores the previous unit. The original data directory is kept.
