# Fleet local overrides

Tool, model, and agent files in the local ring may declare `overlay: true`. Their
present fields are merged onto the next lower winning file (embedded, shared,
or cloud). Nested mappings merge by key. A list is one field and replaces the
lower list when specified. In an agent file, each named agent in `agents:`
overlays its matching agent in the lower file; other agents remain intact.

An absent field inherits the lower value. A present `null`, `false`, `0`, empty
string, or empty list is an explicit value. `tool/model/agent set` and writes
through the fleet catalog create sparse overlays when a lower entry exists.
Custom local entries with no lower entry remain complete definitions. Existing
local YAML without `overlay: true` remains a complete replacement until
migrated, so an upgrade cannot silently reinterpret a user's old file.

## Migrate a full copy

Run `bashy tool migrate NAME` (or `model` / `agent`) to print a candidate.
Review it against the old file and the current lower entry. A diff cannot
identify a value deliberately pinned to the same value as the lower entry, or
an intentional clear omitted by old `omitempty` serialization. Add those to
the candidate explicitly. Save the reviewed YAML in a file and run
`bashy tool migrate NAME --reviewed FILE`. The command checks that the new
effective definition equals the old one, saves the original as
`NAME.yaml.bak` (refusing to overwrite an existing backup), and then writes
the overlay. Keep the backup until the next baseline update has been checked.

For a rehearsal, point `BASHY_FLEET_DIR` at a scratch directory and unset any
`BASHY_TOOLS_DIR`, `BASHY_MODELS_DIR`, or `BASHY_AGENTS_DIR` override, since
those per-noun variables take precedence. No migration runs automatically.
