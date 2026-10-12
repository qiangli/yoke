// Package notrack centralizes third-party tracking and update-check opt-outs
// for child processes bashy launches via binmgr toolchains and CLIs.
//
// All settings are config-only (environment variables), never network writes or
// mutation. The helper is the one place wrappers delegate to, so a change here
// reaches every external/* wrapper that imports it. DO_NOT_TRACK=1 is the
// generic default; specific vars are set per upstream documentation.
//
// Upstream citations per var:
//
//	DO_NOT_TRACK=1
//	  Generic opt-out honored by many tools including Bun (https://bun.sh/docs/project/environment)
//	  and as the Console Do Not Track standard (https://consoledonottrack.com).
//
//	NPM_CONFIG_UPDATE_NOTIFIER=false, NPM_CONFIG_FUND=false
//	  npm config docs: update-notifier and fund (https://docs.npmjs.com/cli/v10/using-npm/config)
//	  Env form NPM_CONFIG_<key> (one spelling only: Env matches keys case-insensitively) maps to npm config `update-notifier` and `fund`.
//
//	PIP_DISABLE_PIP_VERSION_CHECK=1
//	  pip docs: https://pip.pypa.io/en/stable/cli/pip/#cmdoption-disable-pip-version-check
//	  and environment-variable mapping (PIP_DISABLE_PIP_VERSION_CHECK).
//
//	uv equivalents: PIP_DISABLE_PIP_VERSION_CHECK covers uv pip compat; uv itself
//	  is documented at https://docs.astral.sh/uv/reference/environment/ (no
//	  phone-home update check to disable, but keep pip flag for the pip layer).
//	  Additional uv env kept minimal to remain config-only.
//
//	GOTELEMETRY=off
//	  Go telemetry opt-out: https://go.dev/doc/telemetry#environment-variables
//	  ("GOTELEMETRY=off" or `go telemetry off` for the managed toolchain).
//
//	DOTNET_CLI_TELEMETRY_OPTOUT=1, DOTNET_NOLOGO=1
//	  .NET telemetry opt-out and nologo: https://learn.microsoft.com/en-us/dotnet/core/tools/telemetry
//	  and https://learn.microsoft.com/en-us/dotnet/core/tools/dotnet-environment-variables (DOTNET_NOLOGO).
//
//	POWERSHELL_TELEMETRY_OPTOUT=1, POWERSHELL_UPDATECHECK=Off
//	  PowerShell telemetry and update check: https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_telemetry
//	  and PowerShell update-check configuration (POWERSHELL_UPDATECHECK=Off).
//	  Kept in sync with external/pwsh Launch.Overrides/Environ.
//
//	DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 handled in pwsh launch; generic
//	  dotnet opt-outs above complement it.
//
//	GH_NO_UPDATE_NOTIFIER=1
//	  gh CLI: https://cli.github.com/manual/gh_help_environment (GH_NO_UPDATE_NOTIFIER).
//	  GH telemetry has no stable opt-out env; DO_NOT_TRACK covers generic.
//
//	CHECKPOINT_DISABLE=1
//	  HashiCorp checkpoint (used by OpenTofu/terraform via checkpoint API):
//	  https://developer.hashicorp.com/terraform/cli/config/environment-variables (CHECKPOINT_DISABLE).
//
//	Tools with no documented env opt-out (rclone, helm, mise, doctl, rustup)
//	  get nothing here; add a tool-specific var only when upstream documents one.
package notrack

import (
	"os"
	"sort"
	"strings"
)

// Vars is the canonical set of no-tracking env assignments. Keep values
// exactly as upstream expects (case-sensitive). Callers must not mutate it.
var Vars = map[string]string{
	"DO_NOT_TRACK":                  "1",
	"NPM_CONFIG_UPDATE_NOTIFIER":    "false",
	"NPM_CONFIG_FUND":               "false",
	"PIP_DISABLE_PIP_VERSION_CHECK": "1",
	"GOTELEMETRY":                   "off",
	"DOTNET_CLI_TELEMETRY_OPTOUT":   "1",
	"DOTNET_NOLOGO":                 "1",
	"POWERSHELL_TELEMETRY_OPTOUT":   "1",
	"POWERSHELL_UPDATECHECK":        "Off",
	"GH_NO_UPDATE_NOTIFIER":         "1",
	"CHECKPOINT_DISABLE":            "1",
	// COREPACK and pnpm update checks: COREPACK_ENABLE_DOWNLOAD_PROMPT=0 is
	// set per-wrapper; keep it here as well for any direct corepack/pnpm
	// invocation. See https://nodejs.org/api/corepack.html and pnpm docs
	// (PNPM update notifier is gated by npm_config_update_notifier).
	"COREPACK_ENABLE_DOWNLOAD_PROMPT": "0",
}

// Env returns a copy of base with Vars applied. Existing entries whose key
// (case-insensitive, as env is on Windows) matches a wanted key are replaced;
// otherwise they are preserved. The result always contains every entry in Vars.
func Env(base []string) []string {
	wantUpper := make(map[string]string, len(Vars))
	for k, v := range Vars {
		wantUpper[strings.ToUpper(k)] = k + "=" + v
	}
	out := make([]string, 0, len(base)+len(wantUpper))
	for _, kv := range base {
		if name, _, ok := strings.Cut(kv, "="); ok {
			if _, has := wantUpper[strings.ToUpper(name)]; has {
				continue
			}
		}
		out = append(out, kv)
	}
	// Deterministic append: sorted keys for stable tests and process env.
	keys := make([]string, 0, len(wantUpper))
	for k := range wantUpper {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, wantUpper[k])
	}
	return out
}

// Environ returns os.Environ with Vars applied.
func Environ() []string { return Env(os.Environ()) }

// Has checks that env contains key=value (key case-insensitive).
func Has(env []string, key, want string) bool {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
			return v == want
		}
	}
	return false
}
