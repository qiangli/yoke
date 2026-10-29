package binmgr

import "context"

// ChromeHeadlessShellVersion is the reviewed Chrome for Testing stable build
// used when a host has no installed Chromium-family browser. Chrome Headless
// Shell is built from Chromium and its archive carries its license alongside
// the executable.
const ChromeHeadlessShellVersion = "156.0.8078.73"

// ChromeHeadlessShellTool returns the checksum-pinned, whole-tree browser
// bundle. The executable needs the shared libraries and data files beside it,
// so this deliberately uses binmgr's Tree extraction rather than extracting a
// single archive member.
func ChromeHeadlessShellTool() Tool {
	const base = "https://storage.googleapis.com/chrome-for-testing-public/" + ChromeHeadlessShellVersion + "/"
	asset := func(platform, digest, entrypoint string) Asset {
		return Asset{
			URL:        base + platform + "/chrome-headless-shell-" + platform + ".zip",
			SHA256:     digest,
			Tree:       true,
			Entrypoint: entrypoint,
		}
	}
	return Tool{
		Name:    "chrome-headless-shell",
		Version: ChromeHeadlessShellVersion,
		Assets: map[string]Asset{
			"darwin/amd64":  asset("mac-x64", "be798095cd693357a7a6a0e7a0c4ebd81563e16f51225eb98aacc906d6326519", "chrome-headless-shell-mac-x64/chrome-headless-shell"),
			"darwin/arm64":  asset("mac-arm64", "512c7d34cd5b65d617a39a48915b490eed4208a63e9d47b0fd7893e83be85765", "chrome-headless-shell-mac-arm64/chrome-headless-shell"),
			"linux/amd64":   asset("linux64", "77eb8d52798efc7a857ab61abcc62a456d8ea9635b02341d06dffee2761768c8", "chrome-headless-shell-linux64/chrome-headless-shell"),
			"linux/arm64":   asset("linux-arm64", "98c084708f0f1fa4ed589ad601cc79b437f0cebe60ffc9794af78a085c01a2a7", "chrome-headless-shell-linux-arm64/chrome-headless-shell"),
			"windows/amd64": asset("win64", "e6d26810df71efd0a3dfd0f7246d75d279ebdf8a8ec153d1ac44069657298628", "chrome-headless-shell-win64/chrome-headless-shell.exe"),
			// Chrome for Testing does not publish a native Windows arm64 shell;
			// Windows 11 arm64 supports its win64 build through x64 emulation.
			"windows/arm64": asset("win64", "e6d26810df71efd0a3dfd0f7246d75d279ebdf8a8ec153d1ac44069657298628", "chrome-headless-shell-win64/chrome-headless-shell.exe"),
		},
	}
}

// EnsureChromeHeadlessShell installs the current platform's pinned bundle.
func EnsureChromeHeadlessShell(ctx context.Context) (string, error) {
	return Ensure(ctx, ChromeHeadlessShellTool())
}
