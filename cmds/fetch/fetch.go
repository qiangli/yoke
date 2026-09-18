// Package fetchcmd implements `fetch`: an agent-friendly URL / REST client.
//
// It is the structured-output counterpart to curl/wget — GET a URL (or any
// method) and write the body to stdout, with first-class options for an agent:
//
//	--md    convert an HTML response to clean markdown (read docs/issues without a browser)
//	--json  emit a bashy-fetch-v1 envelope {status, headers, body, …} instead of the raw body
//	-H/-d/-X/-u/-t  full REST control (headers, body, method, basic/bearer auth)
//
// HTTP is handled by resty (resty.dev/v3, MIT, pure-Go); HTML→markdown by
// JohannesKaufmann/html-to-markdown (MIT). curl/wget stay untouched on PATH.
package fetchcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	resty "resty.dev/v3"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/coreutils/tool"
)

const fetchSchemaVersion = "bashy-fetch-v1"

var cmd = &tool.Tool{
	Name:     "fetch",
	Synopsis: "Fetch a URL (REST client) and print the body; --md to markdown, --json for an envelope.",
	Usage:    "fetch [-X METHOD] [-H 'K: V']... [-d DATA] [-u USER:PASS] [-t TOKEN] [--md] [--json] URL",
}

func init() { cmd.Run = run; tool.Register(cmd) }

func run(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	method := fs.StringP("method", "X", "", "HTTP method (default GET, or POST when -d is given)")
	headers := fs.StringArrayP("header", "H", nil, "request header 'Key: Value' (repeatable)")
	data := fs.StringP("data", "d", "", "request body; @FILE reads a file, @- reads stdin")
	query := fs.StringP("query", "q", "", "raw query string (k=v&k2=v2)")
	user := fs.StringP("user", "u", "", "basic auth USER:PASS")
	token := fs.StringP("token", "t", "", "bearer auth token")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	md := fs.Bool("md", false, "convert an HTML response body to markdown")
	asJSON := fs.Bool("json", weavecli.IsAgent(), "emit a bashy-fetch-v1 envelope (default under $BASHY_AGENTIC)")
	include := fs.BoolP("include", "i", false, "prepend response status + headers to text output")
	output := fs.StringP("output", "o", "", "write the body to FILE instead of stdout")
	fail := fs.BoolP("fail", "f", false, "exit non-zero on HTTP status >= 400")

	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) != 1 {
		return tool.UsageError(rc, cmd, "exactly one URL is required")
	}
	url := operands[0]

	m := strings.ToUpper(*method)
	if m == "" {
		if *data != "" {
			m = "POST"
		} else {
			m = "GET"
		}
	}

	client := resty.New()
	defer client.Close()
	client.SetTimeout(*timeout)
	req := client.R()

	for _, h := range *headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return tool.UsageError(rc, cmd, "bad header %q (want 'Key: Value')", h)
		}
		req.SetHeader(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	if *query != "" {
		req.SetQueryString(*query)
	}
	if *user != "" {
		u, p, _ := strings.Cut(*user, ":")
		req.SetBasicAuth(u, p)
	}
	if *token != "" {
		req.SetAuthToken(*token)
	}
	if *data != "" {
		body, err := readData(rc, *data)
		if err != nil {
			fmt.Fprintf(rc.Err, "fetch: %v\n", err)
			return 1
		}
		req.SetBody(body)
	}

	resp, err := req.Execute(m, url)
	if err != nil {
		fmt.Fprintf(rc.Err, "fetch: %s %s: %v\n", m, url, err)
		return 1
	}

	body := resp.String()
	if *md {
		if out, cerr := htmltomarkdown.ConvertString(body); cerr == nil {
			body = out
		} else {
			fmt.Fprintf(rc.Err, "fetch: --md: %v (printing raw body)\n", cerr)
		}
	}

	status := resp.StatusCode()
	exit := 0
	if *fail && status >= 400 {
		exit = 22 // curl's "HTTP error" exit code
	}

	if *asJSON {
		hdr := map[string]string{}
		for k := range resp.Header() {
			hdr[k] = resp.Header().Get(k)
		}
		env := map[string]any{
			"schema_version": fetchSchemaVersion,
			"url":            url,
			"method":         m,
			"status":         resp.Status(),
			"status_code":    status,
			"headers":        hdr,
			"body":           body,
			"duration_ms":    resp.Duration().Milliseconds(),
		}
		b, _ := json.Marshal(env)
		if werr := writeOut(rc, *output, string(b)+"\n"); werr != nil {
			fmt.Fprintf(rc.Err, "fetch: %v\n", werr)
			return 1
		}
		return exit
	}

	var sb strings.Builder
	if *include {
		fmt.Fprintf(&sb, "%s\n", resp.Status())
		for k := range resp.Header() {
			fmt.Fprintf(&sb, "%s: %s\n", k, resp.Header().Get(k))
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		sb.WriteByte('\n')
	}
	if werr := writeOut(rc, *output, sb.String()); werr != nil {
		fmt.Fprintf(rc.Err, "fetch: %v\n", werr)
		return 1
	}
	return exit
}

// readData resolves the -d value: @FILE reads a file, @- reads stdin, else literal.
func readData(rc *tool.RunContext, d string) (string, error) {
	if d == "@-" {
		b, err := io.ReadAll(rc.In)
		return string(b), err
	}
	if strings.HasPrefix(d, "@") {
		b, err := os.ReadFile(rc.Path(d[1:]))
		return string(b), err
	}
	return d, nil
}

func writeOut(rc *tool.RunContext, file, s string) error {
	if file == "" {
		fmt.Fprint(rc.Out, s)
		return nil
	}
	return os.WriteFile(rc.Path(file), []byte(s), 0o644)
}
