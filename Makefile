###########################################################################
##  GENERATED FILE — DO NOT EDIT.                                        ##
##  Produced from dag.md by `bashy dag --makefile`. Edit dag.md instead, ##
##  then regenerate; manual changes are overwritten and fail --check.    ##
###########################################################################
# Tasks live in dag.md (a bashy dag file); this Makefile only forwards.
#   bashy dag <target>           run a target (its dependencies first)
#   bashy dag --list             list targets
#   bashy dag --dry-run <target> print the plan, run nothing
#   bashy dag --skill DIR        export these tasks as an Agent Skill
# Regenerate with: bashy dag --makefile  (verify: bashy dag --makefile --check)

# List the dag targets, then how to run them natively.
help: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag --list; echo "Run: bashy dag <target> | skill: bashy dag --skill DIR | more: bashy help"

# Build the busybox-style multicall binary (`coreutils <tool>` / argv[0] dispatch) into ./bin.
build: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag build

# Test coreutils' own packages — the cross-platform CI scope (excludes the vendored external/ forks).
test: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag test

# Build the native Ollama runner subprocess from the exact vendored gitlink at `external/ollama/src`, then gzip it to `external/ollama/runner_embed/ycode-runner.…
runner-build-thin: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag runner-build-thin

# Verify the runner build contract without running code generation or the heavy native C++/cgo compile.
runner-build-check: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag runner-build-check

# Run the Sprint 418 S8 shared-context handoff recipe.
synergy-s8: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag synergy-s8

# Cross-OS typecheck of the CI scope WITHOUT needing a Windows/Linux box — `go vet` compiles every package (tests included) for the target GOOS, which is exactly…
crossvet: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag crossvet

# The consumer-direction gate — the one `test` and `crossvet` structurally cannot be, because both only ever compile coreutils against coreutils' own module reso…
consumer-build: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag consumer-build

# The formatting gate.
fmtcheck: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag fmtcheck

# Verify the generated Makefile matches this file.
makefile-check: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag makefile-check

# Static check, same scope as `test`.
vet: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag vet

# Full test including the vendored external/ forks.
test-all: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag test-all

# Cross-compile the multicall binary for every release platform into bin/dist/.
dist: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag dist

# Remove built binaries.
clean: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag clean

# Test the bashy browser live Chrome extension: the manifest version against the hub floor, the release packer, and the browser command surface.
chrome-ext-test: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag chrome-ext-test

# Pack the embedded live extension for release into dist/chrome-ext/: the zip the Chrome Web Store takes (`bashy-live-<manifest version>.zip`) plus its sha256.
chrome-ext-release: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag chrome-ext-release

# Also pack a signed CRX3 (for enterprise-policy / self-hosted installs; Chrome on Windows and macOS refuses off-store CRX otherwise, so the Web Store zip stays…
chrome-ext-crx: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag chrome-ext-crx

# Upload the release zip to the Chrome Web Store.
chrome-ext-publish: FORCE
	@command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag chrome-ext-publish

.DEFAULT: ; @command -v bashy >/dev/null || { echo "requires bashy: https://github.com/qiangli/bashy"; exit 127; }; bashy dag $@

# FORCE names no file and has no recipe: as the one prerequisite it
# keeps every forward running even when a file or directory on disk
# shares a target's name. Dependency order stays in dag, not here.
FORCE:
