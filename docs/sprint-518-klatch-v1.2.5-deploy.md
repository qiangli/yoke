# S518.6j Klatch v1.2.5 deployment transcript

Target: `<host>` via the S518 claim ACCESS line; intended working directory
`<sat>/s518-klatch`. Services: `klatch` on loopback 7100 and `klatch2` on
loopback 7101.

1. Downloaded all four server archives and the web archive from release
   `bashy-v1.2.5-1`. Measured SHA-256:

   | Asset | SHA-256 |
   | --- | --- |
   | `klatch-v1.2.5-linux-amd64.tar.gz` | `c0ef0d9dc7596aa256944869faa5a39a291bf2240213074d08d0b2574cbc0347` |
   | `klatch-v1.2.5-linux-arm64.tar.gz` | `879e18276c65aca4b208e92443ca2c63d752fe9feb1b1d9d08437834e71cf37b` |
   | `klatch-v1.2.5-darwin-amd64.tar.gz` | `7508bf9f24e42be496f602ab47c992de888cc856e6c9264cba4569e19c5dbd3e` |
   | `klatch-v1.2.5-darwin-arm64.tar.gz` | `f3044c53f8ff04fa234b6a51e9c037a10a0d92a7106ce230ce55b55068139219` |
   | `klatch-web-v1.2.5.tar.gz` | `85ceb1358f754ef35a8f7947df64a38c729c8fbaa8fe62b5a5626be9d2eee89b` |

2. Updated the fleet pin to `1.2.5-bashy.1`, release tag
   `bashy-v1.2.5-1`, and workflow identity
   `https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.5-1`.
3. Final `go test ./pkg/fleet/...` passed: `pkg/fleet` 53.971s;
   `pkg/fleet/fleetkinds` and `pkg/fleet/fleettest` passed from cache.
4. Live deployment was not performed. The supplied ACCESS endpoint reached
   only the jump host, where `<sat>/s518-klatch` was absent. The registered
   target route refused execution because its outpost elevation had expired,
   and the target name did not resolve from the jump host for nested SSH.
   No remote files, services, data stores, or front-door configuration were
   changed.

The live `cosign verify-blob`, binary/web swap, service restart, health checks,
and public `/admin/status` checks remain blocked pending refreshed target
access. The sprint manager was notified through the Bashy message board.
