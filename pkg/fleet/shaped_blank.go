// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

// NewBlankShapedApp makes a shaped app from nothing, the way a document
// editor makes a new document: a managed checkout whose base revision is an
// empty page plus the contract and smoke check every shaped app carries, then
// the same registry record `app add --type shaped` writes. There is no
// project, build or deploy step: the user's first wish on the blank page is
// an ordinary shaping candidate, verified against this base and kept as the
// user's own version.
//
// The catalog's own record check (with the console's WithAppValidate:
// reserved mounts, builtin names) runs before anything is written, so a
// refused name leaves nothing behind. A name that is already registered or
// already has a managed checkout is refused: `app new` never overwrites an
// app.
func NewBlankShapedApp(c *Catalog, a App) (App, error) {
	a.Name = strings.TrimSpace(a.Name)
	if err := validName(a.Name); err != nil {
		return App{}, err
	}
	if _, ok := c.App(a.Name); ok {
		return App{}, fmt.Errorf("fleet: app %q already exists", a.Name)
	}
	dest, err := filepath.Abs(shapedManagedDir(a.Name))
	if err != nil {
		return App{}, fmt.Errorf("fleet: app %q: managed source: %w", a.Name, err)
	}
	if _, err := os.Stat(dest); err == nil {
		return App{}, fmt.Errorf("fleet: app %q: a managed checkout already exists at %s", a.Name, dest)
	} else if !os.IsNotExist(err) {
		return App{}, fmt.Errorf("fleet: app %q: managed source: %w", a.Name, err)
	}

	a.Type = AppTypeShaped
	a.Port = 0
	a.Source = dest
	if strings.TrimSpace(a.Label) == "" {
		a.Label = a.Name
	}
	if err := c.checkApp(a); err != nil {
		return App{}, err
	}

	label := a.Label
	if _, err := SeedShapedSource(dest, "New app "+a.Name+": blank page", func(root string) error {
		return writeBlankApp(root, label)
	}); err != nil {
		return App{}, err
	}
	if err := c.SaveApp(a); err != nil {
		_ = os.RemoveAll(dest)
		return App{}, err
	}
	saved, _ := c.App(a.Name)
	return saved, nil
}

// writeBlankApp lays out the blank base tree: one empty page that says how
// to begin, a contract with a read-only effect cap (the app's own data
// directory stays writable through the data boundary, as for every shaped
// app), and a smoke check that keeps the page served.
func writeBlankApp(root, label string) error {
	files := map[string]string{
		"public/index.html": strings.ReplaceAll(blankPage, "{{LABEL}}", html.EscapeString(label)),
		"contract.bsh":      blankContract,
		"checks/smoke.bsh":  blankSmoke,
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

const blankPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{LABEL}}</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; min-height: 100svh; display: grid; place-items: center;
    font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; }
  main { max-width: 32rem; padding: 2rem 1rem; text-align: center; }
  h1 { margin: 0 0 .5rem; font-size: 1.6rem; }
  p { margin: 0; opacity: .7; }
</style>
</head>
<body>
<main data-bashy-blank>
  <h1>{{LABEL}}</h1>
  <p>This app is empty. Use the Genie button to describe what it should do,
  the way you would start writing in a new document.</p>
</main>
</body>
</html>
`

const blankContract = `# A new app, created blank with "bashy app new". It grows by shaping.
@effects("read")
`

const blankSmoke = `set -eu
test -f public/index.html
`
