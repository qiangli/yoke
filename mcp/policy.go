package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

// Policy grants privileged effects explicitly. A nil or zero Policy grants none.
// Configure Allow and Audit before serving; Audit may be called concurrently.
//
// A gated-effect denial on a known command never executes here and never
// raises an approval here: the call answers {state: "gated", effects: [...]}
// (exit 126) so the master's router re-dispatches the same call over the
// pull route with the same idempotency key. The pull conductor raises the
// task-bound digest-bound ask approval there and executes only after the
// owning host settles it through ask.ApplyRemote. Unknown commands still
// deny outright with exit 2.
type Policy struct {
	Allow map[string]bool
	Audit func(Record)
	// Execute, when set, wraps every tools/call that will execute: run_tool
	// and direct tools that passed the policy. Pure list_tools/server_info,
	// policy denials and gated answers never reach it, so a gated call is
	// answered before any caller-side bookkeeping (such as an idempotency
	// claim). It may answer without calling next.
	Execute   func(ctx context.Context, call *mcpsdk.CallToolRequest, next func(context.Context) (mcpsdk.Result, error)) (mcpsdk.Result, error)
	synthetic *sync.Map
}

// Record describes one tools/call, including policy denials and dispatch failures.
type Record struct {
	Time       string        `json:"time"`
	Tool       string        `json:"tool"`
	Command    string        `json:"command"`
	Effects    []string      `json:"effects"`
	Allowed    bool          `json:"allowed"`
	ExitCode   int           `json:"exit_code"`
	Duration   time.Duration `json:"duration"`
	Denial     string        `json:"denial"`
	ApprovalID string        `json:"approval_id,omitempty"`
}

func privileged(effect string) bool {
	switch effect {
	case "destroy", "spend", "cred", "priv":
		return true
	}
	return false
}

// Check rejects unknown commands and ungranted privileged effects in Atlas
// vocabulary order, independently of the order supplied by the caller.
func (p *Policy) Check(name string, effects []string) error {
	if !p.knownCommand(name) {
		return fmt.Errorf("unknown command: %s", name)
	}
	for _, effect := range atlas.Effects() {
		if !privileged(effect) || (p != nil && p.Allow[effect]) {
			continue
		}
		for _, actual := range effects {
			if actual == effect {
				return fmt.Errorf("denied: effect %s requires --allow %s", effect, effect)
			}
		}
	}
	return nil
}

// ParseAllow parses a comma-separated list of privileged effect grants.
func ParseAllow(csv string) (map[string]bool, error) {
	allow := make(map[string]bool)
	if strings.TrimSpace(csv) == "" {
		return allow, nil
	}
	for _, word := range strings.Split(csv, ",") {
		word = strings.TrimSpace(word)
		if !privileged(word) {
			return nil, fmt.Errorf("invalid allowed effect %q: allowed values are destroy, spend, cred, priv", word)
		}
		allow[word] = true
	}
	return allow, nil
}

func (p *Policy) audit(record Record) {
	if p != nil && p.Audit != nil {
		p.Audit(record)
		return
	}
	// Marshal first so each record is emitted in one write, even with concurrent calls.
	if data, err := json.Marshal(record); err == nil {
		_, _ = os.Stderr.Write(append(data, '\n'))
	}
}

func (p *Policy) knownCommand(name string) bool {
	if p != nil && p.synthetic != nil {
		if _, ok := p.synthetic.Load(name); ok {
			return true
		}
	}
	_, ok := atlas.Lookup(name)
	return ok || tool.Lookup(name) != nil
}

func (p *Policy) commandEffects(name string) []string {
	if p != nil && p.synthetic != nil {
		if effects, ok := p.synthetic.Load(name); ok {
			return append([]string{}, effects.([]string)...)
		}
	}
	if entry, ok := atlas.Lookup(name); ok {
		return append([]string{}, entry.Effects...)
	}
	return []string{}
}

// GatedState is the sync answer when a call's effects need a pull-route
// approval. It hands the call to the pull route; only the pull conductor
// raises the task-bound digest approval and executes after approve.
const GatedState = "gated"

// Gated is the structured sync answer for a gated call: the pull-route
// handoff. It carries no approval binding because nothing was raised here.
type Gated struct {
	State   string   `json:"state"`
	Effects []string `json:"effects"`
}

// gatedResult answers a known command's privileged-effect denial with the
// pull-route handoff. Unknown commands, and denials with no privileged
// effect in the command's own set, report false so the caller keeps the
// outright denial. The call never executes on this route.
func (p *Policy) gatedResult(name string, effects []string) (*mcpsdk.CallToolResult, bool) {
	if !p.knownCommand(name) {
		return nil, false
	}
	var gated []string
	for _, effect := range atlas.Effects() {
		if !privileged(effect) {
			continue
		}
		for _, actual := range effects {
			if actual == effect {
				gated = append(gated, effect)
				break
			}
		}
	}
	if len(gated) == 0 {
		return nil, false
	}
	text := fmt.Sprintf("gated: effects %s need pull-route approval; not executed",
		strings.Join(gated, ","))
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		StructuredContent: Gated{
			State:   GatedState,
			Effects: gated,
		},
	}, true
}

func (p *Policy) policyDenial(name string, err error) (*mcpsdk.CallToolResult, RunToolOutput) {
	out := RunToolOutput{Stderr: err.Error(), ExitCode: 126}
	if !p.knownCommand(name) {
		// Preserve the generic runner's historical unknown-command status and stderr.
		out.ExitCode = 2
		out.Stderr = fmt.Sprintf("%s: not a supported command\n", name)
	}
	return &mcpsdk.CallToolResult{
		IsError:           true,
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
		StructuredContent: out,
	}, out
}

// middleware covers all tools/call requests, including schema failures and
// tools registered after NewServerWithOptions returns.
func (p *Policy) middleware(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
	return func(ctx context.Context, method string, req mcpsdk.Request) (result mcpsdk.Result, err error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		start := time.Now()
		record := Record{Time: start.UTC().Format(time.RFC3339Nano), Effects: []string{}}
		defer func() {
			record.Duration = time.Since(start)
			if err != nil {
				record.ExitCode = 1
			}
			if res, ok := result.(*mcpsdk.CallToolResult); ok && res != nil {
				if res.IsError && record.ExitCode == 0 {
					record.ExitCode = 1
				}
				data, marshalErr := json.Marshal(res.StructuredContent)
				var out struct {
					ExitCode *int `json:"exit_code"`
				}
				if marshalErr == nil && json.Unmarshal(data, &out) == nil && out.ExitCode != nil {
					record.ExitCode = *out.ExitCode
				}
			}
			p.audit(record)
		}()
		call := req.(*mcpsdk.CallToolRequest)
		record.Tool = call.Params.Name
		record.Command = call.Params.Name
		switch call.Params.Name {
		case "list_tools", "server_info":
			record.Effects = []string{"pure"}
		case "run_tool":
			var in RunToolInput
			if json.Unmarshal(call.Params.Arguments, &in) != nil {
				return next(ctx, method, req)
			}
			record.Command = in.Name
			record.Effects = atlas.NarrowEffects(append([]string{in.Name}, in.Args...), (*Policy)(nil).commandEffects(in.Name))
			if denial := p.Check(in.Name, record.Effects); denial != nil {
				if res, ok := p.gatedResult(in.Name, record.Effects); ok {
					record.Denial = denial.Error()
					record.ExitCode = 126
					return res, nil
				}
				record.Denial = denial.Error()
				res, _ := p.policyDenial(in.Name, denial)
				return res, nil
			}
		default:
			var verb struct {
				Args []string `json:"args"`
			}
			_ = json.Unmarshal(call.Params.Arguments, &verb)
			record.Effects = atlas.NarrowEffects(append([]string{record.Command}, verb.Args...), p.commandEffects(record.Command))
			if denial := p.Check(record.Command, record.Effects); denial != nil {
				if res, ok := p.gatedResult(record.Command, record.Effects); ok {
					record.Denial = denial.Error()
					record.ExitCode = 126
					return res, nil
				}
				record.Denial = denial.Error()
				res, _ := p.policyDenial(record.Command, denial)
				return res, nil
			}
		}
		record.Allowed = true
		if p != nil && p.Execute != nil && call.Params.Name != "list_tools" && call.Params.Name != "server_info" {
			return p.Execute(ctx, call, func(ctx context.Context) (mcpsdk.Result, error) {
				return next(ctx, method, req)
			})
		}
		return next(ctx, method, req)
	}
}

// ToolsSHA256 hashes canonical JSON (sorted object keys, no whitespace) of
// the complete tools/list result. It queries the live server so later tool
// registrations are included; pagination is collapsed into one sorted list.
func ToolsSHA256(ctx context.Context, srv *mcpsdk.Server) (string, error) {
	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		return "", err
	}
	defer ss.Close()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "tools-hash", Version: "1"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return "", err
	}
	defer cs.Close()
	var result *mcpsdk.ListToolsResult
	params := &mcpsdk.ListToolsParams{}
	for {
		page, err := cs.ListTools(ctx, params)
		if err != nil {
			return "", err
		}
		if result == nil {
			result = page
		} else {
			result.Tools = append(result.Tools, page.Tools...)
		}
		if page.NextCursor == "" {
			break
		}
		params.Cursor = page.NextCursor
	}
	result.NextCursor = ""
	sort.Slice(result.Tools, func(i, j int) bool { return result.Tools[i].Name < result.Tools[j].Name })
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	var canonical any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&canonical); err != nil {
		return "", err
	}
	data, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

type serverInfoOutput struct {
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	ProtocolVersion string   `json:"protocol_version"`
	ToolsSHA256     string   `json:"tools_sha256"`
	AllowedEffects  []string `json:"allowed_effects"`
}

func addServerInfo(srv *mcpsdk.Server, name, version string, policy *Policy) {
	mcpsdk.AddTool(srv, &mcpsdk.Tool{Name: "server_info", Description: "Server identity, protocol version, live tool-definition SHA-256, and granted privileged effects."},
		func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ ListToolsInput) (*mcpsdk.CallToolResult, serverInfoOutput, error) {
			hash, err := ToolsSHA256(ctx, srv)
			out := serverInfoOutput{Name: name, Version: version, ProtocolVersion: mcpsdk.SupportedProtocolVersions()[0], ToolsSHA256: hash, AllowedEffects: []string{}}
			for _, effect := range atlas.Effects() {
				if privileged(effect) && policy.Allow[effect] {
					out.AllowedEffects = append(out.AllowedEffects, effect)
				}
			}
			return nil, out, err
		})
}
