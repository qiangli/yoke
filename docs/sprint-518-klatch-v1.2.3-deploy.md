# S518.6g Klatch v1.2.3 deployment transcript

Target: `<host>` via the S518 claim ACCESS line; working directory `<sat>/s518-klatch`. Services: `klatch` on loopback 7100 and `klatch2` on loopback 7101.

1. Downloaded all four server archives and the web archive from release `bashy-v1.2.3-1`. Measured SHA-256:

   | Asset | SHA-256 |
   | --- | --- |
   | `klatch-v1.2.3-linux-amd64.tar.gz` | `0b4ab8cadea8e2fd49a04f92f42af120353b16d3e501934be93e5aac2a6b374e` |
   | `klatch-v1.2.3-linux-arm64.tar.gz` | `493db4212f03c50647e346ab994e8fcf6ec681168a51e4e85743936d581681a8` |
   | `klatch-v1.2.3-darwin-amd64.tar.gz` | `0736f1c23f3a9ade12e30cdf85470b2fce675734092a02384d8e1919ac5e3e3c` |
   | `klatch-v1.2.3-darwin-arm64.tar.gz` | `108c997616a855399150b25f987b7c8fb98d1a9a9253310382386a393f8ab3fe` |
   | `klatch-web-v1.2.3.tar.gz` | `24fd826eda82d8d67f61e1122d9f5822eae761c64b659a826cac2a2e6b6a4557` |

2. Downloaded the Darwin ARM64 server archive, web archive, and both `.sigstore.json` bundles on `<host>`. Host SHA-256 values matched the table. `cosign verify-blob --bundle ... --certificate-identity https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.3-1 --certificate-oidc-issuer https://token.actions.githubusercontent.com` returned `Verified OK` for each archive.
3. Extracted both archives into staging directories. Stopped both services, moved `release` and `data/web` to v1.2.2 backups, moved the staged directories into place, and started both services. The `data` and `data2` stores were retained; `data2/web` still resolves to `data/web`.
4. Measured after restart: `klatch` and `klatch2` each reported `active` and health `ok`; public GET `/admin/status` through each app path returned HTTP `200`.
5. `go test ./pkg/fleet/...` passed: `pkg/fleet` 56.440s, `pkg/fleet/fleetkinds` 0.902s, `pkg/fleet/fleettest` 1.313s.

The manager will run the end-to-end browser gate.
