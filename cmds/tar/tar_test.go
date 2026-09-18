package tarcmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/tool"
)

// runTool is the canonical test harness shape for cmds packages
// (output captured AFTER Run).
func runTool(t *testing.T, dir string, stdin io.Reader, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   dir,
		Stdio: tool.Stdio{In: stdin, Out: &out, Err: &errb},
	}
	code = cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

// makeTree builds the source tree used by the roundtrip tests and
// returns the fixed mtime stamped on src/a.txt.
func makeTree(t *testing.T, dir string) time.Time {
	t.Helper()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	// WriteFile perms are umask-filtered; pin the mode explicitly.
	if err := os.Chmod(filepath.Join(src, "a.txt"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2024, 3, 14, 15, 9, 26, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(src, "a.txt"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
			t.Fatal(err)
		}
	}
	return mtime
}

func TestCreateListExtractRoundtrip(t *testing.T) {
	dir := t.TempDir()
	mtime := makeTree(t, dir)

	_, errb, code := runTool(t, dir, nil, "-cf", "a.tar", "src")
	if code != 0 {
		t.Fatalf("create: code=%d err=%q", code, errb)
	}

	out, _, code := runTool(t, dir, nil, "-tf", "a.tar")
	if code != 0 {
		t.Fatalf("list: code=%d", code)
	}
	for _, want := range []string{"src/\n", "src/a.txt\n", "src/sub/\n", "src/sub/b.txt\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q in:\n%s", want, out)
		}
	}
	if runtime.GOOS != "windows" && !strings.Contains(out, "src/link\n") {
		t.Errorf("list missing symlink entry:\n%s", out)
	}

	dest := filepath.Join(dir, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	_, errb, code = runTool(t, dir, nil, "-xf", "a.tar", "-C", "dest")
	if code != 0 {
		t.Fatalf("extract: code=%d err=%q", code, errb)
	}
	got, err := os.ReadFile(filepath.Join(dest, "src", "a.txt"))
	if err != nil || string(got) != "alpha\n" {
		t.Fatalf("extracted a.txt = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dest, "src", "sub", "b.txt"))
	if err != nil || string(got) != "beta\n" {
		t.Fatalf("extracted b.txt = %q, %v", got, err)
	}
	fi, err := os.Stat(filepath.Join(dest, "src", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime not preserved: got %v want %v", fi.ModTime(), mtime)
	}
	if runtime.GOOS != "windows" {
		if perm := fi.Mode().Perm(); perm != 0o640 {
			t.Errorf("mode not preserved: got %o want 640", perm)
		}
		target, err := os.Readlink(filepath.Join(dest, "src", "link"))
		if err != nil || target != "a.txt" {
			t.Errorf("symlink not preserved: %q, %v", target, err)
		}
	}
}

func TestGzipRoundtripAndOldStyle(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)

	// old option style: bundled letters without dash, f consumes operand
	_, errb, code := runTool(t, dir, nil, "czf", "a.tgz", "src")
	if code != 0 {
		t.Fatalf("old-style czf: code=%d err=%q", code, errb)
	}
	out, _, code := runTool(t, dir, nil, "tzf", "a.tgz")
	if code != 0 || !strings.Contains(out, "src/a.txt\n") {
		t.Fatalf("old-style tzf: code=%d out=%q", code, out)
	}

	// gzip auto-detection on read (no -z)
	out, _, code = runTool(t, dir, nil, "-tf", "a.tgz")
	if code != 0 || !strings.Contains(out, "src/a.txt\n") {
		t.Fatalf("auto-detect gzip: code=%d out=%q", code, out)
	}

	dest := filepath.Join(dir, "dest")
	_, errb, code = runTool(t, dir, nil, "-xzf", "a.tgz", "-C", "dest")
	if code == 0 {
		t.Fatalf("extract to missing -C dir should fail")
	}
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	_, errb, code = runTool(t, dir, nil, "-xzf", "a.tgz", "-C", "dest")
	if code != 0 {
		t.Fatalf("extract -z: code=%d err=%q", code, errb)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "src", "sub", "b.txt")); err != nil || string(got) != "beta\n" {
		t.Fatalf("extracted b.txt = %q, %v", got, err)
	}
}

func TestListFromStdinAndCreateToStdout(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)

	// create to stdout (-f -); verbose goes to stderr then
	out, errb, code := runTool(t, dir, nil, "-cvf", "-", "src")
	if code != 0 {
		t.Fatalf("create to stdout: code=%d", code)
	}
	if !strings.Contains(errb, "src/a.txt\n") {
		t.Errorf("-cv with stdout archive should list on stderr, got %q", errb)
	}

	// list the bytes back via stdin
	lst, _, code := runTool(t, dir, strings.NewReader(out), "-tf", "-")
	if code != 0 || !strings.Contains(lst, "src/a.txt\n") {
		t.Fatalf("list from stdin: code=%d out=%q", code, lst)
	}
}

func archiveNames(t *testing.T, data []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("invalid tar stream: %v", err)
		}
		names = append(names, hdr.Name)
	}
}

func archiveNameSet(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	for _, name := range archiveNames(t, data) {
		names[name] = true
	}
	return names
}

func TestCreateExcludePatternsPruneDirectoriesAndKeepStdoutArchiveValid(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"tree/keep.txt":             "keep",
		"tree/nested/remove.tmp":    "tmp",
		"tree/pruned/also-keep.txt": "must be pruned",
	} {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Exercise the reported -cf form and old-style cf. In both cases f must
	// consume - as the archive name before the long options are processed.
	for _, prefix := range [][]string{{"-cf", "-"}, {"cf", "-"}} {
		args := append(prefix, "--exclude=tree/pruned", "--exclude", "*.tmp", "tree")
		out, errb, code := runTool(t, dir, nil, args...)
		if code != 0 {
			t.Fatalf("create with excludes %v: code=%d err=%q", prefix, code, errb)
		}
		names := archiveNames(t, []byte(out))
		got := strings.Join(names, "\n")
		if !strings.Contains(got, "tree/\n") || !strings.Contains(got, "tree/keep.txt") {
			t.Errorf("wanted retained members for %v, got %q", prefix, got)
		}
		for _, unwanted := range []string{"tree/nested/remove.tmp", "tree/pruned/", "tree/pruned/also-keep.txt"} {
			if strings.Contains(got, unwanted) {
				t.Errorf("excluded member %q present for %v in %q", unwanted, prefix, got)
			}
		}
	}
}

func writeFiles(t *testing.T, dir string, files ...string) {
	t.Helper()
	for _, name := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCreateExcludeGNUDefaultMatching(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"top/foo", "top/sub/foo", "top/foobar", "top/sub/keep",
		"top/prune/deep/hidden", "top/file1.txt", "top/filea.txt", "top/fileb.txt", "top/filec.txt",
	)

	tests := []struct {
		name     string
		patterns []string
		present  []string
		absent   []string
	}{
		{
			name:     "unanchored basename",
			patterns: []string{"foo"},
			present:  []string{"top/foobar"},
			absent:   []string{"top/foo", "top/sub/foo"},
		},
		{
			name:     "unanchored path suffix",
			patterns: []string{"sub/foo"},
			present:  []string{"top/foo"},
			absent:   []string{"top/sub/foo"},
		},
		{
			name:     "directory pruning",
			patterns: []string{"prune"},
			present:  []string{"top/sub/keep"},
			absent:   []string{"top/prune/", "top/prune/deep/hidden"},
		},
		{
			name:     "GNU bracket negation",
			patterns: []string{"file[!b].txt"},
			present:  []string{"top/fileb.txt"},
			absent:   []string{"top/filea.txt", "top/filec.txt"},
		},
		{
			name:     "POSIX named bracket class",
			patterns: []string{"file[[:digit:]].txt"},
			present:  []string{"top/foo", "top/filea.txt", "top/fileb.txt", "top/filec.txt"},
			absent:   []string{"top/file1.txt"},
		},
		{
			name:     "wildcard crosses slash",
			patterns: []string{"sub*keep"},
			present:  []string{"top/foo"},
			absent:   []string{"top/sub/keep"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"-cf", "-"}
			for i, pattern := range tt.patterns {
				if i%2 == 0 {
					args = append(args, "--exclude="+pattern)
				} else {
					args = append(args, "--exclude", pattern)
				}
			}
			args = append(args, "top")
			out, errb, code := runTool(t, dir, nil, args...)
			if code != 0 {
				t.Fatalf("create: code=%d err=%q", code, errb)
			}
			names := archiveNameSet(t, []byte(out))
			for _, want := range tt.present {
				if !names[want] {
					t.Errorf("missing retained member %q in %v", want, names)
				}
			}
			for _, unwanted := range tt.absent {
				if names[unwanted] {
					t.Errorf("excluded member %q present in %v", unwanted, names)
				}
			}
		})
	}
}

func TestTarRejectsLongOptionAbbreviationsBeforeOrderedScan(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "tree/foo")
	for _, arg := range []string{"--excl=foo", "--excl", "--ex=foo"} {
		_, errb, code := runTool(t, dir, nil, "-cf", "-", arg, "tree")
		if code != 2 || !strings.Contains(errb, "not supported") || !strings.Contains(errb, "pure-Go") {
			t.Errorf("tar %s: code=%d err=%q", arg, code, errb)
		}
	}
}

func TestCreateExcludeMatchesDotOperandMemberNames(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "foo", "keep")
	out, errb, code := runTool(t, dir, nil, "-cf", "-", "--exclude=./foo", ".")
	if code != 0 {
		t.Fatalf("create: code=%d err=%q", code, errb)
	}
	names := archiveNameSet(t, []byte(out))
	if !names["./"] || names["./foo"] || !names["./keep"] {
		t.Fatalf("archive members = %v, want directory header and unexcluded ./keep only", names)
	}
}

func TestCreateExcludeRepeatedFormsAndArchiveDestinations(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "top/foo", "top/drop.tmp", "top/keep")

	for _, archive := range []string{"-", "named.tar"} {
		t.Run(archive, func(t *testing.T) {
			out, errb, code := runTool(t, dir, nil, "-cf", archive,
				"--exclude=foo", "--exclude", "*.tmp", "top")
			if code != 0 {
				t.Fatalf("create: code=%d err=%q", code, errb)
			}
			data := []byte(out)
			if archive != "-" {
				var err error
				data, err = os.ReadFile(filepath.Join(dir, archive))
				if err != nil {
					t.Fatal(err)
				}
			}
			names := archiveNameSet(t, data)
			if !names["top/keep"] || names["top/foo"] || names["top/drop.tmp"] {
				t.Fatalf("archive members = %v", names)
			}
		})
	}
}

func TestCreateExcludeIsPositionalForDashedAndOldStyle(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "a/foo", "a/keep", "b/a/foo", "b/keep")

	for _, prefix := range [][]string{{"-cf", "-"}, {"cf", "-"}} {
		args := append(append([]string(nil), prefix...), "a", "--exclude=a/foo", "b")
		out, errb, code := runTool(t, dir, nil, args...)
		if code != 0 {
			t.Fatalf("create %v: code=%d err=%q", prefix, code, errb)
		}
		names := archiveNameSet(t, []byte(out))
		if !names["a/foo"] {
			t.Errorf("later exclusion retroactively filtered a for %v: %v", prefix, names)
		}
		if names["b/a/foo"] {
			t.Errorf("cumulative exclusion did not filter b for %v: %v", prefix, names)
		}
		if !names["b/keep"] {
			t.Errorf("missing retained b member for %v: %v", prefix, names)
		}
	}
}

func TestCreateAllExcludedProducesValidArchive(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "only/file")
	for _, archive := range []string{"-", "empty.tar"} {
		out, errb, code := runTool(t, dir, nil, "-cf", archive, "--exclude=only", "only")
		if code != 0 {
			t.Fatalf("create %q: code=%d err=%q", archive, code, errb)
		}
		data := []byte(out)
		if archive != "-" {
			var err error
			data, err = os.ReadFile(filepath.Join(dir, archive))
			if err != nil {
				t.Fatal(err)
			}
		}
		if names := archiveNames(t, data); len(names) != 0 {
			t.Fatalf("all-excluded archive %q contains %q", archive, names)
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("forced write failure") }

func TestCreateReportsFailingWriter(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "file")
	var errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx: context.Background(),
		Dir: dir,
		Stdio: tool.Stdio{
			In:  strings.NewReader(""),
			Out: failingWriter{},
			Err: &errb,
		},
	}
	if code := cmd.Run(rc, []string{"-cf", "-", "file"}); code != 1 {
		t.Fatalf("code=%d err=%q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "forced write failure") {
		t.Fatalf("missing writer error: %q", errb.String())
	}
}

func TestExcludeOnlySupportedForCreateAndUnsupportedVariantsFailLoudly(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)
	if _, errb, code := runTool(t, dir, nil, "-tf", "a.tar", "--exclude=*.tmp"); code != 2 || !strings.Contains(errb, "only supported with -c") {
		t.Errorf("exclude with list: code=%d err=%q", code, errb)
	}
	if _, errb, code := runTool(t, dir, nil, "-cf", "a.tar", "--exclude-from=patterns", "src"); code != 2 || !strings.Contains(errb, "exclude-from") || !strings.Contains(errb, "pure-Go") {
		t.Errorf("unsupported exclusion variant: code=%d err=%q", code, errb)
	}
}

func TestVerboseListing(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)
	if _, _, code := runTool(t, dir, nil, "-cf", "a.tar", "src"); code != 0 {
		t.Fatal("create failed")
	}
	out, _, code := runTool(t, dir, nil, "-tvf", "a.tar")
	if code != 0 {
		t.Fatalf("tv: code=%d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var aline string
	for _, l := range lines {
		if strings.HasSuffix(l, " src/a.txt") {
			aline = l
		}
		if strings.HasPrefix(l, "d") && !strings.Contains(l, "drwx") {
			t.Errorf("dir line lacks d-perms: %q", l)
		}
	}
	if aline == "" {
		t.Fatalf("no -tv line for src/a.txt in:\n%s", out)
	}
	// permissions owner/group size date name
	if runtime.GOOS != "windows" && !strings.HasPrefix(aline, "-rw-r-----") {
		t.Errorf("perm string: %q", aline)
	}
	if !strings.Contains(aline, " 6 ") { // len("alpha\n")
		t.Errorf("size missing in %q", aline)
	}
	if !strings.Contains(aline, "/") || !strings.Contains(aline, "-03-14 ") && !strings.Contains(aline, "2024-") {
		t.Errorf("date/owner missing in %q", aline)
	}
}

func TestStripComponents(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)
	if _, _, code := runTool(t, dir, nil, "-cf", "a.tar", "src"); code != 0 {
		t.Fatal("create failed")
	}
	dest := filepath.Join(dir, "flat")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	_, errb, code := runTool(t, dir, nil, "-xf", "a.tar", "-C", "flat", "--strip-components=1")
	if code != 0 {
		t.Fatalf("strip: code=%d err=%q", code, errb)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "a.txt")); err != nil || string(got) != "alpha\n" {
		t.Fatalf("stripped a.txt = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "src")); !os.IsNotExist(err) {
		t.Errorf("src/ should not exist after --strip-components=1")
	}
	// only valid with -x
	_, errb, code = runTool(t, dir, nil, "-tf", "a.tar", "--strip-components=1")
	if code != 2 || !strings.Contains(errb, "strip-components") {
		t.Errorf("strip with -t: code=%d err=%q", code, errb)
	}
}

func TestMemberSelection(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)
	if _, _, code := runTool(t, dir, nil, "-cf", "a.tar", "src"); code != 0 {
		t.Fatal("create failed")
	}
	out, _, code := runTool(t, dir, nil, "-tf", "a.tar", "src/a.txt")
	if code != 0 || strings.TrimSpace(out) != "src/a.txt" {
		t.Errorf("member select: code=%d out=%q", code, out)
	}
	// directory operand selects everything beneath
	out, _, code = runTool(t, dir, nil, "-tf", "a.tar", "src/sub")
	if code != 0 || !strings.Contains(out, "src/sub/b.txt\n") {
		t.Errorf("dir member select: code=%d out=%q", code, out)
	}
	_, errb, code := runTool(t, dir, nil, "-tf", "a.tar", "nope")
	if code != 1 || !strings.Contains(errb, "Not found in archive") {
		t.Errorf("missing member: code=%d err=%q", code, errb)
	}
}

// writeRawArchive writes a tar with arbitrary member names — used to
// craft hostile archives.
func writeRawArchive(t *testing.T, path string, names map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range names {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPathTraversalRefused(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRawArchive(t, filepath.Join(dir, "evil.tar"), map[string]string{
		"../evil.txt":      "pwned",
		"ok/../../zap.txt": "pwned",
		"safe.txt":         "fine",
	})
	_, errb, code := runTool(t, dir, nil, "-xf", "evil.tar", "-C", "dest")
	if code != 1 {
		t.Fatalf("traversal extract: code=%d err=%q", code, errb)
	}
	if !strings.Contains(errb, "refusing to extract") {
		t.Errorf("missing refusal diagnostic: %q", errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("evil.txt escaped the target directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "zap.txt")); !os.IsNotExist(err) {
		t.Fatalf("zap.txt escaped the target directory")
	}
	if got, err := os.ReadFile(filepath.Join(dest, "safe.txt")); err != nil || string(got) != "fine" {
		t.Fatalf("safe member should still extract: %q, %v", got, err)
	}
}

func TestAbsoluteMemberRefusedOnExtract(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRawArchive(t, filepath.Join(dir, "abs.tar"), map[string]string{
		"/abs.txt": "pwned",
	})
	_, errb, code := runTool(t, dir, nil, "-xf", "abs.tar", "-C", "dest")
	if code != 1 || !strings.Contains(errb, "absolute") {
		t.Errorf("absolute member: code=%d err=%q", code, errb)
	}
}

func TestLeadingSlashStrippedOnCreate(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("absolute-operand member naming differs with drive letters")
	}
	_, errb, code := runTool(t, dir, nil, "-cf", "a.tar", abs)
	if code != 0 || !strings.Contains(errb, "Removing leading '/'") {
		t.Fatalf("create abs: code=%d err=%q", code, errb)
	}
	out, _, _ := runTool(t, dir, nil, "-tf", "a.tar")
	if strings.HasPrefix(out, "/") {
		t.Errorf("member name kept leading slash: %q", out)
	}
}

func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-f", "a.tar"}, "You must specify one of"},
		{[]string{"-ctf", "a.tar"}, "may not specify more than one"},
		{[]string{"-c", "x"}, "no archive file specified"},
		{[]string{"-cf", "a.tar"}, "Cowardly refusing to create an empty archive"},
	}
	for _, c := range cases {
		_, errb, code := runTool(t, dir, nil, c.args...)
		if code != 2 || !strings.Contains(errb, c.want) {
			t.Errorf("tar %v: code=%d err=%q (want %q)", c.args, code, errb, c.want)
		}
	}
	// unknown flag: contract error
	_, errb, code := runTool(t, dir, nil, "--frobnicate")
	if code != 2 || !strings.Contains(errb, "frobnicate") || !strings.Contains(errb, "pure-Go") {
		t.Errorf("unknown flag: code=%d err=%q", code, errb)
	}
	// missing archive file
	_, errb, code = runTool(t, dir, nil, "-tf", "nope.tar")
	if code != 1 || !strings.Contains(errb, "Cannot open") {
		t.Errorf("missing archive: code=%d err=%q", code, errb)
	}
}

func TestHelpAndVersion(t *testing.T) {
	out, _, code := runTool(t, t.TempDir(), nil, "--help")
	if code != 0 || !strings.Contains(out, "Usage: tar") {
		t.Errorf("--help: code=%d out=%q", code, out)
	}
	out, _, code = runTool(t, t.TempDir(), nil, "--version")
	if code != 0 || !strings.Contains(out, "tar") {
		t.Errorf("--version: code=%d out=%q", code, out)
	}
}

func TestExtractVerbosePrintsNames(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)
	if _, _, code := runTool(t, dir, nil, "-cf", "a.tar", "src"); code != 0 {
		t.Fatal("create failed")
	}
	dest := filepath.Join(dir, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _, code := runTool(t, dir, nil, "-xvf", "a.tar", "-C", "dest")
	if code != 0 || !strings.Contains(out, "src/a.txt\n") {
		t.Errorf("-xv: code=%d out=%q", code, out)
	}
}

func TestNotGzipWithZ(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk.tgz"), []byte("not gzip at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errb, code := runTool(t, dir, nil, "-tzf", "junk.tgz")
	if code != 1 || !strings.Contains(errb, "not in gzip format") {
		t.Errorf("bad gzip: code=%d err=%q", code, errb)
	}
}

// sanity: the gzip stream a -z archive produces really is gzip
func TestCreateZProducesGzip(t *testing.T) {
	dir := t.TempDir()
	makeTree(t, dir)
	if _, _, code := runTool(t, dir, nil, "-czf", "a.tgz", "src"); code != 0 {
		t.Fatal("create -z failed")
	}
	f, err := os.Open(filepath.Join(dir, "a.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	if _, err := tr.Next(); err != nil {
		t.Fatalf("not a tar inside gzip: %v", err)
	}
}
