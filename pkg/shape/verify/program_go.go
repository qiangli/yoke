package verify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/shape/evidence"
)

// nativeGoTest recognizes only direct Go test and the frozen product wrapper.
// Unknown candidate-owned gate executables are not silently treated as trusted.
func programExecutableName(path string) string {
	return strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(path, "\\", "/"))), ".exe")
}
func nativeGoTest(argv []string) (int, bool, error) {
	if len(argv) == 0 {
		return 0, false, errors.New("empty native gate")
	}
	name := programExecutableName(argv[0])
	index := -1
	recognized := name == "go"
	if name == "go" && len(argv) > 1 && argv[1] == "test" {
		index = 1
	}
	if name == "bashy" && len(argv) > 1 && argv[1] == "go" {
		recognized = true
		if len(argv) > 2 && argv[2] == "test" {
			index = 2
		}
	}
	for i, a := range argv {
		if strings.HasSuffix(strings.ReplaceAll(filepath.ToSlash(a), "\\", "/"), "scripts/go-product.sh") {
			recognized = true
			if i+1 < len(argv) && argv[i+1] == "test" {
				index = i + 1
			}
		}
	}
	if index < 0 {
		if recognized {
			return 0, true, errors.New("unsupported Go shaping gate layout; use direct go test -json or the frozen product wrapper")
		}
		return 0, false, nil
	}
	hasJSON := false
	for _, a := range argv[index+1:] {
		if a == "-json" || a == "-json=true" {
			hasJSON = true
		}
		if strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-toolexec") || strings.HasPrefix(a, "-overlay") || strings.HasPrefix(a, "-list") {
			return 0, true, errors.New("native shaping gate cannot replace compiler/test execution or supply its own listing")
		}
	}
	if !hasJSON {
		return 0, true, errors.New("native Go shaping gate requires -json accounting")
	}
	return index, true, nil
}

type programGoEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

func programGoEvents(output string) ([]programGoEvent, error) {
	var events []programGoEvent
	scan := bufio.NewScanner(strings.NewReader(output))
	scan.Buffer(make([]byte, 4096), 4<<20)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event programGoEvent
		if json.Unmarshal([]byte(line), &event) == nil && event.Action != "" {
			events = append(events, event)
		}
	}
	return events, scan.Err()
}
func programBaselineChecks(ctx context.Context, o ProgramOptions, runner ProgramRunner, isolated ...string) (map[string]bool, bool, error) {
	index, native, e := nativeGoTest(o.Test)
	if e != nil {
		return nil, native, e
	}
	if !native {
		exe := o.Test[0]
		if !filepath.IsAbs(exe) {
			return nil, false, errors.New("non-Go shaping gate must be a fixed checker executable outside the candidate")
		}
		resolved, e := filepath.EvalSymlinks(exe)
		if e != nil {
			return nil, false, fmt.Errorf("external checker must exist: %w", e)
		}
		for _, candidateRoot := range append([]string{o.RepoDir}, isolated...) {
			root, e := filepath.EvalSymlinks(candidateRoot)
			if e != nil {
				return nil, false, e
			}
			relative, e := filepath.Rel(root, resolved)
			if e != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
				return nil, false, errors.New("external checker must resolve outside candidate repository")
			}
		}
		return nil, false, nil
	}
	baseline, e := IsolatedClone(o.RepoDir, o.BaseSHA)
	if e != nil {
		return nil, true, e
	}
	defer os.RemoveAll(baseline)
	args := append([]string(nil), o.Test...)
	selection := "."
	for i := index + 1; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-run=") {
			selection = strings.TrimPrefix(args[i], "-run=")
		}
		if args[i] == "-run" && i+1 < len(args) {
			selection = args[i+1]
		}
	}
	selection = strings.Split(selection, "/")[0]
	args = append(args, "-list="+selection)
	var output string
	if e = runner.RunProgram(ctx, ProgramRun{Dir: baseline, Argv: args, Output: &output, Timeout: 30 * time.Minute}); e != nil {
		return nil, true, fmt.Errorf("trusted base test listing failed: %w", e)
	}
	expected := map[string]bool{}
	events, e := programGoEvents(output)
	if e != nil {
		return nil, true, fmt.Errorf("invalid baseline event stream: %w", e)
	}
	for _, event := range events {
		if event.Action != "output" || event.Package == "" {
			continue
		}
		name := strings.TrimSpace(event.Output)
		if strings.ContainsAny(name, " \t\r\n") {
			continue
		}
		if strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Example") || strings.HasPrefix(name, "Fuzz") {
			expected[event.Package+"\x00"+name] = true
		}
	}
	if len(expected) == 0 {
		return nil, true, errors.New("native shaping gate selected no baseline checks")
	}
	return expected, true, nil
}
func accountProgramGoTests(output string, expected map[string]bool) ([]evidence.CheckResult, error) {
	completed := map[string]bool{}
	failed := ""
	events, e := programGoEvents(output)
	if e != nil {
		return nil, fmt.Errorf("invalid test event stream: %w", e)
	}
	for _, event := range events {
		if event.Action == "skip" || event.Action == "fail" {
			failed = "native gate reported " + event.Action + " for " + event.Package + "/" + event.Test
		}
		if event.Action == "pass" && event.Test != "" {
			completed[event.Package+"\x00"+event.Test] = true
		}
	}
	var checks []evidence.CheckResult
	for key := range expected {
		status := evidence.StatusPass
		detail := ""
		if !completed[key] {
			status = evidence.StatusFail
			detail = "baseline check did not complete"
			failed = detail
		}
		checks = append(checks, evidence.CheckResult{Name: strings.ReplaceAll(key, "\x00", "/"), Status: status, Detail: detail})
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].Name < checks[j].Name })
	if failed != "" {
		return checks, errors.New(failed)
	}
	return checks, nil
}
