#!/usr/bin/env bash
# Run on a coordinated browser test host with a disposable Chrome profile.
set -euo pipefail

: "${BASHY_BROWSER_EXT_CHROME:?Set the Chrome for Testing executable}"
: "${BASHY_BROWSER_E2E_BIN:?Set the candidate Bashy or Yoke executable}"
for executable in "$BASHY_BROWSER_EXT_CHROME" "$BASHY_BROWSER_E2E_BIN"; do
    if [[ "$executable" != /* || ! -x "$executable" ]]; then
        printf 'Expected an absolute path to an executable: %s\n' "$executable" >&2
        exit 1
    fi
done

export BASHY_BROWSER_LIVE=1
export BASHY_BROWSER_EXT_CHROME BASHY_BROWSER_E2E_BIN
# The legacy page, solo and copy-sheet fixtures also use Chrome discovery.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/google-chrome" <<'WRAPPER'
#!/usr/bin/env sh
exec "$BASHY_BROWSER_EXT_CHROME" "$@"
WRAPPER
chmod +x "$tmp/google-chrome"
export PATH="$tmp:$PATH"
export CHROME="$BASHY_BROWSER_EXT_CHROME"
export BASHY_HOME="$tmp/bashy"
export GOWORK=off

# Run browser packages serially to keep screenshot and clipboard fixtures isolated.
# The probe CLI scenario starts its own debugger. Avoid probing a host's
# default debugging endpoint during the regression gate.
go test -json -count=1 -p 1 -skip '^TestLiveProbeSkippedWhenUnavailable$' \
    ./cmds/browser ./pkg/browser/... | tee "$tmp/results.jsonl"
# These families are the required real-browser checks. Other tests may skip
# legitimately when they require a different platform or browser capability.
if grep -E '"Action":"skip".*"Test":"(TestExtensionE2E|TestBrowserE2E|TestPage|TestSolo(Effects|Screenshot|Guard|Extract|Evaluate|Selector|Click)|TestLiveSoloNavigate|TestCopySheet.*InChrome|TestStitchRestoresAfterLongOperationTimeout)' "$tmp/results.jsonl"; then
    printf 'A required real-browser regression check was skipped.\n' >&2
    exit 1
fi
