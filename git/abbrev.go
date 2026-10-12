package git

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// errAbbrevUnknown means host git's abbreviation length cannot be
// reproduced for this repository (a multi-pack-index, an unreadable
// pack index, an unusual core.abbrev); callers that print abbreviated
// ids decline instead of guessing.
var errAbbrevUnknown = errors.New("git: abbreviation length not reproducible")

// abbreviator reproduces host git's default object-name abbreviation:
// core.abbrev (default "auto" — ceil((msb(packed objects)+1)/2), at
// least 7) extended until the prefix is unique among every object, as
// find_unique_abbrev does.
type abbreviator struct {
	min int
	ids []plumbing.Hash // every object id, sorted
}

func newAbbreviator(repo *gogit.Repository) (*abbreviator, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return nil, errAbbrevUnknown
	}
	gitDir, err := resolveGitDir(wt.Filesystem.Root())
	if err != nil {
		return nil, errAbbrevUnknown
	}
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(data))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		gitDir = c
	}
	objDirs := []string{filepath.Join(gitDir, "objects")}
	if data, err := os.ReadFile(filepath.Join(gitDir, "objects", "info", "alternates")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !filepath.IsAbs(line) {
				line = filepath.Join(gitDir, "objects", line)
			}
			objDirs = append(objDirs, line)
		}
	}

	a := &abbreviator{}
	packed := 0
	for _, dir := range objDirs {
		packDir := filepath.Join(dir, "pack")
		if _, err := os.Stat(filepath.Join(packDir, "multi-pack-index")); err == nil {
			return nil, errAbbrevUnknown
		}
		idxs, _ := filepath.Glob(filepath.Join(packDir, "*.idx"))
		for _, idx := range idxs {
			ids, err := readPackIndexIDs(idx)
			if err != nil {
				return nil, errAbbrevUnknown
			}
			packed += len(ids)
			a.ids = append(a.ids, ids...)
		}
		loose, _ := filepath.Glob(filepath.Join(dir, "[0-9a-f][0-9a-f]", "*"))
		for _, p := range loose {
			name := filepath.Base(filepath.Dir(p)) + filepath.Base(p)
			if plumbing.IsHash(name) {
				a.ids = append(a.ids, plumbing.NewHash(name))
			}
		}
	}
	sort.Slice(a.ids, func(i, j int) bool { return bytes.Compare(a.ids[i][:], a.ids[j][:]) < 0 })

	cfg, err := repo.Config()
	if err != nil {
		return nil, errAbbrevUnknown
	}
	switch v := cfg.Raw.Section("core").Option("abbrev"); v {
	case "", "auto":
		bits := 0
		for n := packed; n > 1; n >>= 1 {
			bits++ // msb(packed)
		}
		a.min = (bits + 2) / 2 // ceil((msb+1)/2)
		if a.min < 7 {
			a.min = 7
		}
	case "no":
		a.min = 40
	default:
		n, err := strconv.Atoi(v)
		if err != nil || n < 4 {
			return nil, errAbbrevUnknown
		}
		a.min = min(n, 40)
	}
	return a, nil
}

// abbrev returns h's host-git abbreviation: start hex digits (the
// repository default when start <= 0; at least 4 when explicit, as for
// --short=N), extended until no other object shares the prefix.
func (a *abbreviator) abbrev(h plumbing.Hash, start int) string {
	n := a.min
	if start > 0 {
		n = max(start, 4)
	}
	i := sort.Search(len(a.ids), func(i int) bool { return bytes.Compare(a.ids[i][:], h[:]) >= 0 })
	for _, j := range []int{i - 1, i, i + 1} {
		if j < 0 || j >= len(a.ids) || a.ids[j] == h {
			continue
		}
		if l := commonHexPrefix(h, a.ids[j]) + 1; l > n {
			n = l
		}
	}
	return h.String()[:min(n, 40)]
}

func commonHexPrefix(x, y plumbing.Hash) int {
	xs, ys := hex.EncodeToString(x[:]), hex.EncodeToString(y[:])
	n := 0
	for n < len(xs) && xs[n] == ys[n] {
		n++
	}
	return n
}

// readPackIndexIDs reads the sorted object ids of a version 1 or 2 pack
// index file.
func readPackIndexIDs(path string) ([]plumbing.Hash, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	var fanout [256]uint32
	v2 := string(head[:4]) == "\xfftOc"
	if v2 {
		if binary.BigEndian.Uint32(head[4:]) != 2 {
			return nil, errAbbrevUnknown
		}
		if err := binary.Read(r, binary.BigEndian, &fanout); err != nil {
			return nil, err
		}
	} else {
		fanout[0] = binary.BigEndian.Uint32(head[:4])
		fanout[1] = binary.BigEndian.Uint32(head[4:])
		if err := binary.Read(r, binary.BigEndian, fanout[2:]); err != nil {
			return nil, err
		}
	}
	n := int(fanout[255])
	ids := make([]plumbing.Hash, n)
	for i := 0; i < n; i++ {
		if !v2 {
			if _, err := io.ReadFull(r, make([]byte, 4)); err != nil { // offset
				return nil, err
			}
		}
		if _, err := io.ReadFull(r, ids[i][:]); err != nil {
			return nil, err
		}
	}
	return ids, nil
}
