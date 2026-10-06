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
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/atlas"
)

// Policy grants privileged effects explicitly. A nil or zero Policy grants none.
// Configure Allow and Audit before serving; Audit may be called concurrently.
type Policy struct {
	Allow map[string]bool
	Audit func(Record)
}

// Record describes one tools/call, including policy denials and dispatch failures.
type Record struct {
	Time     string        `json:"time"`
	Tool     string        `json:"tool"`
	Command  string        `json:"command"`
	Effects  []string      `json:"effects"`
	Allowed  bool          `json:"allowed"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration"`
	Denial   string        `json:"denial"`
}

// Options configures the server's effect policy.
type Options struct{ Policy *Policy }

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
	if _, ok := atlas.Lookup(name); !ok && tool.Lookup(name) == nil {
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

func commandEffects(name string) []string {
	if entry, ok := atlas.Lookup(name); ok {
		return append([]string{}, entry.Effects...)
	}
	return []string{}
}

func policyDenial(name string, err error) (*mcpsdk.CallToolResult, RunToolOutput) {
	out := RunToolOutput{Stderr: err.Error(), ExitCode: 126}
	if _, ok := atlas.Lookup(name); !ok && tool.Lookup(name) == nil {
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
				if record.Tool == "run_tool" || record.Denial != "" {
					data, marshalErr := json.Marshal(res.StructuredContent)
					var out struct {
						ExitCode *int `json:"exit_code"`
					}
					if marshalErr == nil && json.Unmarshal(data, &out) == nil && out.ExitCode != nil {
						record.ExitCode = *out.ExitCode
					}
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
			record.Effects = commandEffects(in.Name)
			if denial := p.Check(in.Name, record.Effects); denial != nil {
				record.Denial = denial.Error()
				res, _ := policyDenial(in.Name, denial)
				return res, nil
			}
		default:
			record.Effects = commandEffects(record.Command)
			if denial := p.Check(record.Command, record.Effects); denial != nil {
				record.Denial = denial.Error()
				res, _ := policyDenial(record.Command, denial)
				return res, nil
			}
		}
		record.Allowed = true
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
