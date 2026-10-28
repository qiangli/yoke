# S518.6e Klatch v1.2.2 deployment transcript

Target: `<host>` via the S518 claim ACCESS line; working directory `<sat>/s518-klatch`. The two services are `klatch` (loopback 7100) and `klatch2` (loopback 7101).

1. Downloaded all four server archives and the web archive from signed release `bashy-v1.2.2-1`. Measured SHA-256 of the downloaded archives:

   | Asset | SHA-256 |
   | --- | --- |
   | `klatch-v1.2.2-linux-amd64.tar.gz` | `d8b7f391e6d370b2aa1b4d3fe7488d36ad5553ea4f6f834204ccef33e9ecf170` |
   | `klatch-v1.2.2-linux-arm64.tar.gz` | `5ec76eb04f4c2ea48bb687e618a05bf94997646dee9b92a18b524201924d50b0` |
   | `klatch-v1.2.2-darwin-amd64.tar.gz` | `f71a460a677028554a3d2ba66a45d4d42aa1ea3b38157c234043106301a0d145` |
   | `klatch-v1.2.2-darwin-arm64.tar.gz` | `baa4424afe71d7fe3890e43b77c8bc10903a7840e9da9f80e9ac547c97d6f93a` |
   | `klatch-web-v1.2.2.tar.gz` | `457912a471ee08f6e899a8a4dce73264924a1d1562f09df2e5d038e18ac31349` |

2. Downloaded the Darwin ARM64 server archive, web archive, and their Sigstore bundles on `<host>`. Host SHA-256 matched the values above. `cosign verify-blob --bundle ... --certificate-identity <release-workflow-identity> --certificate-oidc-issuer https://token.actions.githubusercontent.com` returned `Verified OK` for **both** archives. The identity was the `bashy-build.yml` workflow at tag `bashy-v1.2.2-1`.
3. Extracted the server and web archives into staging directories. Stopped both services, moved the old `release` and `data/web` to versioned backups, and moved the staged directories into place. The `data` and `data2` stores were retained; `data2/web` still points to `data/web`. Started both services.
4. Measured after restart: `klatch` and `klatch2` each reported `active` and health `ok`; public GET `/admin/status` through each app path returned HTTP `200`.
5. `go test ./pkg/fleet/...` passed: `pkg/fleet` 55.991s, `pkg/fleet/fleetkinds` 1.198s, `pkg/fleet/fleettest` 0.824s.

The manager will run the end-to-end browser gate.
