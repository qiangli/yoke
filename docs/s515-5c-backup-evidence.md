# S515.5c: never-delete backup acceptance

Story 02d36388ceba / #2934, sprint #515. Companion: umbrella 3151ba39261f.
Run: 2026-10-29. Product runbook and backend contract: [loom-backup.md](loom-backup.md).

## Hermetic gate (Elephant, Linux/amd64)

Source copied from this worker's HEAD plus its changed files into isolated
`/root/s515-backup/yoke`. Go 1.27.2; no heavy builds on Dragon. No new modules
or dependency replacements were needed.

- `go test ./external/loom -count=1`: PASS (8.242s on final source).
- `go vet ./external/loom`: PASS.
- `GOOS=windows GOARCH=amd64 go test -c ... ./external/loom`: PASS
  (compile gate, not a Windows runtime claim).
- `TestBackupLocalNeverDelete`: new branch/tag copied, fast-forward copied,
  source branch deletion retains target branch, divergent branch reports its
  name and leaves target refs unchanged, changed tag rejected.
- `TestBackupCommandsPersistAndRemoveOnlyConfig`: fresh command instances
  reload configuration and sync; removal keeps target Git objects intact.
- `TestBackupLocalLockedRefLeavesBatchUnchanged`: a competing Git ref lock
  rejects the complete batch without updating other refs or removing that lock.
- Migration tests reject destructive `mirror add` before contacting a forge;
  existing mirror inventory/removal tests pass.

Red/green mutation gate: temporarily bypassed the divergence rejection only
in the isolated Elephant copy. `TestBackupLocalNeverDelete` failed with
`expected loud divergence: <nil>`. Restored the implementation; the complete
loom package passed (8.263s). Final gate above also includes the later lock
regression and migration-help changes. Red/green logs remain in
`/root/s515-backup/{red,green,tests}.log` on Elephant.

## Live GitHub and restart gate

Venue: S515.6 peer `noviwin1`, WSL root, repository
`/tmp/s515-ci-loom/repositories/qiangli/cloudbox.git`. Windows SSH key access
worked; the named outpost alias required an uncached elevation. The old loom
state file named dead processes and healthz timed out, so loom was started
with the existing data before the proof.

Built a narrow executable on Elephant whose `main` invokes the production
`loom.NewLoomCmd()` and handles the daemon's `loom proxy` re-exec. This tests
the actual CLI, backup adapters and daemon lifecycle without building or
publishing a full Bashy release. Final proof executable SHA256:
`01429a448080a5a022876e23ee52cd468cae66fdf5681034410bf14aa0f23152`.
SFTP through the Windows filesystem preserved that digest on the peer;
streaming the binary over Elephant's SSH exec channel did not and was rejected.

Created **private** `qiangli/s515-backup-check-2934` using the authorized vault
credential `dragon-github`; disabled GitHub Actions on it before pushing.
The credential went directly into a mode-0600 peer file and never into a
command argument or a committed artifact. The production GitHub cloudbox
repository was never used as a target.

Configured once:

```sh
loom-proof loom backup add --data /tmp/s515-ci-loom \
  --name s515-proof --backend github \
  --source /tmp/s515-ci-loom/repositories/qiangli/cloudbox.git \
  --target https://github.com/qiangli/s515-backup-check-2934.git \
  --token-file /tmp/s515-backup/github.token
```

Initial seeding completed successfully (the source Git directory was 237 MB).
An early observation attempted while seeding was still underway received
GitHub's empty-repository HTTP 409; it is not counted as a passing proof.
After initial seeding, the following empty commits were pushed over HTTP to
**loom**, on `refs/heads/s515-backup-proof`. The peer script then only polled
GitHub's ref API. No `backup sync`, mirror API, hook script or manual GitHub
push occurred after either source push.

| Phase / push time UTC | Loom SHA | GitHub SHA | Push-to-observation time |
| --- | --- | --- | --- |
| Before restart / 20:50:31.700112 | `70fa48bc4a8e35bdd702f62b558db7c95864bdfb` | `70fa48bc4a8e35bdd702f62b558db7c95864bdfb` | 3.889 s |
| After restart / 20:51:25.100995 | `b3aee028371afbefbe34b583d0b65be4bd65cce0` | `b3aee028371afbefbe34b583d0b65be4bd65cce0` | 5.118 s |

Restart: `loom stop --data /tmp/s515-ci-loom`, then `loom start` with the same
work directory, address and ports. Forge PID changed 404415 → 404939; managed
proxy/backup worker PID changed 404416 → 404940. There was no re-add of the
backup configuration. The post-restart proof additionally checked loom's
branch API SHA against the pushed commit before polling GitHub.

Cleanup: removed the backup configuration, stopped the proof process and
removed the peer token file. **Repository deletion remains blocked:** GitHub
returned HTTP 403 to DELETE with both the authorized vault credential and the
existing local GitHub login. Archived `qiangli/s515-backup-check-2934` instead;
verified it remains private and archived, with Actions disabled. An operator
credential with repository deletion permission must delete this throwaway repo.
Restarted the venue with its pre-existing `/tmp/s515-ci-bin/bashy` executable.
The proof commits remain on the peer's separate test branch; no release or
production cutover was performed. Peer receipt/scripts remain under
`/tmp/s515-backup` (without the token).
