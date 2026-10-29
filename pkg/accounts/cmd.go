package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

const CommandSchemaVersion = "bashy-account-v1"

type commandConfig struct {
	store    *Store
	registry *Registry
}

// CommandOption replaces a dependency of the account command. It is primarily
// useful to keep command fixtures hermetic.
type CommandOption func(*commandConfig)

func WithStore(s *Store) CommandOption       { return func(c *commandConfig) { c.store = s } }
func WithRegistry(r *Registry) CommandOption { return func(c *commandConfig) { c.registry = r } }

type commandState struct {
	store    *Store
	registry *Registry
	json     bool
	fields   string
	category string
	provider string
	team     string
	status   string
}

type commandError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *commandError) Error() string { return "account: " + e.Code + ": " + e.Message }

func fail(code, message string) error { return &commandError{Code: code, Message: message} }

// NewAccountCmd returns the host-local account inventory command tree. Cached
// reads (list, show and schema) only consult the local Store; provider calls are
// confined to discover, refresh, usage and resources.
func NewAccountCmd(opts ...CommandOption) *cobra.Command {
	cfg := commandConfig{store: Open(""), registry: DefaultRegistry()}
	for _, opt := range opts {
		opt(&cfg)
	}
	s := &commandState{store: cfg.store, registry: cfg.registry}
	root := &cobra.Command{
		Use:           "account",
		Short:         "Inspect and manage the host-local account inventory",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, _ error) error {
		ce := &commandError{Code: "usage", Message: "invalid command flags"}
		if s.json {
			_ = json.NewEncoder(cmd.ErrOrStderr()).Encode(map[string]any{"schema_version": CommandSchemaVersion, "command": cmd.Name(), "error": ce})
		}
		return ce
	})
	f := root.PersistentFlags()
	f.BoolVar(&s.json, "json", false, "emit the versioned JSON envelope")
	f.StringVar(&s.fields, "fields", "", "comma-separated result fields")
	f.StringVar(&s.category, "category", "", "filter accounts by category")
	f.StringVar(&s.provider, "provider", "", "filter accounts by provider")
	f.StringVar(&s.team, "team", "", "filter accounts by team")
	f.StringVar(&s.status, "status", "", "filter accounts by status")

	root.AddCommand(
		s.listCmd(), s.showCmd(), s.addCmd(), s.setCmd(), s.removeCmd(),
		s.restoreCmd(), s.discoverCmd(), s.refreshCmd(), s.usageCmd(),
		s.resourcesCmd(), s.resolveCmd(), s.schemaCmd(),
	)
	return root
}

func (s *commandState) args(name string, validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err == nil {
			return nil
		}
		ce := &commandError{Code: "usage", Message: "invalid command arguments"}
		if s.json {
			_ = json.NewEncoder(cmd.ErrOrStderr()).Encode(map[string]any{"schema_version": CommandSchemaVersion, "command": name, "error": ce})
		}
		return ce
	}
}

func (s *commandState) run(name string, fn func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := fn(cmd, args)
		if err == nil {
			return nil
		}
		ce := classifyError(err)
		if s.json {
			_ = json.NewEncoder(cmd.ErrOrStderr()).Encode(map[string]any{
				"schema_version": CommandSchemaVersion,
				"command":        name,
				"error":          ce,
			})
		}
		return ce
	}
}

func classifyError(err error) *commandError {
	var ce *commandError
	if errors.As(err, &ce) {
		return ce
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return &commandError{Code: "not_found", Message: "account not found"}
	case errors.Is(err, ErrReferenced):
		return &commandError{Code: "referenced", Message: "account still has bindings"}
	case errors.Is(err, ErrAmbiguous):
		return &commandError{Code: "ambiguous", Message: "account reference is ambiguous"}
	case errors.Is(err, ErrUnsupported):
		return &commandError{Code: "unsupported", Message: "operation is not supported for this account"}
	default:
		// Provider and storage errors can contain credential material. The CLI
		// deliberately returns a stable diagnostic instead of forwarding them.
		return &commandError{Code: "operation_failed", Message: "account operation failed"}
	}
}

func (s *commandState) envelope(cmd *cobra.Command, name, key string, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
		"schema_version": CommandSchemaVersion,
		"command":        name,
		key:              value,
	})
}

var accountFieldOrder = []string{
	"id", "alias", "category", "provider", "provider_account_id", "name", "username", "email",
	"avatar_url", "profile_url", "dashboard_url", "team", "org", "project", "tenant", "role",
	"region", "environment", "lane", "connections", "resources", "ssh_keys", "links", "status",
	"provenance", "source", "observed_at", "last_error",
}

var resourceFieldOrder = []string{"id", "kind", "name", "source"}
var metricFieldOrder = []string{"name", "value", "unit", "currency", "source", "classification", "observed_at", "window_start", "window_end", "period", "coverage", "limitations"}
var selectionFieldOrder = []string{"account_id", "connection_id", "endpoint", "profile", "credential_ref", "auth_method", "username"}

func selectedFields(raw string, allowed []string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), allowed...), nil
	}
	valid := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		valid[key] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		key := strings.TrimSpace(part)
		if key == "" || !valid[key] || seen[key] {
			return nil, fail("invalid_fields", "unknown or duplicate result field")
		}
		seen[key] = true
		out = append(out, key)
	}
	return out, nil
}

func project(value any, fields []string) (map[string]any, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var all map[string]any
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(fields))
	for _, key := range fields {
		if value, ok := all[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func projectMany[T any](items []T, fields []string) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		row, err := project(item, fields)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *commandState) filtered(accounts []Account) ([]Account, error) {
	if s.category != "" {
		if _, err := parseCategory(s.category); err != nil {
			return nil, err
		}
	}
	if s.status != "" && !validStatus(Status(s.status)) {
		return nil, fail("invalid_filter", "invalid account status")
	}
	out := accounts[:0]
	for _, a := range accounts {
		if s.category != "" && string(a.Category) != s.category {
			continue
		}
		if s.provider != "" && !strings.EqualFold(a.Provider, s.provider) {
			continue
		}
		if s.team != "" && !strings.EqualFold(a.Team, s.team) {
			continue
		}
		if s.status != "" && string(a.Status) != s.status {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func validStatus(s Status) bool {
	return s == Resolved || s == Unresolved || s == Error || s == Suppressed
}

func (s *commandState) lookup(ref string) (Account, error) {
	accounts, err := s.store.List()
	if err != nil {
		return Account{}, err
	}
	for _, a := range accounts {
		if a.ID == ref || strings.EqualFold(a.Alias, ref) {
			return a, nil
		}
	}
	var matches []Account
	for _, a := range accounts {
		if strings.HasPrefix(a.ID, ref) {
			matches = append(matches, a)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Account{}, ErrAmbiguous
	}
	return Account{}, ErrNotFound
}

func (s *commandState) listCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List cached accounts",
		Args:  s.args("list", cobra.NoArgs),
		RunE: s.run("list", func(cmd *cobra.Command, _ []string) error {
			fields, err := selectedFields(s.fields, accountFieldOrder)
			if err != nil {
				return err
			}
			items, err := s.store.List()
			if err != nil {
				return err
			}
			items, err = s.filtered(items)
			if err != nil {
				return err
			}
			rows, err := projectMany(items, fields)
			if err != nil {
				return err
			}
			if s.json {
				return s.envelope(cmd, "list", "accounts", rows)
			}
			return writeTable(cmd.OutOrStdout(), fields, rows)
		}),
	}
}

func (s *commandState) showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
		Short: "Show one cached account by ID prefix or alias",
		Args:  s.args("show", cobra.ExactArgs(1)),
		RunE: s.run("show", func(cmd *cobra.Command, args []string) error {
			fields, err := selectedFields(s.fields, accountFieldOrder)
			if err != nil {
				return err
			}
			a, err := s.lookup(args[0])
			if err != nil {
				return err
			}
			row, err := project(a, fields)
			if err != nil {
				return err
			}
			if s.json {
				return s.envelope(cmd, "show", "account", row)
			}
			return writeDetail(cmd.OutOrStdout(), fields, row)
		}),
	}
}

type addFlags struct {
	kind, user, endpoint, provider, team, credential, profile          string
	name, email, org, project, tenant, role, region, environment, lane string
}

func (s *commandState) addCmd() *cobra.Command {
	var f addFlags
	c := &cobra.Command{
		Use:   "add ALIAS",
		Short: "Add account metadata using a credential reference",
		Args:  s.args("add", cobra.ExactArgs(1)),
		RunE: s.run("add", func(cmd *cobra.Command, args []string) error {
			fields, err := selectedFields(s.fields, accountFieldOrder)
			if err != nil {
				return err
			}
			category, err := parseCategory(f.kind)
			if err != nil {
				return err
			}
			if f.credential != "" && !ValidCredentialRef(f.credential) {
				return fail("invalid_credential_ref", "credential must be a secret:, ssh-key:, env:, or profile: reference")
			}
			provider := f.provider
			if provider == "" {
				provider = string(category)
			}
			if category == HostLogin && (f.user == "" || f.endpoint == "" || f.credential == "") {
				return fail("invalid_account", "host-login requires --user, --endpoint, and --credential")
			}
			a := Account{
				Alias: args[0], Category: category, Provider: provider, Name: f.name,
				Username: f.user, Email: f.email, Team: f.team, Org: f.org, Project: f.project,
				Tenant: f.tenant, Role: f.role, Region: f.region, Environment: f.environment, Lane: f.lane,
			}
			if f.endpoint != "" || f.profile != "" || f.credential != "" {
				a.Connections = []Connection{{ID: "primary", Endpoint: f.endpoint, Profile: f.profile, CredentialRef: f.credential}}
			}
			added, err := s.store.Add(a)
			if err != nil {
				return fail("invalid_account", "account metadata was rejected")
			}
			row, err := project(added, fields)
			if err != nil {
				return err
			}
			if s.json {
				return s.envelope(cmd, "add", "account", row)
			}
			return writeDetail(cmd.OutOrStdout(), fields, row)
		}),
	}
	c.Flags().StringVar(&f.kind, "kind", "", "account category (required)")
	c.Flags().StringVar(&f.user, "user", "", "username")
	c.Flags().StringVar(&f.endpoint, "endpoint", "", "connection endpoint")
	c.Flags().StringVar(&f.provider, "provider", "", "provider name (defaults to kind)")
	c.Flags().StringVar(&f.team, "team", "", "team name")
	c.Flags().StringVar(&f.credential, "credential", "", "safe credential reference")
	c.Flags().StringVar(&f.profile, "profile", "", "connection profile")
	c.Flags().StringVar(&f.name, "name", "", "display name")
	c.Flags().StringVar(&f.email, "email", "", "account email")
	c.Flags().StringVar(&f.org, "org", "", "organization")
	c.Flags().StringVar(&f.project, "project", "", "project")
	c.Flags().StringVar(&f.tenant, "tenant", "", "tenant")
	c.Flags().StringVar(&f.role, "role", "", "role")
	c.Flags().StringVar(&f.region, "region", "", "region")
	c.Flags().StringVar(&f.environment, "environment", "", "environment")
	c.Flags().StringVar(&f.lane, "lane", "", "subscription or API lane")
	return c
}

func parseCategory(value string) (Category, error) {
	c := Category(value)
	switch c {
	case Cloudbox, Satellite, DigitalOcean, AWS, LLM, HostLogin, Other:
		return c, nil
	default:
		return "", fail("invalid_category", "unknown account category")
	}
}

func (s *commandState) setCmd() *cobra.Command {
	var sets, unsets []string
	c := &cobra.Command{
		Use:   "set ID",
		Short: "Set or unset editable account metadata",
		Args:  s.args("set", cobra.ExactArgs(1)),
		RunE: s.run("set", func(cmd *cobra.Command, args []string) error {
			if len(sets) == 0 && len(unsets) == 0 {
				return fail("usage", "give --set or --unset")
			}
			a, err := s.lookup(args[0])
			if err != nil {
				return err
			}
			values := map[string]string{}
			for _, assignment := range sets {
				key, value, ok := strings.Cut(assignment, "=")
				if !ok || key == "" {
					return fail("invalid_field", "--set requires field=value")
				}
				if _, exists := values[key]; exists {
					return fail("invalid_field", "field was set more than once")
				}
				values[key] = value
			}
			if err := s.store.PatchOverride(a.ID, values, unsets); err != nil {
				if errors.Is(err, ErrNotFound) {
					return err
				}
				return fail("invalid_field", "only alias, name, region, and environment are editable")
			}
			return s.emitAccount(cmd, "set", a.ID)
		}),
	}
	c.Flags().StringArrayVar(&sets, "set", nil, "set editable field=value (repeatable)")
	c.Flags().StringArrayVar(&unsets, "unset", nil, "remove an editable override (repeatable)")
	return c
}

func (s *commandState) emitAccount(cmd *cobra.Command, name, ref string) error {
	fields, err := selectedFields(s.fields, accountFieldOrder)
	if err != nil {
		return err
	}
	a, err := s.lookup(ref)
	if err != nil {
		return err
	}
	row, err := project(a, fields)
	if err != nil {
		return err
	}
	if s.json {
		return s.envelope(cmd, name, "account", row)
	}
	return writeDetail(cmd.OutOrStdout(), fields, row)
}

func (s *commandState) removeCmd() *cobra.Command {
	return &cobra.Command{Use: "rm ID", Short: "Suppress account metadata and rediscovery", Args: s.args("rm", cobra.ExactArgs(1)), RunE: s.run("rm", func(cmd *cobra.Command, args []string) error {
		a, err := s.lookup(args[0])
		if err != nil {
			return err
		}
		if err := s.store.Remove(a.ID); err != nil {
			return err
		}
		return s.emitAccount(cmd, "rm", a.ID)
	})}
}

func (s *commandState) restoreCmd() *cobra.Command {
	return &cobra.Command{Use: "restore ID", Short: "Restore suppressed account metadata", Args: s.args("restore", cobra.ExactArgs(1)), RunE: s.run("restore", func(cmd *cobra.Command, args []string) error {
		a, err := s.lookup(args[0])
		if err != nil {
			return err
		}
		if err := s.store.Restore(a.ID); err != nil {
			return err
		}
		return s.emitAccount(cmd, "restore", a.ID)
	})}
}

func materialize(adapter string, observations []Observation) ([]Account, error) {
	out := make([]Account, 0, len(observations))
	for _, observation := range observations {
		key, status, err := identity(adapter, observation)
		if err != nil {
			return nil, err
		}
		a := observation.Account
		a.ID = localID(key)
		a.Provenance = adapter
		if a.Status == "" || status == Unresolved {
			a.Status = status
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Alias != out[j].Alias {
			return out[i].Alias < out[j].Alias
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *commandState) discoverCmd() *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "discover",
		Short: "Discover accounts from registered local adapters",
		Args:  s.args("discover", cobra.NoArgs),
		RunE: s.run("discover", func(cmd *cobra.Command, _ []string) error {
			fields, err := selectedFields(s.fields, accountFieldOrder)
			if err != nil {
				return err
			}
			var found []Account
			for _, adapter := range s.registry.Adapters(Discover) {
				observations, err := adapter.Discover(cmd.Context())
				if err != nil {
					return fail("adapter_failed", "account discovery failed")
				}
				items, err := materialize(adapter.Name(), observations)
				if err != nil {
					return fail("invalid_observation", "adapter returned invalid account metadata")
				}
				found = append(found, items...)
				if !dryRun {
					if err := s.store.Observe(adapter.Name(), observations); err != nil {
						return err
					}
				}
			}
			sort.Slice(found, func(i, j int) bool { return found[i].ID < found[j].ID })
			rows, err := projectMany(found, fields)
			if err != nil {
				return err
			}
			if s.json {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": CommandSchemaVersion, "command": "discover", "dry_run": dryRun, "accounts": rows})
			}
			if dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "# dry-run")
			}
			return writeTable(cmd.OutOrStdout(), fields, rows)
		}),
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "report discoveries without changing the store")
	return c
}

func (s *commandState) adapterFor(a Account, capability Capability) (Adapter, error) {
	for _, name := range []string{a.Provenance, a.Provider, string(a.Category)} {
		if name == "" || name == "manual" {
			continue
		}
		if adapter, err := s.registry.Adapter(name, capability); err == nil {
			return adapter, nil
		}
	}
	return nil, ErrUnsupported
}

func (s *commandState) refreshOne(ctx context.Context, a Account) (Account, error) {
	adapter, err := s.adapterFor(a, Verify)
	if err != nil {
		return Account{}, err
	}
	observation, err := adapter.Verify(ctx, a)
	if err != nil {
		return Account{}, fail("adapter_failed", "account refresh failed")
	}
	if err := s.store.Observe(adapter.Name(), []Observation{observation}); err != nil {
		return Account{}, err
	}
	items, err := materialize(adapter.Name(), []Observation{observation})
	if err != nil || len(items) != 1 {
		return Account{}, fail("invalid_observation", "adapter returned invalid account metadata")
	}
	return items[0], nil
}

func (s *commandState) refreshCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "refresh ID | --all",
		Short: "Refresh account identity through registered adapters",
		Args:  s.args("refresh", cobra.MaximumNArgs(1)),
		RunE: s.run("refresh", func(cmd *cobra.Command, args []string) error {
			if all == (len(args) == 1) {
				return fail("usage", "give exactly one ID or --all")
			}
			fields, err := selectedFields(s.fields, accountFieldOrder)
			if err != nil {
				return err
			}
			var targets []Account
			if all {
				targets, err = s.store.List()
			} else {
				var a Account
				a, err = s.lookup(args[0])
				targets = []Account{a}
			}
			if err != nil {
				return err
			}
			var refreshed []Account
			for _, a := range targets {
				if a.Status == Suppressed {
					continue
				}
				item, err := s.refreshOne(cmd.Context(), a)
				if err != nil {
					if all && errors.Is(err, ErrUnsupported) {
						continue
					}
					return err
				}
				refreshed = append(refreshed, item)
			}
			rows, err := projectMany(refreshed, fields)
			if err != nil {
				return err
			}
			if s.json {
				return s.envelope(cmd, "refresh", "accounts", rows)
			}
			return writeTable(cmd.OutOrStdout(), fields, rows)
		}),
	}
	c.Flags().BoolVar(&all, "all", false, "refresh every nonsuppressed account")
	return c
}

func (s *commandState) usageCmd() *cobra.Command {
	var period string
	c := &cobra.Command{
		Use:   "usage ID",
		Short: "Query sourced usage and billing metrics",
		Args:  s.args("usage", cobra.ExactArgs(1)),
		RunE: s.run("usage", func(cmd *cobra.Command, args []string) error {
			if period != "day" && period != "week" && period != "month" {
				return fail("invalid_period", "period must be day, week, or month")
			}
			fields, err := selectedFields(s.fields, metricFieldOrder)
			if err != nil {
				return err
			}
			a, err := s.lookup(args[0])
			if err != nil {
				return err
			}
			adapter, err := s.adapterFor(a, Usage)
			if err != nil {
				return err
			}
			metrics, err := adapter.Usage(cmd.Context(), a, period)
			if err != nil {
				return fail("adapter_failed", "account usage query failed")
			}
			if billing, berr := s.adapterFor(a, Billing); berr == nil && billing.Name() == adapter.Name() {
				more, berr := billing.Billing(cmd.Context(), a, period)
				if berr != nil {
					return fail("adapter_failed", "account billing query failed")
				}
				metrics = append(metrics, more...)
			}
			sort.Slice(metrics, func(i, j int) bool {
				if metrics[i].Name != metrics[j].Name {
					return metrics[i].Name < metrics[j].Name
				}
				return metrics[i].Source < metrics[j].Source
			})
			rows, err := projectMany(metrics, fields)
			if err != nil {
				return err
			}
			if s.json {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": CommandSchemaVersion, "command": "usage", "account_id": a.ID, "period": period, "metrics": rows})
			}
			return writeTable(cmd.OutOrStdout(), fields, rows)
		}),
	}
	c.Flags().StringVar(&period, "period", "month", "usage period: day, week, or month")
	return c
}

func (s *commandState) resourcesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resources ID",
		Short: "Query resources through the account adapter",
		Args:  s.args("resources", cobra.ExactArgs(1)),
		RunE: s.run("resources", func(cmd *cobra.Command, args []string) error {
			fields, err := selectedFields(s.fields, resourceFieldOrder)
			if err != nil {
				return err
			}
			a, err := s.lookup(args[0])
			if err != nil {
				return err
			}
			adapter, err := s.adapterFor(a, Resources)
			if err != nil {
				return err
			}
			resources, err := adapter.Resources(cmd.Context(), a)
			if err != nil {
				return fail("adapter_failed", "account resource query failed")
			}
			sort.Slice(resources, func(i, j int) bool {
				if resources[i].Kind != resources[j].Kind {
					return resources[i].Kind < resources[j].Kind
				}
				return resources[i].ID < resources[j].ID
			})
			rows, err := projectMany(resources, fields)
			if err != nil {
				return err
			}
			if s.json {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": CommandSchemaVersion, "command": "resources", "account_id": a.ID, "resources": rows})
			}
			return writeTable(cmd.OutOrStdout(), fields, rows)
		}),
	}
}

func (s *commandState) resolveCmd() *cobra.Command {
	var operation string
	c := &cobra.Command{
		Use:   "resolve ID",
		Short: "Resolve safe connection metadata for an operation",
		Args:  s.args("resolve", cobra.ExactArgs(1)),
		RunE: s.run("resolve", func(cmd *cobra.Command, args []string) error {
			if operation == "" {
				return fail("usage", "--for is required")
			}
			fields, err := selectedFields(s.fields, selectionFieldOrder)
			if err != nil {
				return err
			}
			a, err := s.lookup(args[0])
			if err != nil {
				return err
			}
			var selection Selection
			if adapter, aerr := s.adapterFor(a, Select); aerr == nil {
				selection, err = adapter.Select(cmd.Context(), a, Operation(operation))
				if err != nil {
					return fail("selection_failed", "adapter could not select this account")
				}
			} else {
				selection, err = s.store.Resolve(a.ID, Operation(operation))
				if err != nil {
					return err
				}
			}
			row, err := project(selection, fields)
			if err != nil {
				return err
			}
			if s.json {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": CommandSchemaVersion, "command": "resolve", "operation": operation, "selection": row})
			}
			return writeDetail(cmd.OutOrStdout(), fields, row)
		}),
	}
	c.Flags().StringVar(&operation, "for", "", "operation that will consume the selection")
	return c
}

func (s *commandState) schemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Show account categories, fields, and credential-reference forms",
		Args:  s.args("schema", cobra.NoArgs),
		RunE: s.run("schema", func(cmd *cobra.Command, _ []string) error {
			if s.fields != "" {
				return fail("invalid_fields", "schema does not accept --fields")
			}
			data := map[string]any{
				"schema_version":      SchemaVersion,
				"categories":          []Category{Cloudbox, Satellite, DigitalOcean, AWS, LLM, HostLogin, Other},
				"statuses":            []Status{Resolved, Unresolved, Error, Suppressed},
				"editable_fields":     []string{"alias", "environment", "name", "region"},
				"credential_prefixes": []string{"env:", "profile:", "secret:", "ssh-key:"},
			}
			if s.json {
				return s.envelope(cmd, "schema", "schema", data)
			}
			b, err := json.MarshalIndent(data, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return err
		}),
	}
}

func writeTable(w io.Writer, fields []string, rows []map[string]any) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.ToUpper(strings.Join(fields, "\t"))); err != nil {
		return err
	}
	for _, row := range rows {
		values := make([]string, 0, len(fields))
		for _, key := range fields {
			values = append(values, textValue(row[key]))
		}
		if _, err := fmt.Fprintln(tw, strings.Join(values, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func writeDetail(w io.Writer, fields []string, row map[string]any) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, key := range fields {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", key, textValue(row[key])); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func textValue(v any) string {
	if v == nil {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	case float64, bool:
		return fmt.Sprint(value)
	default:
		b, _ := json.Marshal(value)
		return string(b)
	}
}
