package ctty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// FormRequest is a single operator decision with an optional value.
type FormRequest struct {
	Title, Prompt, Name, Kind string
	Choices                   []string
	Timeout                   time.Duration
}

type FormAnswer struct {
	Choice string `json:"choice"`
	Value  string `json:"value"`
}

// FormGUI displays a form on the attended desktop. A missing or dismissed
// desktop never counts as an operator choice.
func FormGUI(ctx context.Context, r FormRequest) (FormAnswer, error) {
	if !guiAttended() {
		return FormAnswer{}, ErrNoGUI
	}
	if r.Kind != "text" && r.Kind != "secret" {
		return FormAnswer{}, fmt.Errorf("ctty: invalid field kind %q", r.Kind)
	}
	if len(r.Choices) < 2 {
		return FormAnswer{}, errors.New("ctty: form needs choices")
	}
	if r.Timeout <= 0 {
		r.Timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var cmd *exec.Cmd
	choices, _ := json.Marshal(r.Choices)
	switch runtime.GOOS {
	case "darwin":
		path, err := exec.LookPath("osascript")
		if err != nil {
			return FormAnswer{}, ErrNoGUI
		}
		cmd = exec.CommandContext(ctx, path, "-l", "JavaScript", "-e", macFormSrc, r.Prompt, r.Title, r.Name, r.Kind, string(choices))
	case "windows":
		path, err := exec.LookPath("powershell.exe")
		if err != nil {
			return FormAnswer{}, ErrNoGUI
		}
		cmd = exec.CommandContext(ctx, path, "-NoProfile", "-NonInteractive", "-STA", "-Command", windowsFormSrc, "-Text", r.Prompt, "-Title", r.Title, "-Kind", r.Kind, "-Choices", string(choices))
	default:
		path, err := exec.LookPath("zenity")
		if err != nil {
			return FormAnswer{}, ErrNoGUI
		}
		return zenityForm(ctx, path, r)
	}
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return FormAnswer{}, ErrTimeout
	}
	if err != nil {
		return FormAnswer{}, guiFailure("form", err)
	}
	return parseFormAnswer(out, r.Choices)
}

func parseFormAnswer(out []byte, choices []string) (FormAnswer, error) {
	var a FormAnswer
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &a); err != nil {
		return a, ErrDismissed
	}
	if a.Choice == "" {
		return a, ErrDismissed
	}
	for _, choice := range choices {
		if choice == a.Choice {
			return a, nil
		}
	}
	return FormAnswer{}, ErrDismissed
}

func zenityForm(ctx context.Context, path string, r FormRequest) (FormAnswer, error) {
	args := []string{"--forms", "--add-entry=" + r.Name, "--title=" + r.Title,
		"--text=" + r.Prompt, "--ok-label=" + r.Choices[0],
		"--cancel-label=" + r.Choices[len(r.Choices)-1],
		"--timeout=" + strconv.Itoa(int(r.Timeout.Seconds()))}
	if r.Kind == "secret" {
		args = []string{"--password", "--title=" + r.Title, "--text=" + r.Prompt,
			"--ok-label=" + r.Choices[0], "--cancel-label=" + r.Choices[len(r.Choices)-1],
			"--timeout=" + strconv.Itoa(int(r.Timeout.Seconds()))}
	}
	for _, choice := range r.Choices[1 : len(r.Choices)-1] {
		args = append(args, "--extra-button="+choice)
	}
	out, err := exec.CommandContext(ctx, path, args...).Output()
	if ctx.Err() != nil {
		return FormAnswer{}, ErrTimeout
	}
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			if ee.ExitCode() == 5 {
				return FormAnswer{}, ErrTimeout
			}
			if ee.ExitCode() == 1 {
				return FormAnswer{Choice: r.Choices[len(r.Choices)-1]}, nil
			}
		}
		return FormAnswer{}, guiFailure("zenity", err)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
	for _, choice := range r.Choices[1 : len(r.Choices)-1] {
		if value == choice {
			return FormAnswer{Choice: choice}, nil
		}
	}
	return FormAnswer{Choice: r.Choices[0], Value: value}, nil
}

// JXA receives all untrusted text as argv; it never interpolates it into code.
const macFormSrc = `ObjC.import('Cocoa');
function run(argv) {
 var prompt=argv[0], title=argv[1], name=argv[2], kind=argv[3], choices=JSON.parse(argv[4]);
 var app=$.NSApplication.sharedApplication; app.setActivationPolicy($.NSApplicationActivationPolicyAccessory);
 var alert=$.NSAlert.alloc.init; alert.setMessageText(title); alert.setInformativeText(prompt);
 for (var i=0;i<choices.length;i++) alert.addButtonWithTitle(choices[i]);
 var box=$.NSView.alloc.initWithFrame($.NSMakeRect(0,0,480,65));
 var label=$.NSTextField.labelWithString(name); label.setFrame($.NSMakeRect(0,42,480,20)); box.addSubview(label);
 var field=(kind==='secret' ? $.NSSecureTextField : $.NSTextField).alloc.initWithFrame($.NSMakeRect(0,16,480,26)); box.addSubview(field);
 var show=$.NSButton.alloc.initWithFrame($.NSMakeRect(0,0,100,16));
 if(kind==='secret') {
  var plain=$.NSTextField.alloc.initWithFrame($.NSMakeRect(0,16,480,26)); plain.setHidden(true); box.addSubview(plain);
  ObjC.registerSubclass({name:'BashyReveal', methods:{'toggle:':{types:['void',['id']], implementation:function(sender){
   if (Number(sender.state)===1) { plain.setStringValue(field.stringValue); plain.setHidden(false); field.setHidden(true); plain.becomeFirstResponder(); }
   else { field.setStringValue(plain.stringValue); plain.setHidden(true); field.setHidden(false); field.becomeFirstResponder(); }
  }}}});
  var reveal=$.BashyReveal.alloc.init; show.setButtonType($.NSSwitchButton); show.setTitle('Show'); show.setTarget(reveal); show.setAction('toggle:'); box.addSubview(show);
 }
 alert.setAccessoryView(box); app.activateIgnoringOtherApps(true); alert.window.makeKeyAndOrderFront(null);
 var response=alert.runModal; var index=Number(response)-Number($.NSAlertFirstButtonReturn);
 if(index<0 || index>=choices.length) return JSON.stringify({choice:'',value:''});
 return JSON.stringify({choice:choices[index],value:ObjC.unwrap((kind==='secret' && Number(show.state)===1 ? plain : field).stringValue)});
}`

const windowsFormSrc = `
param([string]$Text,[string]$Title,[string]$Kind,[string]$Choices)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$labels = ConvertFrom-Json -InputObject $Choices
$f = New-Object System.Windows.Forms.Form
$f.Text=$Title; $f.Width=660; $f.Height=310; $f.TopMost=$true; $f.StartPosition='CenterScreen'
$l=New-Object System.Windows.Forms.Label; $l.Text=$Text; $l.SetBounds(15,15,620,140)
$t=New-Object System.Windows.Forms.TextBox; $t.SetBounds(15,160,620,28)
if($Kind -eq 'secret') { $t.UseSystemPasswordChar=$true }
$show=New-Object System.Windows.Forms.CheckBox; $show.Text='Show'; $show.SetBounds(15,195,80,24)
$show.Add_CheckedChanged({ $t.UseSystemPasswordChar = -not $show.Checked })
$f.Controls.AddRange(@($l,$t,$show))
$script:chosen=''
for($i=0;$i -lt $labels.Count;$i++) {
 $b=New-Object System.Windows.Forms.Button; $b.Text=$labels[$i]; $b.SetBounds((15+$i*155),230,150,30)
 $choice=$labels[$i]; $b.Add_Click({ $script:chosen=$this.Text; $f.Close() })
 $f.Controls.Add($b)
}
$f.Add_Shown({ $f.Activate(); $t.Focus() }); [void]$f.ShowDialog()
@{choice=$script:chosen; value=$t.Text} | ConvertTo-Json -Compress
`
