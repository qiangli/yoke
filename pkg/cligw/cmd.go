package cligw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// The `llm` front door. Three verbs and no more: serve the gateway, look at
// the pools it is holding, and print the environment an OpenAI client needs.
//
// `pools` and `env` deliberately talk to a RUNNING server through the
// endpoint record rather than constructing a second Server: a second one
// would have its own pools and its own view of quota, and the operator would
// be reading numbers that describe nothing that is serving traffic.

type serveOptions struct {
	bind    string
	port    int
	policy  string
	prewarm bool
}

// NewCmd returns the `llm` command tree — the host-agnostic entry point a
// front end mounts (`bashy llm`), mirroring ask.NewAskCmd / weave.NewWeaveCmd.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "llm",
		Short: "serve the local OpenAI-compatible gateway over the agent CLI fleet",
		Long: `llm serves an OpenAI-protocol endpoint backed by the agent CLIs this host
already pays for (claude, codex, agy, ...), so any OpenAI client — the openai
SDK, an editor, an eval harness — can reach a flat-billed seat.

Each request is answered by a PRE-SPAWNED, ONE-SHOT worker: the process is
started ahead of time, fed exactly one prompt, and retired. No context crosses
requests, the CLI's own tools are off, and the sandbox is read-only.

Ask for a capability band rather than a vendor model:

  model: L4      exactly band 4, any vendor
  model: L4+     band 4 or better, lowest sufficient band first
  model: auto    classify the band from the prompt
  model: opus5   a registry model, on whichever agent serves it
  model: agy-gemini3.1   one pinned agent

The router ranks the band's agents by remaining subscription quota, then warm
availability, then operator weight, and answers with X-Bashy-Routed so the
choice is auditable.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newServeCmd(), newPoolsCmd(), newEnvCmd())
	return cmd
}

func newServeCmd() *cobra.Command {
	var o serveOptions
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "serve /v1/chat/completions and /v1/models in the foreground",
		Long: `serve binds the gateway and stays in the foreground until interrupted.

It binds LOOPBACK by default. --bind lan must be asked for explicitly: the
gateway spends the owner's own subscription seats behind a single bearer token,
so a LAN port is a way to spend someone else's quota. --bind unix:PATH creates
an owner-only (0600) socket, which is the strongest of the three.

The bearer token lives in the cligw state directory and is generated on first
serve; ` + "`llm env`" + ` prints it in the form a client expects.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(c *cobra.Command, _ []string) error { return runServe(c, &o) },
	}
	f := cmd.Flags()
	f.StringVar(&o.bind, "bind", BindLoopback, "where to listen: loopback|lan|unix:PATH")
	f.IntVar(&o.port, "port", DefaultPort, "TCP port for a loopback or lan bind")
	f.StringVar(&o.policy, "policy", "", "routing policy file (default: policy.yaml in the cligw state directory)")
	f.BoolVar(&o.prewarm, "prewarm", false, "spawn each band leader's spare worker at startup instead of on first request")
	return cmd
}

func runServe(cmd *cobra.Command, o *serveOptions) error {
	server, err := NewServer(ServerOptions{PolicyFile: o.policy, Prewarm: o.prewarm})
	if err != nil {
		return err
	}
	defer server.Close()

	listener, endpoint, err := Listen(o.bind, o.port)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "cligw serving on %s\n", endpoint.BaseURL)
	if endpoint.Socket != "" {
		fmt.Fprintf(out, "unix socket %s (0600)\n", endpoint.Socket)
	}
	fmt.Fprintf(out, "OPENAI_BASE_URL=%s\n", endpoint.OpenAIBaseURL())
	if url := endpoint.AnthropicBaseURL(); url != "" {
		fmt.Fprintf(out, "ANTHROPIC_BASE_URL=%s\n", url)
	}
	fmt.Fprintf(out, "eval \"$(bashy llm env)\" to configure a client\n")

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.ServeListener(ctx, listener, endpoint)
}

func newPoolsCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pools",
		Short: "live view of a running server's worker pools and prewarmer",
		Long: `pools reads /health from the running server and prints, per pool, the idle,
busy and queued worker counts, the current spawn rate, the arrival rate
lambda, the mean service time W, and the seat's remaining quota headroom —
then the same totals per capability band.

A pool that has never been routed to does not exist yet: pools are created on
first use, so an empty list means no request has arrived, not that nothing is
configured.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(c *cobra.Command, _ []string) error { return runPools(c, asJSON) },
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the bashy-cligw-health-v1 envelope")
	return cmd
}

func runPools(cmd *cobra.Command, asJSON bool) error {
	endpoint, err := ReadEndpoint()
	if err != nil {
		return err
	}
	token, err := LoadOrCreateToken()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	report, err := FetchHealth(ctx, endpoint, token)
	if err != nil {
		return err
	}
	if asJSON {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return nil
	}
	return writePoolsTable(cmd.OutOrStdout(), report)
}

func writePoolsTable(out io.Writer, report HealthReport) error {
	cooling := make(map[string]bool, len(report.Agents))
	for _, agent := range report.Agents {
		cooling[agent.Agent] = agent.Cooling
	}
	fmt.Fprintf(out, "status %s  workers %d/%d\n", report.Status, report.Autoscale.Workers, report.Autoscale.HostMaxWorkers)
	if len(report.Autoscale.Pools) == 0 {
		fmt.Fprintln(out, "no pool has been created yet (pools are created on first request)")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tBAND\tIDLE\tBUSY\tQUEUED\tSPAWN\tLAMBDA\tW\tHEADROOM\tSTATE")
	for _, pool := range report.Autoscale.Pools {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%.2f\t%s\t%s\t%s\n",
			pool.Agent, bandLabel(pool.Band), pool.Idle, pool.Busy, pool.Queued, pool.SpawnRate,
			pool.Lambda, serviceLabel(pool), headroomLabel(pool.Headroom, pool.HeadroomKnown),
			poolStateLabel(pool, cooling[pool.Agent]))
	}
	fmt.Fprintln(tw, "\t\t\t\t\t\t\t\t\t")
	fmt.Fprintln(tw, "BAND\tAGENTS\tIDLE\tBUSY\tQUEUED\tLAMBDA\tPREWARM\t\t\t")
	for _, band := range report.Autoscale.Bands {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%.2f\t%d\t\t\t\n",
			bandLabel(band.Band), band.Agents, band.Idle, band.Busy, band.Queued, band.Lambda, band.Prewarm)
	}
	return tw.Flush()
}

func bandLabel(band int) string {
	if band <= 0 {
		return "-"
	}
	return fmt.Sprintf("L%d", band)
}

func serviceLabel(pool PoolSnapshot) string {
	if !pool.ServiceKnown {
		return "-"
	}
	return time.Duration(pool.ServiceSeconds * float64(time.Second)).Round(10 * time.Millisecond).String()
}

func headroomLabel(headroom float64, known bool) string {
	if !known {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", headroom*100)
}

func poolStateLabel(pool PoolSnapshot, cooling bool) string {
	var states []string
	if cooling {
		states = append(states, "cooling")
	}
	if pool.BreakerOpen {
		states = append(states, "spawn-breaker")
	}
	if pool.Cold {
		states = append(states, "cold")
	}
	if len(states) == 0 {
		return "ready"
	}
	return strings.Join(states, ",")
}

func newEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "print the OPENAI_* (and ANTHROPIC_*) variables a client needs",
		Long: `env prints shell assignments for a running (or default) server:

  eval "$(bashy llm env)"
  python -c 'import openai; print(openai.OpenAI().chat.completions.create(
      model="L4", messages=[{"role":"user","content":"say ok"}]).choices[0].message.content)'

OPENAI_BASE_URL ends in /v1 because that is what the SDKs append their paths
to. The key is the local bearer token, not a vendor key: nothing here reaches
a vendor API, and the token only guards this host's own port.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          func(c *cobra.Command, _ []string) error { return runEnv(c) },
	}
	return cmd
}

func runEnv(cmd *cobra.Command) error {
	endpoint, err := ReadEndpoint()
	if err != nil {
		return err
	}
	token, err := LoadOrCreateToken()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if endpoint.Socket != "" {
		fmt.Fprintf(out, "# cligw is bound to the unix socket %s; use a socket-aware client\n", endpoint.Socket)
	}
	fmt.Fprintf(out, "OPENAI_BASE_URL=%s\n", endpoint.OpenAIBaseURL())
	fmt.Fprintf(out, "OPENAI_API_KEY=%s\n", token)
	if url := endpoint.AnthropicBaseURL(); url != "" {
		fmt.Fprintf(out, "ANTHROPIC_BASE_URL=%s\n", url)
		fmt.Fprintf(out, "ANTHROPIC_API_KEY=%s\n", token)
	}
	fmt.Fprintln(out, "export OPENAI_BASE_URL OPENAI_API_KEY"+anthropicExports(endpoint))
	return nil
}

func anthropicExports(endpoint Endpoint) string {
	if endpoint.AnthropicBaseURL() == "" {
		return ""
	}
	return " ANTHROPIC_BASE_URL ANTHROPIC_API_KEY"
}
