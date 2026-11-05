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
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.ReplaceAll(path, "\\", "/"))), ".exe")
	if name == "bashy.real" {
		return "bashy"
	}
	return name
}
func nativeGoTest(argv []string) (int, bool, error) {
	if len(argv) == 0 {
		return 0, false, errors.New("empty native gate")
	}
	name := programExecutableName(argv[0])
	index := -1
	recognized := name == "go"
	offset := 1
	if name == "bashy" && len(argv) > 1 && argv[1] == "go" {
		recognized = true
		offset = 2
	}
	if recognized {
		cursor := offset
		if cursor < len(argv) && argv[cursor] == "-C" {
			cursor += 2
		} else if cursor < len(argv) && strings.HasPrefix(argv[cursor], "-C=") {
			cursor++
		}
		if cursor < len(argv) && argv[cursor] == "test" {
			index = cursor
		}
		if _, e := nativeGoDirectory(argv); e != nil {
			return 0, true, e
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
			return 0, true, errors.New("unsupported Go shaping gate layout; use go [-C relative-directory] test -json")
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

// nativeGoDirectory accepts only Go's optional single global -C before test.
// The same relative directory is confined independently in each exact clone.
func nativeGoDirectory(argv []string) (string, error) {
	offset := 1
	if programExecutableName(argv[0]) == "bashy" {
		offset = 2
	}
	if offset >= len(argv) {
		return "", errors.New("missing Go test command")
	}
	dir := ""
	if argv[offset] == "-C" {
		if offset+1 >= len(argv) {
			return "", errors.New("Go -C needs a relative directory")
		}
		dir = argv[offset+1]
	} else if strings.HasPrefix(argv[offset], "-C=") {
		dir = strings.TrimPrefix(argv[offset], "-C=")
	} else {
		return "", nil
	}
	normalized := strings.ReplaceAll(dir, "\\", "/")
	if normalized == "" || strings.HasPrefix(normalized, "/") || filepath.IsAbs(dir) || filepath.VolumeName(dir) != "" || strings.ContainsAny(normalized, ":\x00") {
		return "", errors.New("Go -C must be a nonempty relative directory without a volume")
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return "", errors.New("Go -C cannot traverse parent directories")
		}
	}
	return dir, nil
}
func confineProgramGoDirectory(root, relative string) error {
	if relative == "" {
		return nil
	}
	resolvedRoot, e := filepath.EvalSymlinks(root)
	if e != nil {
		return e
	}
	target, e := filepath.EvalSymlinks(filepath.Join(root, relative))
	if e != nil {
		return fmt.Errorf("Go -C directory: %w", e)
	}
	rel, e := filepath.Rel(resolvedRoot, target)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("Go -C directory escapes the exact source clone")
	}
	info, e := os.Stat(target)
	if e != nil {
		return e
	}
	if !info.IsDir() {
		return errors.New("Go -C target is not a directory")
	}
	return nil
}

type programGoEvent struct {
	Action     string
	ImportPath string
	Package    string
	Test       string
	Output     string
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

type programGoBaseline struct {
	Checks        map[string]bool
	EmptyPackages map[string]bool
}

// noTestPackages recognizes Go's package-level no-test-files result. Both the
// output metadata and terminal skip must identify the same package; a named
// skipped test is never an empty package.
func noTestPackages(events []programGoEvent) map[string]bool {
	messages, skipped := map[string]bool{}, map[string]bool{}
	for _, event := range events {
		if event.Action == "skip" && event.Test == "" && event.Package != "" {
			skipped[event.Package] = true
		}
		pkg := ""
		if event.Action == "output" && event.Test == "" {
			pkg = event.Package
		}
		if event.Action == "build-output" {
			pkg = event.ImportPath
		}
		if pkg != "" {
			fields := strings.Fields(event.Output)
			if len(fields) == 5 && fields[0] == "?" && fields[1] == pkg && fields[2] == "[no" && fields[3] == "test" && fields[4] == "files]" {
				messages[pkg] = true
			}
		}
	}
	empty := map[string]bool{}
	for pkg := range messages {
		if skipped[pkg] {
			empty[pkg] = true
		}
	}
	return empty
}

func programBaselineChecks(ctx context.Context, o ProgramOptions, runner ProgramRunner, isolated ...string) (*programGoBaseline, bool, error) {
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
	relative, e := nativeGoDirectory(o.Test)
	if e != nil {
		return nil, true, e
	}
	for _, root := range append([]string{o.RepoDir, baseline}, isolated...) {
		if e = confineProgramGoDirectory(root, relative); e != nil {
			return nil, true, e
		}
	}

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
	expected := &programGoBaseline{Checks: map[string]bool{}}
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
			expected.Checks[event.Package+"\x00"+name] = true
		}
	}
	if len(expected.Checks) == 0 {
		return nil, true, errors.New("native shaping gate selected no baseline checks")
	}
	expected.EmptyPackages = noTestPackages(events)
	for key := range expected.Checks {
		delete(expected.EmptyPackages, strings.SplitN(key, "\x00", 2)[0])
	}
	return expected, true, nil
}
func accountProgramGoTests(output string, expected *programGoBaseline) ([]evidence.CheckResult, error) {
	completed := map[string]bool{}
	failed := ""
	events, e := programGoEvents(output)
	if e != nil {
		return nil, fmt.Errorf("invalid test event stream: %w", e)
	}
	empty := noTestPackages(events)
	for _, event := range events {
		if event.Action == "skip" && event.Test == "" && expected.EmptyPackages[event.Package] && empty[event.Package] {
			continue
		}
		if event.Action == "skip" || event.Action == "fail" {
			failed = "native gate reported " + event.Action + " for " + event.Package + "/" + event.Test
		}
		if event.Action == "pass" && event.Test != "" {
			completed[event.Package+"\x00"+event.Test] = true
		}
	}
	var checks []evidence.CheckResult
	for key := range expected.Checks {
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
