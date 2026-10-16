# Capability change policy

A *capability* is anything a host relies on: a fleet service unit and its
default, a route or app path, a downloaded asset (binary, image, model), or a
config key. Two rules govern changing one.

## 1. Replace before remove

A capability is never retired until its replacement is proven. In order:

1. **Ship the new route / asset / default** next to the old one.
2. **Prove it per OS** — Linux, macOS and Windows each run the owning check
   green against the new path (a skipped or untested OS is not proof).
3. **Verify the compat reader and migration** — an existing host upgrading
   in place keeps working, and its stored config is read correctly (rule 2).
4. **Then retire** the old one, in a separate change that names the proof
   from steps 2–3.

A change that removes and replaces in one step, or that removes first and
"fixes the replacement later", is refused at review.

## 2. Compat reader for retired config keys

When a config key is renamed, moved or retired, the loader keeps reading the
old key and maps it onto the new one for at least one release:

- **Never silently re-enable an explicit opt-out.** If the operator turned a
  capability off under the old key (or via a local overlay), it stays off
  after the upgrade, whatever the new default is. A default only applies
  where the operator expressed nothing.
- An explicit opt-in is preserved the same way.
- A key the reader can no longer map is reported loudly, never dropped.

The fleet defaults golden (`pkg/fleet/service_defaults_golden_test.go`) pins
every service unit's default `enabled` value and key settings, and proves an
explicit opt-out survives a fresh load for every default-on unit. Changing a
default means editing that table with a reason.

## Sprint close-out gate

A sprint card may list the capability IDs its work impacts:

```sh
bashy sprint capability add <sprint> <capability-id>
bashy sprint capability verdict <sprint> <capability-id> --sha <sha> --result PASS -m "<proof>"
bashy sprint capability waive <sprint> <capability-id> --reason "<why>" --for 24h
```

`bashy sprint end` then refuses until every listed ID has a PASS verdict
recorded for the final SHA (HEAD of the checkout `sprint end` runs in:
`--gate-dir`, else the current directory), or a recorded waiver that has not
expired. The latest verdict for a SHA wins, so a FAIL after a PASS revokes
it; a new commit needs a new verdict. Waivers always carry a reason, an
author and an expiry. Cards listing no IDs are unaffected.
