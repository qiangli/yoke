package dag

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/qiangli/yoke/pkg/policy/advice"
	"github.com/qiangli/yoke/pkg/secrets"
	"github.com/qiangli/yoke/pkg/snippet"
)

var deniedSnippetCommand = regexp.MustCompile(`effect cap denied "([^"]+)"`)

type snippetOutput struct {
	bytes.Buffer
	max       int64
	truncated bool
}

func (w *snippetOutput) Write(p []byte) (int, error) {
	n := len(p)
	remain := w.max - int64(w.Len())
	if remain <= 0 {
		w.truncated = true
		return n, nil
	}
	if int64(n) > remain {
		w.truncated = true
		p = p[:int(remain)]
	}
	_, err := w.Buffer.Write(p)
	return n, err
}

func runSnippet(ctx context.Context, s snippet.Snippet, tio snippet.IO) (snippet.Result, error) {
	cap, err := advice.ParseCap(strings.Join(s.Effects, ","))
	if err != nil {
		return snippet.Result{}, err
	}
	ctx = advice.WithCap(ctx, cap)
	i, err := interpFor(s.Lang)
	if err != nil {
		return snippet.Result{}, err
	}
	if tio.Dir == "" {
		tio.Dir, err = os.Getwd()
		if err != nil {
			return snippet.Result{}, err
		}
	}
	if tio.Env == nil {
		tio.Env = os.Environ()
	}
	// The interpreter inherits ordinary environment settings, but only
	// credentials named by the snippet's references may cross this boundary.
	unscrubbed := tio.Env
	tio.Env = secrets.ScrubAgentEnv(unscrubbed)
	for name := range s.Env {
		for j := len(unscrubbed) - 1; j >= 0; j-- {
			if strings.HasPrefix(unscrubbed[j], name+"=") {
				tio.Env = append(tio.Env, unscrubbed[j])
				break
			}
		}
	}
	var stdout snippetOutput
	stdout.max = tio.MaxStdout
	var stderr bytes.Buffer
	r := i.Run(ctx, &Task{Name: "snippet", Lang: s.Lang, Body: s.Body}, TaskIO{Dir: tio.Dir, Env: tio.Env, Stdin: tio.Stdin, Stdout: &stdout, Stderr: &stderr})
	out := snippet.Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Exit: r.ExitCode}
	seen := map[string]bool{}
	for _, m := range deniedSnippetCommand.FindAllSubmatch(out.Stderr, -1) {
		name := string(m[1])
		if !seen[name] {
			out.Refused = append(out.Refused, name)
			seen[name] = true
		}
	}
	if stdout.truncated {
		err = fmt.Errorf("snippet: stdout exceeds %d bytes", tio.MaxStdout)
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if r.Err != nil {
		err = errors.Join(err, r.Err)
	}
	return out, err
}

func init() {
	snippet.SetRunner(runSnippet)
	snippet.SetSecretResolver(func(_ context.Context, ref string) (string, error) {
		kv, ok := secrets.GrantAgentKey(os.Environ(), ref)
		if !ok {
			return "", snippet.ErrNotConfigured
		}
		return kv[strings.IndexByte(kv, '=')+1:], nil
	})
}
