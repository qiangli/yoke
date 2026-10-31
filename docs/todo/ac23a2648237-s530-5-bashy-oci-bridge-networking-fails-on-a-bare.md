---
id: ac23a2648237
kind: bug
title: 'S530.5 bashy oci bridge networking fails on a bare Linux host: netavark needs nft; supply bashy''s pure-Go nft (default build, JSON input) via helper_binaries_dir'
seq: 2997
status: done
priority: p1
created: 2026-10-31T02:11:22.132737Z
weave: 752
assignee: codex-gpt6-sol
sprint: 530
sprint_id: 1d6b9478-c054-5b09-961e-c649a3bc1705
sprint_title: 'Master-provisioned AWS satellites: cloudbox provisions Lightsail satellites from owner-held AWS credentials (S522.5)'
closed: 2026-10-31T03:58:30.800547Z
closed_by: s530-manager
---

Operator 2026-10-30 (Sprint 530): a real bashy bug, filed in-sprint ("bashy is all you need" must hold on a bare Linux host). Does not gate S530.4 (v0.18.50 uses --network host as a recorded known gap). If it cannot land before the sprint cutoff, the conductor reports it at close.

## Failure (exact, elephant WSL2 Ubuntu, no nft/iptables installed)

`bashy podman run --rm golang:1.27.2-alpine ...` (default bridge network) and `bashy podman network create X` + `run --network X`:

    Error: netavark: nftables error: unable to execute "nft": No such file or directory (os error 2)

`bashy podman info`: NetworkBackend netavark, rootless false. Embedded podman v6.1.2 under ~/.cache/bashy/bin/podman/v6.1.2/podman-linux-amd64. `--network host` works.

## Facts found

- yoke `cmds/nft` (pure-Go nft over pkg/nft, netlink) implements the knftables subset: text `-f FILE|-`, operands, and `--json` only for `list`. It is compiled in only with `-tags bashy_dks_runtime` (cmds/all/dks_runtime.go), deliberately, so default bashy keeps the host's own nft.
- netavark's nftables firewall driver execs `nft` and feeds it JSON (`nft -j -f -` style transactions) for its tables/chains/rules (NAT masquerade, forward, port maps).
- yoke `external/podman/engine/cli.go` already writes a helper override (`helper_binaries_dir = [...]`, helperConfContent) for the embedded podman.

## Scope (root fix)

1. Make bashy's nft available to the embedded podman/netavark in the DEFAULT build, only as netavark's helper (helper_binaries_dir / a helper-dir `nft` entry that runs bashy's nft), without shadowing a host nft for the user's shell. Keep the dks_runtime gating semantics for the general command namespace.
2. Extend yoke nft to accept netavark's JSON-input transactions (the subset netavark actually emits: add table/chain/rule/set/map elements for bridge NAT + forward + port forwarding, delete on teardown). Fail loudly on anything outside it.
3. Red test FIRST: a test that feeds the recorded netavark JSON transaction(s) (capture them from netavark source or a run) to `nft -j -f -` and asserts the resulting ruleset (netlink, privileged test skips cleanly when not root/Linux), plus a unit test that the engine's helper config points netavark at bashy's nft in the default build. Then green.
4. Live proof (conductor-arranged, elephant WSL, no nft installed): `bashy podman run --rm alpine wget -qO- https://proxy.golang.org >/dev/null` on the default bridge network succeeds; `network create` + named-network run succeeds.

Not in scope: iptables driver, rootless pasta. Repos: yoke (nft, engine helper config, cmds gating) and bashy only if its build tags change; keep pkg/provider untouched.
