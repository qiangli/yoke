package lenscmd

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens"
)

// scriptTask is the exact JSON body passed by weave through WEAVE_ISSUE_BODY.
// Policy is repeated in the launch argv so a changed body cannot widen authority.
type scriptTask struct {
	Schema   string                      `json:"schema"`
	Artifact lens.CompiledScriptArtifact `json:"artifact"`
	Caps     []string                    `json:"caps"`
	Effects  []string                    `json:"effects"`
	Help     string                      `json:"help"`
	NoInput  bool                        `json:"no_input"`
}

var scriptCmd = &tool.Tool{
	Name:     "lens-script",
	Synopsis: "Execute a bounded content-addressed script from a weave JSON task body; no model calls.",
	Usage:    "lens-script --digest SHA256 --caps LIST --effects LIST",
}

func init() { scriptCmd.Run = runScriptWorker; tool.Register(scriptCmd) }

func runScriptWorker(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(scriptCmd.Name)
	digest := fs.String("digest", "", "expected sha256 content identity")
	caps := fs.String("caps", "", "explicit comma-separated allowed capabilities")
	effects := fs.String("effects", "", "explicit comma-separated allowed effects")
	operands, code := tool.Parse(rc, scriptCmd, fs, args)
	if code >= 0 {
		return code
	}
	fail := func(err error) int { fmt.Fprintln(rc.Err, "lens-script:", err); return lens.ExitInvalid }
	if len(operands) != 0 || *digest == "" || *caps == "" || *effects == "" {
		return tool.UsageError(rc, scriptCmd, "requires digest, nonempty caps/effects and no operands")
	}
	body := rc.Getenv("WEAVE_ISSUE_BODY")
	if len(body) == 0 || len(body) > 64*1024 {
		return fail(fmt.Errorf("task body must be 1..65536 bytes"))
	}
	var task scriptTask
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&task); err != nil {
		return fail(err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fail(fmt.Errorf("trailing task data"))
	}
	allowedCaps, allowedEffects := strings.Split(*caps, ","), strings.Split(*effects, ",")
	for _, values := range [][]string{allowedCaps, allowedEffects} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return fail(fmt.Errorf("empty policy entry"))
			}
		}
	}
	if task.Schema != "lens-script-task-v1" || task.Help != "script" || !task.NoInput || task.Artifact.Digest != *digest ||
		!slices.Equal(task.Caps, allowedCaps) || !slices.Equal(task.Effects, allowedEffects) {
		return fail(fmt.Errorf("task identity, execution metadata or policy differs from launch contract"))
	}
	runner := &lens.LocalScriptRunner{AllowedCaps: allowedCaps, AllowedEffects: allowedEffects, WorkDir: rc.Path(".")}
	res, err := runner.RunScript(rc.Ctx, &task.Artifact)
	if err != nil {
		return fail(err)
	}
	if _, err := rc.Out.Write(res.Stdout); err != nil {
		return fail(err)
	}
	if _, err := rc.Err.Write(res.Stderr); err != nil {
		return fail(err)
	}
	if res.Error != nil {
		fmt.Fprintln(rc.Err, "lens-script:", res.Error)
	}
	// The owning weave wrapper records this real process status and captured log.
	// This command does not claim a library recording receipt or mutate a queue.
	return res.ExitCode
}
