package fleet

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
)

// A service unit is the declaration `bashy service` owns and `bashy
// supervisord` keeps running (docs/bashy-service-design.md). It is the
// seventh extension kind beside resource / command / model / tool / agent /
// skill, and every daemon bashy knows about — built-in, internal or third
// party — is one of these records. The record is DATA: no Go code anywhere
// selects behaviour by a unit's name. The provider field picks the runtime
// that realizes it; everything else (ports, health, restart, secrets,
// exposure, desired state) is read the same way for every unit.
//
// Unlike commands and apps, services DO carry an embedded ring: the built-in
// units are compiled-in seeds (baseline/services), never copied into the
// local store. An operator overrides one with `bashy service set`, which
// writes a sparse local overlay, and `rm` of that overlay restores the seed.

// Service providers — how a unit is realized. The set is closed: a new
// daemon is a new record, never a new provider.
const (
	// ServiceProviderProcess runs exec as a foreground child the monitor owns.
	ServiceProviderProcess = "process"
	// ServiceProviderInternal applies the declaration to a capability
	// registered in-process by its owning subsystem (no child, no pid).
	ServiceProviderInternal = "internal"
)

// Restart policies.
const (
	RestartAlways    = "always"
	RestartOnFailure = "on-failure"
	RestartNo        = "no"
)

// ServiceOwnerOutpost names the outpost daemon as the process hosting an
// internal capability. Any other owner is the name of another unit.
const ServiceOwnerOutpost = "outpost"

// Service is one unit record: `name:` + `kind: service`.
type Service struct {
	RecordLifecycle `yaml:",inline" schema:"-"`
	Name            string `yaml:"name" json:"name" doc:"unit name; one path segment"`
	Kind            string `yaml:"kind" json:"kind" doc:"always service"`
	Description     string `yaml:"description,omitempty" json:"description,omitempty" doc:"one line: what the unit serves"`
	Provider        string `yaml:"provider,omitempty" json:"provider,omitempty" doc:"process (default: a foreground child) | internal (an in-process capability)"`

	// process provider
	Exec     []string         `yaml:"exec,omitempty" json:"exec,omitempty" doc:"foreground argv; {port} {port.NAME} {bind} {bind.NAME} {settings.KEY} {download} are expanded"`
	Download *CommandDownload `yaml:"download,omitempty" json:"download,omitempty" doc:"pinned release binmgr provisions; {download} in exec names its path"`
	Workdir  string           `yaml:"workdir,omitempty" json:"workdir,omitempty" doc:"working directory; ~ expands"`

	// internal provider
	Capability string `yaml:"capability,omitempty" json:"capability,omitempty" doc:"internal: registered capability id, e.g. outpost.files"`
	Owner      string `yaml:"owner,omitempty" json:"owner,omitempty" doc:"internal: the process hosting the capability — outpost, or another unit's name"`

	Env      map[string]string `yaml:"env,omitempty" json:"env,omitempty" doc:"environment for the unit"`
	Secrets  []ServiceSecret   `yaml:"secrets,omitempty" json:"secrets,omitempty" doc:"vault references injected as env; never an inline secret"`
	Settings map[string]string `yaml:"settings,omitempty" json:"settings,omitempty" doc:"provider-visible configuration; exec reads it as {settings.KEY}"`
	Ports    []ServicePort     `yaml:"ports,omitempty" json:"ports,omitempty" doc:"declared listeners; conflicts with other enabled units are refused"`
	Health   ServiceHealth     `yaml:"health,omitempty" json:"health,omitempty" doc:"one of http | tcp | exec; none = process alive"`
	Restart  ServiceRestart    `yaml:"restart,omitempty" json:"restart,omitempty" doc:"restart policy, backoff and start limit"`
	Stop     ServiceStop       `yaml:"stop,omitempty" json:"stop,omitempty" doc:"stop signal and grace period"`
	Requires []string          `yaml:"requires,omitempty" json:"requires,omitempty" doc:"units that must be active first (Requires+After)"`
	Expose   ServiceExpose     `yaml:"expose,omitempty" json:"expose,omitempty" doc:"app tunnel and mesh exposure, withdrawn when the unit is not active"`
	Upgrade  string            `yaml:"upgrade,omitempty" json:"upgrade,omitempty" doc:"after a bashy upgrade: restart (default) | keep"`
	Enabled  bool              `yaml:"enabled" json:"enabled" doc:"desired state: the monitor keeps an enabled unit up"`

	Ring assetring.Ring `yaml:"-" json:"ring"`
}

// ServiceSecret injects one vault entry as an environment variable.
type ServiceSecret struct {
	Env      string `yaml:"env" json:"env" doc:"environment variable to set"`
	Ref      string `yaml:"ref" json:"ref" doc:"vault key name (bashy secrets)"`
	Optional bool   `yaml:"optional,omitempty" json:"optional,omitempty" doc:"omit this environment variable when the vault key is not bound"`
}

// ServicePort is one declared listener.
type ServicePort struct {
	Name     string `yaml:"name,omitempty" json:"name,omitempty" doc:"listener name, referenced as {port.NAME} and by expose; default the first is primary"`
	Port     int    `yaml:"port" json:"port" doc:"TCP/UDP port"`
	Bind     string `yaml:"bind,omitempty" json:"bind,omitempty" doc:"address or loopback | lan; default loopback"`
	Protocol string `yaml:"protocol,omitempty" json:"protocol,omitempty" doc:"tcp (default) | udp"`
}

// ServiceHealth is the unit's health probe. At most one of HTTP / TCP / Exec.
type ServiceHealth struct {
	HTTP        string   `yaml:"http,omitempty" json:"http,omitempty" doc:"path probed on the primary port, e.g. /healthz"`
	TCP         bool     `yaml:"tcp,omitempty" json:"tcp,omitempty" doc:"a TCP connect to the primary port"`
	Exec        []string `yaml:"exec,omitempty" json:"exec,omitempty" doc:"argv that exits 0 when healthy"`
	Port        string   `yaml:"port,omitempty" json:"port,omitempty" doc:"probe this named listener instead of the primary"`
	Interval    string   `yaml:"interval,omitempty" json:"interval,omitempty" doc:"probe period (duration)"`
	Timeout     string   `yaml:"timeout,omitempty" json:"timeout,omitempty" doc:"probe timeout (duration)"`
	Failures    int      `yaml:"failures,omitempty" json:"failures,omitempty" doc:"consecutive failures before the unit is degraded and restarted"`
	StartPeriod string   `yaml:"start_period,omitempty" json:"start_period,omitempty" doc:"grace after start before failures count (duration)"`
}

// ServiceRestart is the restart policy.
type ServiceRestart struct {
	Policy     string          `yaml:"policy,omitempty" json:"policy,omitempty" doc:"always | on-failure (default) | no"`
	Backoff    ServiceBackoff  `yaml:"backoff,omitempty" json:"backoff,omitempty" doc:"bounded exponential backoff"`
	StartLimit ServiceStartLim `yaml:"start_limit,omitempty" json:"start_limit,omitempty" doc:"more than burst starts within interval -> failed"`
}

// ServiceBackoff bounds the delay between restarts.
type ServiceBackoff struct {
	Min          string `yaml:"min,omitempty" json:"min,omitempty" doc:"first delay (duration)"`
	Max          string `yaml:"max,omitempty" json:"max,omitempty" doc:"delay ceiling (duration)"`
	HealthyAfter string `yaml:"healthy_after,omitempty" json:"healthy_after,omitempty" doc:"uptime that resets the backoff (duration)"`
}

// ServiceStartLim is the crash-loop limit.
type ServiceStartLim struct {
	Burst    int    `yaml:"burst,omitempty" json:"burst,omitempty" doc:"starts allowed within interval"`
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty" doc:"window (duration)"`
}

// ServiceStop is how the unit is stopped.
type ServiceStop struct {
	Signal  string `yaml:"signal,omitempty" json:"signal,omitempty" doc:"TERM (default) | INT | HUP | QUIT | KILL"`
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty" doc:"grace before KILL (duration)"`
}

// ServiceExpose is the unit's reach beyond loopback. The exposure reconciler
// publishes it while the unit is active and withdraws it otherwise.
type ServiceExpose struct {
	App  *ServiceApp   `yaml:"app,omitempty" json:"app,omitempty" doc:"matrix-tunnel app route"`
	Mesh []ServiceMesh `yaml:"mesh,omitempty" json:"mesh,omitempty" doc:"mesh service names"`
}

// ServiceApp is a tunnel app route and its access policy.
type ServiceApp struct {
	Name               string `yaml:"name,omitempty" json:"name,omitempty" doc:"app route name; default the unit name"`
	Port               string `yaml:"port,omitempty" json:"port,omitempty" doc:"named listener to route; default the primary"`
	RootURL            string `yaml:"root_url,omitempty" json:"root_url,omitempty" doc:"public root URL the app advertises; empty = derived from the tunnel"`
	RequireLogin       bool   `yaml:"require_login,omitempty" json:"require_login,omitempty" doc:"require an authenticated principal"`
	Elevation          bool   `yaml:"elevation,omitempty" json:"elevation,omitempty" doc:"require an elevated session"`
	TrustCloudIdentity bool   `yaml:"trust_cloud_identity,omitempty" json:"trust_cloud_identity,omitempty" doc:"accept the cloudbox SSO identity header"`
}

// ServiceMesh is one mesh service name for a listener.
type ServiceMesh struct {
	Name string `yaml:"name" json:"name" doc:"mesh service name, e.g. git, registry, s3"`
	Port string `yaml:"port,omitempty" json:"port,omitempty" doc:"named listener; default the primary"`
}

// ParseService reads a unit asset. name is the fallback identity.
func ParseService(name string, body []byte, src assetring.Source) (Service, error) {
	var s Service
	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !strings.Contains(err.Error(), "EOF") {
		return Service{}, fmt.Errorf("fleet: service %q: %w", name, err)
	}
	if s.Name == "" {
		s.Name = name
	}
	if s.Kind == "" {
		s.Kind = KindService
	}
	if src != nil {
		s.Ring = src.Ring()
	}
	return s, nil
}

// ProviderName is the provider realizing the unit, with the default applied.
func (s Service) ProviderName() string {
	if s.Provider == "" {
		return ServiceProviderProcess
	}
	return s.Provider
}

// PrimaryPort is the first declared listener, if any.
func (s Service) PrimaryPort() (ServicePort, bool) {
	if len(s.Ports) == 0 {
		return ServicePort{}, false
	}
	return s.Ports[0], true
}

// PortNamed resolves a listener reference; "" is the primary.
func (s Service) PortNamed(name string) (ServicePort, bool) {
	if name == "" {
		return s.PrimaryPort()
	}
	for _, p := range s.Ports {
		if p.Name == name {
			return p, true
		}
	}
	return ServicePort{}, false
}

// Dependencies are the units that must be active first: requires plus, for
// an internal capability, the unit hosting it.
func (s Service) Dependencies() []string {
	out := append([]string(nil), s.Requires...)
	if s.ProviderName() == ServiceProviderInternal && s.Owner != "" && s.Owner != ServiceOwnerOutpost && !containsStr(out, s.Owner) {
		out = append(out, s.Owner)
	}
	return out
}

var (
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	placeholder = regexp.MustCompile(`\{([a-z_]+)(?:\.([A-Za-z0-9_.-]+))?\}`)
)

// Validate is the structural check of one unit on its own.
func (s Service) Validate() error {
	if err := validName(s.Name); err != nil {
		return err
	}
	if strings.ContainsAny(s.Name, " \t\r\n") {
		return fmt.Errorf("fleet: service name %q: no whitespace", s.Name)
	}
	bad := func(format string, a ...any) error {
		return fmt.Errorf("fleet: service %q: "+format, append([]any{s.Name}, a...)...)
	}
	if s.Kind != "" && s.Kind != KindService {
		return bad("kind %q, want %s", s.Kind, KindService)
	}
	switch s.ProviderName() {
	case ServiceProviderProcess:
		if len(s.Exec) == 0 {
			return bad("a process unit needs exec")
		}
		if s.Capability != "" || s.Owner != "" {
			return bad("capability/owner belong to provider internal")
		}
		if s.Download != nil {
			if err := s.Download.validate(s.Name); err != nil {
				return err
			}
		}
	case ServiceProviderInternal:
		if s.Capability == "" || s.Owner == "" {
			return bad("an internal unit needs capability and owner")
		}
		if len(s.Exec) != 0 || s.Download != nil || s.Workdir != "" {
			return bad("an internal unit runs in its owner; exec/download/workdir are not allowed")
		}
		if s.Owner == s.Name {
			return bad("owner cannot be the unit itself")
		}
	default:
		return bad("provider %q, want %s | %s", s.Provider, ServiceProviderProcess, ServiceProviderInternal)
	}
	for k := range s.Env {
		if !envName.MatchString(k) {
			return bad("env name %q is not a valid variable name", k)
		}
	}
	for _, sec := range s.Secrets {
		if !envName.MatchString(sec.Env) || sec.Ref == "" {
			return bad("secret needs env (a variable name) and ref (a vault key), got %+v", sec)
		}
	}
	names := map[string]bool{}
	for _, p := range s.Ports {
		if p.Port < 1 || p.Port > 65535 {
			return bad("port %d out of range", p.Port)
		}
		if p.Protocol != "" && p.Protocol != "tcp" && p.Protocol != "udp" {
			return bad("port %d protocol %q, want tcp | udp", p.Port, p.Protocol)
		}
		if p.Name != "" {
			if names[p.Name] {
				return bad("listener name %q declared twice", p.Name)
			}
			names[p.Name] = true
		}
	}
	h := s.Health
	probes := 0
	if h.HTTP != "" {
		probes++
		if !strings.HasPrefix(h.HTTP, "/") {
			return bad("health.http %q must be a path", h.HTTP)
		}
	}
	if h.TCP {
		probes++
	}
	if len(h.Exec) != 0 {
		probes++
	}
	if probes > 1 {
		return bad("health declares more than one of http | tcp | exec")
	}
	if (h.HTTP != "" || h.TCP) && h.Port == "" && len(s.Ports) == 0 {
		return bad("health.http/tcp needs a declared port")
	}
	if h.Failures < 0 || s.Restart.StartLimit.Burst < 0 {
		return bad("health.failures and restart.start_limit.burst cannot be negative")
	}
	for path, d := range map[string]string{
		"health.interval": h.Interval, "health.timeout": h.Timeout, "health.start_period": h.StartPeriod,
		"restart.backoff.min": s.Restart.Backoff.Min, "restart.backoff.max": s.Restart.Backoff.Max,
		"restart.backoff.healthy_after": s.Restart.Backoff.HealthyAfter,
		"restart.start_limit.interval":  s.Restart.StartLimit.Interval, "stop.timeout": s.Stop.Timeout,
	} {
		if d == "" {
			continue
		}
		if v, err := time.ParseDuration(d); err != nil || v < 0 {
			return bad("%s %q is not a duration", path, d)
		}
	}
	switch s.Restart.Policy {
	case "", RestartAlways, RestartOnFailure, RestartNo:
	default:
		return bad("restart.policy %q, want always | on-failure | no", s.Restart.Policy)
	}
	switch strings.TrimPrefix(strings.ToUpper(s.Stop.Signal), "SIG") {
	case "", "TERM", "INT", "HUP", "QUIT", "KILL":
	default:
		return bad("stop.signal %q, want TERM | INT | HUP | QUIT | KILL", s.Stop.Signal)
	}
	switch s.Upgrade {
	case "", "restart", "keep":
	default:
		return bad("upgrade %q, want restart | keep", s.Upgrade)
	}
	for _, r := range s.Requires {
		if r == s.Name {
			return bad("requires itself")
		}
	}
	for _, ref := range []string{h.Port, s.exposeAppPort()} {
		if ref != "" && !names[ref] {
			return bad("listener %q is not declared in ports", ref)
		}
	}
	if s.Expose.App != nil && len(s.Ports) == 0 {
		return bad("expose.app needs a declared port")
	}
	for _, m := range s.Expose.Mesh {
		if m.Name == "" {
			return bad("expose.mesh entry needs a name")
		}
		if m.Port != "" && !names[m.Port] {
			return bad("listener %q is not declared in ports", m.Port)
		}
		if m.Port == "" && len(s.Ports) == 0 {
			return bad("expose.mesh %q needs a declared port", m.Name)
		}
	}
	if _, err := s.expand(func(string) (string, bool) { return "", true }); err != nil {
		return err
	}
	return nil
}

func (s Service) exposeAppPort() string {
	if s.Expose.App == nil {
		return ""
	}
	return s.Expose.App.Port
}

// ExpandExec returns the unit's argv with every placeholder resolved.
// download is the provisioned binary path ({download}); it is required only
// when exec names it. Expansion is the whole of argument templating: a unit
// overridden with `set ports.0.port=N` changes its argv without a code path.
func (s Service) ExpandExec(download string) ([]string, error) {
	return s.expand(func(string) (string, bool) { return download, download != "" })
}

func (s Service) expand(download func(string) (string, bool)) ([]string, error) {
	out := make([]string, len(s.Exec))
	for i, arg := range s.Exec {
		var err error
		out[i] = placeholder.ReplaceAllStringFunc(arg, func(m string) string {
			sub := placeholder.FindStringSubmatch(m)
			v, ok := s.resolve(sub[1], sub[2], download)
			if !ok && err == nil {
				err = fmt.Errorf("fleet: service %q: exec placeholder %s has no value", s.Name, m)
			}
			return v
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s Service) resolve(key, sub string, download func(string) (string, bool)) (string, bool) {
	switch key {
	case "port", "bind":
		p, ok := s.PortNamed(sub)
		if !ok {
			return "", false
		}
		if key == "port" {
			return strconv.Itoa(p.Port), true
		}
		if p.Bind == "" {
			return "127.0.0.1", true
		}
		return p.Bind, true
	case "settings":
		v, ok := s.Settings[sub]
		return v, ok
	case "download":
		if s.Download == nil {
			return "", false
		}
		return download(sub)
	}
	return "", false
}

// Services returns every unit, name-sorted, across the embedded, shared and
// local rings. Entries that fail to parse are reported, never hidden.
func (c *Catalog) Services() ([]Service, []error) {
	var errs []error
	cat := &assetring.Catalog[Service]{
		Sources: c.sources(dirServices),
		Parse: func(n string, b []byte, src assetring.Source) Service {
			s, err := ParseService(n, b, src)
			if err != nil {
				errs = append(errs, parseErr{n, err})
				return Service{Name: n, Kind: KindService, Ring: src.Ring()}
			}
			return s
		},
	}
	rows, err := cat.Rows()
	if err != nil {
		return nil, append(errs, err)
	}
	out := make([]Service, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// Service resolves a unit by name.
func (c *Catalog) Service(name string) (Service, bool) {
	all, _ := c.Services()
	for _, s := range all {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// checkService is every rule a unit write answers to: the structural check
// and, for an enabled unit, the port check against every other enabled unit.
func (c *Catalog) checkService(s Service) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if !s.Enabled {
		return nil
	}
	return c.ServicePortConflict(s)
}

// ServicePortConflict reports the first enabled unit other than s holding a
// listener s declares. A disabled unit holds nothing, so two units may
// declare one port as alternatives; only one of them can be enabled.
func (c *Catalog) ServicePortConflict(s Service) error {
	all, _ := c.Services()
	for _, other := range all {
		if other.Name == s.Name || !other.Enabled || other.IsRetired() {
			continue
		}
		for _, p := range s.Ports {
			for _, q := range other.Ports {
				if p.Port == q.Port && protoOf(p) == protoOf(q) && bindsOverlap(p.Bind, q.Bind) {
					return fmt.Errorf("fleet: service %q: port %d/%s is held by service %q (bind %s); disable %q or change the port",
						s.Name, p.Port, protoOf(p), other.Name, bindLabel(q.Bind), other.Name)
				}
			}
		}
	}
	return nil
}

func protoOf(p ServicePort) string {
	if p.Protocol == "" {
		return "tcp"
	}
	return p.Protocol
}

func bindLabel(b string) string {
	if b == "" {
		return "loopback"
	}
	return b
}

// bindsOverlap: a wildcard (lan, 0.0.0.0, ::) overlaps everything; loopback
// spellings are one address; otherwise addresses must match.
func bindsOverlap(a, b string) bool {
	norm := func(s string) string {
		switch s {
		case "", "loopback", "localhost", "127.0.0.1", "::1":
			return "loopback"
		case "lan", "0.0.0.0", "::", "*", "all":
			return "*"
		}
		return s
	}
	a, b = norm(a), norm(b)
	return a == "*" || b == "*" || a == b
}

// SaveService validates and writes a unit into the local store.
func (c *Catalog) SaveService(s Service) error {
	s.Kind = KindService
	if err := c.checkService(s); err != nil {
		return err
	}
	data, err := Marshal(s)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirServices), s.Name, data)
}

// MaterializeService copies a unit into the local store if needed and
// returns the path an editor should open.
func (c *Catalog) MaterializeService(name string) (string, error) {
	s, ok := c.Service(name)
	if !ok {
		return "", fmt.Errorf("fleet: no service %q", name)
	}
	if s.Ring != ringLocal() {
		if err := c.SaveService(s); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirServices), s.Name)
}

// RemoveService deletes a unit from the local store. A built-in's local
// override goes; the seed shows through again.
func (c *Catalog) RemoveService(name string) error {
	return removeEntry(c.nounDir(dirServices), dirServices, name)
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
