package fleet

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/atlas"
)

// shPath is an executable every unix test host has; the exec-mode fixtures
// point at it so the check exercises presence, not a particular program.
func shPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this host")
	}
	return p
}

func reservedFixture(name string) (string, bool) {
	switch name {
	case "ls", "rm", "cat":
		return "GNU coreutils applet", true
	case "set", "command", "cd":
		return "bash builtin", true
	case "weave":
		return "yoke command", true
	}
	return "", false
}

// The mechanism defaults what it knows for certain: exec spawns, download
// spawns + fetches, a script body declares nothing on the author's behalf.
func TestParseCommandDefaultsPerMode(t *testing.T) {
	cases := map[string]struct {
		body    string
		mode    string
		effects []string
		os      []string
	}{
		"exec":     {"exec: [/bin/echo, hi]\n", atlas.RegisteredExec, []string{"exec"}, atlas.OSes()},
		"download": {"download:\n  url: http://x/{version}\n  version: v1\n  sha256:\n    linux/amd64: " + strings.Repeat("a", 64) + "\n", atlas.RegisteredDownload, []string{"exec", "net"}, []string{"linux"}},
		"script":   {"script: echo hi\n", atlas.RegisteredScript, nil, atlas.OSes()},
	}
	for name, tc := range cases {
		c, err := ParseCommand(name, []byte(tc.body), nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Name != name || c.Kind != KindCommand || c.Mode() != tc.mode {
			t.Errorf("%s: name/kind/mode = %q/%q/%q", name, c.Name, c.Kind, c.Mode())
		}
		if !slices.Equal(c.Effects, tc.effects) {
			t.Errorf("%s: effects = %v, want %v", name, c.Effects, tc.effects)
		}
		if !slices.Equal(c.OS, tc.os) {
			t.Errorf("%s: os = %v, want %v", name, c.OS, tc.os)
		}
		if c.Group != atlas.GroupShellutils || c.Tier != atlas.TierUserland || c.Stage != atlas.StageCross || c.Shape != "result" {
			t.Errorf("%s: metadata defaults = %q %q %q %q", name, c.Group, c.Tier, c.Stage, c.Shape)
		}
	}
	if c, _ := ParseCommand("s", []byte("script: x\n"), nil); c.Dialect != DialectBashPP {
		t.Errorf("script dialect default = %q", c.Dialect)
	}
	if _, err := ParseCommand("bad", []byte("nope: 1\n"), nil); err == nil {
		t.Error("unknown top-level key must be refused (strict decode)")
	}
}

func TestValidateCommandRefusals(t *testing.T) {
	digest := strings.Repeat("b", 64)
	cases := []struct {
		name string
		rec  Command
		want string
	}{
		{"no mode", Command{Name: "x"}, "exactly one of"},
		{"two modes", Command{Name: "x", Exec: []string{"a"}, Script: "b"}, "exactly one of"},
		{"script no effects", Command{Name: "x", Script: "true"}, "effects is empty"},
		{"bad effect", Command{Name: "x", Exec: []string{"a"}, Effects: []string{"launch"}}, "effects value"},
		{"bad cap", Command{Name: "x", Exec: []string{"a"}, Caps: []string{"magic"}}, "caps value"},
		{"two args tokens", Command{Name: "x", Exec: []string{"a", ArgsToken, ArgsToken}}, "at most once"},
		{"crud word", Command{Name: "rm", Exec: []string{"a"}}, "keeps for itself"},
		{"crud word alias", Command{Name: "x", Aliases: []string{"schema"}, Exec: []string{"a"}}, "keeps for itself"},
		{"reserved builtin", Command{Name: "cat2", Aliases: []string{"cd"}, Exec: []string{"a"}}, "already resolves to bash builtin"},
		{"reserved applet", Command{Name: "ls", Exec: []string{"a"}}, "GNU coreutils applet"},
		{"download no digest", Command{Name: "x", Download: &CommandDownload{URL: "http://x/{version}", Version: "v1"}}, "sha256 is empty"},
		{"download latest", Command{Name: "x", Download: &CommandDownload{URL: "http://x", Version: "latest", SHA256: map[string]string{"linux/amd64": digest}}}, "never latest"},
		{"download both sources", Command{Name: "x", Download: &CommandDownload{URL: "http://x", GitHub: "a/b", Version: "v1", SHA256: map[string]string{"linux/amd64": digest}}}, "exactly one of github"},
		{"download bad key", Command{Name: "x", Download: &CommandDownload{URL: "http://x", Version: "v1", SHA256: map[string]string{"plan9": digest}}}, "not goos/goarch"},
		{"download bad hex", Command{Name: "x", Download: &CommandDownload{URL: "http://x", Version: "v1", SHA256: map[string]string{"linux/amd64": "zz"}}}, "not a 64-hex"},
		{"bad dialect", Command{Name: "x", Script: "true", Dialect: "zsh", Effects: []string{"pure"}}, "dialect must be"},
		{"bad group", Command{Name: "x", Exec: []string{"a"}, Group: "shell"}, "group"},
		{"bad env", Command{Name: "x", Exec: []string{"a"}, Env: []string{"novalue"}}, "not KEY=VALUE"},
		{"relative cwd", Command{Name: "x", Exec: []string{"a"}, Cwd: "rel"}, "must be absolute"},
		{"path in name", Command{Name: "a/b", Exec: []string{"a"}}, "path separator"},
	}
	for _, tc := range cases {
		rec := tc.rec
		rec.applyDefaults()
		err := rec.Validate(reservedFixture)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
	ok := Command{Name: "gl", Script: "git log", Effects: []string{"read"}}
	ok.applyDefaults()
	if err := ok.Validate(reservedFixture); err != nil {
		t.Errorf("valid script refused: %v", err)
	}
}

func TestCommandArgv(t *testing.T) {
	ex := Command{Name: "jqs", Exec: []string{"/usr/bin/jq", "-S", ArgsToken, "--tail"}}
	if got := ex.Argv("bashy", "", []string{"a", "b"}); !slices.Equal(got, []string{"/usr/bin/jq", "-S", "a", "b", "--tail"}) {
		t.Errorf("splice = %v", got)
	}
	ap := Command{Name: "e", Exec: []string{"/bin/echo", "x"}}
	if got := ap.Argv("bashy", "", []string{"y"}); !slices.Equal(got, []string{"/bin/echo", "x", "y"}) {
		t.Errorf("append = %v", got)
	}
	sc := Command{Name: "gl", Script: "git log \"$@\"", Dialect: DialectBash}
	if got := sc.Argv("/opt/bashy", "", []string{"-n", "3"}); !slices.Equal(got, []string{"/opt/bashy", "--no-bashpp", "-c", "git log \"$@\"", "gl", "-n", "3"}) {
		t.Errorf("script = %v", got)
	}
	sc.Dialect = DialectBashPP
	if got := sc.Argv("/opt/bashy", "", nil); got[1] != "-c" {
		t.Errorf("bashpp script must pass no dialect flag: %v", got)
	}
	dl := Command{Name: "w", Download: &CommandDownload{}}
	if got := dl.Argv("bashy", "/cache/w", []string{"z"}); !slices.Equal(got, []string{"/cache/w", "z"}) {
		t.Errorf("download = %v", got)
	}
}

func TestCommandsBashPPReservedNames(t *testing.T) {
	for _, word := range []string{"var", "const", "func", "import", "package", "goto"} {
		t.Run(word, func(t *testing.T) {
			root := t.TempDir()
			opts := []Option{WithRoot(root)}
			cat := New(opts...)
			if err := cat.SaveCommand(Command{Name: "safe", Exec: []string{"external-program"}}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "commands", "safe.yaml")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"add", word, "--set", "exec.0=external-program"},
				{"add", "other", "--set", "exec.0=external-program", "--set", "aliases.0=" + word},
				{"set", "safe", "--set", "name=" + word},
				{"set", "safe", "--set", "aliases.0=" + word},
				{"set", "safe", "--add-alias", word},
			} {
				_, err := runCmd(t, NewCommandsCmd(opts...), args...)
				if err == nil {
					t.Fatalf("%v accepted reserved name", args)
				}
				for _, want := range []string{"reserved by Bash++", "rename the registered command or alias", "external program", "command " + word} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("%v: error %q lacks %q", args, err, want)
					}
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatalf("rejected update changed existing record: %v", err)
			}
			for _, name := range []string{word, "other"} {
				if _, err := os.Stat(filepath.Join(root, "commands", name+".yaml")); !os.IsNotExist(err) {
					t.Errorf("rejected write left %s: %v", name, err)
				}
			}
			// Records created by an older binary or supplied by a ring still
			// load, but verification must identify their invalid name/alias.
			for _, rec := range []Command{
				{Name: word, Exec: []string{"external-program"}},
				{Name: "old", Aliases: []string{word}, Exec: []string{"external-program"}},
			} {
				data, err := Marshal(rec)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "commands", rec.Name+".yaml"), data, 0o644); err != nil {
					t.Fatal(err)
				}
				if chk := cat.VerifyCommand(context.Background(), rec.Name); chk.OK || !strings.Contains(chk.Reason, "reserved by Bash++") {
					t.Errorf("old record verify = %+v", chk)
				}
				if _, err := runCmd(t, NewCommandsCmd(opts...), "verify", rec.Name); err == nil {
					t.Errorf("CLI verify accepted old record %s", rec.Name)
				}
			}
		})
	}
}

func TestCommandGoAndTypeUseShippedNamePolicy(t *testing.T) {
	for _, word := range []string{"go", "type"} {
		for _, alias := range []bool{false, true} {
			rec := Command{Name: word, Exec: []string{"external-program"}}
			if alias {
				rec.Name, rec.Aliases = "safe", []string{word}
			}
			rec.applyDefaults()
			if err := rec.Validate(nil); err != nil {
				t.Errorf("%s (alias %v) must not be a language-keyword ban: %v", word, alias, err)
			}
			reserved := func(name string) (string, bool) { return "shipped command", name == word }
			if err := rec.Validate(reserved); err == nil || !strings.Contains(err.Error(), "already resolves to shipped command") {
				t.Errorf("%s (alias %v) lost shipped protection: %v", word, alias, err)
			}
		}
	}
}

// A download record without a digest for THIS platform is refused before any
// network is touched.
func TestCommandEnsureRefusesWithoutPlatformDigest(t *testing.T) {
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	rec := Command{Name: "w", Download: &CommandDownload{URL: "http://127.0.0.1:1/{version}", Version: "v1", SHA256: map[string]string{"plan9/mips": strings.Repeat("c", 64)}}}
	if _, err := rec.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "no sha256 for") {
		t.Fatalf("err = %v", err)
	}
}

// The CRUD tree: add from --set on an empty record, show --field, set,
// schema, unknown path refusal, rm; embedded ring absent; shared ring is
// read-only and materializes on set.
func TestCommandsCRUD(t *testing.T) {
	root := t.TempDir()
	opts := []Option{WithRoot(root), WithReservedNames(reservedFixture)}
	tree := func() *cobra.Command { return NewCommandsCmd(opts...) }

	if out, err := runCmd(t, tree(), "add", "gl", "--set", "script=git log --oneline", "--set", "effects.0=read", "--set", "synopsis=compact log"); err != nil || !strings.Contains(out, "gl (script: read)") {
		t.Fatalf("add: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(root, "commands", "gl.yaml")); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, tree(), "show", "gl", "--field", "synopsis"); err != nil || strings.TrimSpace(out) != "compact log" {
		t.Errorf("show --field: %v %q", err, out)
	}
	if out, err := runCmd(t, tree(), "list"); err != nil || !strings.Contains(out, "gl") || !strings.Contains(out, "script") {
		t.Errorf("list: %v %q", err, out)
	}
	if out, err := runCmd(t, tree(), "set", "gl", "--set", "synopsis=changed"); err != nil || strings.TrimSpace(out) != "gl" {
		t.Errorf("set: %v %q", err, out)
	}
	if out, _ := runCmd(t, tree(), "show", "gl", "--field", "synopsis"); strings.TrimSpace(out) != "changed" {
		t.Errorf("set did not land: %q", out)
	}
	out, err := runCmd(t, tree(), "set", "gl", "--set", "nope=1")
	if err == nil || !strings.Contains(out, "script") || !strings.Contains(out, "download.sha256.<key>") {
		t.Errorf("unknown path must print the schema: %v %q", err, out)
	}
	if out, err := runCmd(t, tree(), "schema"); err != nil || !strings.Contains(out, "exec.<index>") || !strings.Contains(out, "download.sha256.<key>") || !strings.Contains(out, "effects.<index>") {
		t.Errorf("schema: %v %q", err, out)
	}
	// Collision filter: builtins/applets/verbs and the CRUD words, no --force.
	for _, bad := range []string{"ls", "set", "weave", "add", "command"} {
		if _, err := runCmd(t, tree(), "add", bad, "--set", "exec.0=/bin/true"); err == nil {
			t.Errorf("add %s must be refused", bad)
		}
		if _, err := os.Stat(filepath.Join(root, "commands", bad+".yaml")); err == nil {
			t.Errorf("add %s wrote a file", bad)
		}
	}
	// Script without effects is refused, naming the path.
	if out, err := runCmd(t, tree(), "add", "sc", "--set", "script=true"); err == nil || !strings.Contains(err.Error(), "effects.0=") {
		t.Errorf("script without effects: %v %q", err, out)
	}
	// Download without a digest is refused before any network.
	if _, err := runCmd(t, tree(), "add", "dl", "--set", "download.url=http://127.0.0.1:1/{version}", "--set", "download.version=v1"); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("download without digest: %v", err)
	}
	if out, err := runCmd(t, tree(), "rm", "gl"); err != nil || !strings.Contains(out, "removed command gl") {
		t.Errorf("rm: %v %q", err, out)
	}
	if _, err := runCmd(t, tree(), "rm", "gl"); err == nil {
		t.Error("second rm must fail (not in the local store)")
	}
}

func TestCommandsSharedRingIsReadOnlyAndMaterializes(t *testing.T) {
	root := t.TempDir()
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "org.yaml"), []byte("name: org\nkind: command\nexec: [/bin/true]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := []Option{WithRoot(root), WithSource(dirCommands, assetring.FileDir(shared, assetring.RingShared, ext))}
	cat := New(opts...)
	r, ok := cat.Command("org")
	if !ok || r.Ring != assetring.RingShared {
		t.Fatalf("shared entry not read: %v %v", ok, r.Ring)
	}
	if _, err := runCmd(t, NewCommandsCmd(opts...), "rm", "org"); err == nil {
		t.Error("rm of a shared entry must be refused")
	}
	out, err := runCmd(t, NewCommandsCmd(opts...), "set", "org", "--set", "hidden=true")
	if err != nil || !strings.Contains(out, "copied org from the shared ring") {
		t.Errorf("set on shared: %v %q", err, out)
	}
	if r, _ := cat.Command("org"); r.Ring != assetring.RingLocal || !r.Hidden {
		t.Errorf("materialized copy = %+v", r)
	}
	// No embedded ring: an empty root lists nothing.
	if cmds, errs := New(WithRoot(t.TempDir())).Commands(); len(cmds) != 0 || len(errs) != 0 {
		t.Errorf("empty ring = %v %v", cmds, errs)
	}
}

// A ring entry a newer bashy has since claimed is reported, never resolved.
func TestCommandShadowsAndVerify(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "commands", "cat.yaml"), []byte("name: cat\nkind: command\nexec: [/bin/true]\n"), 0o644); err != nil {
		_ = os.MkdirAll(filepath.Join(root, "commands"), 0o755)
		if err := os.WriteFile(filepath.Join(root, "commands", "cat.yaml"), []byte("name: cat\nkind: command\nexec: [/bin/true]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cat := New(WithRoot(root), WithReservedNames(reservedFixture))
	if got := cat.CommandShadows(); got["cat"] != "GNU coreutils applet" {
		t.Errorf("shadows = %v", got)
	}
	chk := cat.VerifyCommand(context.Background(), "cat")
	if chk.OK || !strings.Contains(chk.Reason, "shadowed") {
		t.Errorf("verify = %+v", chk)
	}
	if err := cat.SaveCommand(Command{Name: "t", Exec: []string{shPath(t)}}); err != nil {
		t.Fatal(err)
	}
	if chk := cat.VerifyCommand(context.Background(), "t"); !chk.OK {
		t.Errorf("exec verify = %+v", chk)
	}
	if err := cat.SaveCommand(Command{Name: "missing", Exec: []string{"/nonexistent/prog"}}); err != nil {
		t.Fatal(err)
	}
	if chk := cat.VerifyCommand(context.Background(), "missing"); chk.OK || !strings.Contains(chk.Reason, "not installed") {
		t.Errorf("missing exec verify = %+v", chk)
	}
	probed := New(WithRoot(root), WithCommandProbe(func(c Command) (string, bool) { return "line 1: bad", false }))
	if err := probed.SaveCommand(Command{Name: "sc", Script: "if", Effects: []string{"pure"}}); err != nil {
		t.Fatal(err)
	}
	if chk := probed.VerifyCommand(context.Background(), "sc"); chk.OK || !strings.Contains(chk.Reason, "line 1: bad") {
		t.Errorf("probe verify = %+v", chk)
	}
	if chk := cat.VerifyCommand(context.Background(), "sc"); !chk.OK || chk.Warn == "" {
		t.Errorf("unprobed script verify must warn: %+v", chk)
	}
}
