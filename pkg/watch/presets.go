package watch

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/assetring"
)

// PRESETS ARE A ROD, NOT A FISH.
//
// Every preset here is an ORDINARY rule file. None of them is a verb, none of
// them is reachable in a way a hand-written file is not, and the conductor's
// scratchpad watcher becomes one by being copied into the local ring and
// edited. The moment a preset can do something a user's file cannot, the engine
// has grown a second, privileged configuration path.
//
//go:embed presets/*.json
var presetFS embed.FS

// presetExt is the one file shape: a bashy-watch-rules-v1 JSON document.
const presetExt = ".json"

// Preset is one catalog entry. Err records a file that did not parse: a
// catalog never hides an entry it could not understand (assetring's contract),
// because a preset that silently vanished looks exactly like one that was
// never installed.
type Preset struct {
	Name   string
	Body   []byte
	File   RuleFile
	Err    error
	Ring   assetring.Ring
	Origin string
}

// PresetsDir resolves the LOCAL preset ring:
//
//	$BASHY_WATCH_DIR/presets   the specific override, most precise
//	$BASHY_HOME/watch/presets  a relocated bashy home (tests, sandboxed runs)
//	~/.bashy/watch/presets     the default
//
// The middle rung matters for the same reason it does for the room and the
// sprint store: a run sandboxed with $BASHY_HOME that still read the operator's
// real presets would be a sandbox that silently leaks.
func PresetsDir() string {
	if d := strings.TrimSpace(os.Getenv("BASHY_WATCH_DIR")); d != "" {
		return filepath.Join(d, "presets")
	}
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return filepath.Join(h, "watch", "presets")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".bashy", "watch", "presets")
}

// PresetCatalog merges the embedded baseline with the local ring, local last
// so an operator's copy of a preset shadows the shipped one.
func PresetCatalog() *assetring.Catalog[Preset] {
	var sources []assetring.Source
	if sub, err := fs.Sub(presetFS, "presets"); err == nil {
		sources = append(sources, assetring.FileFS(sub, assetring.RingEmbedded, presetExt))
	}
	if dir := PresetsDir(); dir != "" {
		sources = append(sources, assetring.FileDir(dir, assetring.RingLocal, presetExt))
	}
	return &assetring.Catalog[Preset]{Sources: sources, Parse: parsePreset}
}

func parsePreset(name string, body []byte, src assetring.Source) Preset {
	p := Preset{Name: name, Body: body, Ring: src.Ring()}
	if ds, ok := src.(assetring.DirSourcer); ok {
		p.Origin = ds.Dir()
	}
	file, err := ParseRuleFile(body)
	if err != nil {
		p.Err = err
		return p
	}
	if file.Name == "" {
		file.Name = name
	}
	p.File = file
	return p
}

// LoadPreset returns one preset by name.
func LoadPreset(name string) (Preset, error) {
	entry, _, ok := PresetCatalog().Get(name)
	if !ok {
		return Preset{}, fmt.Errorf("watch: no preset %q (have: %s)", name, strings.Join(PresetNames(), ", "))
	}
	if entry.Err != nil {
		return entry, entry.Err
	}
	return entry, nil
}

// PresetNames lists the merged catalog.
func PresetNames() []string {
	rows, err := PresetCatalog().Rows()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}
