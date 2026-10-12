package feed

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// DoctorSchema versions the `feed doctor --json` envelope.
const DoctorSchema = "bashy-feed-doctor-v1"

// Doctor check states.
const (
	DoctorConfigured = "configured"
	DoctorMissing    = "missing"
)

// openclawMinNode is the oldest Node the OpenClaw CLI supports.
var openclawMinNode = [3]int{24, 16, 0}

// openclawGateway is the OpenClaw gateway's default loopback endpoint, probed
// when the channel's command does not declare its own HTTP URL.
const openclawGateway = "http://127.0.0.1:18789/"

// doctorProbeTimeout bounds every outside call doctor makes.
const doctorProbeTimeout = 5 * time.Second

// DoctorCheck is one finding: what was checked, its state, and the exact
// command that moves a missing check forward. Detail never carries a secret
// value — at most a NAME.
type DoctorCheck struct {
	Area string `json:"area"`
	Name string `json:"name,omitempty"`
	// Label is the user-facing messaging channel name ("Matrix chat", never
	// bare "matrix", which means the cloudbox matrix tunnel).
	Label  string `json:"label,omitempty"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Next   string `json:"next,omitempty"`
}

// DoctorReport is the `feed doctor --json` envelope.
type DoctorReport struct {
	SchemaVersion string        `json:"schema_version"`
	OK            bool          `json:"ok"`
	Checks        []DoctorCheck `json:"checks"`
}

// doctorProbes are the outside-world reads doctor composes. nil fields take
// the production wiring; tests inject them so no network, git, node or
// openclaw is touched.
type doctorProbes struct {
	lookPath      func(string) (string, error)
	nodeVersion   func(context.Context) (string, error)
	gatewayUp     func(ctx context.Context, url string) error
	gitResolve    func(ctx context.Context, remote, branch string) error
	lookupCommand func(ref string) (fleet.Tool, fleet.ToolCommand, error)
	vaultNames    func() map[string]struct{}
	lookupEnv     func(string) (string, bool)
}

func (p doctorProbes) withDefaults() doctorProbes {
	if p.lookPath == nil {
		p.lookPath = exec.LookPath
	}
	if p.nodeVersion == nil {
		p.nodeVersion = func(ctx context.Context) (string, error) {
			out, err := exec.CommandContext(ctx, "node", "--version").Output()
			return strings.TrimSpace(string(out)), err
		}
	}
	if p.gatewayUp == nil {
		p.gatewayUp = func(ctx context.Context, url string) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := (&http.Client{Timeout: doctorProbeTimeout}).Do(req)
			if err != nil {
				return err
			}
			resp.Body.Close()
			return nil
		}
	}
	if p.gitResolve == nil {
		p.gitResolve = func(ctx context.Context, remote, branch string) error {
			_, err := runGit(ctx, "", "ls-remote", "--heads", remote, branch)
			return err
		}
	}
	if p.lookupCommand == nil {
		p.lookupCommand = toolcmd.LookupCommand
	}
	if p.vaultNames == nil {
		p.vaultNames = secrets.VaultEnvNames
	}
	if p.lookupEnv == nil {
		p.lookupEnv = os.LookupEnv
	}
	return p
}

// RunDoctor inspects the feed setup and reports every area. It is read-only:
// it loads config but never saves it, never creates the signing key, and
// never resolves a secret value.
func RunDoctor(ctx context.Context, env Env) DoctorReport {
	p := env.doctor.withDefaults()
	dir := env.dir()
	var checks []DoctorCheck
	checks = append(checks, doctorPairing(env))
	checks = append(checks, doctorKey(dir))
	checks = append(checks, doctorSinks(ctx, env, p, dir)...)
	chans, chanChecks := doctorChannels(ctx, p, dir)
	checks = append(checks, chanChecks...)
	checks = append(checks, doctorSecrets(p, chans)...)
	ok := true
	for _, c := range checks {
		if c.Status != DoctorConfigured {
			ok = false
		}
	}
	return DoctorReport{SchemaVersion: DoctorSchema, OK: ok, Checks: checks}
}

func doctorPairing(env Env) DoctorCheck {
	cloud := env.Cloud
	if cloud == nil {
		cloud = func() (string, string) { return fleet.ResolveCloud("", "") }
	}
	if _, token := cloud(); token == "" {
		return DoctorCheck{Area: "pairing", Status: DoctorMissing,
			Detail: "host is not paired; posts are not indexed", Next: "bashy login"}
	}
	return DoctorCheck{Area: "pairing", Status: DoctorConfigured}
}

func doctorKey(dir string) DoctorCheck {
	path := filepath.Join(dir, keyFileName)
	info, err := os.Stat(path)
	if err != nil {
		return DoctorCheck{Area: "signing-key", Status: DoctorMissing,
			Detail: "no signing key; the first post generates one", Next: "bashy feed post FILE"}
	}
	if err := checkKeyPerms(path); err != nil {
		return DoctorCheck{Area: "signing-key", Status: DoctorMissing,
			Detail: "key file is readable by group or others", Next: "chmod 600 " + path}
	}
	if info.Size() != ed25519.SeedSize {
		return DoctorCheck{Area: "signing-key", Status: DoctorMissing,
			Detail: "malformed key file", Next: "bashy feed key ls"}
	}
	return DoctorCheck{Area: "signing-key", Status: DoctorConfigured}
}

func doctorSinks(ctx context.Context, env Env, p doctorProbes, dir string) []DoctorCheck {
	cfgs, err := LoadSinks(dir)
	if err != nil {
		return []DoctorCheck{{Area: "sinks", Status: DoctorMissing, Detail: err.Error(), Next: "bashy feed sink list"}}
	}
	if len(cfgs) == 0 {
		return []DoctorCheck{{Area: "sinks", Status: DoctorMissing, Detail: "no sink configured",
			Next: "bashy feed sink add github NAME GIT_REMOTE_URL"}}
	}
	var out []DoctorCheck
	for _, s := range cfgs {
		c := DoctorCheck{Area: "sinks", Name: s.Name, Status: DoctorConfigured, Detail: s.Kind + " " + s.Target}
		pctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
		switch s.Kind {
		case "github":
			branch := s.Branch
			if branch == "" {
				branch = "main"
			}
			if err := p.gitResolve(pctx, s.Target, branch); err != nil {
				c.Status, c.Detail = DoctorMissing, "git remote does not resolve: "+s.Target
				c.Next = "git ls-remote " + s.Target
			}
		case "rclone":
			if err := checkRcloneRemote(pctx, s.Target, env.rcloneRun); err != nil {
				c.Status, c.Detail = DoctorMissing, err.Error()
				c.Next = "bashy rclone config"
			}
		}
		cancel()
		out = append(out, c)
	}
	return out
}

// firstReleaseChannels maps each OpenClaw --channel value the first release
// supports to its user-facing label.
var firstReleaseChannels = map[string]string{
	"telegram": "Telegram",
	"discord":  "Discord",
	"slack":    "Slack",
	"matrix":   "Matrix chat",
}

const firstReleaseSet = "Telegram, Discord, Slack, Matrix chat"

// openclawChannelArg returns the --channel value in a channel's args
// ("--channel X" or "--channel=X"), or "" when there is none.
func openclawChannelArg(args string) string {
	f := strings.Fields(args)
	for i, a := range f {
		if v, ok := strings.CutPrefix(a, "--channel="); ok {
			return v
		}
		if a == "--channel" && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

// localHomeserver returns a Matrix chat channel's homeserver URL when it is
// on this host (loopback), or "". A local homeserver is a server bashy would
// start, so it answers to the signed-build policy; a remote one does not.
func localHomeserver(args string) string {
	f := strings.Fields(args)
	for i, a := range f {
		v, ok := strings.CutPrefix(a, "--homeserver=")
		if !ok && a == "--homeserver" && i+1 < len(f) {
			v, ok = f[i+1], true
		}
		if !ok {
			continue
		}
		u, err := url.Parse(v)
		if err != nil {
			continue
		}
		h := u.Hostname()
		if h == "localhost" || strings.HasSuffix(h, ".localhost") {
			return v
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return v
		}
	}
	return ""
}

// doctorChannel pairs a channel with its resolved tool command, for the
// secrets area.
type doctorChannel struct {
	ch  Channel
	cmd fleet.ToolCommand
}

func doctorChannels(ctx context.Context, p doctorProbes, dir string) ([]doctorChannel, []DoctorCheck) {
	cs, err := LoadChannels(dir)
	if err != nil {
		return nil, []DoctorCheck{{Area: "channels", Status: DoctorMissing, Detail: err.Error(), Next: "bashy feed channel list"}}
	}
	if len(cs) == 0 {
		return nil, []DoctorCheck{{Area: "channels", Status: DoctorMissing, Detail: "no delivery channel configured",
			Next: "bashy feed channel add NAME --cmd TOOL:NAME"}}
	}
	var resolved []doctorChannel
	var out []DoctorCheck
	for _, ch := range cs {
		c := DoctorCheck{Area: "channels", Name: ch.Name, Status: DoctorConfigured, Detail: ch.Cmd}
		tool, tc, err := p.lookupCommand(ch.Cmd)
		if err != nil {
			c.Status, c.Detail, c.Next = DoctorMissing, err.Error(), "bashy tool list"
			out = append(out, c)
			continue
		}
		resolved = append(resolved, doctorChannel{ch: ch, cmd: tc})
		if strings.EqualFold(tool.Name, "openclaw") {
			kind := openclawChannelArg(ch.Args)
			label, ok := firstReleaseChannels[strings.ToLower(kind)]
			if !ok {
				c.Status, c.Next = DoctorMissing, "bashy feed channel rm "+ch.Name
				if kind == "" {
					c.Detail = "no --channel in args; the first-release messaging set is " + firstReleaseSet
				} else {
					c.Detail = kind + " is not in the first-release messaging set (" + firstReleaseSet + ")"
				}
				out = append(out, c)
				continue
			}
			c.Label = label
			if miss := doctorOpenClaw(ctx, p, tc); miss != nil {
				c.Status, c.Detail, c.Next = DoctorMissing, miss.Detail, miss.Next
			}
			if hs := localHomeserver(ch.Args); kind == "matrix" && hs != "" {
				out = append(out, c)
				c = DoctorCheck{Area: "homeserver", Name: ch.Name, Label: label, Status: DoctorMissing,
					Detail: blockedMatrixChat + " (local homeserver " + hs + ")"}
			}
		}
		out = append(out, c)
	}
	return resolved, out
}

// Service supply-chain policy (docs/bashy-service-supply-chain-policy.md):
// bashy never starts a server built by a third party, only a bashy-CI build
// signed keylessly with Sigstore. Until those builds exist, doctor reports a
// missing gateway or local homeserver as BLOCKED with no next command —
// never an npm install or a `gateway start` that would run a third-party
// build.
const (
	blockedOpenClaw   = "blocked: waiting for the bashy-signed OpenClaw build"
	blockedMatrixChat = "blocked: waiting for the bashy-signed Matrix chat homeserver build"
)

// doctorOpenClaw checks the OpenClaw prerequisites in dependency order and
// returns the first one missing, or nil.
func doctorOpenClaw(ctx context.Context, p doctorProbes, tc fleet.ToolCommand) *DoctorCheck {
	if _, err := p.lookPath("openclaw"); err != nil {
		return &DoctorCheck{Detail: blockedOpenClaw + " (openclaw is not installed)"}
	}
	want := fmt.Sprintf("%d.%d.%d", openclawMinNode[0], openclawMinNode[1], openclawMinNode[2])
	nctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	v, err := p.nodeVersion(nctx)
	cancel()
	if err != nil {
		return &DoctorCheck{Detail: "node is not on PATH (OpenClaw needs Node >= " + want + ")", Next: "bashy mise use -g node@24"}
	}
	if !nodeAtLeast(v, openclawMinNode) {
		return &DoctorCheck{Detail: "node " + v + " is older than " + want, Next: "bashy mise use -g node@24"}
	}
	url := openclawGateway
	if tc.HTTP != nil && tc.HTTP.URL != "" {
		url = tc.HTTP.URL
	}
	gctx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	err = p.gatewayUp(gctx, url)
	cancel()
	if err != nil {
		return &DoctorCheck{Detail: blockedOpenClaw + " (the gateway does not answer at " + url + "; bashy does not start an unsigned gateway)"}
	}
	return nil
}

// nodeAtLeast reports whether a `node --version` string ("v24.16.0") is at
// least min. An unparsable version is too old.
func nodeAtLeast(v string, min [3]int) bool {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	var got [3]int
	for i, s := range parts {
		if i == 2 {
			s, _, _ = strings.Cut(s, "-")
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return false
		}
		got[i] = n
	}
	for i := range got {
		if got[i] != min[i] {
			return got[i] > min[i]
		}
	}
	return true
}

// doctorSecrets checks, by NAME only, that every token a channel's command
// references is bound on this host. Values are never read: presence comes
// from the vault's declared/rendered names, the environment's names, or the
// token file's existence.
func doctorSecrets(p doctorProbes, chans []doctorChannel) []DoctorCheck {
	var out []DoctorCheck
	vault := p.vaultNames()
	for _, dc := range chans {
		h := dc.cmd.HTTP
		if h == nil {
			continue
		}
		if ref := strings.TrimSpace(h.TokenRef); ref != "" {
			names := secrets.CredentialEnvNames(ref)
			c := DoctorCheck{Area: "secrets", Name: ref, Status: DoctorMissing,
				Detail: "channel " + dc.ch.Name + ": token ref " + ref + " is not bound",
				Next:   "bashy secret set " + names[0]}
			for _, n := range names {
				_, inVault := vault[n]
				if inVault || envNamePresent(p, n) {
					c.Status, c.Detail, c.Next = DoctorConfigured, "channel "+dc.ch.Name+": bound as "+n, ""
					break
				}
			}
			out = append(out, c)
		}
		if tf := strings.TrimSpace(h.TokenFile); tf != "" {
			c := DoctorCheck{Area: "secrets", Name: filepath.Base(tf), Status: DoctorConfigured,
				Detail: "channel " + dc.ch.Name + ": token file present"}
			if _, err := os.Stat(tf); err != nil {
				c.Status, c.Detail = DoctorMissing, "channel "+dc.ch.Name+": token file "+tf+" is missing"
				c.Next = "bashy tool show " + strings.SplitN(dc.ch.Cmd, ":", 2)[0]
			}
			out = append(out, c)
		}
	}
	return out
}

// envNamePresent reports whether name is set non-empty. The value is
// inspected only for emptiness and is discarded.
func envNamePresent(p doctorProbes, name string) bool {
	v, ok := p.lookupEnv(name)
	return ok && strings.TrimSpace(v) != ""
}

// errDoctorMissing makes `feed doctor` exit nonzero when any area is missing.
var errDoctorMissing = errors.New("feed doctor: setup incomplete")

func newDoctorCmd(env Env) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "check feed setup: pairing, signing key, sinks, channels, secrets",
		Long: `doctor reports each feed setup area as configured or missing, with the
exact next command to run for anything missing. It is read-only and never
prints a secret value — secrets are checked by name. Exits 0 when everything
is configured, nonzero otherwise.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			rep := RunDoctor(ctxOf(c), env)
			if asJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return err
				}
			} else {
				printDoctor(c.OutOrStdout(), rep)
			}
			if !rep.OK {
				return errDoctorMissing
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit the "+DoctorSchema+" envelope")
	return c
}

func printDoctor(w io.Writer, rep DoctorReport) {
	for _, c := range rep.Checks {
		area := c.Area
		if c.Name != "" {
			area += " " + c.Name
		}
		if c.Label != "" {
			area += " (" + c.Label + ")"
		}
		line := fmt.Sprintf("%-10s %s", c.Status, area)
		if c.Detail != "" {
			line += ": " + oneLine(c.Detail)
		}
		fmt.Fprintln(w, line)
		if c.Next != "" {
			fmt.Fprintln(w, "           next: "+c.Next)
		}
	}
}
