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
//	"go@1.27.2/linux/amd64". Value: the lowercase hex sha256 of the exact
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
	// prima.cpp bashy-build.yml tag bashy-bashy-prima-0.1.2-13 (hosted run
	// 38486571631, green on all five platforms). Each raw asset matches the
	// release's signed SHA256SUMS; per-asset bundles verified against the
	// exact tag workflow identity and GitHub Actions OIDC issuer. The CLI
	// and server are pinned per platform; libomp.dll is the Windows OpenMP
	// runtime the executables load, installed beside them.
	"prima@bashy-bashy-prima-0.1.2-13/darwin/amd64":  "d6fe35e0a331ea2b17aaf941ed9bb367e55d27e08bb8ea189945a253f0c1567b",
	"prima@bashy-bashy-prima-0.1.2-13/darwin/arm64":  "43df85cb7d1622a62ff7f6d12d6d844dec8b98650ed323f315c689c9f2b88d9c",
	"prima@bashy-bashy-prima-0.1.2-13/linux/amd64":   "d8870b8c06c956b450400c1d15e26cb10582a0fb4cc0874decf8ef1d03eb3f71",
	"prima@bashy-bashy-prima-0.1.2-13/linux/arm64":   "7e4e70b6e2121691b6e51ecd23ed4c3792f39472c0a2a4119e58e165d12432b3",
	"prima@bashy-bashy-prima-0.1.2-13/windows/amd64": "61d4b38df22ea014f65cb919cce0acb17f1c2f440ba7ebc528133113a6f040b8",

	"prima-server@bashy-bashy-prima-0.1.2-13/darwin/amd64":  "e08abb2b162edbf46ecd3d7cd783bd2bed827bd8ae9a59c6225620c552fc4442",
	"prima-server@bashy-bashy-prima-0.1.2-13/darwin/arm64":  "c62b848bf5309b4e16931942c0ad3715932425c6f2bc3a103ea7ba1c778a9707",
	"prima-server@bashy-bashy-prima-0.1.2-13/linux/amd64":   "ac8afd44b8e09105390fe5cbab2babf4aed053f59cef26e7913f7ef3d5122a22",
	"prima-server@bashy-bashy-prima-0.1.2-13/linux/arm64":   "745504f75f1fd98f9c22e719232d8b62daf13bc102f95706f39dfca35055251c",
	"prima-server@bashy-bashy-prima-0.1.2-13/windows/amd64": "99163f5b9900fa4b1dae87492b5e059278e8d56f4b26d2a687ebf19304b09525",

	"prima-libomp@bashy-bashy-prima-0.1.2-13/windows/amd64": "bc3880baa3d7770c3b598dd709a01fb54540a99139a526cb503764e6d6520a06",

	// binmgr.CosignTool (CosignVersion v3.1.1) — the pinned Sigstore verifier.
	// Each digest matched three ways: GitHub's published asset digest, the
	// release's own cosign_checksums.txt, and an independent download + sha256.
	"cosign@v3.1.1/darwin/amd64":  "14d2678dfbfde18798151e86fbd91ebdadbb7424b18412a42a155dd8a2df4c7a",
	"cosign@v3.1.1/darwin/arm64":  "94b42a9e697be95675f6160ab031a9a5f1ec1e646d6f648d7b2f5cd59ececbc5",
	"cosign@v3.1.1/linux/amd64":   "ae1ecd212663f3693ad9edf8b1a183900c9a52d3155ba6e354237f9a0f6463fc",
	"cosign@v3.1.1/linux/arm64":   "2ec865872e331c32fd12b08dae15332d3f92c0aa029219589684a4903ca85d11",
	"cosign@v3.1.1/windows/amd64": "9d2c026e667bfd979fa7ba1cab8c4b24d2e73f336ec2d57f7fc72c7e73e5b4b6",

	// gotoolchain.DefaultVersion — official go.dev archives, digest confirmed
	// by an independent download + sha256 of the artifact itself.
	"go@1.27.2/darwin/amd64":  "587b59182488b23aa6e5fc25110405a3e0e5b38ed2f5b2f46ed13c32aee356fe",
	"go@1.27.2/darwin/arm64":  "76812b213b1b2302c978d28fa52fa92d541704b9e7d9d5db8002c50e4018c4c5",
	"go@1.27.2/linux/amd64":   "ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5",
	"go@1.27.2/linux/arm64":   "94f3e30b8e374bc285e7dadc11e0865726b9bc6e85b841ccceaabc0214c6b7c8",
	"go@1.27.2/windows/amd64": "1314008898bd40df77af4b014f777f08873dbdfbcd3d92308728ee03304fe04f",
	"go@1.27.2/windows/arm64": "5c4b294f23b558619ae8a4aa94d023e3bca03f52c8398bb41b9026002b0013de",

	"witr@v0.3.3/darwin/amd64":  "39934f6a8d6a0413c52324ccdbd3a0867371785b6c066005ea063a78279487ef",
	"witr@v0.3.3/darwin/arm64":  "d05b51825604d608da8757e549a1f5322549a350f8336c593429f3f2cd507927",
	"witr@v0.3.3/linux/amd64":   "08fc46e3f80a374476f71d0d6e6579477cd98c6df5cc59d98224adf948f5ebf5",
	"witr@v0.3.3/linux/arm64":   "dca2be6cf56a5274de0a036b83345d055b8e94f7f4fd23dc54dd102d7669e2d8",
	"witr@v0.3.3/windows/amd64": "1ae95a354fa7f767828ad7942497f3801e5299f8afad5844ec6d1819703a6b28",
	"witr@v0.3.3/windows/arm64": "e644a1e152437a0aff93c672660b363de690361ca90f35a792f88b361ca569e4",
	"witr@v0.3.3/freebsd/amd64": "0fcc966fc8adbdf901174c96901e15f4b202e8925bc2ce6d20daf11b9f12305c",
	"witr@v0.3.3/freebsd/arm64": "41ec530a07062797d3143a286c2d094643a3c4b7f1c2171bee81e6e0345b17f1",

	// doctl@v1.179.0 — official digitalocean/doctl release checksums
	"doctl@v1.179.0/darwin/amd64":  "be0f7c01635a290cc99fb81e73dd1968cec28e24a1609c68fd30df012e2e8312",
	"doctl@v1.179.0/darwin/arm64":  "a3ac281a5685a73c28047b73e846228b1fcf1c402491512ba68772396344e48d",
	"doctl@v1.179.0/linux/amd64":   "2871f12bde6defbeb1bb3ccaeb260b2b0328118fa16e2294065c7c74efb2a58a",
	"doctl@v1.179.0/linux/arm64":   "d1d452e0e631ea5998c11f621732afcab4955a5ae05e9ae75e0e7039377f7923",
	"doctl@v1.179.0/windows/amd64": "107d12d884d46962f0838c864f08bc06d3e9e8e5329d2eea5466fca05d5aae19",
	"doctl@v1.179.0/windows/arm64": "0e072dd2df5aadd4d899781e21fb5a46d0b057dcef1161a27f458e7d58a2f9ac",

	// rg@15.1.0 — official BurntSushi/ripgrep release sha256 sidecars
	"rg@15.1.0/darwin/amd64":  "64811cb24e77cac3057d6c40b63ac9becf9082eedd54ca411b475b755d334882",
	"rg@15.1.0/darwin/arm64":  "378e973289176ca0c6054054ee7f631a065874a352bf43f0fa60ef079b6ba715",
	"rg@15.1.0/linux/amd64":   "1c9297be4a084eea7ecaedf93eb03d058d6faae29bbc57ecdaf5063921491599",
	"rg@15.1.0/linux/arm64":   "2b661c6ef508e902f388e9098d9c4c5aca72c87b55922d94abdba830b4dc885e",
	"rg@15.1.0/windows/amd64": "124510b94b6baa3380d051fdf4650eaa80a302c876d611e9dba0b2e18d87493a",
	"rg@15.1.0/windows/arm64": "00d931fb5237c9696ca49308818edb76d8eb6fc132761cb2a1bd616b2df02f8e",

	// tofu@v1.13.1 — official opentofu/opentofu release tofu_1.13.1_SHA256SUMS
	"tofu@v1.13.1/darwin/amd64":  "a73720443ba38712d7d96dc1e857add02c15a790919c653ad07492e9952f8c27",
	"tofu@v1.13.1/darwin/arm64":  "be78f659f04ef06a9dbd9b3934d46af95d787a3aa38396d459dea395261816a9",
	"tofu@v1.13.1/linux/amd64":   "378ada19d4bc70c43732004e8159be771b23b9a5afdf059e5f8a2b3fa2c70a69",
	"tofu@v1.13.1/linux/arm64":   "9c1ef375aa1852db0b2888aa921b640c71f8140d4682aa4fec99378a64fa7dc3",
	"tofu@v1.13.1/windows/amd64": "5b653d1d95719eeec4ccf55f0e2102081b6f589d788197f1cabeb89cbd4078a7",
	"tofu@v1.13.1/windows/arm64": "b835f4aed6c447dda73f547dec340aa8ef1e7a80a513303091f8169ad7159afc",

	// gitea@v28.1.0 & loom@v28.1.0 — official go-gitea/gitea release sha256 sidecars.
	// Never pin below the release a host's loom database was migrated by: Gitea
	// refuses to open a newer database (v1.27.3 broke Dragon's loom, 2026-10-08).
	"gitea@v28.1.0/darwin/amd64":  "32c2cde3a677a7ca20be1a581851eac707bf1b97476321bef60febaa2167bb90",
	"gitea@v28.1.0/darwin/arm64":  "26eed10447854770894faf7ae6bf5618a8788522c9565e58d5de6a55845226ee",
	"gitea@v28.1.0/linux/amd64":   "ea93cfa4a300564ac457d3903469fea49ec5a4da37d0cebaa435f8e329ea64d0",
	"gitea@v28.1.0/linux/arm64":   "47bf18a8e4602b17910cd660a902541e2c13b67c732f2607da8326fa3cd46805",
	"gitea@v28.1.0/windows/amd64": "68495c963bab1bfcbbe5b48916c7f362a49aca931583575e09763a02ddf63e2d",
	"gitea@v28.1.0/windows/arm64": "04e2bc31d61e8e3335be8816ea7fc6dea22fa723822d9a6159161f95a49bae5a",

	"loom@v28.1.0/darwin/amd64":  "32c2cde3a677a7ca20be1a581851eac707bf1b97476321bef60febaa2167bb90",
	"loom@v28.1.0/darwin/arm64":  "26eed10447854770894faf7ae6bf5618a8788522c9565e58d5de6a55845226ee",
	"loom@v28.1.0/linux/amd64":   "ea93cfa4a300564ac457d3903469fea49ec5a4da37d0cebaa435f8e329ea64d0",
	"loom@v28.1.0/linux/arm64":   "47bf18a8e4602b17910cd660a902541e2c13b67c732f2607da8326fa3cd46805",
	"loom@v28.1.0/windows/amd64": "68495c963bab1bfcbbe5b48916c7f362a49aca931583575e09763a02ddf63e2d",
	"loom@v28.1.0/windows/arm64": "04e2bc31d61e8e3335be8816ea7fc6dea22fa723822d9a6159161f95a49bae5a",

	// goreleaser@v2.18.2-bashy.1 — qiangli/goreleaser fork release, built from
	// v2.18.2 + a README fork notice (commit 7af3788) with Go 1.27.2.
	// Provisioned silently by `bashy release`; never a user-facing verb.
	"goreleaser@v2.18.2-bashy.1/darwin/amd64":  "6541f0aed90654fcf69bcdd6979e3a7b9407aabcc450b028d547bb05ba87df4d",
	"goreleaser@v2.18.2-bashy.1/darwin/arm64":  "5e4dce5d497058651b453b14e69a189b8928e01cfb0b6117bab75f4a7badf384",
	"goreleaser@v2.18.2-bashy.1/linux/amd64":   "0801ec64d19e18393e1620d9cccb71ee01af0376b79f961c3e90c6c2f0132b5a",
	"goreleaser@v2.18.2-bashy.1/linux/arm64":   "7b65d7e90db05b980969ab2db42993f35b3946b78d3a800d5d82a7a10232cd3b",
	"goreleaser@v2.18.2-bashy.1/windows/amd64": "6e9ff7dd2d9d88810b26c25d0fafcacca2f0df341209bdf7c6c4c4ab1fbf52cd",
	"goreleaser@v2.18.2-bashy.1/windows/arm64": "c6d83c623d3b5b4ecb4b5bbe2dd18b9a00aded82be536bc9e26d69a733be6eb1",

	// No ollama pins, by design: per docs/bashy-service-supply-chain-policy.md
	// (Sprint 436, story 8d1e72e5dff6) bashy never downloads or execs a
	// third-party ollama release archive — the bashy-signed embedded runner
	// (story 732cbb22) is the only supported runtime — so there is nothing to pin.
}

// PinnedSHA256 returns the committed sha256 for a tool tuple, if one exists.
func PinnedSHA256(name, version, platform string) (string, bool) {
	return pinnedSHA256(name, version, platform)
}

// pinnedSHA256 returns the committed sha256 for a tool tuple, if one exists.
func pinnedSHA256(name, version, platform string) (string, bool) {
	sha, ok := pinnedDigests[name+"@"+version+"/"+platform]
	if !ok {
		if strings.HasPrefix(version, "v") {
			sha, ok = pinnedDigests[name+"@"+strings.TrimPrefix(version, "v")+"/"+platform]
		} else {
			sha, ok = pinnedDigests[name+"@v"+version+"/"+platform]
		}
	}
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
