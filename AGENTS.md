# Repository Guidelines

**`bashy sprint` is the source of requests, plans and details for every agent** — every todo is tracked and accounted for as a STORY in a sprint: read the sprint card (spec-ref, acceptance, continuity) for what to do, and never pick up a todo without a story in a `bashy sprint` (file one first). Delivery commits carry `Sprint:` / `Story:` / `Story-ID:` trailers.

## Project Structure & Module Organization

This Go module (`github.com/qiangli/yoke`) is bashy's agentic userland: everything on top of the certified POSIX package `github.com/qiangli/coreutils` (a flat sibling this module imports, never the reverse). Non-POSIX command packages live in `cmds/`, one utility per directory (`cmds/tar`, `cmds/jq`, `cmds/browser`); `cmds/all` blank-imports coreutils' certified `cmds/all` and adds these, so it registers the WHOLE userland. `cmd/yoke` is the busybox-style binary (+ `yoke mcp`). Shared runtime and flags come from coreutils' `tool/`; in-process git support is in `git/`; the AgentOS hub packages are in `pkg/`. Docs live in `docs/`; managed externals and the mirrored ollama/podman forks live under `external/`.

## Build, Test, and Development Commands

- `go build ./cmd/yoke` builds the whole-userland multicall binary.
- `go test $(go list ./... | grep -v /external/)` runs main tests while avoiding heavyweight vendored forks.
- `go test ./cmds/jq ./pkg/kb` runs focused packages while iterating.
- `go vet <pkgs>` runs static checks; mirror CI by using `go list ./...`, excluding `/external/`, then adding `external/gotoolchain`, `external/act`, and `external/gh`.
- `gofmt -w <files>` formats changed Go files before review.

Use Go from `go.mod`. The module replaces `github.com/qiangli/coreutils` with sibling `../coreutils`, `mvdan.cc/sh/v3` with `../sh` and the File Browser fork with `../filebrowser`; standalone clones need those checkouts. CI initializes the `external/ollama/src` and `external/podman/src` submodules.

## Coding Style & Naming Conventions

Use standard Go formatting: tabs from `gofmt`, short package names, and idiomatic `CamelCase` exported identifiers. Command packages are named after the utility (`cmds/tar`, `cmds/jq`) and register through coreutils' `tool`. Use `tool.RunContext` for stdio, working directory, and environment instead of process globals. Preserve `LC_ALL=C`-style output; unsupported flags or modes must fail loudly.

## Testing Guidelines

Tests use Go's standard `testing` package and `*_test.go` files. Place command tests beside implementations (`cmds/tar/tar_test.go`); shared package tests belong with their package (`git/*_test.go`, `pkg/kb/*_test.go`). Prefer table-driven coverage for flags, stdio, stderr, exit codes, and platform paths.

## Commit & Pull Request Guidelines

Recent commits use concise, imperative subjects with an optional scope, for example `kb: reject a fold that names a host`, `cmds: add tree --json`, or `docs: refresh the atlas groups`. Keep the first line focused on behavior. Pull requests should include a short description, test/vet results, linked issues when relevant, and notes for GNU/POSIX compatibility gaps or platform-specific behavior.

## Compatibility & Licensing Notes

Implement commands from public documentation and permissively licensed sources only; do not copy GPL implementation code. Maintain pure-Go behavior with no host shell-outs for userland commands. Update `THIRD_PARTY_LICENSES.md` when adding dependencies or adapted code that requires attribution.
