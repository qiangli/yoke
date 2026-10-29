# S518.6j blockers

The Klatch v1.2.5 fleet pin and unit expectations are complete and the focused
fleet gate passes. The required live deployment is blocked by target access:

- the S518 ACCESS endpoint reaches a jump host that does not contain the
  `<sat>/s518-klatch` working directory;
- the registered target route requires a refreshed outpost elevation cookie;
- nested SSH through the jump cannot resolve the target name.

No remote state was changed. Refresh/grant the registered target's outpost
elevation or provide a working target alias, then complete the signed
Darwin ARM64 and web verification, swap both services while retaining their
data, restart them, and measure loopback/public status. Exact completed
evidence is in
[`docs/sprint-518-klatch-v1.2.5-deploy.md`](docs/sprint-518-klatch-v1.2.5-deploy.md).
