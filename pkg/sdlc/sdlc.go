package sdlc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const schemaVersion = "bashy-sdlc-v1"

const sdlcLongHelp = `
bashy sdlc is a local-first SDLC coordinator. It accepts an issue/request pointer,
builds a conductor brief, and delegates implementation to the configured agent.
SDLC owns intake routing, lifecycle state, validation gates, and deployment target
checks; the conductor owns architecture, planning, implementation, tests, commits,
and pushes.

For interactive use, no config file is required. If .bashy/sdlc.yaml is absent,
bashy uses embedded defaults: local intake, the active agent as conductor when it
can be detected, and generic validation/rollout target names. Use "bashy sdlc
init" when you want project-specific intake, reviewer, QA, or deployment targets.
Use "--no-config" plus routing flags when a script or human wants to pass the
whole SDLC boundary on the command line.

Run "bashy sdlc guide" for the full embedded usage guide.
`

const sdlcGuide = `
bashy sdlc guide

Purpose

bashy sdlc is the interface between any issue intake source and any deployment
target. It is designed to stay small: it captures or receives a request pointer,
turns it into a clear conductor brief, invokes the implementation conductor agent,
and provides simple checks for validation and deployment status.

The SDLC command does not try to be the architect or implementer. The conductor
agent owns planning, coding, tests, source control operations, and iteration. SDLC
keeps the boundary: request in, agent delegation, validation gates, deployment
status, and explicit approval before rollout when policy requires it.

Local interactive quick start

  bashy sdlc --issue "Remove the obsolete Miscellaneous section"

For a longer request:

  bashy sdlc --issue-file ./request.md

Run from another checkout or project directory:

  bashy sdlc --issue-file ./request.md --cwd /path/to/project

Print what would be run without invoking the conductor:

  bashy sdlc --issue "Update the landing page" --dry-run

Machine-readable output:

  bashy sdlc --issue "Update the landing page" --json

Issue text format

The first non-empty line becomes the title. The full multi-line text becomes the
body. This works for humans and tools:

  bashy sdlc --issue "Fix staging deploy"

  cat request.md
  Fix staging deploy

  The Pages workflow is green but the live page still shows old content.
  Check deployment status, inspect the live page, and fix the cause.

  bashy sdlc --issue-file request.md

Configuration

For interactive local runs, .bashy/sdlc.yaml is optional. If it is missing, bashy
uses an embedded default config with:

  intake: local
  conductor: the detected active agent, or codex as a fallback
  reviewer: codex
  qa: codex
  staging: staging
  production: production

Inspect the active config/defaults:

  bashy sdlc config explain

Create a project config:

  bashy sdlc init

Validate a project config:

  bashy sdlc doctor

Skip the config file and supply routing on the command line:

  bashy sdlc --no-config --conductor codex --intake-provider github \
    --intake-repo owner/repo --staging pages-staging --production pages-prod \
    --issue "Fix staging deploy"

Supported routing flags:

  --conductor AGENT
  --reviewer AGENT
  --qa AGENT
  --intake-provider PROVIDER
  --intake-repo OWNER/REPO
  --intake-query QUERY
  --intake-label LABEL
  --staging NAME
  --staging-host HOST
  --staging-env ENV
  --staging-command COMMAND
  --staging-healthcheck URL
  --staging-rollback COMMAND
  --production NAME
  --production-host HOST
  --production-env ENV
  --production-command COMMAND
  --production-healthcheck URL
  --production-rollback COMMAND
  --metadata KEY=VALUE
  --policy KEY=VALUE
  --agent NAME=AGENT

Zero-config intake policy

For Loom/local intake, labels are optional. A plain open issue should be enough
to start work in most projects. The default pickup policy is:

  pick open issues unless they are already assigned/in-progress or carry one of
  the reserved skip labels:
    sdlc:ignore
    sdlc:blocked
    sdlc:in-progress
    sdlc:qa
    sdlc:approved
    sdlc:done

Type and priority labels are useful but not required:

  type:bug, type:enhancement, type:task, type:docs, type:chore
  priority:p0, priority:p1, priority:p2, priority:p3

Projects may add any custom labels. Only the reserved sdlc:* lifecycle labels
have built-in control meaning.

Example .bashy/sdlc.yaml:

  conductor:
    agent: codex
  reviewer:
    agent: codex
  qa:
    agent: codex
  intake:
    provider: github
    repository: owner/repo
    query: is:issue is:open no:assignee
  deployment:
    staging:
      name: pages-staging
      healthcheck: https://staging.example.com
    production:
      name: pages-production
      healthcheck: https://example.com

Conductor selection

The conductor defaults to the active agent when bashy can detect one. Explicit
environment overrides are also supported:

  BASHY_SDLC_CONDUCTOR_AGENT=claude bashy sdlc --issue "Fix checkout"
  BASHY_SDLC_CONDUCTOR_AGENT=opencode bashy sdlc --issue-file request.md
  BASHY_SDLC_CONDUCTOR_AGENT=codex bashy sdlc --issue "Add docs"

Use this to inspect the active agent context when available:

  bashy agent whoami

Delegation and permissions

SDLC defaults the conductor sandbox to danger-full-access. This is intentional for
developer workstations where the delegated conductor is expected to edit files,
run tests, commit, and push. For a restricted run:

  bashy sdlc --issue "Try the change" --sandbox workspace-write

For a dry-run of the exact conductor invocation:

  bashy sdlc delegate --issue-title "Try the change" --dry-run

Background runs and monitoring

Start a conductor in the background and write a local run log:

  bashy sdlc --background --issue-file request.md

Background runs are supervised. The supervisor streams conductor stdout/stderr
to the run log and records final status, exit code, and finish time in run.json.

List local runs:

  bashy sdlc runs

Show the latest run:

  bashy sdlc watch

Follow a run log until the process exits:

  bashy sdlc watch RUN_ID --follow

Run records are stored under .bashy/generated/sdlc/runs by default. The conductor log and
brief can contain request details and agent output, so keep that directory local
or gitignored when working in repositories that may be published.

Local approval and resolution

Every delegated request creates a local run reference ID. Use that exact ID for
approval, rollout, and final resolution:

  bashy sdlc runs
  bashy sdlc qa RUN_ID --status accepted --note "QA passed"
  bashy sdlc approve RUN_ID --note "UAT passed on staging"
  bashy sdlc rollout RUN_ID --background
  bashy sdlc resolve RUN_ID --status resolved --note "Deployment target verified"

Approval requires an explicit RUN_ID. SDLC does not assume only one issue is
active at a time.

Local issue records

Record a local issue without delegating it:

  bashy sdlc issue --text "Add a deployment status check"
  bashy sdlc issue --file ./request.md

Local issue files are written under .bashy/generated/sdlc/issues by default. They are meant
for local reference and automation handoff.

Briefs and scheduled ticks

Render only the conductor brief:

  bashy sdlc brief --issue-title "Refresh project list"

Run one externally triggered cycle for a single issue:

  bashy sdlc tick --issue-title "Refresh project list"

The tick command is suitable for schedulers or CI jobs that already selected the
issue to work on. Future intake integrations can select GitHub, Jira, or other
project-management issues before calling tick/delegate.

Local Loom-first control plane

For public repositories, do not put long-lived subscription credentials, private
work logs, or self-hosted runner authority into public GitHub Actions unless you
have an explicit security model for it. The safer default is:

  GitHub = public source control and external deployment integration.
  Loom = local/private issue mirror, comments, CI control plane, and SDLC state.
  bashy sdlc = the bridge from private intake to conductor, QA, approval, rollout.

Start a local forge/control plane:

  bashy loom start --addr 127.0.0.1 --port 31880
  bashy loom status

For peer-to-peer access through outpost mesh, keep the UI root URL on a stable
loopback port and ask remote peers to dial the same local port:

  bashy loom expose
  outpost mesh dial --local 127.0.0.1:31880 git

Or start and expose in one step:

  bashy loom start --expose

For cloudbox/internet access, start Loom with the stable HTTPS URL users will
open through cloudbox. This keeps Gitea UI links and clone URLs coherent:

  bashy loom start \
    --root-url https://CLOUDBOX_HOST/matrix/h/HOST/app/loom/

Prepare a local workspace on the loom source-of-truth repo, then run SDLC from
that workspace using the local issue/request pointer (single-repo model):

  bashy sdlc workspace prepare \
    --origin http://127.0.0.1:31880/loom/owner-repo.git \
    --dir ~/work/repo

  origin   = the loom repo (source of truth): clone from it, push back to it
  upstream = optional read-only source/backup (GitHub/GitLab/…), push disabled

The loom repo is the source of truth and also the issue-intake surface. A loom
repo can be created fresh or MIGRATED from any upstream (GitHub, GitLab, plain
git) via Gitea's migrate — GitHub is just one source, and the migrated repo is
writable. Agents commit + push to origin; GitHub writes happen only during an
explicit, approved rollout/deploy (deploy.md), so GitHub stays a replaceable
deploy/backup target, not the source.

Legacy guarded two-repo mode (an immutable Loom mirror to clone/diff against,
with origin the writable workspace) is still available by passing --baseline.

  cd ~/work/repo
  bashy sdlc issue --text "Fix the broken link"
  bashy sdlc tick --issue-file .bashy/generated/sdlc/issues/<printed-issue-file>.md \
    --intake-provider loom --intake-repo owner/repo \
    --production github-pages --background

The local scheduler can drive the same command periodically:

  bashy schedule add --every 5m -- \
    bashy sdlc tick --issue-file .bashy/generated/sdlc/issues/<printed-issue-file>.md \
      --intake-provider loom --intake-repo owner/repo --background

After the conductor finishes, the trigger or a human records QA and approval
against the exact SDLC reference id:

  bashy sdlc runs
  bashy sdlc qa RUN_ID --status accepted --note "local smoke passed"
  bashy sdlc approve RUN_ID --note "approved for GitHub rollout"
  bashy sdlc publish github-pages RUN_ID --branch main
  bashy sdlc resolve RUN_ID --status resolved --note "GitHub Pages verified"

This keeps confidential discussion and runtime output local. Only source commits
and intentional external deployment signals are pushed back to GitHub.

Validation helpers

Check that text is present or absent in a URL:

  bashy sdlc verify --url https://example.com --present "Projects"
  bashy sdlc verify --url https://example.com --absent "Miscellaneous"

Check a local file:

  bashy sdlc verify --file ./index.html --present "Projects"

Inspect a live web page when bashy web is available:

  bashy web inspect https://example.com --contains "Projects"

Check GitHub Actions or Pages deployment status, for GitHub-backed deployments:

  bashy sdlc deploy-status --repo owner/repo --branch main

Run local guard checks before pushing:

  bashy sdlc guard
  bashy sdlc guard --check "go test ./..."

Common workflows

Local human-triggered change:

  bashy sdlc --background --issue-file request.md --cwd /path/to/repo --timeout 45m
  bashy sdlc watch --follow
  bashy sdlc deploy-status --repo owner/repo --branch main
  bashy sdlc verify --url https://example.com --absent "Old text"

Background or CI-triggered change:

  bashy sdlc tick --issue-id 123 --issue-url https://github.com/owner/repo/issues/123 \
    --issue-title "Fix broken deploy" --json --timeout 60m

Incomplete run recovery:

  bashy sdlc status
  bashy sdlc --issue "Recover the incomplete SDLC run. Inspect current git state, preserve user changes, finish tests, commit, and push only the appropriate changes."

Deployment gate

SDLC may inspect validation and deployment target status, but rollout should
remain gated by explicit approval unless your project config and operating policy
say otherwise.
`

// Config describes SDLC's boundary: intake system, agent roles, and deployment
// targets. Implementation details stay delegated to conductor/review/QA agents.
type Config struct {
	Conductor RoleConfig            `json:"conductor" yaml:"conductor"`
	Reviewer  RoleConfig            `json:"reviewer,omitempty" yaml:"reviewer,omitempty"`
	QA        RoleConfig            `json:"qa,omitempty" yaml:"qa,omitempty"`
	Intake    IntakeConfig          `json:"intake" yaml:"intake"`
	Deploy    DeploymentConfig      `json:"deployment" yaml:"deployment"`
	Metadata  map[string]string     `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Policies  map[string]string     `json:"policies,omitempty" yaml:"policies,omitempty"`
	Agents    map[string]RoleConfig `json:"agents,omitempty" yaml:"agents,omitempty"`
}

type RoleConfig struct {
	Agent string `json:"agent" yaml:"agent"`
}

type IntakeConfig struct {
	Provider   string   `json:"provider" yaml:"provider"`
	Repository string   `json:"repository,omitempty" yaml:"repository,omitempty"`
	Query      string   `json:"query,omitempty" yaml:"query,omitempty"`
	Labels     []string `json:"labels,omitempty" yaml:"labels,omitempty"`
}

type DeploymentConfig struct {
	Staging    TargetConfig `json:"staging" yaml:"staging"`
	Production TargetConfig `json:"production" yaml:"production"`
	// Order is the promotion sequence the baton walks (default dev→qa→prod). A
	// change may only advance to the env immediately after the one it has
	// reached — no skipping. Each env's deploy is a bashy dag target (the dag
	// decides how); prod stays owner-gated by the prod_approval policy.
	Order []string `json:"order,omitempty" yaml:"order,omitempty"`
	// Dag is the deploy DAG file promote runs per env (default deploy.md; each
	// env is the `deploy-<env>` target). Empty leaves deploy to the label-fired
	// path.
	Dag string `json:"dag,omitempty" yaml:"dag,omitempty"`
}

// EnvOrder returns the configured promotion sequence, defaulting to dev→qa→prod.
func (d DeploymentConfig) EnvOrder() []string {
	var out []string
	for _, e := range d.Order {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return []string{"dev", "qa", "prod"}
	}
	return out
}

// ValidPromotion reports whether advancing the baton from -> to is a legal single
// step: `to` must be the env immediately after `from` in the order (and `from`
// must be the first env, or empty, when `to` is first). A reason is returned when
// invalid. An unknown `to` is rejected; an empty `from` is allowed only for the
// first env.
func ValidPromotion(cfg Config, from, to string) (bool, string) {
	order := cfg.Deploy.EnvOrder()
	from = strings.ToLower(strings.TrimSpace(from))
	to = strings.ToLower(strings.TrimSpace(to))
	toIdx := indexOf(order, to)
	if toIdx < 0 {
		return false, fmt.Sprintf("env %q is not in the promotion order %v", to, order)
	}
	if toIdx == 0 {
		if from == "" || from == to {
			return true, ""
		}
		return false, fmt.Sprintf("%q is the first env; promote to it without --from", to)
	}
	want := order[toIdx-1]
	if from == want {
		return true, ""
	}
	if from == "" {
		return false, fmt.Sprintf("promote to %q first (the step before %q)", want, to)
	}
	return false, fmt.Sprintf("cannot promote %q → %q; %q must come from %q (order %v)", from, to, to, want, order)
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

type TargetConfig struct {
	Name        string `json:"name,omitempty" yaml:"name,omitempty"`
	Host        string `json:"host,omitempty" yaml:"host,omitempty"`
	Environment string `json:"environment,omitempty" yaml:"environment,omitempty"`
	Command     string `json:"command,omitempty" yaml:"command,omitempty"`
	Healthcheck string `json:"healthcheck,omitempty" yaml:"healthcheck,omitempty"`
	Rollback    string `json:"rollback,omitempty" yaml:"rollback,omitempty"`
}

type Issue struct {
	ID    string
	URL   string
	Title string
	Body  string
}

type DelegateOptions struct {
	ConfigPath string
	Config     ConfigOverrides
	Issue      Issue
	IssueText  string
	IssueFile  string
	DryRun     bool
	JSON       bool
	Timeout    time.Duration
	Cwd        string
	Sandbox    string
	Background bool
	RunsDir    string
}

type ConfigOverrides struct {
	NoConfig bool

	ConductorAgent string
	ReviewerAgent  string
	QAAgent        string

	IntakeProvider string
	IntakeRepo     string
	IntakeQuery    string
	IntakeLabels   []string

	StagingName        string
	StagingHost        string
	StagingEnvironment string
	StagingCommand     string
	StagingHealthcheck string
	StagingRollback    string

	ProductionName        string
	ProductionHost        string
	ProductionEnvironment string
	ProductionCommand     string
	ProductionHealthcheck string
	ProductionRollback    string

	Metadata []string
	Policies []string
	Agents   []string
}

type DelegateResult struct {
	SchemaVersion string      `json:"schema_version"`
	Status        string      `json:"status"`
	ConfigPath    string      `json:"config_path,omitempty"`
	DefaultConfig bool        `json:"default_config,omitempty"`
	Conductor     string      `json:"conductor,omitempty"`
	RunID         string      `json:"run_id,omitempty"`
	RunPath       string      `json:"run_path,omitempty"`
	LogPath       string      `json:"log_path,omitempty"`
	Issue         Issue       `json:"issue"`
	Brief         string      `json:"brief,omitempty"`
	Chat          chat.Result `json:"chat,omitempty"`
	// AutoSelected is true when Issue was picked from the intake provider (github)
	// rather than passed on the CLI — the signal Delegate uses to claim it.
	AutoSelected bool `json:"auto_selected,omitempty"`
}

type RunRecord struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"id"`
	ReferenceID   string      `json:"reference_id"`
	Status        string      `json:"status"`
	PID           int         `json:"pid,omitempty"`
	IssueTitle    string      `json:"issue_title,omitempty"`
	IssueID       string      `json:"issue_id,omitempty"`
	IssueURL      string      `json:"issue_url,omitempty"`
	Conductor     string      `json:"conductor,omitempty"`
	Cwd           string      `json:"cwd,omitempty"`
	Command       []string    `json:"command,omitempty"`
	RunPath       string      `json:"run_path"`
	LogPath       string      `json:"log_path"`
	BriefPath     string      `json:"brief_path"`
	StartedAt     time.Time   `json:"started_at"`
	FinishedAt    time.Time   `json:"finished_at,omitempty"`
	ExitCode      int         `json:"exit_code,omitempty"`
	Error         string      `json:"error,omitempty"`
	QA            *GateReview `json:"qa,omitempty"`
	Approval      *Approval   `json:"approval,omitempty"`
	Resolution    *Resolution `json:"resolution,omitempty"`
}

type GateReview struct {
	Status     string    `json:"status"`
	ReviewedAt time.Time `json:"reviewed_at"`
	ReviewedBy string    `json:"reviewed_by,omitempty"`
	Note       string    `json:"note,omitempty"`
}

type Approval struct {
	Status     string    `json:"status"`
	ApprovedAt time.Time `json:"approved_at"`
	ApprovedBy string    `json:"approved_by,omitempty"`
	Note       string    `json:"note,omitempty"`
}

type Resolution struct {
	Status     string    `json:"status"`
	ResolvedAt time.Time `json:"resolved_at"`
	ResolvedBy string    `json:"resolved_by,omitempty"`
	Note       string    `json:"note,omitempty"`
}

type ConfigExplanation struct {
	SchemaVersion string `json:"schema_version"`
	Source        string `json:"source"`
	ConfigPath    string `json:"config_path,omitempty"`
	Conductor     string `json:"conductor"`
	Intake        string `json:"intake"`
	Staging       string `json:"staging"`
	Production    string `json:"production"`
}

type VerifyOptions struct {
	Target  string
	Present []string
	Absent  []string
	Timeout time.Duration
}

type VerifyResult struct {
	SchemaVersion string        `json:"schema_version"`
	Status        string        `json:"status"`
	Target        string        `json:"target"`
	Checks        []VerifyCheck `json:"checks"`
}

type VerifyCheck struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

type DeployStatus struct {
	SchemaVersion string `json:"schema_version"`
	Repo          string `json:"repo"`
	Workflow      string `json:"workflow,omitempty"`
	RunNumber     int    `json:"run_number,omitempty"`
	Status        string `json:"status,omitempty"`
	Conclusion    string `json:"conclusion,omitempty"`
	HTMLURL       string `json:"html_url,omitempty"`
	HeadSHA       string `json:"head_sha,omitempty"`
	Title         string `json:"title,omitempty"`
}

type WatchOptions struct {
	RunsDir  string
	RunID    string
	Follow   bool
	Tail     int
	Interval time.Duration
	JSON     bool
}

type WorkspaceOptions struct {
	Dir               string `json:"dir"`
	Origin            string `json:"origin"`
	Baseline          string `json:"baseline"`
	Upstream          string `json:"upstream,omitempty"`
	AllowGitHubOrigin bool   `json:"allow_github_origin,omitempty"`
	JSON              bool   `json:"-"`
}

type WorkspaceResult struct {
	SchemaVersion string    `json:"schema_version"`
	Status        string    `json:"status"`
	Dir           string    `json:"dir"`
	Origin        string    `json:"origin"`
	Baseline      string    `json:"baseline"`
	Upstream      string    `json:"upstream,omitempty"`
	MetadataPath  string    `json:"metadata_path"`
	Created       bool      `json:"created"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type PublishOptions struct {
	RunsDir string
	RunID   string
	Remote  string
	Repo    string
	Branch  string
	Source  string
	Cwd     string
	DryRun  bool
	JSON    bool
}

type PublishResult struct {
	SchemaVersion string    `json:"schema_version"`
	Status        string    `json:"status"`
	RunID         string    `json:"run_id"`
	Cwd           string    `json:"cwd"`
	Target        string    `json:"target"`
	Source        string    `json:"source"`
	Branch        string    `json:"branch"`
	Command       []string  `json:"command"`
	Output        string    `json:"output,omitempty"`
	PublishedAt   time.Time `json:"published_at,omitempty"`
}

type PagesOnceOptions struct {
	// Intake: Provider=github|loom selects the next eligible issue; --issue /
	// --issue-file supply one directly (no forge). Direct intake wins when set.
	Provider    string // "github" | "loom" | "local" (default: local if Issue* set, else loom)
	GitHubToken string
	Issue       Issue
	IssueText   string
	IssueFile   string
	// RequireInitiate gates github intake on the sdlc:go label. Default false for
	// pages — "file an issue, the site updates" needs no ceremony.
	RequireInitiate bool

	LoomURL      string
	Token        string
	IntakeRepo   string
	WorkspaceDir string
	RunsDir      string
	BuildCommand string
	PublicURL    string
	Branch       string
	Repo         string // explicit GitHub publish target owner/repo (else the workspace's upstream remote)
	Conductor    string
	Sandbox      string
	Timeout      time.Duration
	DryRun       bool
	JSON         bool
}

type PagesOnceResult struct {
	SchemaVersion string        `json:"schema_version"`
	Status        string        `json:"status"`
	Issue         *Issue        `json:"issue,omitempty"`
	RunID         string        `json:"run_id,omitempty"`
	WorkspaceDir  string        `json:"workspace_dir"`
	BuildCommand  string        `json:"build_command,omitempty"`
	BuildOutput   string        `json:"build_output,omitempty"`
	Publish       PublishResult `json:"publish,omitempty"`
	Verify        *VerifyResult `json:"verify,omitempty"`
	Error         string        `json:"error,omitempty"`
}

type LoomIssue struct {
	Number int         `json:"number"`
	Title  string      `json:"title"`
	Body   string      `json:"body"`
	HTML   string      `json:"html_url"`
	State  string      `json:"state"`
	Labels []LoomLabel `json:"labels"`
}

type LoomLabel struct {
	Name string `json:"name"`
}

// NewSDLCCmd returns the `bashy sdlc` command tree.
func NewSDLCCmd() *cobra.Command {
	var opt DelegateOptions
	cmd := &cobra.Command{
		Use:   "sdlc [--issue TEXT | --issue-file PATH]",
		Short: "route intake issues through agentic implementation and deployment gates",
		Long:  strings.TrimSpace(sdlcLongHelp),
		Example: strings.TrimSpace(`
bashy sdlc --issue "Remove the obsolete Miscellaneous section"
bashy sdlc --issue-file ./request.md --cwd ../site --timeout 45m
bashy sdlc --no-config --conductor codex --intake-provider github --intake-repo owner/repo --staging staging --production production --issue "Fix deploy"
bashy sdlc guide
bashy sdlc init
bashy sdlc config explain
bashy sdlc verify --url https://example.com --absent "Miscellaneous"
bashy sdlc deploy-status --repo owner/repo --branch main
`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(opt.IssueText) == "" && strings.TrimSpace(opt.IssueFile) == "" &&
				strings.TrimSpace(opt.Issue.Title) == "" && strings.TrimSpace(opt.Issue.Body) == "" {
				return cmd.Help()
			}
			opt.JSON = opt.JSON || os.Getenv("BASHY_AGENTIC") != ""
			res, err := Delegate(cmd.Context(), opt)
			if opt.JSON {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				printDelegateOutput(cmd.OutOrStdout(), res)
			}
			return err
		},
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	bindIssueFlags(cmd, &opt)
	bindDelegateFlags(cmd, &opt)
	cmd.AddCommand(
		newGuideCmd(), newInitCmd(), newDoctorCmd(), newConfigCmd(), newStatusCmd(), newIssueCmd(),
		newBriefCmd(), newDelegateCmd(), newTickCmd(), newRunsCmd(), newWatchCmd(), newQACmd(),
		newApproveCmd(), newRolloutCmd(), newPromoteCmd(), newResolveCmd(), newVerifyCmd(), newDeployStatusCmd(), newGuardCmd(),
		newWorkspaceCmd(), newPublishCmd(), newPagesCmd(), newSuperviseCmd(), newServiceCmd(), newMirrorCmd(),
		newPushSourceCmd(), newDeployOnceCmd(), newChangesCmd(),
	)
	return cmd
}

func newGuideCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guide",
		Short: "print a self-contained SDLC usage guide",
		Long:  "Prints the embedded SDLC guide. The output is bundled into bashy and does not depend on repo documentation.",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprint(cmd.OutOrStdout(), strings.TrimSpace(sdlcGuide))
			fmt.Fprintln(cmd.OutOrStdout())
		},
	}
	return cmd
}

func newInitCmd() *cobra.Command {
	var configPath string
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "write a starter SDLC config",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !force {
				if _, err := os.Stat(configPath); err == nil {
					return fmt.Errorf("sdlc: %s already exists; pass --force to overwrite", configPath)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(configPath, []byte(DefaultConfigYAML()), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", configPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", ".bashy/sdlc.yaml", "SDLC config file")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config")
	return cmd
}

func newDoctorCmd() *cobra.Command {
	var configPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "validate SDLC config",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig(configPath)
			out := map[string]any{
				"schema_version": schemaVersion,
				"status":         "ok",
				"config_path":    configPath,
			}
			if err != nil {
				out["status"] = "error"
				out["error"] = err.Error()
			} else {
				out["conductor"] = cfg.Conductor.Agent
				out["intake"] = cfg.Intake.Provider
				out["staging"] = cfg.Deploy.Staging.Name
				out["production"] = cfg.Deploy.Production.Name
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(out)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "ok: conductor=%s intake=%s staging=%s production=%s\n",
					cfg.Conductor.Agent, cfg.Intake.Provider, displayTarget(cfg.Deploy.Staging), displayTarget(cfg.Deploy.Production))
			}
			return err
		},
	}
	cmd.Flags().StringVar(&configPath, "config", ".bashy/sdlc.yaml", "SDLC config file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print a bashy-sdlc-v1 JSON envelope")
	return cmd
}

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "inspect SDLC configuration"}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newConfigExplainCmd())
	return cmd
}

func newConfigExplainCmd() *cobra.Command {
	var configPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "explain",
		Short: "show which SDLC config/defaults are active",
		RunE: func(cmd *cobra.Command, args []string) error {
			exp, err := ExplainConfig(configPath)
			if err != nil {
				return err
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(exp)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source=%s conductor=%s intake=%s staging=%s production=%s\n",
				exp.Source, exp.Conductor, exp.Intake, exp.Staging, exp.Production)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", ".bashy/sdlc.yaml", "SDLC config file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var configPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "summarize local SDLC state",
		RunE: func(cmd *cobra.Command, args []string) error {
			exp, err := ExplainConfig(configPath)
			if err != nil {
				return err
			}
			status := map[string]any{
				"schema_version": schemaVersion,
				"config":         exp,
				"git":            gitSummary("."),
				"issues":         listIssueFiles(".bashy/generated/sdlc/issues"),
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(status)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "config: %s conductor=%s intake=%s\n", exp.Source, exp.Conductor, exp.Intake)
			if g, _ := status["git"].(map[string]any); g != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "git: branch=%v dirty=%v ahead=%v behind=%v\n", g["branch"], g["dirty"], g["ahead"], g["behind"])
			}
			if issues, _ := status["issues"].([]string); len(issues) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "issues: %d local\n", len(issues))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", ".bashy/sdlc.yaml", "SDLC config file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newIssueCmd() *cobra.Command {
	var text, file, dir string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "issue",
		Short: "record a local SDLC issue",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && strings.TrimSpace(text) == "" {
				text = strings.Join(args, " ")
			}
			issue, path, err := SaveLocalIssue(text, file, dir)
			if err != nil {
				return err
			}
			out := map[string]any{"schema_version": schemaVersion, "status": "saved", "path": path, "issue": issue}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(out)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "saved %s\n", path)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&text, "text", "", "issue/request text")
	cmd.Flags().StringVar(&file, "file", "", "file containing issue/request text")
	cmd.Flags().StringVar(&dir, "dir", ".bashy/generated/sdlc/issues", "local issue directory")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newBriefCmd() *cobra.Command {
	var opt DelegateOptions
	cmd := &cobra.Command{
		Use:   "brief --issue-title TITLE",
		Short: "render the conductor brief without invoking an agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			opt.DryRun = true
			res, err := Prepare(cmd.Context(), opt)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), res.Brief)
			return nil
		},
	}
	bindIssueFlags(cmd, &opt)
	return cmd
}

func newDelegateCmd() *cobra.Command {
	return newDelegateLikeCmd("delegate --issue-title TITLE", "send an intake issue to the configured conductor agent")
}

func newTickCmd() *cobra.Command {
	return newDelegateLikeCmd("tick --issue-title TITLE", "run one externally-triggered SDLC cycle for one intake issue")
}

func newRunsCmd() *cobra.Command {
	var dir string
	var asJSON bool
	var limit int
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "list local SDLC conductor runs",
		RunE: func(cmd *cobra.Command, args []string) error {
			runs, err := ListRuns(dir, limit)
			if err != nil {
				return err
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(map[string]any{"schema_version": schemaVersion, "runs": runs})
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			for _, run := range runs {
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s conductor=%s issue=%q approved=%t resolved=%t log=%s\n",
					run.ReferenceID, run.Status, run.Conductor, run.IssueTitle, RunApproved(run), run.Resolution != nil, run.LogPath)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of runs to list")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newWatchCmd() *cobra.Command {
	var dir string
	var follow bool
	var tail int
	var interval time.Duration
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "watch [RUN_ID]",
		Short: "show or follow a local SDLC conductor run",
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := ""
			if len(args) > 0 {
				runID = args[0]
			}
			return WatchRun(cmd.Context(), cmd.OutOrStdout(), WatchOptions{
				RunsDir:  dir,
				RunID:    runID,
				Follow:   follow,
				Tail:     tail,
				Interval: interval,
				JSON:     asJSON || os.Getenv("BASHY_AGENTIC") != "",
			})
		},
	}
	cmd.Flags().StringVar(&dir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	cmd.Flags().BoolVar(&follow, "follow", false, "follow log output until the run exits")
	cmd.Flags().IntVar(&tail, "tail", 80, "number of log lines to show")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "follow polling interval")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newSuperviseCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:    "supervise RUN_ID -- COMMAND [ARG...]",
		Short:  "internal SDLC background run supervisor",
		Hidden: true,
		Args:   cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return SuperviseRun(cmd.Context(), dir, args[0], args[1:])
		},
	}
	cmd.Flags().StringVar(&dir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	return cmd
}

func newApproveCmd() *cobra.Command {
	var dir, note, by string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "approve RUN_ID",
		Short: "approve a specific local SDLC run reference",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := ApproveRun(dir, args[0], note, by)
			if err != nil {
				return err
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(run)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s status=%s note=%q\n", run.ReferenceID, run.Status, note)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	cmd.Flags().StringVar(&note, "note", "", "approval note")
	cmd.Flags().StringVar(&by, "by", "", "approver identity")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newQACmd() *cobra.Command {
	var dir, status, note, by string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "qa RUN_ID",
		Short: "record optional QA accept/reject for a local SDLC run reference",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := ReviewRun(dir, args[0], status, note, by)
			if err != nil {
				return err
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(run)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "qa %s status=%s note=%q\n", run.ReferenceID, run.QA.Status, note)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	cmd.Flags().StringVar(&status, "status", "accepted", "QA status: accepted or rejected")
	cmd.Flags().StringVar(&note, "note", "", "QA note")
	cmd.Flags().StringVar(&by, "by", "", "QA reviewer identity")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newRolloutCmd() *cobra.Command {
	var opt DelegateOptions
	var note, by string
	cmd := &cobra.Command{
		Use:   "rollout RUN_ID",
		Short: "delegate deployment rollout for an approved local SDLC run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := LoadRunByID(opt.RunsDir, args[0])
			if err != nil {
				return err
			}
			if !RunApproved(run) {
				return fmt.Errorf("sdlc: run %s is not approved; run `bashy sdlc approve %s --note ...` first", args[0], args[0])
			}
			opt.Issue = Issue{
				ID:    run.ReferenceID,
				URL:   run.IssueURL,
				Title: "Deployment rollout for " + run.IssueTitle,
				Body:  BuildRolloutInstruction(run, note, by),
			}
			opt.JSON = opt.JSON || os.Getenv("BASHY_AGENTIC") != ""
			res, err := Delegate(cmd.Context(), opt)
			if opt.JSON {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				printDelegateOutput(cmd.OutOrStdout(), res)
			}
			return err
		},
	}
	bindIssueFlags(cmd, &opt)
	bindDelegateFlags(cmd, &opt)
	cmd.Flags().StringVar(&note, "note", "", "rollout note")
	cmd.Flags().StringVar(&by, "by", "", "operator identity")
	return cmd
}

func newResolveCmd() *cobra.Command {
	var dir, status, note, by string
	var asJSON, noClose bool
	cmd := &cobra.Command{
		Use:   "resolve RUN_ID",
		Short: "mark a local SDLC run reference resolved",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := ResolveLifecycleRun(dir, args[0], status, note, by)
			if err != nil {
				return err
			}
			// Reflect the resolution on the GitHub issue: clear the claim, apply
			// sdlc:done, close on success. No-op for non-github/tokenless runs.
			comment := note
			if comment == "" && (run.Status == "resolved" || run.Status == "rolled-out") {
				comment = fmt.Sprintf("SDLC resolved this issue (%s). Reference: %s", run.Status, run.ReferenceID)
			}
			if _, serr := SyncGitHubResolution(cmd.Context(), run, run.Status, !noClose, comment, GitHubToken()); serr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: github lifecycle sync failed: %v\n", serr)
			}
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(run)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s note=%q\n", run.ReferenceID, run.Status, note)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	cmd.Flags().StringVar(&status, "status", "resolved", "resolution status, for example resolved, rejected, rolled-out, failed")
	cmd.Flags().StringVar(&note, "note", "", "resolution note")
	cmd.Flags().StringVar(&by, "by", "", "resolver identity")
	cmd.Flags().BoolVar(&noClose, "no-close", false, "do not close the GitHub issue on successful resolution")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newVerifyCmd() *cobra.Command {
	var target string
	var absent, present []string
	var asJSON bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "verify --url URL --absent TEXT",
		Short: "verify URL/file content with present/absent checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := VerifyContent(cmd.Context(), VerifyOptions{
				Target:  target,
				Present: present,
				Absent:  absent,
				Timeout: timeout,
			})
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", res.Status, res.Target)
				for _, c := range res.Checks {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s %q: %s\n", c.Kind, c.Text, c.Status)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&target, "url", "", "URL to inspect")
	cmd.Flags().StringVar(&target, "file", "", "local file to inspect")
	cmd.Flags().StringArrayVar(&absent, "absent", nil, "text that must be absent")
	cmd.Flags().StringArrayVar(&present, "present", nil, "text that must be present")
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "HTTP timeout")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newDeployStatusCmd() *cobra.Command {
	var repo, branch string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "deploy-status",
		Short: "show latest GitHub Actions/Pages deployment status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				repo = inferGitHubRepo(".")
			}
			res, err := GitHubDeployStatus(cmd.Context(), repo, branch)
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%s #%v %s/%s %s\n", res.Workflow, res.RunNumber, res.Status, res.Conclusion, res.HTMLURL)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub repo owner/name; defaults from origin remote")
	cmd.Flags().StringVar(&branch, "branch", "", "branch filter")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newGuardCmd() *cobra.Command {
	var checks []string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "run local pre-push SDLC guard checks",
		RunE: func(cmd *cobra.Command, args []string) error {
			res := Guard(".", checks)
			if asJSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\n", res["status"])
				if items, _ := res["checks"].([]map[string]any); items != nil {
					for _, item := range items {
						fmt.Fprintf(cmd.OutOrStdout(), "  %v: %v\n", item["name"], item["status"])
					}
				}
			}
			if res["status"] != "ok" {
				return errors.New("sdlc: guard failed")
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&checks, "check", nil, "command to run as a guard check")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "prepare guarded mutable SDLC workspaces",
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newWorkspacePrepareCmd())
	return cmd
}

func newWorkspacePrepareCmd() *cobra.Command {
	var opt WorkspaceOptions
	cmd := &cobra.Command{
		Use:   "prepare --origin LOOM_REPO_URL --dir PATH [--baseline MIRROR_URL] [--upstream URL]",
		Short: "prepare a local workspace on the loom source-of-truth repo (single-repo; --baseline opts into the legacy guarded two-repo mode)",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := PrepareWorkspace(cmd.Context(), opt)
			if opt.JSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "workspace ready: %s\n", res.Dir)
				fmt.Fprintf(cmd.OutOrStdout(), "  origin   %s\n", res.Origin)
				if res.Baseline != "" && res.Baseline != res.Origin {
					fmt.Fprintf(cmd.OutOrStdout(), "  baseline %s (immutable, push disabled)\n", res.Baseline)
				}
				if res.Upstream != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  upstream %s (push disabled)\n", res.Upstream)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&opt.Dir, "dir", "", "local mutable workspace directory")
	cmd.Flags().StringVar(&opt.Origin, "origin", "", "the loom source-of-truth repo URL (single-repo: clone + push here)")
	cmd.Flags().StringVar(&opt.Baseline, "baseline", "", "optional immutable Loom mirror to clone from (legacy guarded two-repo mode; omit for single-repo)")
	cmd.Flags().StringVar(&opt.Upstream, "upstream", "", "optional upstream URL (GitHub/GitLab/…), configured read-only")
	cmd.Flags().BoolVar(&opt.AllowGitHubOrigin, "allow-github-origin", false, "allow origin to point at github.com instead of Loom")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print JSON")
	return cmd
}

func newPublishCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "publish approved SDLC results to external targets",
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newPublishGitHubPagesCmd())
	return cmd
}

func newPublishGitHubPagesCmd() *cobra.Command {
	var opt PublishOptions
	cmd := &cobra.Command{
		Use:   "github-pages RUN_ID",
		Short: "push an approved workspace result to a GitHub Pages branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opt.RunID = args[0]
			res, err := PublishGitHubPages(cmd.Context(), opt)
			if opt.JSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if err == nil {
				if opt.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "dry-run publish %s -> %s:%s\n", res.Source, res.Target, res.Branch)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "published %s -> %s:%s\n", res.Source, res.Target, res.Branch)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&opt.RunsDir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
	cmd.Flags().StringVar(&opt.Cwd, "cwd", "", "workspace directory; defaults to the run cwd or current directory")
	cmd.Flags().StringVar(&opt.Remote, "remote", "upstream", "git remote containing the GitHub target URL")
	cmd.Flags().StringVar(&opt.Repo, "repo", "", "GitHub repo owner/name; overrides --remote URL")
	cmd.Flags().StringVar(&opt.Branch, "branch", "main", "GitHub branch to update")
	cmd.Flags().StringVar(&opt.Source, "source", "HEAD", "local ref to publish")
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "print the push command without running it")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print JSON")
	return cmd
}

func newPagesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pages",
		Short: "run the zero-config personal pages SDLC workflow",
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newPagesOnceCmd())
	return cmd
}

func newPagesOnceCmd() *cobra.Command {
	var opt PagesOnceOptions
	cmd := &cobra.Command{
		Use:   "once",
		Short: "process one eligible Loom issue and publish an approved GitHub Pages update",
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := RunPagesOnce(cmd.Context(), opt)
			if opt.JSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if err == nil {
				if res.Issue != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "processed issue %s: %s\n", res.Issue.ID, res.Issue.Title)
				}
				if res.RunID != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "sdlc reference: %s\n", res.RunID)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "status: %s\n", res.Status)
			}
			return err
		},
	}
	// Intake (pick one): a provider queue, or a direct issue.
	cmd.Flags().StringVar(&opt.Provider, "intake-provider", "", "intake provider: github | loom (default loom; ignored when --issue/--issue-file given)")
	cmd.Flags().StringVar(&opt.IntakeRepo, "intake-repo", "", "issue-intake repo owner/name (github) or Loom mirror repo")
	cmd.Flags().StringVar(&opt.GitHubToken, "github-token", "", "GitHub token; defaults to GITHUB_TOKEN/GIT_TOKEN")
	cmd.Flags().BoolVar(&opt.RequireInitiate, "require-initiate", false, "github: only pick issues labelled sdlc:go")
	cmd.Flags().StringVar(&opt.IssueText, "issue", "", "direct issue/request text (no forge)")
	cmd.Flags().StringVar(&opt.IssueFile, "issue-file", "", "file containing the issue/request text")
	// Loom-specific.
	cmd.Flags().StringVar(&opt.LoomURL, "loom-url", "http://127.0.0.1:31880", "Loom/Gitea base URL")
	cmd.Flags().StringVar(&opt.Token, "token", "", "Loom API token; defaults to BASHY_LOOM_TOKEN or GITEA_TOKEN")
	// Workspace + publish.
	cmd.Flags().StringVar(&opt.WorkspaceDir, "workspace", "", "guarded local workspace directory")
	cmd.Flags().StringVar(&opt.RunsDir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory, relative to workspace when not absolute")
	cmd.Flags().StringVar(&opt.BuildCommand, "build", "", "optional build/test command to run before publish (empty = none)")
	cmd.Flags().StringVar(&opt.PublicURL, "public-url", "", "public page URL to verify after publish")
	cmd.Flags().StringVar(&opt.Branch, "branch", "main", "GitHub Pages source branch")
	cmd.Flags().StringVar(&opt.Repo, "repo", "", "GitHub publish target owner/repo (default: the workspace's upstream remote)")
	cmd.Flags().StringVar(&opt.Conductor, "conductor", "", "conductor agent override")
	cmd.Flags().StringVar(&opt.Sandbox, "sandbox", "danger-full-access", "conductor sandbox")
	cmd.Flags().DurationVar(&opt.Timeout, "timeout", 0, "conductor timeout")
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "select and brief issue without invoking conductor or publishing")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print JSON")
	return cmd
}

func newDelegateLikeCmd(use, short string) *cobra.Command {
	var opt DelegateOptions
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			opt.JSON = opt.JSON || os.Getenv("BASHY_AGENTIC") != ""
			res, err := Delegate(cmd.Context(), opt)
			if opt.JSON {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				printDelegateOutput(cmd.OutOrStdout(), res)
			}
			return err
		},
	}
	bindIssueFlags(cmd, &opt)
	bindDelegateFlags(cmd, &opt)
	return cmd
}

func printDelegateOutput(out io.Writer, res DelegateResult) {
	if res.Chat.Output != "" {
		fmt.Fprint(out, res.Chat.Output)
		if !strings.HasSuffix(res.Chat.Output, "\n") {
			fmt.Fprintln(out)
		}
	}
	if res.RunID != "" {
		fmt.Fprintf(out, "sdlc reference: %s\n", res.RunID)
	}
	if res.LogPath != "" {
		fmt.Fprintf(out, "sdlc log: %s\n", res.LogPath)
	}
}

func bindDelegateFlags(cmd *cobra.Command, opt *DelegateOptions) {
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "print the resolved agent invocation without running it")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print a bashy-sdlc-v1 JSON envelope")
	cmd.Flags().DurationVar(&opt.Timeout, "timeout", 0, "conductor timeout, for example 45m")
	cmd.Flags().StringVar(&opt.Cwd, "cwd", "", "working directory for the conductor")
	cmd.Flags().StringVar(&opt.Sandbox, "sandbox", "danger-full-access", "conductor sandbox for agents that support it")
	cmd.Flags().BoolVar(&opt.Background, "background", false, "start the conductor in the background and write a local run log")
	cmd.Flags().StringVar(&opt.RunsDir, "runs-dir", ".bashy/generated/sdlc/runs", "local SDLC runs directory")
}

func bindIssueFlags(cmd *cobra.Command, opt *DelegateOptions) {
	cmd.Flags().StringVar(&opt.ConfigPath, "config", ".bashy/sdlc.yaml", "SDLC config file")
	bindConfigOverrideFlags(cmd, &opt.Config)
	cmd.Flags().StringVar(&opt.IssueText, "issue", "", "local issue/request text")
	cmd.Flags().StringVar(&opt.IssueFile, "issue-file", "", "file containing local issue/request text")
	cmd.Flags().StringVar(&opt.Issue.ID, "issue-id", "", "issue number or external issue id")
	cmd.Flags().StringVar(&opt.Issue.URL, "issue-url", "", "issue URL")
	cmd.Flags().StringVar(&opt.Issue.Title, "issue-title", "", "issue title")
	cmd.Flags().StringVar(&opt.Issue.Body, "issue-body", "", "issue body")
	cmd.Flags().String("issue-body-file", "", "file containing the issue body")
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		file, _ := cmd.Flags().GetString("issue-body-file")
		if file != "" {
			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("sdlc: read issue body file: %w", err)
			}
			opt.Issue.Body = string(data)
		}
		return ApplyIssueRequest(opt)
	}
}

func bindConfigOverrideFlags(cmd *cobra.Command, opt *ConfigOverrides) {
	cmd.Flags().BoolVar(&opt.NoConfig, "no-config", false, "ignore .bashy/sdlc.yaml and use embedded defaults plus CLI overrides")
	cmd.Flags().StringVar(&opt.ConductorAgent, "conductor", "", "conductor agent override")
	cmd.Flags().StringVar(&opt.ReviewerAgent, "reviewer", "", "review agent override")
	cmd.Flags().StringVar(&opt.QAAgent, "qa", "", "QA agent override")
	cmd.Flags().StringVar(&opt.IntakeProvider, "intake-provider", "", "intake provider override")
	cmd.Flags().StringVar(&opt.IntakeRepo, "intake-repo", "", "intake repository override, for example owner/repo")
	cmd.Flags().StringVar(&opt.IntakeQuery, "intake-query", "", "intake query override")
	cmd.Flags().StringArrayVar(&opt.IntakeLabels, "intake-label", nil, "intake label override; may be repeated")
	cmd.Flags().StringVar(&opt.StagingName, "staging", "", "staging target name override")
	cmd.Flags().StringVar(&opt.StagingHost, "staging-host", "", "staging target host override")
	cmd.Flags().StringVar(&opt.StagingEnvironment, "staging-env", "", "staging environment override")
	cmd.Flags().StringVar(&opt.StagingCommand, "staging-command", "", "staging deployment command override")
	cmd.Flags().StringVar(&opt.StagingHealthcheck, "staging-healthcheck", "", "staging healthcheck override")
	cmd.Flags().StringVar(&opt.StagingRollback, "staging-rollback", "", "staging rollback command override")
	cmd.Flags().StringVar(&opt.ProductionName, "production", "", "production target name override")
	cmd.Flags().StringVar(&opt.ProductionHost, "production-host", "", "production target host override")
	cmd.Flags().StringVar(&opt.ProductionEnvironment, "production-env", "", "production environment override")
	cmd.Flags().StringVar(&opt.ProductionCommand, "production-command", "", "production deployment command override")
	cmd.Flags().StringVar(&opt.ProductionHealthcheck, "production-healthcheck", "", "production healthcheck override")
	cmd.Flags().StringVar(&opt.ProductionRollback, "production-rollback", "", "production rollback command override")
	cmd.Flags().StringArrayVar(&opt.Metadata, "metadata", nil, "metadata override as key=value; may be repeated")
	cmd.Flags().StringArrayVar(&opt.Policies, "policy", nil, "policy override as key=value; may be repeated")
	cmd.Flags().StringArrayVar(&opt.Agents, "agent", nil, "named agent override as name=agent; may be repeated")
}

func ApplyIssueRequest(opt *DelegateOptions) error {
	if opt == nil {
		return nil
	}
	text := strings.TrimSpace(opt.IssueText)
	if strings.TrimSpace(opt.IssueFile) != "" {
		data, err := os.ReadFile(opt.IssueFile)
		if err != nil {
			return fmt.Errorf("sdlc: read issue file: %w", err)
		}
		text = strings.TrimSpace(string(data))
	}
	if text == "" {
		return nil
	}
	title, body := splitIssueRequest(text)
	if strings.TrimSpace(opt.Issue.Title) == "" {
		opt.Issue.Title = title
	}
	if strings.TrimSpace(opt.Issue.Body) == "" && body != "" {
		opt.Issue.Body = body
	}
	return nil
}

func splitIssueRequest(text string) (title, body string) {
	text = strings.TrimSpace(text)
	for _, line := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			title = s
			break
		}
	}
	if title == "" {
		title = text
	}
	if strings.Contains(text, "\n") {
		body = text
	}
	return title, body
}

func LoadConfig(path string) (Config, error) {
	cfg, _, err := loadConfig(path, false)
	return cfg, err
}

func LoadConfigOrDefault(path string) (Config, bool, error) {
	return loadConfig(path, true)
}

func ExplainConfig(path string) (ConfigExplanation, error) {
	cfg, usedDefault, err := LoadConfigOrDefault(path)
	if err != nil {
		return ConfigExplanation{SchemaVersion: schemaVersion, ConfigPath: path}, err
	}
	source := "file"
	if usedDefault {
		source = "embedded-default"
	}
	return ConfigExplanation{
		SchemaVersion: schemaVersion,
		Source:        source,
		ConfigPath:    path,
		Conductor:     cfg.Conductor.Agent,
		Intake:        cfg.Intake.Provider,
		Staging:       displayTarget(cfg.Deploy.Staging),
		Production:    displayTarget(cfg.Deploy.Production),
	}, nil
}

func ApplyConfigOverrides(cfg Config, opt ConfigOverrides) Config {
	if strings.TrimSpace(opt.ConductorAgent) != "" {
		cfg.Conductor.Agent = strings.TrimSpace(opt.ConductorAgent)
	}
	if strings.TrimSpace(opt.ReviewerAgent) != "" {
		cfg.Reviewer.Agent = strings.TrimSpace(opt.ReviewerAgent)
	}
	if strings.TrimSpace(opt.QAAgent) != "" {
		cfg.QA.Agent = strings.TrimSpace(opt.QAAgent)
	}
	if strings.TrimSpace(opt.IntakeProvider) != "" {
		cfg.Intake.Provider = strings.TrimSpace(opt.IntakeProvider)
	}
	if strings.TrimSpace(opt.IntakeRepo) != "" {
		cfg.Intake.Repository = strings.TrimSpace(opt.IntakeRepo)
	}
	if strings.TrimSpace(opt.IntakeQuery) != "" {
		cfg.Intake.Query = strings.TrimSpace(opt.IntakeQuery)
	}
	if len(opt.IntakeLabels) > 0 {
		cfg.Intake.Labels = trimNonEmpty(opt.IntakeLabels)
	}
	cfg.Deploy.Staging = applyTargetOverrides(cfg.Deploy.Staging, TargetConfig{
		Name:        opt.StagingName,
		Host:        opt.StagingHost,
		Environment: opt.StagingEnvironment,
		Command:     opt.StagingCommand,
		Healthcheck: opt.StagingHealthcheck,
		Rollback:    opt.StagingRollback,
	})
	cfg.Deploy.Production = applyTargetOverrides(cfg.Deploy.Production, TargetConfig{
		Name:        opt.ProductionName,
		Host:        opt.ProductionHost,
		Environment: opt.ProductionEnvironment,
		Command:     opt.ProductionCommand,
		Healthcheck: opt.ProductionHealthcheck,
		Rollback:    opt.ProductionRollback,
	})
	if len(opt.Metadata) > 0 {
		cfg.Metadata = mergeStringMap(cfg.Metadata, parseKeyValueList(opt.Metadata))
	}
	if len(opt.Policies) > 0 {
		cfg.Policies = mergeStringMap(cfg.Policies, parseKeyValueList(opt.Policies))
	}
	if len(opt.Agents) > 0 {
		if cfg.Agents == nil {
			cfg.Agents = map[string]RoleConfig{}
		}
		for key, value := range parseKeyValueList(opt.Agents) {
			cfg.Agents[key] = RoleConfig{Agent: value}
		}
	}
	return cfg
}

func applyTargetOverrides(base TargetConfig, overrides TargetConfig) TargetConfig {
	if strings.TrimSpace(overrides.Name) != "" {
		base.Name = strings.TrimSpace(overrides.Name)
	}
	if strings.TrimSpace(overrides.Host) != "" {
		base.Host = strings.TrimSpace(overrides.Host)
	}
	if strings.TrimSpace(overrides.Environment) != "" {
		base.Environment = strings.TrimSpace(overrides.Environment)
	}
	if strings.TrimSpace(overrides.Command) != "" {
		base.Command = strings.TrimSpace(overrides.Command)
	}
	if strings.TrimSpace(overrides.Healthcheck) != "" {
		base.Healthcheck = strings.TrimSpace(overrides.Healthcheck)
	}
	if strings.TrimSpace(overrides.Rollback) != "" {
		base.Rollback = strings.TrimSpace(overrides.Rollback)
	}
	return base
}

func trimNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if v := strings.TrimSpace(value); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func parseKeyValueList(values []string) map[string]string {
	out := map[string]string{}
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if key != "" {
			out[key] = val
		}
	}
	return out
}

func mergeStringMap(base, overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		return base
	}
	if base == nil {
		base = map[string]string{}
	}
	for key, value := range overrides {
		base[key] = value
	}
	return base
}

func loadConfig(path string, fallback bool) (Config, bool, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		if fallback && errors.Is(err, os.ErrNotExist) {
			cfg = DefaultConfig()
			return cfg, true, cfg.Validate()
		}
		return cfg, false, fmt.Errorf("sdlc: read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, false, fmt.Errorf("sdlc: parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, false, err
	}
	return cfg, false, nil
}

func DefaultConfig() Config {
	conductor := DefaultConductorAgent()
	return Config{
		Conductor: RoleConfig{Agent: conductor},
		Intake:    IntakeConfig{Provider: "local"},
		Deploy: DeploymentConfig{
			Staging:    TargetConfig{Name: "staging"},
			Production: TargetConfig{Name: "production"},
		},
		Policies: map[string]string{
			"intake_pickup":          "open-unassigned-unblocked",
			"intake_labels_required": "false",
			"reserved_skip_labels":   "sdlc:ignore,sdlc:blocked,sdlc:in-progress,sdlc:qa,sdlc:approved,sdlc:done",
			"priority_labels":        "priority:p0,priority:p1,priority:p2,priority:p3",
			"type_labels":            "type:bug,type:enhancement,type:task,type:docs,type:chore",
			"custom_labels":          "allowed",
		},
	}
}

func DefaultConfigYAML() string {
	b, err := yaml.Marshal(DefaultConfig())
	if err != nil {
		return "conductor:\n  agent: " + DefaultConductorAgent() + "\nintake:\n  provider: local\n"
	}
	return string(b)
}

// DefaultConductorAgent resolves the operator agent for local interactive SDLC.
// A child process cannot reliably infer its parent chat client, so explicit
// BASHY_* signals win; the rest are best-effort environment fingerprints.
func DefaultConductorAgent() string {
	for _, key := range []string{
		"BASHY_SDLC_CONDUCTOR_AGENT",
		"BASHY_CONDUCTOR_AGENT",
		"BASHY_AGENT_TOOL",
		"BASHY_AGENT_NAME",
		"BASHY_AGENT",
		"AGENT_TOOL",
		"AGENT_NAME",
		"AI_AGENT",
	} {
		if agent := normalizeAgentName(os.Getenv(key)); agent != "" {
			return agent
		}
	}
	env := os.Environ()
	switch {
	case hasAnyEnv(env, "CLAUDECODE", "CLAUDE_CODE", "CLAUDE_SESSION_ID", "CLAUDE_CONFIG_DIR"):
		return "claude"
	case hasAnyEnv(env, "OPENCODE", "OPENCODE_SESSION_ID", "OPENCODE_CONFIG_DIR"):
		return "opencode"
	case hasAnyEnv(env, "AGY", "AGY_SESSION_ID", "ANTIGRAVITY"):
		return "agy"
	case hasAnyEnv(env, "CODEX_SANDBOX", "CODEX_HOME", "CODEX_SESSION_ID", "CODEX_CLI"):
		return "codex"
	default:
		return "codex"
	}
}

func normalizeAgentName(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "claude", "claude-code", "claude_code", "claudecode":
		return "claude"
	case "codex", "openai-codex", "openai_codex":
		return "codex"
	case "opencode", "open-code", "open_code":
		return "opencode"
	case "agy", "antigravity", "anti-gravity", "anti_gravity":
		return "agy"
	default:
		return ""
	}
}

func hasAnyEnv(env []string, keys ...string) bool {
	for _, item := range env {
		name, value, ok := strings.Cut(item, "=")
		if !ok || value == "" {
			continue
		}
		for _, key := range keys {
			if strings.EqualFold(name, key) || strings.HasPrefix(strings.ToUpper(name), strings.ToUpper(key)+"_") {
				return true
			}
		}
	}
	return false
}

func (cfg Config) Validate() error {
	if strings.TrimSpace(cfg.Conductor.Agent) == "" {
		return errors.New("sdlc: conductor.agent is required")
	}
	if strings.TrimSpace(cfg.Intake.Provider) == "" {
		return errors.New("sdlc: intake.provider is required")
	}
	if strings.TrimSpace(cfg.Deploy.Staging.Name) == "" && strings.TrimSpace(cfg.Deploy.Staging.Environment) == "" {
		return errors.New("sdlc: deployment.staging.name or deployment.staging.environment is required")
	}
	if strings.TrimSpace(cfg.Deploy.Production.Name) == "" && strings.TrimSpace(cfg.Deploy.Production.Environment) == "" {
		return errors.New("sdlc: deployment.production.name or deployment.production.environment is required")
	}
	return nil
}

func Prepare(ctx context.Context, opt DelegateOptions) (DelegateResult, error) {
	if err := ApplyIssueRequest(&opt); err != nil {
		return DelegateResult{SchemaVersion: schemaVersion, Status: "error", ConfigPath: opt.ConfigPath}, err
	}
	var cfg Config
	var usedDefault bool
	var err error
	if opt.Config.NoConfig {
		cfg = DefaultConfig()
		usedDefault = true
	} else {
		cfg, usedDefault, err = LoadConfigOrDefault(opt.ConfigPath)
		if err != nil {
			return DelegateResult{SchemaVersion: schemaVersion, Status: "error", ConfigPath: opt.ConfigPath}, err
		}
	}
	cfg = ApplyConfigOverrides(cfg, opt.Config)
	if err := cfg.Validate(); err != nil {
		return DelegateResult{SchemaVersion: schemaVersion, Status: "error", ConfigPath: opt.ConfigPath}, err
	}
	autoSelected := false
	if strings.TrimSpace(opt.Issue.Title) == "" {
		// No explicit issue: try to auto-select from the configured provider
		// (github). Loom/local providers still require an explicit issue.
		selected, serr := resolveIntakeIssue(ctx, cfg, &opt)
		if serr != nil {
			return DelegateResult{SchemaVersion: schemaVersion, Status: "error", ConfigPath: opt.ConfigPath}, serr
		}
		autoSelected = selected
		if !selected {
			if strings.EqualFold(strings.TrimSpace(cfg.Intake.Provider), "github") {
				// Empty queue is a clean no-op for a scheduler, not an error.
				return DelegateResult{
					SchemaVersion: schemaVersion,
					Status:        "idle",
					ConfigPath:    opt.ConfigPath,
					DefaultConfig: usedDefault,
					Conductor:     cfg.Conductor.Agent,
				}, nil
			}
			return DelegateResult{SchemaVersion: schemaVersion, Status: "error", ConfigPath: opt.ConfigPath}, errors.New("sdlc: --issue-title is required")
		}
	}
	brief := BuildConductorBrief(cfg, opt.Issue)
	return DelegateResult{
		SchemaVersion: schemaVersion,
		Status:        "ready",
		ConfigPath:    opt.ConfigPath,
		DefaultConfig: usedDefault,
		Conductor:     cfg.Conductor.Agent,
		Issue:         opt.Issue,
		Brief:         brief,
		AutoSelected:  autoSelected,
	}, nil
}

func Delegate(ctx context.Context, opt DelegateOptions) (DelegateResult, error) {
	res, err := Prepare(ctx, opt)
	if err != nil {
		return res, err
	}
	if res.Status == "idle" {
		return res, nil // empty intake queue — nothing to do this tick
	}
	if res.AutoSelected && !opt.DryRun {
		// Claim the auto-selected github issue so a concurrent tick skips it.
		_ = claimGitHubIssue(ctx, res.Issue)
	}
	chatOpt := chatOptionsForDelegate(res, opt)
	if opt.Background && !opt.DryRun {
		return StartBackgroundRun(ctx, res, opt, chatOpt)
	}
	var run *RunRecord
	if !opt.DryRun {
		run, _ = NewRunRecord(res, opt, chat.Result{})
		if run != nil {
			_ = SaveRunRecord(*run)
			res.RunID = run.ID
			res.RunPath = run.RunPath
			res.LogPath = run.LogPath
		}
	}
	chatRes, err := chat.Invoke(ctx, chatOpt, nil)
	res.Chat = chatRes
	if run != nil {
		run.Conductor = chatRes.Agent
		run.Command = redactedCommand(chatRes.Agent, chatRes.Args)
		run.ExitCode = chatRes.ExitCode
		run.FinishedAt = time.Now().UTC()
		run.Status = "succeeded"
		if err != nil || chatRes.ExitCode != 0 {
			run.Status = "failed"
		}
		if err != nil {
			run.Error = err.Error()
		}
		_ = appendRunLog(*run, chatRes.Output)
		_ = SaveRunRecord(*run)
	}
	if err != nil {
		res.Status = "error"
		return res, err
	}
	res.Status = "delegated"
	if opt.DryRun {
		res.Status = "dry-run"
	}
	return res, nil
}

func chatOptionsForDelegate(res DelegateResult, opt DelegateOptions) chat.Options {
	return chat.Options{
		Agent:       res.Conductor,
		Role:        "conductor",
		Instruction: res.Brief,
		Cwd:         opt.Cwd,
		Timeout:     opt.Timeout,
		Sandbox:     opt.Sandbox,
		DryRun:      opt.DryRun,
	}
}

func StartBackgroundRun(ctx context.Context, res DelegateResult, opt DelegateOptions, chatOpt chat.Options) (DelegateResult, error) {
	opt.RunsDir = runsDirForOption(opt.RunsDir)
	dryOpt := chatOpt
	dryOpt.DryRun = true
	dryRes, err := chat.Invoke(ctx, dryOpt, nil)
	if err != nil {
		res.Status = "error"
		res.Chat = dryRes
		return res, err
	}
	run, err := NewRunRecord(res, opt, dryRes)
	if err != nil {
		res.Status = "error"
		return res, err
	}
	run.Status = "starting"
	if err := SaveRunRecord(*run); err != nil {
		res.Status = "error"
		return res, err
	}
	exe, err := os.Executable()
	if err != nil {
		res.Status = "error"
		return res, err
	}
	supervisorArgs := []string{"sdlc", "supervise", "--runs-dir", opt.RunsDir, run.ID, "--", dryRes.Agent}
	supervisorArgs = append(supervisorArgs, dryRes.Args...)
	cmd := exec.Command(exe, supervisorArgs...)
	applyBackgroundProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		run.Status = "failed"
		run.Error = err.Error()
		run.FinishedAt = time.Now().UTC()
		_ = SaveRunRecord(*run)
		res.Status = "error"
		return res, err
	}
	run.PID = cmd.Process.Pid
	run.Status = "running"
	run.Command = redactedCommand(dryRes.Agent, dryRes.Args)
	if err := cmd.Process.Release(); err != nil {
		run.Error = err.Error()
	}
	if err := SaveRunRecord(*run); err != nil {
		res.Status = "error"
		return res, err
	}
	dryRes.Args = redactedArgs(dryRes.Args)
	dryRes.Output = fmt.Sprintf("started sdlc run %s pid=%d log=%s\n", run.ID, run.PID, run.LogPath)
	res.Chat = dryRes
	res.Status = "background"
	res.RunID = run.ID
	res.RunPath = run.RunPath
	res.LogPath = run.LogPath
	return res, nil
}

func NewRunRecord(res DelegateResult, opt DelegateOptions, chatRes chat.Result) (*RunRecord, error) {
	runsDir := strings.TrimSpace(opt.RunsDir)
	if runsDir == "" {
		runsDir = ".bashy/generated/sdlc/runs"
	}
	started := time.Now().UTC()
	baseID := started.Format("20060102T150405Z")
	if slug := slugify(res.Issue.Title); slug != "" {
		baseID += "-" + slug
	}
	id := baseID
	runDir := filepath.Join(runsDir, id)
	for i := 2; ; i++ {
		if _, err := os.Stat(runDir); errors.Is(err, os.ErrNotExist) {
			break
		}
		id = fmt.Sprintf("%s-%d", baseID, i)
		runDir = filepath.Join(runsDir, id)
	}
	run := &RunRecord{
		SchemaVersion: schemaVersion,
		ID:            id,
		ReferenceID:   id,
		Status:        "running",
		IssueTitle:    res.Issue.Title,
		IssueID:       res.Issue.ID,
		IssueURL:      res.Issue.URL,
		Conductor:     res.Conductor,
		Cwd:           chatRes.Cwd,
		Command:       redactedCommand(chatRes.Agent, chatRes.Args),
		RunPath:       filepath.Join(runDir, "run.json"),
		LogPath:       filepath.Join(runDir, "conductor.log"),
		BriefPath:     filepath.Join(runDir, "brief.txt"),
		StartedAt:     started,
	}
	if run.Cwd == "" {
		if opt.Cwd != "" {
			run.Cwd = opt.Cwd
		} else {
			run.Cwd, _ = os.Getwd()
		}
	}
	if run.Conductor == "" {
		run.Conductor = chatRes.Agent
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(run.BriefPath, []byte(res.Brief), 0o600); err != nil {
		return nil, err
	}
	return run, nil
}

func SaveRunRecord(run RunRecord) error {
	if err := os.MkdirAll(filepath.Dir(run.RunPath), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(run.RunPath, b, 0o644)
}

func SuperviseRun(ctx context.Context, dir, id string, command []string) error {
	if len(command) == 0 {
		return errors.New("sdlc: supervise requires a command")
	}
	run, err := LoadRunByID(dir, id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(run.LogPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(run.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "sdlc run %s started %s\n", run.ReferenceID, run.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(logFile, "conductor=%s cwd=%s issue=%q\n\n", run.Conductor, run.Cwd, run.IssueTitle)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	if run.Cwd != "" {
		cmd.Dir = run.Cwd
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	run.Status = "running"
	run.Command = redactedCommand(command[0], command[1:])
	if err := cmd.Start(); err != nil {
		run.Status = "failed"
		run.Error = err.Error()
		run.FinishedAt = time.Now().UTC()
		_ = SaveRunRecord(run)
		return err
	}
	run.PID = cmd.Process.Pid
	if err := SaveRunRecord(run); err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	err = cmd.Wait()
	run.FinishedAt = time.Now().UTC()
	run.ExitCode = 0
	run.Status = "succeeded"
	if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			run.ExitCode = exitErr.ExitCode()
		} else if ctx.Err() != nil {
			run.ExitCode = 124
			run.Error = ctx.Err().Error()
		} else {
			run.ExitCode = 127
		}
	}
	fmt.Fprintf(logFile, "\nsdlc run %s finished status=%s exit_code=%d\n", run.ReferenceID, run.Status, run.ExitCode)
	if saveErr := SaveRunRecord(run); saveErr != nil {
		return saveErr
	}
	return err
}

func appendRunLog(run RunRecord, output string) error {
	if strings.TrimSpace(output) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(run.LogPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(run.LogPath, []byte(output), 0o644)
}

func runsDirForOption(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		dir = ".bashy/generated/sdlc/runs"
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func LoadRun(path string) (RunRecord, error) {
	var run RunRecord
	data, err := os.ReadFile(path)
	if err != nil {
		return run, err
	}
	if err := json.Unmarshal(data, &run); err != nil {
		return run, err
	}
	if run.ReferenceID == "" {
		run.ReferenceID = run.ID
	}
	return refreshRun(run), nil
}

func ListRuns(dir string, limit int) ([]RunRecord, error) {
	if strings.TrimSpace(dir) == "" {
		dir = ".bashy/generated/sdlc/runs"
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []RunRecord
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		run, err := LoadRun(filepath.Join(dir, entry.Name(), "run.json"))
		if err == nil {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func LoadRunByID(dir, id string) (RunRecord, error) {
	if strings.TrimSpace(dir) == "" {
		dir = ".bashy/generated/sdlc/runs"
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return RunRecord{}, errors.New("sdlc: run id is required")
	}
	run, err := LoadRun(filepath.Join(dir, id, "run.json"))
	if err == nil {
		return run, nil
	}
	runs, listErr := ListRuns(dir, 0)
	if listErr != nil {
		return RunRecord{}, listErr
	}
	for _, candidate := range runs {
		if candidate.ID == id || candidate.ReferenceID == id {
			return candidate, nil
		}
	}
	return RunRecord{}, err
}

func ApproveRun(dir, id, note, by string) (RunRecord, error) {
	run, err := LoadRunByID(dir, id)
	if err != nil {
		return run, err
	}
	run = refreshRun(run)
	if run.Status == "running" {
		return run, fmt.Errorf("sdlc: run %s is still running; approve after staging evidence is ready", run.ReferenceID)
	}
	run.Status = "approved"
	run.Approval = &Approval{
		Status:     "approved",
		ApprovedAt: time.Now().UTC(),
		ApprovedBy: strings.TrimSpace(by),
		Note:       strings.TrimSpace(note),
	}
	return run, SaveRunRecord(run)
}

func ReviewRun(dir, id, status, note, by string) (RunRecord, error) {
	run, err := LoadRunByID(dir, id)
	if err != nil {
		return run, err
	}
	run = refreshRun(run)
	if run.Status == "running" {
		return run, fmt.Errorf("sdlc: run %s is still running; review after conductor evidence is ready", run.ReferenceID)
	}
	status = strings.TrimSpace(strings.ToLower(status))
	if status == "" {
		status = "accepted"
	}
	switch status {
	case "accepted", "rejected":
	default:
		return run, fmt.Errorf("sdlc: unsupported QA status %q", status)
	}
	run.Status = "qa-" + status
	run.QA = &GateReview{
		Status:     status,
		ReviewedAt: time.Now().UTC(),
		ReviewedBy: strings.TrimSpace(by),
		Note:       strings.TrimSpace(note),
	}
	return run, SaveRunRecord(run)
}

func RunApproved(run RunRecord) bool {
	return run.Approval != nil && run.Approval.Status == "approved"
}

func BuildRolloutInstruction(run RunRecord, note, by string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Deployment approval granted for SDLC reference %s.\n\n", run.ReferenceID)
	if run.QA != nil {
		fmt.Fprintf(&b, "QA:\n")
		if !run.QA.ReviewedAt.IsZero() {
			fmt.Fprintf(&b, "- Reviewed at: %s\n", run.QA.ReviewedAt.Format(time.RFC3339))
		}
		if run.QA.ReviewedBy != "" {
			fmt.Fprintf(&b, "- Reviewed by: %s\n", run.QA.ReviewedBy)
		}
		fmt.Fprintf(&b, "- Status: %s\n", run.QA.Status)
		if run.QA.Note != "" {
			fmt.Fprintf(&b, "- Note: %s\n", run.QA.Note)
		}
		fmt.Fprintf(&b, "\n")
	}
	if run.Approval != nil {
		fmt.Fprintf(&b, "Approval:\n")
		if !run.Approval.ApprovedAt.IsZero() {
			fmt.Fprintf(&b, "- Approved at: %s\n", run.Approval.ApprovedAt.Format(time.RFC3339))
		}
		if run.Approval.ApprovedBy != "" {
			fmt.Fprintf(&b, "- Approved by: %s\n", run.Approval.ApprovedBy)
		}
		if run.Approval.Note != "" {
			fmt.Fprintf(&b, "- Note: %s\n", run.Approval.Note)
		}
		fmt.Fprintf(&b, "\n")
	}
	if strings.TrimSpace(by) != "" || strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "Rollout request:\n")
		if strings.TrimSpace(by) != "" {
			fmt.Fprintf(&b, "- Requested by: %s\n", strings.TrimSpace(by))
		}
		if strings.TrimSpace(note) != "" {
			fmt.Fprintf(&b, "- Note: %s\n", strings.TrimSpace(note))
		}
		fmt.Fprintf(&b, "\n")
	}
	fmt.Fprintf(&b, "Original request:\n")
	fmt.Fprintf(&b, "- Reference ID: %s\n", run.ReferenceID)
	if run.IssueID != "" && run.IssueID != run.ReferenceID {
		fmt.Fprintf(&b, "- External issue ID: %s\n", run.IssueID)
	}
	if run.IssueURL != "" {
		fmt.Fprintf(&b, "- Issue URL: %s\n", run.IssueURL)
	}
	fmt.Fprintf(&b, "- Title: %s\n", run.IssueTitle)
	fmt.Fprintf(&b, "- Original run log: %s\n", run.LogPath)
	fmt.Fprintf(&b, "- Original conductor brief: %s\n\n", run.BriefPath)
	fmt.Fprintf(&b, "Proceed with the approved deployment rollout only for this reference and configured target. Preserve unrelated user changes. After rollout, report deployment evidence and verification results, then the scheduler can mark the reference resolved.")
	return b.String()
}

func ResolveLifecycleRun(dir, id, status, note, by string) (RunRecord, error) {
	run, err := LoadRunByID(dir, id)
	if err != nil {
		return run, err
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = "resolved"
	}
	switch status {
	case "resolved", "rejected", "rolled-out", "failed", "cancelled":
	default:
		return run, fmt.Errorf("sdlc: unsupported resolution status %q", status)
	}
	run.Status = status
	run.Resolution = &Resolution{
		Status:     status,
		ResolvedAt: time.Now().UTC(),
		ResolvedBy: strings.TrimSpace(by),
		Note:       strings.TrimSpace(note),
	}
	return run, SaveRunRecord(run)
}

func WatchRun(ctx context.Context, out io.Writer, opt WatchOptions) error {
	if opt.RunsDir == "" {
		opt.RunsDir = ".bashy/generated/sdlc/runs"
	}
	if opt.Tail <= 0 {
		opt.Tail = 80
	}
	if opt.Interval <= 0 {
		opt.Interval = 2 * time.Second
	}
	var seen int
	lastStatus := ""
	for {
		run, err := ResolveRun(opt.RunsDir, opt.RunID)
		if err != nil {
			return err
		}
		run = refreshRun(run)
		if opt.JSON {
			b, _ := json.Marshal(run)
			fmt.Fprintln(out, string(b))
		} else {
			if run.Status != lastStatus {
				fmt.Fprintf(out, "%s %s conductor=%s pid=%d approved=%t resolved=%t log=%s\n",
					run.ReferenceID, run.Status, run.Conductor, run.PID, RunApproved(run), run.Resolution != nil, run.LogPath)
				lastStatus = run.Status
			}
			data, _ := os.ReadFile(run.LogPath)
			if opt.Follow {
				if seen < len(data) {
					fmt.Fprint(out, string(data[seen:]))
					seen = len(data)
				}
			} else {
				fmt.Fprint(out, tailString(string(data), opt.Tail))
			}
		}
		if !opt.Follow || !runActive(run.Status) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(opt.Interval):
		}
	}
}

func ResolveRun(dir, id string) (RunRecord, error) {
	if strings.TrimSpace(id) != "" {
		return LoadRun(filepath.Join(dir, id, "run.json"))
	}
	runs, err := ListRuns(dir, 1)
	if err != nil {
		return RunRecord{}, err
	}
	if len(runs) == 0 {
		return RunRecord{}, fmt.Errorf("sdlc: no runs found in %s", dir)
	}
	return runs[0], nil
}

func refreshRun(run RunRecord) RunRecord {
	if runActive(run.Status) && run.PID > 0 && !processAlive(run.PID) {
		run.Status = "exited"
		run.FinishedAt = time.Now().UTC()
		_ = SaveRunRecord(run)
	}
	return run
}

func runActive(status string) bool {
	return status == "starting" || status == "running"
}

func redactedCommand(agent string, args []string) []string {
	if agent == "" && len(args) == 0 {
		return nil
	}
	cmd := []string{}
	if agent != "" {
		cmd = append(cmd, agent)
	}
	cmd = append(cmd, redactedArgs(args)...)
	return cmd
}

func redactedArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	out := append([]string{}, args...)
	out[len(out)-1] = "<instruction>"
	return out
}

func tailString(s string, lines int) string {
	if lines <= 0 || strings.TrimSpace(s) == "" {
		return s
	}
	parts := strings.SplitAfter(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) <= lines {
		return s
	}
	return strings.Join(parts[len(parts)-lines:], "")
}

func BuildConductorBrief(cfg Config, issue Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the SDLC conductor for this repository.\n\n")
	fmt.Fprintf(&b, "Boundary:\n")
	fmt.Fprintf(&b, "- SDLC owns intake pointers, lifecycle state, validation gates, and deployment routing.\n")
	fmt.Fprintf(&b, "- You own implementation planning and sprint execution. Use bashy sprint/weave when useful.\n")
	if cfg.Reviewer.Agent != "" || cfg.QA.Agent != "" {
		fmt.Fprintf(&b, "- Delegate configured follow-up roles to the available agent fleet.\n")
	}
	fmt.Fprintf(&b, "- Source guardrail: work in a mutable workspace whose origin is a local Loom workspace repo; use a baseline remote for the immutable Loom mirror. Do not push to GitHub or any production upstream during implementation.\n")
	fmt.Fprintf(&b, "- Do not deploy to the configured rollout target without explicit approval when policy requires it.\n\n")
	fmt.Fprintf(&b, "Issue:\n")
	if issue.ID != "" {
		fmt.Fprintf(&b, "ID: %s\n", issue.ID)
	}
	if issue.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", issue.URL)
	}
	fmt.Fprintf(&b, "Title: %s\n", strings.TrimSpace(issue.Title))
	if strings.TrimSpace(issue.Body) != "" {
		fmt.Fprintf(&b, "Body:\n%s\n", strings.TrimSpace(issue.Body))
	}
	fmt.Fprintf(&b, "\nRouting:\n")
	fmt.Fprintf(&b, "- Intake provider: %s", cfg.Intake.Provider)
	if cfg.Intake.Repository != "" {
		fmt.Fprintf(&b, " (%s)", cfg.Intake.Repository)
	}
	fmt.Fprintf(&b, "\n")
	if cfg.Reviewer.Agent != "" {
		fmt.Fprintf(&b, "- Review agent: %s\n", cfg.Reviewer.Agent)
	}
	if cfg.QA.Agent != "" {
		fmt.Fprintf(&b, "- QA agent: %s\n", cfg.QA.Agent)
	}
	fmt.Fprintf(&b, "- Validation target: %s\n", displayTarget(cfg.Deploy.Staging))
	fmt.Fprintf(&b, "- Rollout target: %s\n", displayTarget(cfg.Deploy.Production))
	if cfg.Deploy.Staging.Healthcheck != "" {
		fmt.Fprintf(&b, "- Validation healthcheck: %s\n", cfg.Deploy.Staging.Healthcheck)
	}
	if cfg.Deploy.Production.Healthcheck != "" {
		fmt.Fprintf(&b, "- Rollout healthcheck: %s\n", cfg.Deploy.Production.Healthcheck)
	}
	if len(cfg.Policies) > 0 {
		fmt.Fprintf(&b, "- Policies:\n")
		keys := make([]string, 0, len(cfg.Policies))
		for key := range cfg.Policies {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "  - %s=%s\n", key, cfg.Policies[key])
		}
	}
	fmt.Fprintf(&b, "\nExpected loop:\n")
	fmt.Fprintf(&b, "1. Analyze priority/risk and create a sprint plan.\n")
	fmt.Fprintf(&b, "2. Implement the requested change.\n")
	step := 3
	if cfg.Reviewer.Agent != "" {
		fmt.Fprintf(&b, "%d. Send the result to the configured review agent.\n", step)
		step++
	}
	if cfg.QA.Agent != "" {
		fmt.Fprintf(&b, "%d. Send the result to the configured QA agent.\n", step)
		step++
	}
	fmt.Fprintf(&b, "%d. Prepare rollout instructions for the configured deployment target.\n", step)
	return b.String()
}

func displayTarget(t TargetConfig) string {
	for _, v := range []string{t.Name, t.Environment, t.Host} {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return "unconfigured"
}

func SaveLocalIssue(text, file, dir string) (Issue, string, error) {
	text = strings.TrimSpace(text)
	if strings.TrimSpace(file) != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return Issue{}, "", fmt.Errorf("sdlc: read issue file: %w", err)
		}
		text = strings.TrimSpace(string(data))
	}
	if text == "" {
		return Issue{}, "", errors.New("sdlc: --text, --file, or positional issue text is required")
	}
	title, body := splitIssueRequest(text)
	issue := Issue{ID: time.Now().UTC().Format("20060102T150405Z"), Title: title, Body: body}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Issue{}, "", err
	}
	slug := slugify(title)
	if slug == "" {
		slug = "issue"
	}
	path := filepath.Join(dir, issue.ID+"-"+slug+".md")
	content := fmt.Sprintf("---\nid: %s\ntitle: %q\ncreated_at: %s\n---\n\n%s\n",
		issue.ID, issue.Title, time.Now().UTC().Format(time.RFC3339), text)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return Issue{}, "", err
	}
	return issue, path, nil
}

func VerifyContent(ctx context.Context, opt VerifyOptions) (VerifyResult, error) {
	target := strings.TrimSpace(opt.Target)
	res := VerifyResult{SchemaVersion: schemaVersion, Target: target, Status: "ok"}
	if target == "" {
		res.Status = "error"
		return res, errors.New("sdlc: --url or --file is required")
	}
	body, err := readTarget(ctx, target, opt.Timeout)
	if err != nil {
		res.Status = "error"
		return res, err
	}
	for _, text := range opt.Present {
		ok := strings.Contains(body, text)
		status := "ok"
		if !ok {
			status, res.Status = "missing", "failed"
		}
		res.Checks = append(res.Checks, VerifyCheck{Kind: "present", Text: text, Status: status})
	}
	for _, text := range opt.Absent {
		ok := !strings.Contains(body, text)
		status := "ok"
		if !ok {
			status, res.Status = "present", "failed"
		}
		res.Checks = append(res.Checks, VerifyCheck{Kind: "absent", Text: text, Status: status})
	}
	if res.Status != "ok" {
		return res, errors.New("sdlc: verify failed")
	}
	return res, nil
}

func readTarget(ctx context.Context, target string, timeout time.Duration) (string, error) {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if timeout <= 0 {
			timeout = 20 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return "", err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("sdlc: GET %s: %s", target, resp.Status)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
		return string(data), err
	}
	data, err := os.ReadFile(target)
	return string(data), err
}

func GitHubDeployStatus(ctx context.Context, repo, branch string) (DeployStatus, error) {
	repo = strings.TrimSpace(repo)
	res := DeployStatus{SchemaVersion: schemaVersion, Repo: repo}
	if repo == "" {
		res.Status = "error"
		return res, errors.New("sdlc: --repo is required and origin is not a GitHub remote")
	}
	url := "https://api.github.com/repos/" + repo + "/actions/runs?per_page=1"
	if branch != "" {
		url += "&branch=" + branch
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return res, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return res, fmt.Errorf("sdlc: GitHub API: %s", resp.Status)
	}
	var payload struct {
		WorkflowRuns []struct {
			Name        string `json:"name"`
			RunNumber   int    `json:"run_number"`
			Status      string `json:"status"`
			Conclusion  string `json:"conclusion"`
			HTMLURL     string `json:"html_url"`
			HeadSHA     string `json:"head_sha"`
			DisplayName string `json:"display_title"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return res, err
	}
	if len(payload.WorkflowRuns) == 0 {
		res.Status = "none"
		return res, nil
	}
	run := payload.WorkflowRuns[0]
	res.Workflow, res.RunNumber, res.Status, res.Conclusion = run.Name, run.RunNumber, run.Status, run.Conclusion
	res.HTMLURL, res.HeadSHA, res.Title = run.HTMLURL, run.HeadSHA, run.DisplayName
	return res, nil
}

func Guard(dir string, commands []string) map[string]any {
	checks := []map[string]any{}
	git := gitSummary(dir)
	gitStatus := "ok"
	if dirty, _ := git["dirty"].(bool); dirty {
		gitStatus = "dirty"
	}
	if originGitHub, _ := git["origin_github"].(bool); originGitHub {
		gitStatus = "failed"
		git["guardrail"] = "origin points at github.com; use a Loom workspace origin and keep GitHub as explicit rollout target"
	}
	if _, ok := git["baseline"]; !ok {
		git["baseline_warning"] = "baseline remote is not configured"
	}
	checks = append(checks, map[string]any{"name": "git", "status": gitStatus, "detail": git})
	secretHits := secretScan(dir)
	secretStatus := "ok"
	if len(secretHits) > 0 {
		secretStatus = "failed"
	}
	checks = append(checks, map[string]any{"name": "secrets", "status": secretStatus, "hits": secretHits})
	for _, c := range commands {
		status, output := runGuardCommand(dir, c)
		checks = append(checks, map[string]any{"name": c, "status": status, "output": output})
	}
	status := "ok"
	for _, c := range checks {
		if c["status"] != "ok" {
			status = "failed"
			break
		}
	}
	return map[string]any{"schema_version": schemaVersion, "status": status, "checks": checks}
}

func PrepareWorkspace(ctx context.Context, opt WorkspaceOptions) (WorkspaceResult, error) {
	opt.Dir = strings.TrimSpace(opt.Dir)
	opt.Origin = strings.TrimSpace(opt.Origin)
	opt.Baseline = strings.TrimSpace(opt.Baseline)
	opt.Upstream = strings.TrimSpace(opt.Upstream)
	res := WorkspaceResult{
		SchemaVersion: schemaVersion,
		Status:        "error",
		Dir:           opt.Dir,
		Origin:        opt.Origin,
		Baseline:      opt.Baseline,
		Upstream:      opt.Upstream,
		UpdatedAt:     time.Now().UTC(),
	}
	if opt.Dir == "" {
		return res, errors.New("sdlc: workspace --dir is required")
	}
	if opt.Origin == "" {
		return res, errors.New("sdlc: workspace --origin (the loom source-of-truth repo) is required")
	}
	if isGitHubURL(opt.Origin) && !opt.AllowGitHubOrigin {
		return res, errors.New("sdlc: refusing github.com as workspace origin; use a writable Loom workspace repo or pass --allow-github-origin")
	}
	// Single-repo model (default): the loom origin IS the source of truth — clone
	// from it and push back to it. --baseline is OPTIONAL, kept for the legacy
	// guarded two-repo mode (an immutable Loom mirror to clone from + diff
	// against, with origin the writable push target). When it's omitted (or equal
	// to origin) there is exactly one loom repo.
	cloneFrom := opt.Baseline
	if cloneFrom == "" {
		cloneFrom = opt.Origin
	}
	created, err := ensureWorkspaceClone(ctx, opt.Dir, cloneFrom)
	if err != nil {
		return res, err
	}
	res.Created = created
	if opt.Baseline != "" && opt.Baseline != opt.Origin {
		if err := setGitRemote(ctx, opt.Dir, "baseline", opt.Baseline); err != nil {
			return res, err
		}
		if err := runGitErr(ctx, opt.Dir, "remote", "set-url", "--push", "baseline", "DISABLED"); err != nil {
			return res, err
		}
	}
	if err := setGitRemote(ctx, opt.Dir, "origin", opt.Origin); err != nil {
		return res, err
	}
	if opt.Upstream != "" {
		if err := setGitRemote(ctx, opt.Dir, "upstream", opt.Upstream); err != nil {
			return res, err
		}
		if err := runGitErr(ctx, opt.Dir, "remote", "set-url", "--push", "upstream", "DISABLED"); err != nil {
			return res, err
		}
	}
	if err := runGitErr(ctx, opt.Dir, "fetch", "origin", "--prune"); err != nil {
		return res, err
	}
	if opt.Baseline != "" && opt.Baseline != opt.Origin {
		_ = runGitErr(ctx, opt.Dir, "fetch", "baseline", "--prune")
	}
	metaPath := filepath.Join(opt.Dir, ".git", "bashy-sdlc-workspace.json")
	res.MetadataPath = metaPath
	res.Status = "ready"
	if err := os.MkdirAll(filepath.Dir(metaPath), 0o755); err != nil {
		res.Status = "error"
		return res, err
	}
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		res.Status = "error"
		return res, err
	}
	if err := os.WriteFile(metaPath, append(b, '\n'), 0o644); err != nil {
		res.Status = "error"
		return res, err
	}
	return res, nil
}

// PushSourceOptions drives the "close the loop" step: propose the shipped result
// back to the migration source (the upstream the loom repo was migrated from) as
// a PULL REQUEST — never a direct write to upstream's default branch. loom
// proposes; the upstream owner disposes (auto-merge for a private backup, review
// for a third-party upstream).
type PushSourceOptions struct {
	Cwd      string // workspace dir (its `upstream` remote is the source when --upstream is empty)
	Upstream string // owner/repo or URL; empty => read the `upstream` remote
	Branch   string // local ref to push (default: HEAD)
	Head     string // upstream branch to push to + PR from (default: sdlc/ship)
	Base     string // PR base branch (default: main)
	Title    string
	Body     string
	Token    string // GitHub token (default GITHUB_TOKEN/GIT_TOKEN)
	NoPR     bool   // push the branch only (non-GitHub upstream, or PR opened elsewhere)
	RunID    string // when set, require the run be approved first (gate)
	RunsDir  string
	DryRun   bool
	JSON     bool
}

type PushSourceResult struct {
	SchemaVersion string   `json:"schema_version"`
	Status        string   `json:"status"` // dry-run | pushed | pr-opened | error
	Upstream      string   `json:"upstream"`
	Head          string   `json:"head"`
	Base          string   `json:"base"`
	PRURL         string   `json:"pr_url,omitempty"`
	PRNumber      int      `json:"pr_number,omitempty"`
	Command       []string `json:"command,omitempty"` // token-redacted
}

// PushSourceToUpstream pushes the approved branch to the migration source and
// opens a PR from it (GitHub). It closes the source→loom→ship→source cycle so the
// upstream is a live, reviewable record of what shipped.
// newPushSourceCmd is `bashy sdlc push-source` — the "close the loop" mechanism:
// propose the shipped result back to the migration source as a GitHub PR. It is
// ONE backup target a deploy.md/dag step can invoke; the dag decides which
// mechanism a given repo uses (this for an upstream forge; s3/kopia/a fresh
// remote for a repo created from scratch).
func newPushSourceCmd() *cobra.Command {
	var opt PushSourceOptions
	cmd := &cobra.Command{
		Use:   "push-source",
		Short: "propose the shipped result back to the migration source as a pull request (close the loop)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := PushSourceToUpstream(cmd.Context(), opt)
			if opt.JSON || os.Getenv("BASHY_AGENTIC") != "" {
				b, _ := json.Marshal(res)
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else if err == nil {
				switch res.Status {
				case "pr-opened":
					fmt.Fprintf(cmd.OutOrStdout(), "opened PR %s (%s#%d): %s -> %s\n", res.PRURL, res.Upstream, res.PRNumber, res.Head, res.Base)
				case "pushed":
					fmt.Fprintf(cmd.OutOrStdout(), "pushed %s to %s (no PR)\n", res.Head, res.Upstream)
				case "dry-run":
					fmt.Fprintf(cmd.OutOrStdout(), "dry-run: %s\n", strings.Join(res.Command, " "))
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&opt.Cwd, "cwd", "", "workspace dir (its `upstream` remote is the source when --upstream is empty)")
	cmd.Flags().StringVar(&opt.Upstream, "upstream", "", "source owner/repo or URL (else the `upstream` remote)")
	cmd.Flags().StringVar(&opt.Branch, "branch", "HEAD", "local ref to push")
	cmd.Flags().StringVar(&opt.Head, "head", "sdlc/ship", "upstream branch to push + open the PR from")
	cmd.Flags().StringVar(&opt.Base, "base", "main", "PR base branch")
	cmd.Flags().StringVar(&opt.Title, "title", "", "PR title")
	cmd.Flags().StringVar(&opt.Body, "body", "", "PR body")
	cmd.Flags().StringVar(&opt.Token, "github-token", "", "GitHub token; defaults to GITHUB_TOKEN/GIT_TOKEN")
	cmd.Flags().BoolVar(&opt.NoPR, "no-pr", false, "push the branch only (non-GitHub upstream; open the PR on that forge)")
	cmd.Flags().StringVar(&opt.RunID, "run", "", "gate: require this run be approved first")
	cmd.Flags().StringVar(&opt.RunsDir, "runs-dir", ".bashy/generated/sdlc/runs", "SDLC runs directory")
	cmd.Flags().BoolVar(&opt.DryRun, "dry-run", false, "print the plan without pushing")
	cmd.Flags().BoolVar(&opt.JSON, "json", false, "print JSON")
	return cmd
}

func PushSourceToUpstream(ctx context.Context, opt PushSourceOptions) (PushSourceResult, error) {
	if strings.TrimSpace(opt.Branch) == "" {
		opt.Branch = "HEAD"
	}
	if strings.TrimSpace(opt.Base) == "" {
		opt.Base = "main"
	}
	if strings.TrimSpace(opt.Head) == "" {
		opt.Head = "sdlc/ship"
	}
	res := PushSourceResult{SchemaVersion: schemaVersion, Status: "error", Head: opt.Head, Base: opt.Base}
	cwd := strings.TrimSpace(opt.Cwd)
	if cwd == "" {
		var err error
		if cwd, err = os.Getwd(); err != nil {
			return res, err
		}
	}
	// Optional gate: only propose an APPROVED run back to source.
	if strings.TrimSpace(opt.RunID) != "" {
		run, err := LoadRunByID(opt.RunsDir, opt.RunID)
		if err != nil {
			return res, err
		}
		if !RunApproved(run) {
			return res, fmt.Errorf("sdlc: run %s is not approved; approve before pushing back to source", run.ReferenceID)
		}
	}
	// Resolve the upstream owner/repo.
	upstream := strings.TrimSpace(opt.Upstream)
	if upstream == "" {
		upstream = strings.TrimSpace(runGit(cwd, "remote", "get-url", "upstream"))
	}
	if upstream == "" {
		return res, errors.New("sdlc: no upstream; set --upstream owner/repo or configure the `upstream` remote")
	}
	if !isGitHubURL(upstream) && !opt.NoPR {
		// Non-GitHub upstream: we can push the branch but can't open a GitHub PR.
		return res, fmt.Errorf("sdlc: upstream %q is not github.com; pass --no-pr to push the branch only (open the PR on that forge)", upstream)
	}
	ownerRepo := githubOwnerRepo(upstream)
	res.Upstream = ownerRepo

	token := strings.TrimSpace(opt.Token)
	if token == "" {
		token = GitHubToken()
	}
	pushURL := "https://github.com/" + ownerRepo + ".git"
	authURL := pushURL
	if token != "" {
		authURL = "https://x-access-token:" + token + "@github.com/" + ownerRepo + ".git"
	}
	// Redacted command for the envelope (never leak the token).
	res.Command = []string{"git", "push", pushURL, opt.Branch + ":refs/heads/" + opt.Head}
	if opt.DryRun {
		res.Status = "dry-run"
		return res, nil
	}
	pushArgs := []string{"push", "--force-with-lease", authURL, opt.Branch + ":refs/heads/" + opt.Head}
	cmd := exec.CommandContext(ctx, "git", pushArgs...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		return res, fmt.Errorf("git push to source: %w\n%s", err, redactToken(string(out), token))
	}
	res.Status = "pushed"
	if opt.NoPR || !isGitHubURL(upstream) {
		return res, nil
	}
	// Open (or reuse) the PR head->base.
	title := strings.TrimSpace(opt.Title)
	if title == "" {
		title = "sdlc: ship approved changes"
	}
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	body := map[string]any{"title": title, "head": opt.Head, "base": opt.Base, "body": opt.Body}
	if err := githubJSON(ctx, http.MethodPost, "/repos/"+ownerRepo+"/pulls", token, body, &pr); err != nil {
		// A 422 "already exists" is a benign re-run — the branch push updated it.
		if strings.Contains(err.Error(), "pull request already exists") || strings.Contains(err.Error(), "422") {
			res.Status = "pushed"
			return res, nil
		}
		return res, fmt.Errorf("sdlc: open PR on %s: %w", ownerRepo, err)
	}
	res.Status, res.PRURL, res.PRNumber = "pr-opened", pr.HTMLURL, pr.Number
	return res, nil
}

// githubOwnerRepo normalizes an owner/repo or a github URL to "owner/repo".
func githubOwnerRepo(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "git@github.com:")
	s = strings.TrimSuffix(s, ".git")
	return strings.Trim(s, "/")
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

func PublishGitHubPages(ctx context.Context, opt PublishOptions) (PublishResult, error) {
	if strings.TrimSpace(opt.Branch) == "" {
		opt.Branch = "main"
	}
	if strings.TrimSpace(opt.Source) == "" {
		opt.Source = "HEAD"
	}
	if strings.TrimSpace(opt.Remote) == "" {
		opt.Remote = "upstream"
	}
	run, err := LoadRunByID(opt.RunsDir, opt.RunID)
	res := PublishResult{
		SchemaVersion: schemaVersion,
		Status:        "error",
		RunID:         strings.TrimSpace(opt.RunID),
		Source:        strings.TrimSpace(opt.Source),
		Branch:        strings.TrimSpace(opt.Branch),
	}
	if err != nil {
		return res, err
	}
	if !RunApproved(run) {
		return res, fmt.Errorf("sdlc: run %s is not approved; run `bashy sdlc approve %s --note ...` first", run.ReferenceID, run.ReferenceID)
	}
	cwd := strings.TrimSpace(opt.Cwd)
	if cwd == "" {
		cwd = strings.TrimSpace(run.Cwd)
	}
	if cwd == "" {
		var wdErr error
		cwd, wdErr = os.Getwd()
		if wdErr != nil {
			return res, wdErr
		}
	}
	res.Cwd = cwd
	guard := Guard(cwd, nil)
	if guard["status"] != "ok" {
		return res, fmt.Errorf("sdlc: guard failed; run `bashy sdlc guard` in %s", cwd)
	}
	target := strings.TrimSpace(opt.Repo)
	if target != "" {
		target = "https://github.com/" + strings.TrimSuffix(strings.TrimPrefix(target, "https://github.com/"), ".git") + ".git"
	} else {
		target = strings.TrimSpace(runGit(cwd, "remote", "get-url", opt.Remote))
	}
	if target == "" {
		return res, fmt.Errorf("sdlc: no publish target; set --repo owner/repo or configure remote %q", opt.Remote)
	}
	if !isGitHubURL(target) {
		return res, fmt.Errorf("sdlc: publish target is not github.com: %s", target)
	}
	res.Target = target
	args := []string{"push", target, res.Source + ":refs/heads/" + res.Branch}
	res.Command = append([]string{"git"}, args...)
	if opt.DryRun {
		res.Status = "dry-run"
		return res, nil
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	res.Output = string(out)
	if err != nil {
		return res, fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	res.Status = "published"
	res.PublishedAt = time.Now().UTC()
	return res, nil
}

// pagesTarget is the notification sink for the pages flow — the forge the intake
// issue came from (github/loom), or none for direct (--issue) intake.
type pagesTarget struct {
	kind    string // "github" | "loom" | "local"
	repo    string
	number  int
	loomURL string
	token   string
	ghToken string
}

func (t pagesTarget) comment(ctx context.Context, msg string) {
	switch t.kind {
	case "github":
		_ = commentGitHubIssue(ctx, t.repo, t.number, msg, t.ghToken)
	case "loom":
		_ = commentLoomIssue(ctx, t.loomURL, t.token, t.repo, t.number, msg)
	}
}

func (t pagesTarget) close(ctx context.Context) {
	switch t.kind {
	case "github":
		_ = closeGitHubIssue(ctx, t.repo, t.number, t.ghToken)
	case "loom":
		_ = closeLoomIssue(ctx, t.loomURL, t.token, t.repo, t.number)
	}
}

// resolvePagesIntake selects the issue to work on and the notifier for it. Direct
// (--issue/--issue-file) intake wins; otherwise it fetches from the provider.
// Returns (nil, _, nil) when a forge queue is empty (a clean idle no-op).
func resolvePagesIntake(ctx context.Context, opt *PagesOnceOptions) (*Issue, pagesTarget, error) {
	// Direct intake — CLI text/file/fields.
	if strings.TrimSpace(opt.IssueText) != "" || strings.TrimSpace(opt.IssueFile) != "" ||
		strings.TrimSpace(opt.Issue.Title) != "" || strings.TrimSpace(opt.Issue.Body) != "" {
		iss := opt.Issue
		if strings.TrimSpace(iss.Title) == "" && strings.TrimSpace(iss.Body) == "" {
			text := strings.TrimSpace(opt.IssueText)
			if strings.TrimSpace(opt.IssueFile) != "" {
				b, err := os.ReadFile(opt.IssueFile)
				if err != nil {
					return nil, pagesTarget{}, fmt.Errorf("sdlc: read issue file: %w", err)
				}
				text = strings.TrimSpace(string(b))
			}
			iss.Title, iss.Body = splitIssueRequest(text)
		}
		return &iss, pagesTarget{kind: "local"}, nil
	}

	provider := strings.ToLower(strings.TrimSpace(opt.Provider))
	if provider == "" {
		provider = "loom"
	}
	switch provider {
	case "github":
		token := opt.GitHubToken
		if token == "" {
			token = GitHubToken()
		}
		gi, err := nextGitHubIssue(ctx, opt.IntakeRepo, nil, opt.RequireInitiate, token)
		if err != nil || gi == nil {
			return nil, pagesTarget{}, err
		}
		return &Issue{ID: fmt.Sprintf("%s#%d", opt.IntakeRepo, gi.Number), URL: gi.HTML, Title: gi.Title, Body: gi.Body},
			pagesTarget{kind: "github", repo: opt.IntakeRepo, number: gi.Number, ghToken: token}, nil
	case "loom", "gitea":
		li, err := nextLoomIssue(ctx, opt.LoomURL, opt.Token, opt.IntakeRepo)
		if err != nil || li == nil {
			return nil, pagesTarget{}, err
		}
		return &Issue{ID: fmt.Sprintf("%d", li.Number), URL: li.HTML, Title: li.Title, Body: li.Body},
			pagesTarget{kind: "loom", loomURL: opt.LoomURL, token: opt.Token, repo: opt.IntakeRepo, number: li.Number}, nil
	default:
		return nil, pagesTarget{}, fmt.Errorf("sdlc: unknown pages intake provider %q", provider)
	}
}

func RunPagesOnce(ctx context.Context, opt PagesOnceOptions) (PagesOnceResult, error) {
	opt.LoomURL = strings.TrimRight(strings.TrimSpace(opt.LoomURL), "/")
	opt.Token = strings.TrimSpace(opt.Token)
	if opt.Token == "" {
		opt.Token = strings.TrimSpace(os.Getenv("BASHY_LOOM_TOKEN"))
	}
	if opt.Token == "" {
		opt.Token = strings.TrimSpace(os.Getenv("GITEA_TOKEN"))
	}
	opt.IntakeRepo = strings.Trim(strings.TrimSpace(opt.IntakeRepo), "/")
	opt.WorkspaceDir = strings.TrimSpace(opt.WorkspaceDir)
	if opt.Conductor == "" {
		opt.Conductor = DefaultConductorAgent()
	}
	res := PagesOnceResult{
		SchemaVersion: schemaVersion,
		Status:        "error",
		WorkspaceDir:  opt.WorkspaceDir,
		BuildCommand:  opt.BuildCommand,
	}
	if opt.WorkspaceDir == "" {
		return res, errors.New("sdlc: pages once requires --workspace")
	}
	issue, target, err := resolvePagesIntake(ctx, &opt)
	if err != nil {
		res.Error = err.Error()
		return res, err
	}
	if issue == nil {
		res.Status = "idle"
		return res, nil
	}
	res.Issue = issue
	if opt.DryRun {
		res.Status = "dry-run"
		return res, nil
	}
	target.comment(ctx, "SDLC started on the pages workspace.")
	delegateOpt := DelegateOptions{
		Config: ConfigOverrides{
			NoConfig:       true,
			ConductorAgent: opt.Conductor,
			IntakeProvider: target.kind,
			IntakeRepo:     opt.IntakeRepo,
			ProductionName: "github-pages",
			StagingName:    "local-build",
			Policies:       []string{"auto_publish_pages=true"},
		},
		Issue:   *issue,
		Cwd:     opt.WorkspaceDir,
		Sandbox: opt.Sandbox,
		Timeout: opt.Timeout,
		RunsDir: pagesRunsDir(opt.WorkspaceDir, opt.RunsDir),
	}
	delegateRes, err := Delegate(ctx, delegateOpt)
	res.RunID = delegateRes.RunID
	if err != nil {
		res.Error = err.Error()
		target.comment(ctx, "SDLC conductor failed: "+err.Error())
		return res, err
	}
	if opt.BuildCommand != "" {
		out, buildErr := runShellCommand(ctx, opt.WorkspaceDir, opt.BuildCommand)
		res.BuildOutput = out
		if buildErr != nil {
			err := fmt.Errorf("sdlc: pages build failed: %w", buildErr)
			res.Error = err.Error()
			target.comment(ctx, "SDLC build failed:\n\n```text\n"+truncateForComment(out)+"\n```")
			return res, err
		}
	}
	if res.RunID == "" {
		err := errors.New("sdlc: conductor did not produce a run id")
		res.Error = err.Error()
		return res, err
	}
	if _, err := ReviewRun(delegateOpt.RunsDir, res.RunID, "accepted", "pages build passed", "bashy sdlc pages"); err != nil {
		res.Error = err.Error()
		return res, err
	}
	if _, err := ApproveRun(delegateOpt.RunsDir, res.RunID, "auto-approved for GitHub Pages publish after local build", "bashy sdlc pages"); err != nil {
		res.Error = err.Error()
		return res, err
	}
	pub, err := PublishGitHubPages(ctx, PublishOptions{
		RunsDir: delegateOpt.RunsDir,
		RunID:   res.RunID,
		Cwd:     opt.WorkspaceDir,
		Branch:  opt.Branch,
		Repo:    opt.Repo,
	})
	res.Publish = pub
	if err != nil {
		res.Error = err.Error()
		target.comment(ctx, "SDLC publish failed: "+err.Error())
		return res, err
	}
	if opt.PublicURL != "" {
		verify, verr := VerifyContent(ctx, VerifyOptions{Target: opt.PublicURL, Timeout: 30 * time.Second})
		res.Verify = &verify
		if verr != nil {
			res.Error = verr.Error()
			target.comment(ctx, "SDLC publish completed, but public URL verification failed: "+verr.Error())
			return res, verr
		}
	}
	res.Status = "published"
	target.comment(ctx, "SDLC published the approved change to GitHub Pages. Reference: "+res.RunID)
	target.close(ctx)
	return res, nil
}

func pagesRunsDir(workspace, runsDir string) string {
	if filepath.IsAbs(runsDir) {
		return runsDir
	}
	return filepath.Join(workspace, runsDir)
}

func nextLoomIssue(ctx context.Context, baseURL, token, repo string) (*LoomIssue, error) {
	var issues []LoomIssue
	if err := loomJSON(ctx, http.MethodGet, baseURL, token, "/api/v1/repos/"+repo+"/issues?state=open&type=issues", nil, &issues); err != nil {
		return nil, err
	}
	sort.SliceStable(issues, func(i, j int) bool {
		return issuePriority(issues[i]) < issuePriority(issues[j])
	})
	for _, issue := range issues {
		if eligibleLoomIssue(issue) {
			issue := issue
			return &issue, nil
		}
	}
	return nil, nil
}

func loomLabelNames(labels []LoomLabel) []string {
	names := make([]string, len(labels))
	for i, l := range labels {
		names[i] = l.Name
	}
	return names
}

func eligibleLoomIssue(issue LoomIssue) bool {
	if issue.State != "" && issue.State != "open" {
		return false
	}
	// Loom intake stays open-by-default (requireInitiate=false): a plain open
	// issue with no reserved skip label is eligible.
	return eligibleByLabels(loomLabelNames(issue.Labels), issue.Title, false)
}

func issuePriority(issue LoomIssue) int {
	return priorityByLabels(loomLabelNames(issue.Labels))
}

func commentLoomIssue(ctx context.Context, baseURL, token, repo string, number int, body string) error {
	if token == "" {
		return nil
	}
	payload := map[string]string{"body": body}
	return loomJSON(ctx, http.MethodPost, baseURL, token, fmt.Sprintf("/api/v1/repos/%s/issues/%d/comments", repo, number), payload, nil)
}

func closeLoomIssue(ctx context.Context, baseURL, token, repo string, number int) error {
	if token == "" {
		return nil
	}
	payload := map[string]string{"state": "closed"}
	return loomJSON(ctx, http.MethodPatch, baseURL, token, fmt.Sprintf("/api/v1/repos/%s/issues/%d", repo, number), payload, nil)
}

func loomJSON(ctx context.Context, method, baseURL, token, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sdlc: Loom API %s %s: %s\n%s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func runShellCommand(ctx context.Context, dir, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func truncateForComment(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= 4000 {
		return s
	}
	return s[len(s)-4000:]
}

func ensureWorkspaceClone(ctx context.Context, dir, baseline string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Stat(dir); err == nil {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return false, readErr
		}
		if len(entries) > 0 {
			return false, fmt.Errorf("sdlc: workspace dir %s exists but is not a git checkout", dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return false, err
	}
	if err := runGitErr(ctx, "", "clone", "--origin", "baseline", baseline, dir); err != nil {
		return false, err
	}
	return true, nil
}

func setGitRemote(ctx context.Context, dir, name, url string) error {
	if strings.TrimSpace(runGit(dir, "remote", "get-url", name)) == "" {
		return runGitErr(ctx, dir, "remote", "add", name, url)
	}
	return runGitErr(ctx, dir, "remote", "set-url", name, url)
}

func runGitErr(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func isGitHubURL(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Contains(s, "github.com:") ||
		strings.Contains(s, "github.com/") ||
		strings.Contains(s, "@github.com")
}

func gitSummary(dir string) map[string]any {
	branch := strings.TrimSpace(runGit(dir, "branch", "--show-current"))
	status := runGit(dir, "status", "--porcelain=v1", "--branch")
	out := map[string]any{"branch": branch, "dirty": false, "ahead": false, "behind": false}
	if origin := strings.TrimSpace(runGit(dir, "remote", "get-url", "origin")); origin != "" {
		out["origin"] = origin
		out["origin_github"] = isGitHubURL(origin)
	}
	if baseline := strings.TrimSpace(runGit(dir, "remote", "get-url", "baseline")); baseline != "" {
		out["baseline"] = baseline
	}
	if upstream := strings.TrimSpace(runGit(dir, "remote", "get-url", "upstream")); upstream != "" {
		out["upstream"] = upstream
		out["upstream_push"] = strings.TrimSpace(runGit(dir, "remote", "get-url", "--push", "upstream"))
	}
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, "## ") {
			out["ahead"] = strings.Contains(line, "ahead")
			out["behind"] = strings.Contains(line, "behind")
			continue
		}
		if strings.TrimSpace(line) != "" {
			out["dirty"] = true
		}
	}
	return out
}

func runGit(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func listIssueFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func inferGitHubRepo(dir string) string {
	remote := strings.TrimSpace(runGit(dir, "remote", "get-url", "origin"))
	remote = strings.TrimSuffix(remote, ".git")
	switch {
	case strings.HasPrefix(remote, "git@github.com:"):
		return strings.TrimPrefix(remote, "git@github.com:")
	case strings.HasPrefix(remote, "https://github.com/"):
		return strings.TrimPrefix(remote, "https://github.com/")
	default:
		return ""
	}
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password)\s*[:=]\s*['"]?[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`),
}

func secretScan(dir string) []string {
	var hits []string
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && skipSecretScanDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(hits) >= 20 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 1<<20 {
			return nil
		}
		for _, re := range secretPatterns {
			if re.Match(data) {
				hits = append(hits, path)
				break
			}
		}
		return nil
	})
	return hits
}

func skipSecretScanDir(name string) bool {
	switch name {
	case ".git", ".next", ".contentlayer", "node_modules", "out", "dist", "build", "vendor", "external", "priorart", "testdata":
		return true
	default:
		return false
	}
}

func runGuardCommand(dir, command string) (string, string) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "skipped", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "failed", string(out)
	}
	return "ok", string(out)
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
