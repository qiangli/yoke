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
//	"go@1.27.1/linux/amd64". Value: the lowercase hex sha256 of the exact
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
	// gotoolchain.DefaultVersion — official go.dev archives, digest confirmed
	// by an independent download + sha256 of the artifact itself.
	"go@1.27.1/darwin/amd64":  "8f8f52c6649542cf027bbc9b9c68d1ec042f9f34808a40413f0b8b3f66f3caa4",
	"go@1.27.1/darwin/arm64":  "ee215d57e0ec269c60cc9ceca68e6bda321ba9ee5afe24f4b0988703c2d87d12",
	"go@1.27.1/linux/amd64":   "63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445",
	"go@1.27.1/linux/arm64":   "3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec",
	"go@1.27.1/windows/amd64": "a3911b5e0e1b1053f25ed0675f4c1c6aad1e2bfcf253df2b9be4caabd2edd95d",
	"go@1.27.1/windows/arm64": "13b69b87bb0e83f96bc68560a8cace7f0343b1e03469f1110ea18d17e3234069",

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

	// gitea@v1.27.3 & loom@v1.27.3 — official go-gitea/gitea release sha256 sidecars
	"gitea@v1.27.3/darwin/amd64":  "23964155add4490ed73733fa90ea63154b724ab6fe389b7a979db4ef3d7ed8ce",
	"gitea@v1.27.3/darwin/arm64":  "fd83383e05a4185e8f852563a7599f5d8f3e18ede00d412c0f9f044771aef162",
	"gitea@v1.27.3/linux/amd64":   "4da93c2c10b6980c359bcb86d5573ebfd7770e2e151756534edee24c8c12d971",
	"gitea@v1.27.3/linux/arm64":   "04c086d36dba793546e331484a9da34571763efdfa77dc526cc98e0f10917e7b",
	"gitea@v1.27.3/windows/amd64": "d9ed1fc48ec33a8cb97d7fed3882b4e159efc3fcae3a77d0d240e250d5bf6e21",
	"gitea@v1.27.3/windows/arm64": "d590f8be49cdac0a734b04b7936c18986eb0e600cc98d5f3414fd797f092c1d4",

	"loom@v1.27.3/darwin/amd64":  "23964155add4490ed73733fa90ea63154b724ab6fe389b7a979db4ef3d7ed8ce",
	"loom@v1.27.3/darwin/arm64":  "fd83383e05a4185e8f852563a7599f5d8f3e18ede00d412c0f9f044771aef162",
	"loom@v1.27.3/linux/amd64":   "4da93c2c10b6980c359bcb86d5573ebfd7770e2e151756534edee24c8c12d971",
	"loom@v1.27.3/linux/arm64":   "04c086d36dba793546e331484a9da34571763efdfa77dc526cc98e0f10917e7b",
	"loom@v1.27.3/windows/amd64": "d9ed1fc48ec33a8cb97d7fed3882b4e159efc3fcae3a77d0d240e250d5bf6e21",
	"loom@v1.27.3/windows/arm64": "d590f8be49cdac0a734b04b7936c18986eb0e600cc98d5f3414fd797f092c1d4",

	// goreleaser@v2.18.2-bashy.1 — qiangli/goreleaser fork release, built from
	// v2.18.2 + a README fork notice (commit 7af3788) with Go 1.27.2.
	// Provisioned silently by `bashy release`; never a user-facing verb.
	"goreleaser@v2.18.2-bashy.1/darwin/amd64":  "6541f0aed90654fcf69bcdd6979e3a7b9407aabcc450b028d547bb05ba87df4d",
	"goreleaser@v2.18.2-bashy.1/darwin/arm64":  "5e4dce5d497058651b453b14e69a189b8928e01cfb0b6117bab75f4a7badf384",
	"goreleaser@v2.18.2-bashy.1/linux/amd64":   "0801ec64d19e18393e1620d9cccb71ee01af0376b79f961c3e90c6c2f0132b5a",
	"goreleaser@v2.18.2-bashy.1/linux/arm64":   "7b65d7e90db05b980969ab2db42993f35b3946b78d3a800d5d82a7a10232cd3b",
	"goreleaser@v2.18.2-bashy.1/windows/amd64": "6e9ff7dd2d9d88810b26c25d0fafcacca2f0df341209bdf7c6c4c4ab1fbf52cd",
	"goreleaser@v2.18.2-bashy.1/windows/arm64": "c6d83c623d3b5b4ecb4b5bbe2dd18b9a00aded82be536bc9e26d69a733be6eb1",

	// ollama@v0.40.1 — official ollama/ollama release sha256sum.txt
	"ollama@v0.40.1/darwin/amd64":  "66e1587711f3a06315b23782ba74897001da6c8b8edf6c0371f7533015a076dd",
	"ollama@v0.40.1/darwin/arm64":  "66e1587711f3a06315b23782ba74897001da6c8b8edf6c0371f7533015a076dd",
	"ollama@v0.40.1/linux/amd64":   "a7aebbe3dd76ccf1351a56a3e57218ad4863cb5f9a9938c58de87a37555e355d",
	"ollama@v0.40.1/linux/arm64":   "f5cbd9a97e0de9502ef928cb993f6d16468e25be6d55a8529d7fd2b67a130bcb",
	"ollama@v0.40.1/windows/amd64": "b394d14436d38032f23190e3f14eb2c6dad5ebbe4e192414f74c8fdca01703ab",
	"ollama@v0.40.1/windows/arm64": "68681e6822160121c3384ba99c91025cfea2d14dcc7d166af0f94263b69407a9",
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
