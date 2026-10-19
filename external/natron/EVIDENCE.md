# Natron admission evidence — Sprint 382 story c47a9f514504 (Story #352)

Decision: **UNSUPPORTED (evidence-only)**. One admission prerequisite is
absent — no publisher-authenticated digest for any official Natron release
artifact — so per the story contract implementation stops here: no wrapper
(`natron.go`), no fixture tests (`natron_test.go`), no `pkg/binmgr/pins.go`
entry, no download, no execution. Nothing is invented.

## Prerequisite checks (each verified 2026-10-18 from dev-host, metadata only)

1. **Official runnable open-source artifact for one supported platform:
   PRESENT (but unusable without a digest).**
   - Upstream: `NatronGitHub/Natron`, stable release **v2.5.0**
     (`Natron 2.5.0`, published 2022-11-26):
     `https://github.com/NatronGitHub/Natron/releases/tag/v2.5.0`
   - License: **GPL-2.0** (`LICENSE.txt` on branch `RB-2.5` opens with
     "GNU GENERAL PUBLIC LICENSE / Version 2, June 1991";
     repo license metadata `gpl-2.0`).
     A GPL external binary follows the admitted Blender precedent
     (download + sha256-verify + exec only, never linked); license alone
     does not block.
   - binmgr-consumable archives exist for two platforms:
     `Natron-2.5.0-Linux-x86_64-no-installer.tar.xz` (147,079,928 bytes,
     `linux/amd64`) and `Natron-2.5.0-Windows-x86_64.zip` (196,352,227
     bytes, `windows/amd64`).
   - Fail-closed platforms: every macOS asset (1015/12/109/106) is
     **`.dmg` only** (same `ErrNoArtifact` class as Blender darwin);
     no `linux/arm64` or `windows/arm64` build is published.

2. **Publisher-authenticated digest: ABSENT — the blocking gap.**
   - The v2.5.0 release carries **19 assets, zero checksum sidecars**:
     no `.sha256`, `.md5`, `SHA256SUMS`, `CHECKSUMS`, `.sig`, or `.asc`
     file (enumerated via
     `https://api.github.com/repos/NatronGitHub/Natron/releases/84222308/assets?per_page=30`).
   - GitHub's published asset `digest` field is **null on all 19 assets**,
     and also null on every asset of the newer prereleases
     (`v2.6.0-alpha1`, `v2.5.1-pre1`, `v2.5.1-pre2`), which are
     unstable nightly-style builds and not pin candidates anyway.
   - The former download site `https://natron.fr/download/` returns
     **404 ("Page non trouvée")**; no hash is published there either.
   - A locally computed hash is not a substitute: it would require a
     147+ MB download on dev-host (prohibited: no large artifact downloads
     on dev-host) and would still not be publisher-authenticated — the
     exact failure mode the Ardour story (0556ec75) was rejected for.
     Never invent a digest.

3. **Documented bounded NatronRenderer / headless seam: PRESENT.**
   - Official docs: `https://natron.readthedocs.io/en/rb-2.5/devel/natronexecution.html`
     ("Natron in command-line", source
     `Documentation/source/devel/natronexecution.rst` on branch `RB-2.5`).
   - Documents the bounded background seam: `NatronRenderer` (implicit
     `-b/--background`, no GUI), project execution (`.ntp`), `-w/--writer
     <Writer> [output] [firstFrame-lastFrame]`, `-i/--reader <Reader>
     <absolute-path>`, `-l/--onload` script, `-s/--render-stats`,
     `-t` interpreter mode. Absolute-path-only rule for `-i` matches a
     typed-wrapper design, but no wrapper is written until a digest exists.

## Scope compliance

- New path `external/natron/EVIDENCE.md` only (this file). No `natron.go`,
  no `natron_test.go`, no `pins.go` change — mirrors the Story Architect
  evidence-only precedent (Yoke `32a0f587`).
- No Natron process was started and no artifact was downloaded on dev-host.
  Live execution stays gated on separately claimed macos-host/windows-host
  with manager coordination.

## Reversal condition

Admit Natron when the publisher ships an authenticated digest for a stable
release archive on `linux/amd64` or `windows/amd64` (checksum sidecar in
the GitHub release, non-null published asset digest, or signed
SHA256SUMS): then add the `external/natron` wrapper + pins + fixture
tests following the `external/blender` pattern.

## Gate evidence

- `GOWORK=off go test -count=1 ./pkg/binmgr` →
  `ok github.com/qiangli/yoke/pkg/binmgr 1.931s` (untouched package;
  `./external/natron` holds no Go files so the story's test path is N/A
  for it by design).
- `GOWORK=off go vet ./pkg/binmgr` → exit 0, no findings.
