---
id: 2010e4b56b20
kind: task
title: Pin and verify signed Prima 0.1.1 release in binmgr
seq: 2017
status: done
priority: p2
created: 0001-01-01T00:00:00Z
weave: 234
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
sprint_title: 'Sphere regression recovery: p2p sharding + local inference back on, bashy-built engines'
closed: 2026-10-13T16:23:41.88403Z
closed_by: s436-codex-manager
---

Pin the final successful immutable Prima 0.1.1 release after independently
verifying its signed manifest and all four raw executable bundles. Preserve
binmgr's committed-pin, signed-manifest, per-asset signature, and offline-cache
refusals. Earlier `-1`, `-2`, and `-3` candidates are not release pins.

Verification on 2026-10-13:

- Hosted `qiangli/prima.cpp` `bashy-build` run 38427264223 completed successfully
  on push tag `bashy-bashy-prima-0.1.1-4`, head
  `81c8eed7dcc94ee89540fdfc457ea6d2750b452c`. The annotated release tag
  peeled to that same commit. The source tag `bashy-prima-0.1.1` peeled to
  `542d01932a88372a184037c5939048f355dcbd57`.
- Downloaded the hosted release's 68 assets independently. `SHA256SUMS` has
  33 entries; `shasum -a 256 -c SHA256SUMS` passed for every listed asset.
  Its own SHA-256 is
  `6b2161caa139b90878dbcb70d758d5377cccea6d72e0feb82de0fc7e0d6e6fb7`.
- Independently downloaded cosign v3.1.1 for darwin/arm64 and checked its
  SHA-256 against the existing committed cosign pin
  `94b42a9e697be95675f6160ab031a9a5f1ec1e646d6f648d7b2f5cd59ececbc5`.
  Offline `verify-blob` passed for `SHA256SUMS` and each of the four raw assets
  with their respective `.sigstore.json` bundle and this repository's pinned
  `pkg/binmgr/sigstore/trusted_root.json`. Every verification required identity
  `https://github.com/qiangli/prima.cpp/.github/workflows/bashy-build.yml@refs/tags/bashy-bashy-prima-0.1.1-4`
  and OIDC issuer `https://token.actions.githubusercontent.com`.
- The four independently hashed raw binaries agree with the signed manifest
  and GitHub's published asset digests:

  | Platform | SHA-256 |
  | --- | --- |
  | darwin/amd64 | `9203966e27293d958587e87e527b2f996d0207f30b56cccae0aeb9f362432772` |
  | darwin/arm64 | `8a9bffcd0ea3ca9f7edc44a0c318d6ecb3f496a76dd005833d7046321c4aee8e` |
  | linux/amd64 | `0a43bd0232adc517c97e2524ce05be1a3ea61a247a575591b9e7fd52f8203c17` |
  | linux/arm64 | `b347133007fa19a6195814be0250a9a8bb4d42e571b0c901c95c50cf77e148ad` |

The manager owns integration and the sprint-level digest evidence record.
