# S518.6i Klatch v1.2.4 deployment transcript

Target: `<host>` via the S518 claim ACCESS line; working directory `<sat>/s518-klatch`. Services: `klatch` on loopback 7100 and `klatch2` on loopback 7101.

1. Downloaded all four server archives and the web archive from release `bashy-v1.2.4-1`. Measured SHA-256:

   | Asset | SHA-256 |
   | --- | --- |
   | `klatch-v1.2.4-linux-amd64.tar.gz` | `7e0619dbaaacc3c02a84dae477e614db45edda5b0cec1b9fa5a7d0acb122a4a2` |
   | `klatch-v1.2.4-linux-arm64.tar.gz` | `ca334ce488f1a9b2581a8442835c6a0167ced7b9a9d55de94486adc2f1eb7c9d` |
   | `klatch-v1.2.4-darwin-amd64.tar.gz` | `e032567616c51890c62cb71dd7817714f02183f0ed7796a21aae317bc7576b90` |
   | `klatch-v1.2.4-darwin-arm64.tar.gz` | `356158e7b0420b835846ccf795a3d22c1f1666c88623edc4c7506fbf1487689b` |
   | `klatch-web-v1.2.4.tar.gz` | `1928f2e7b147ea3948b5636b0b1ec3ec7a45ffed2a2a8907625c1f7a8aaa35c7` |

2. Downloaded the Darwin ARM64 server archive, web archive, and both `.sigstore.json` bundles on `<host>`. Host SHA-256 values matched the table. `cosign verify-blob --bundle ... --certificate-identity https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.4-1 --certificate-oidc-issuer https://token.actions.githubusercontent.com` returned `Verified OK` for each archive.
3. Extracted both archives into staging directories. Stopped both services, moved the v1.2.3 release and both live web directories to versioned backups, installed the staged v1.2.4 release and web bundle, and restarted both services. The `data` and `data2` stores, including both database files, were retained.
4. Measured after restart: `klatch` and `klatch2` each reported `active` and health `ok`; loopback GET `/admin/status` returned HTTP `200` on ports 7100 and 7101; public GET `/admin/status` through each app path returned HTTP `200`.
5. Final `go test ./pkg/fleet/...` passed: `pkg/fleet` 61.084s; `pkg/fleet/fleetkinds` and `pkg/fleet/fleettest` passed from cache.

The manager will run the end-to-end browser gate.
