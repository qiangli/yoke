package binmgr

import (
	"os"
	"strings"
)

// Pinned digests move the supply-chain trust root OFF the downloaded release and
// INTO this repository's reviewed git history.
//
// The problem they solve: binmgr resolves a tool's expected checksum from the
// SAME GitHub release (or mirror) it downloads the artifact from — a `.sha256`
// sidecar, a checksums list, an `.md5`. That is trust-on-first-use: an attacker
// who can alter the release (a compromised publisher account or CI, a tampered
// mirror) rewrites the artifact AND its sidecar together, and the checksum
// verification then passes against the attacker's own digest. Checking a
// download against a number the same server just handed you proves only that the
// bytes arrived intact, not that they are the bytes we intended.
//
// A pin breaks that loop. When (name, version, platform) is pinned here, Ensure
// verifies the downloaded bytes against THIS digest and ignores whatever the
// release claimed. To change what a pinned tool resolves to, someone edits this
// file and it goes through code review — the same trust path as the rest of the
// binary. A fully compromised upstream release is then caught, not trusted.
//
// A pin is only meaningful for a PINNED version. A tool that tracks "latest"
// (its release tag changes over time) cannot be pinned by digest — there is no
// stable artifact to pin — so those keep the resolve-from-release path and its
// residual TOFU exposure. Most tools here use a fixed version constant and can
// (and eventually should) be pinned.
//
// Key format: "<name>@<version>/<goos>/<goarch>", e.g.
//
//	"go@go1.26.4/linux/amd64". Value: the lowercase hex sha256 of the exact
//
// artifact binmgr downloads (the raw binary or the archive — the file as a
// whole, matching what download() hashes).
//
// How to add one: install the tool once on a trusted machine with networking
// you trust, take the sha256 of the cached download, confirm it against the
// vendor's own signed checksums out of band, and commit the entry. The registry
// is intentionally allowed to be sparse — an absent pin is not an error, it just
// means that tool still resolves its checksum from the release.
var pinnedDigests = map[string]string{
	"witr@v0.3.3/darwin/amd64":  "39934f6a8d6a0413c52324ccdbd3a0867371785b6c066005ea063a78279487ef",
	"witr@v0.3.3/darwin/arm64":  "d05b51825604d608da8757e549a1f5322549a350f8336c593429f3f2cd507927",
	"witr@v0.3.3/linux/amd64":   "08fc46e3f80a374476f71d0d6e6579477cd98c6df5cc59d98224adf948f5ebf5",
	"witr@v0.3.3/linux/arm64":   "dca2be6cf56a5274de0a036b83345d055b8e94f7f4fd23dc54dd102d7669e2d8",
	"witr@v0.3.3/windows/amd64": "1ae95a354fa7f767828ad7942497f3801e5299f8afad5844ec6d1819703a6b28",
	"witr@v0.3.3/windows/arm64": "e644a1e152437a0aff93c672660b363de690361ca90f35a792f88b361ca569e4",
	"witr@v0.3.3/freebsd/amd64": "0fcc966fc8adbdf901174c96901e15f4b202e8925bc2ce6d20daf11b9f12305c",
	"witr@v0.3.3/freebsd/arm64": "41ec530a07062797d3143a286c2d094643a3c4b7f1c2171bee81e6e0345b17f1",
}

// PinnedSHA256 returns the committed sha256 for a tool tuple, if one exists.
func PinnedSHA256(name, version, platform string) (string, bool) {
	return pinnedSHA256(name, version, platform)
}

// pinnedSHA256 returns the committed sha256 for a tool tuple, if one exists.
func pinnedSHA256(name, version, platform string) (string, bool) {
	sha, ok := pinnedDigests[name+"@"+version+"/"+platform]
	if !ok {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(sha)), true
}

// weakChecksumAllowed reports whether the operator has explicitly accepted an
// md5-only integrity check for a download that has no sha256/sha512 and no pin.
// Off by default: the secure posture is to refuse.
func weakChecksumAllowed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BASHY_ALLOW_WEAK_CHECKSUM"))) {
	case "", "0", "false", "off", "no":
		return false
	default:
		return true
	}
}
