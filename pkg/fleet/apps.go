package fleet

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
)

// A registered app is the web-surface counterpart of a registered command
// (`bashy app add`): a local web server the OPERATOR taught the Apps console,
// tiled and proxied beside the panels bashy ships. The record is the
// dhnt-app-meta-v1 contract as YAML, minus the two facts a registered app
// cannot choose: its mode (always proxy — a third party owns its lifecycle)
// and its mount (always its name).
//
// Like commands it has NO embedded ring: bashy ships the mechanism, never a
// catalog of apps (rod, not fish). The console reads the ring as DATA — it
// never execs anything to discover a registered app.

// App is one registered app record: `name:` + `kind: app`.
type App struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string   `yaml:"name" json:"name" doc:"app name; also its mount (/<name>/) — one path segment"`
	Kind            string   `yaml:"kind" json:"kind" doc:"always app"`
	Label           string   `yaml:"label,omitempty" json:"label,omitempty" doc:"tile label; default the name"`
	Icon            string   `yaml:"icon,omitempty" json:"icon,omitempty" doc:"SVG path data on a 24 grid, or one emoji"`
	Tip             string   `yaml:"tip,omitempty" json:"tip,omitempty" doc:"tile tooltip"`
	Port            int      `yaml:"port" json:"port" doc:"loopback port the app listens on; the console proxies /<name>/ to it"`
	Start           []string `yaml:"start,omitempty" json:"start,omitempty" doc:"argv shown as the start hint when the app is down (never run by the console)"`
	Auth            string   `yaml:"auth,omitempty" json:"auth,omitempty" doc:"auth tier: system (default), public or custom"`
	LoginPath       string   `yaml:"login_path,omitempty" json:"login_path,omitempty" doc:"custom auth only: app-relative login path"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// AppValidator is the embedding console's check of an app record — reserved
// mounts, icon safety, the ports and tiers it accepts. INJECTED because the
// console reads this package, so it cannot be imported from here.
type AppValidator func(App) error

// WithAppValidate supplies the console's record check to `app add`/`set`.
func WithAppValidate(f AppValidator) Option { return func(c *Config) { c.appValidate = f } }

// ParseApp reads an app asset. name is the fallback identity.
func ParseApp(name string, body []byte, src assetring.Source) (App, error) {
	var a App
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil && !strings.Contains(err.Error(), "EOF") {
		return App{}, fmt.Errorf("fleet: app %q: %w", name, err)
	}
	if a.Name == "" {
		a.Name = name
	}
	if a.Kind == "" {
		a.Kind = KindApp
	}
	if src != nil {
		a.Ring = src.Ring()
	}
	return a, nil
}

// Validate is the structural check this package can make on its own.
func (a App) Validate() error {
	if err := validName(a.Name); err != nil {
		return err
	}
	if strings.ContainsAny(a.Name, " \t\r\n") {
		return fmt.Errorf("fleet: app name %q: no whitespace", a.Name)
	}
	if a.Kind != "" && a.Kind != KindApp {
		return fmt.Errorf("fleet: app %q: kind %q, want %s", a.Name, a.Kind, KindApp)
	}
	if a.Port < 1 || a.Port > 65535 {
		return fmt.Errorf("fleet: app %q: port %d out of range (--port N)", a.Name, a.Port)
	}
	return nil
}

// Apps returns every registered app, name-sorted, across the shared and
// local rings. Entries that fail to parse are reported, never hidden.
func (c *Catalog) Apps() ([]App, []error) {
	var errs []error
	cat := &assetring.Catalog[App]{
		Sources: c.sources(dirApps),
		Parse: func(n string, b []byte, s assetring.Source) App {
			a, err := ParseApp(n, b, s)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return App{Name: n, Kind: KindApp, Ring: s.Ring()}
			}
			return a
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]App, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}

// App resolves a registered app by name.
func (c *Catalog) App(name string) (App, bool) {
	apps, _ := c.Apps()
	for _, a := range apps {
		if a.Name == name {
			return a, true
		}
	}
	return App{}, false
}

// SaveApp validates and writes an app into the local store.
func (c *Catalog) SaveApp(a App) error {
	a.Kind = KindApp
	if err := a.Validate(); err != nil {
		return err
	}
	if c.cfg.appValidate != nil {
		if err := c.cfg.appValidate(a); err != nil {
			return fmt.Errorf("fleet: app %q: %w", a.Name, err)
		}
	}
	data, err := Marshal(a)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirApps), a.Name, data)
}

// MaterializeApp copies an app into the local store if needed and returns
// the path an editor should open.
func (c *Catalog) MaterializeApp(name string) (string, error) {
	a, ok := c.App(name)
	if !ok {
		return "", fmt.Errorf("fleet: no registered app %q", name)
	}
	if a.Ring != ringLocal() {
		if err := c.SaveApp(a); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirApps), a.Name)
}

// RemoveApp deletes an app from the local store.
func (c *Catalog) RemoveApp(name string) error {
	return removeEntry(c.nounDir(dirApps), dirApps, name)
}
