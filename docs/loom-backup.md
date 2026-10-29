# Loom backups

`bashy loom backup` stores branches and tags from a local loom repository in
one selected backend. This is a one-way backup: new refs and fast-forwards
only. Source branch/tag deletion does not delete the backup ref. A divergent
branch or changed tag fails with the ref name and both SHAs before updating
any refs. Existing tags are immutable. Target-only refs are retained.

Gitea push mirrors force-sync and prune. `loom mirror add` is disabled;
`loom mirror list|rm` remains available to inventory and retire existing
mirrors. **Remove existing push mirrors before configuring backups.** The
upgrade does not silently remove existing configuration. `loom replica` is
still a destructive replica mechanism and must not be used as a backup.

## Configuration

Run on the host running loom, using its actual repository path (see the
`[repository] ROOT` setting in its `app.ini`). No GitHub operation is needed
for the local backend:

```sh
bashy loom backup add --data /srv/loom --name disk --backend local \
  --source /srv/loom/repositories/acme/app.git \
  --target /mnt/network-drive/acme-app.git
bashy loom backup list --data /srv/loom
bashy loom backup sync --data /srv/loom disk
bashy loom backup rm --data /srv/loom disk
```

The local target must initially be absent. Bashy creates and marks a managed
bare repository. Any writable filesystem path is supported, including a
network mount or p2p folder. The filesystem must support exclusive file
creation and atomic rename. Syncing software must not concurrently rewrite
the repository; use a single writer per target. A stale `.backup-lock`
directory or ref `.lock` file after a crash causes a loud refusal: verify no
writer is active before removing that lock. Ordinary `rm` above removes only
the backup configuration; it never removes the bare repo or any target refs.

For GitHub, first create an **empty private throwaway** repository for testing.
The real production repository requires a separate operator-approved cutover.
Provide a persistent mode-0600 token file readable by loom's managed process;
its credential needs repository contents write permission (and workflow write
permission if the source contains workflows). The token is read from that
file on each sync, never stored in the backup JSON, printed, or passed in argv.

```sh
bashy loom backup add --data /srv/loom --name offsite --backend github \
  --source /srv/loom/repositories/acme/app.git \
  --target https://github.com/acme/private-backup.git \
  --token-file /srv/loom-secrets/github.token
```

`add` performs the initial sync before recording the configuration under
`DATA/backups/NAME.json`. It refuses a duplicate name. Configuration changes
are picked up automatically. `loom serve` and the managed proxy launched by
`loom start` poll every five seconds, with a two-minute timeout per configured
backup. Multiple backups run sequentially; a large initial sync or a slow
backend can delay subsequent ones. Failures are reported as `loom backup NAME
FAILED` in loom's stderr/log; manual `backup sync [NAME]` also returns nonzero.
After a loom restart the persisted targets resume automatically. Keep both
the data directory and token file on persistent storage.

Only `refs/heads/*` and `refs/tags/*` are backed up. Remote tracking, pull
request, notes, reflogs, LFS objects, issues, releases, and other forge state
are outside this Git-object backup. No target-only branch is removed, even
when removed on loom. Treat loom as the authoritative writing location and
restrict direct GitHub writes. If someone commits on the backup, sync stops:
preserve and reconcile that work on loom before retrying. There is no force,
prune, reset, or automatic overwrite escape hatch. Backup `rm` cannot revoke
an already in-flight sync, which still obeys the same never-delete contract.

## Backend interface

`external/loom/backup.go` defines `BackupBackend.Sync(ctx, source, target)` and
registers implementations by `BackupTarget.Backend`. Callers load persisted
configuration and invoke the selected implementation through this interface.
Only `local` and `github` are implemented; S3, other Git servers and cloud
storage are future adapters, not accepted aliases for current targets.

Every future adapter must:

- Use the shared branch/tag snapshot and whole-batch divergence preflight.
- Reject overwrites and deletions, including tag changes and source deletions.
- Protect writes against changes after preflight and report failures loudly.
- Leave target data intact when configuration is removed.
- Pass the same hermetic retention, divergence, and concurrent-writer tests.

The local adapter runs entirely in Go, copies Git objects, and holds Git-style
exclusive ref locks before publishing refs. A divergence or lock conflict
leaves all refs unchanged; an I/O failure during publication may leave some
safe fast-forwards applied, and a retry completes the remaining work. GitHub
uses Go's Git transport with a single advertised-ref snapshot: each update
carries its expected old SHA, including zero for a new ref, and requires
atomic receive-pack plus report-status. It sends only objects reachable from
the changed source refs that the target does not already have. It never uses
Gitea's push-mirror endpoint or a host Git subprocess.
