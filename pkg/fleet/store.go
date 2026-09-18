package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ext is the on-disk extension of every fleet entry. One file per entry,
// and the file's bytes are the asset Content blob verbatim.
const ext = ".yaml"

// Noun directory names, used for both the local store and shared dirs.
const (
	dirTools    = "tools"
	dirModels   = "models"
	dirAgents   = "agents"
	dirPeople   = "people"
	dirHosts    = "hosts"
	dirCommands = "commands"
)

// DefaultRoot is the parent of every noun's local store. $BASHY_FLEET_DIR
// overrides it; each noun may be redirected individually (see NounDir).
func DefaultRoot() string {
	if d := os.Getenv("BASHY_FLEET_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".bashy", "fleet")
	}
	return filepath.Join(home, ".config", "bashy")
}

// nounEnv maps a noun to its per-noun directory override.
var nounEnv = map[string]string{
	dirTools:    "BASHY_TOOLS_DIR",
	dirModels:   "BASHY_MODELS_DIR",
	dirAgents:   "BASHY_AGENTS_DIR",
	dirPeople:   "BASHY_PEOPLE_DIR",
	dirHosts:    "BASHY_HOSTS_DIR",
	dirCommands: "BASHY_COMMANDS_DIR",
}

// nounPathEnv maps a noun to its PATH-list of read-only shared dirs.
var nounPathEnv = map[string]string{
	dirTools:    "BASHY_TOOLS_PATH",
	dirModels:   "BASHY_MODELS_PATH",
	dirAgents:   "BASHY_AGENTS_PATH",
	dirPeople:   "BASHY_PEOPLE_PATH",
	dirHosts:    "BASHY_HOSTS_PATH",
	dirCommands: "BASHY_COMMANDS_PATH",
}

// NounDir resolves a noun's local store directory.
func NounDir(root, noun string) string {
	if env, ok := nounEnv[noun]; ok {
		if d := os.Getenv(env); d != "" {
			return d
		}
	}
	return filepath.Join(root, noun)
}

// SeedsEnv is the switch that drops the seeded model + agent roster from the
// embedded ring. The tool launch contracts are never dropped: they are the
// mechanism, the roster is data. An org that wants its hosts to see only the
// catalog it publishes (`sync`) or mounts ($BASHY_*_PATH) sets it to `off`;
// the test ring sets it so a fixture roster is the whole roster.
const SeedsEnv = "BASHY_FLEET_SEEDS"

// seedsOff reports whether the seeded roster is switched off for this process.
func seedsOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(SeedsEnv))) {
	case "off", "none", "0", "false", "no":
		return true
	}
	return false
}

// sharedDirs returns the read-only shared catalog dirs for a noun.
func sharedDirs(noun string) []string {
	env, ok := nounPathEnv[noun]
	if !ok {
		return nil
	}
	var out []string
	for _, d := range filepath.SplitList(os.Getenv(env)) {
		if d != "" {
			out = append(out, d)
		}
	}
	return out
}

// validName rejects anything that would escape the store or collide with
// the extension. Entry names are identifiers, not paths.
func validName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("fleet: empty name")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("fleet: name %q must not contain a path separator", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("fleet: name %q must not start with a dot", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("fleet: name %q must not contain %q", name, "..")
	}
	return nil
}

// entryPath is where a named entry lives, given its already-resolved
// directory. Reads and writes share this resolution so they can never
// disagree about where the local store is.
func entryPath(dir, name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	return filepath.Join(dir, name+ext), nil
}

// writeEntry saves an entry's canonical bytes atomically. A temp file in
// the destination directory is renamed over the target, so a concurrent
// reader never observes a half-written asset.
func writeEntry(dir, name string, data []byte) error {
	path, err := entryPath(dir, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return renameOver(tmpName, path)
}

// renameOver replaces dst with src. Windows can fail a rename onto an
// existing file that another process still has open; retry briefly.
func renameOver(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
	}
	return err
}

// removeEntry deletes an entry from the local store. Removing an entry
// that only exists in a lower ring is an error, not a silent no-op: the
// caller asked to delete something the local store does not own.
func removeEntry(dir, noun, name string) error {
	path, err := entryPath(dir, name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("fleet: %s %q is not in the local store (it may come from a lower ring)", noun, name)
	}
	return os.Remove(path)
}
