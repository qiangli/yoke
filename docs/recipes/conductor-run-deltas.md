# Conductor run supervision with lens

Sprint #446, Story #2134 (13778c0d7a83).

The embedding shell registers weave's existing resolver with
`weave.RegisterRefs(lens.AgentWorldRefs)`. That resolver also exposes persisted
workspace, base commit, launch time, and log handles through
`ref.RunSourceResolver`. No separate history store or process probe is used.

## Poll a run

```sh
bashy lens -t listing run:ID --since=10m > run.delta.jsonl
bashy jq -c 'select(.t == "cursor") | .cursor' run.delta.jsonl > run.cursor.json
bashy jq -c 'select(.t != "cursor")' run.delta.jsonl

bashy lens -t listing run:ID --since=@run.cursor.json > run.next.jsonl
# Only after lens and downstream processing succeed:
bashy jq -c 'select(.t == "cursor") | .cursor' run.next.jsonl > run.cursor.json
```

Listing emits JSONL `bashy-node-v1` rows with types `file`, `commit`, and
`log-line`, followed by a `t:cursor` record. Example attributes:

```json
{"change":"observed","size":81,"modified":"2026-10-18T20:00:00Z","evidence":"workspace-metadata"}
{"commit":"<full SHA>","message":"lens: add run history","time":"2026-10-18T20:00:00Z","evidence":"git-first-parent"}
{"offset":120,"line":"ok github.com/qiangli/yoke/cmds/lens","evidence":"persisted-log"}
```

The envelope binds provider, exact locator, and output format, expires after
24 hours, and holds the source handles, Git head, file metadata, log identity,
complete-line offset and a short preceding-byte anchor. Treat its token as
opaque. It can contain workspace paths; retain it with the run's evidence.
`--cursor-out=FILE` writes the envelope separately, atomically after stdout
succeeds. `jsonl` is also supported, but its cursor cannot resume `listing`.
An unchanged poll emits only the cursor. Run status or process liveness never
counts as file, commit, or log progress.

## Evidence and bounds

* Commits follow first-parent history from the persisted base (or previous
  head). Initial time queries filter by Git committer timestamp, checking every
  commit up to the base so out-of-order timestamps do not hide later matches.
  This is recorded Git time, not proof of when a process performed work.
* Initial file rows are current workspace observations whose mtimes fall in
  the interval. Resume compares path, size, mode, and mtime, including removals.
  They are not an audit of intermediate edits, same-metadata rewrites, or files
  already deleted before the first poll. Ignored/untracked files are included;
  `.git` is excluded and symlink targets are not followed.
* Logs include complete lines only. A run launched within the interval can
  supply raw lines. For an older run, changed logs require per-line RFC3339
  timestamps (JSON `timestamp`, `time`, or `ts`, or a leading timestamp).
  An entirely old log is skipped using its filesystem mtime. Untimestamped
  lines spanning the cutoff fail explicitly: their time cannot be recovered
  from liveness or current log mtime. Start once with `--since=start`, then
  resume by cursor to collect subsequent raw lines without guessing times.

Each poll is capped at 256 commits, 16,384 workspace entries, and 1 MiB of new
log data. An old log can be skipped with a bounded tail read. Resume seeks to
its saved offset; it does not reread the consumed log prefix or prior commits.
File inventory scans are capped every time. Limit errors fail the whole batch
without a new cursor; they do not imply a complete empty interval. Workspaces
and logs must remain stable during a poll; logs must be append-only.

Missing evidence, changed source handles, log rotation/truncation/anchor
mismatch, or a Git head that no longer descends from the checkpoint fail
explicitly. Preserve the failed checkpoint for diagnosis, then deliberately
restart with `--since=start` against the current retained sources. Restart
replays current evidence, cannot restore deleted history, and still obeys the
bounds. Missing workspace/base/start metadata cannot establish history.

Resolvers lacking the optional run-source interface retain the earlier entity
snapshot behavior for start/resume; they reject time selectors. Other entity
schemes likewise expose record changes, not historical transitions. Bus event
time/sequence semantics and regular-file byte cursors remain unchanged.

## Separate integration gap

The cross-repository conductor skill still needs this supervision recipe wired
into its workflow. Its source belongs to the embedding shell repository and is
outside this story slice; no conductor skill files were changed here.
