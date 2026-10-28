package ctty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/sensitive"
)

// FormRequest is a single operator decision with an optional value.
type FormRequest struct {
	Title, Prompt, Name, Kind string
	Choices                   []string
	Timeout                   time.Duration
	ClipboardCategory         string
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
	pattern, known := sensitive.ClipboardPattern(r.ClipboardCategory)
	if !known {
		pattern = "(?!)"
	}
	peek := os.Getenv("BASHY_ASK_CLIPBOARD_PEEK") != "0"
	peekFlag := strconv.FormatBool(peek)
	switch runtime.GOOS {
	case "darwin":
		path, err := exec.LookPath("osascript")
		if err != nil {
			return FormAnswer{}, ErrNoGUI
		}
		cmd = exec.CommandContext(ctx, path, "-l", "JavaScript", "-e", macFormSrc, r.Prompt, r.Title, r.Name, r.Kind, string(choices), pattern, r.ClipboardCategory, peekFlag)
	case "windows":
		path, err := exec.LookPath("powershell.exe")
		if err != nil {
			return FormAnswer{}, ErrNoGUI
		}
		cmd = exec.CommandContext(ctx, path, "-NoProfile", "-NonInteractive", "-STA", "-Command", windowsFormSrc, "-Text", r.Prompt, "-Title", r.Title, "-Kind", r.Kind, "-Choices", string(choices), "-Pattern", pattern, "-Category", r.ClipboardCategory, "-Peek", peekFlag)
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
	// Zenity does not expose a button-enabled hook while its dialog is open.
	// Preview once before opening and always revalidate on a clipboard click.
	peek := os.Getenv("BASHY_ASK_CLIPBOARD_PEEK") != "0"
	choices := r.Choices
	clipNote := ""
	if peek {
		var state clipboardPreview
		state.update(0, r.ClipboardCategory, func() (string, error) { return linuxClipboardRead(ctx) })
		if !state.enabled {
			choices = withoutClipboard(choices)
			clipNote = "\nUse clipboard unavailable: " + state.reason
		}
	}
	args := []string{"--forms", "--add-entry=" + r.Name, "--title=" + r.Title,
		"--text=" + r.Prompt + "\nClipboard history apps and Universal Clipboard may keep a copy." + clipNote, "--ok-label=" + choices[0],
		"--cancel-label=" + choices[len(choices)-1],
		"--timeout=" + strconv.Itoa(int(r.Timeout.Seconds()))}
	if r.Kind == "secret" {
		args = []string{"--password", "--title=" + r.Title, "--text=" + r.Prompt + "\nClipboard history apps and Universal Clipboard may keep a copy." + clipNote,
			"--ok-label=" + choices[0], "--cancel-label=" + choices[len(choices)-1],
			"--timeout=" + strconv.Itoa(int(r.Timeout.Seconds()))}
	}
	for _, choice := range choices[1 : len(choices)-1] {
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
				return FormAnswer{Choice: choices[len(choices)-1]}, nil
			}
		}
		return FormAnswer{}, guiFailure("zenity", err)
	}
	value := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
	for _, choice := range choices[1 : len(choices)-1] {
		if value == choice {
			if choice == "Use clipboard" {
				clip, err := linuxClipboardRead(ctx)
				if err != nil {
					return FormAnswer{}, errors.New("ctty: clipboard unavailable")
				}
				if valid, reason := sensitive.ClipboardShape(r.ClipboardCategory, clip); !valid {
					return FormAnswer{}, fmt.Errorf("ctty: %s", reason)
				}
				linuxClipboardClearIfSame(ctx, clip)
				return FormAnswer{Choice: choice, Value: clip}, nil
			}
			return FormAnswer{Choice: choice}, nil
		}
	}
	return FormAnswer{Choice: r.Choices[0], Value: value}, nil
}

func clipboardEligible(category, value string) bool {
	ok, _ := sensitive.ClipboardShape(category, value)
	return ok
}

// clipboardPreview tracks only the pasteboard change counter and eligibility.
// The sampled value is discarded after each shape check.
type clipboardPreview struct {
	count   int64
	seen    bool
	enabled bool
	reason  string
}

func (p *clipboardPreview) update(count int64, category string, read func() (string, error)) bool {
	if p.seen && p.count == count {
		return false
	}
	p.count, p.seen = count, true
	p.enabled, p.reason = false, "clipboard unavailable"
	if value, err := read(); err == nil {
		p.enabled, p.reason = sensitive.ClipboardShape(category, value)
	}
	return true
}

func withoutClipboard(choices []string) []string {
	out := make([]string, 0, len(choices))
	for _, choice := range choices {
		if choice != "Use clipboard" {
			out = append(out, choice)
		}
	}
	return out
}

func linuxClipboardRead(ctx context.Context) (string, error) {
	if path, err := exec.LookPath("wl-paste"); err == nil {
		b, err := exec.CommandContext(ctx, path, "--no-newline").Output()
		return string(b), err
	}
	path, err := exec.LookPath("xclip")
	if err != nil {
		return "", err
	}
	b, err := exec.CommandContext(ctx, path, "-selection", "clipboard", "-out").Output()
	return string(b), err
}

func linuxClipboardClearIfSame(ctx context.Context, value string) {
	clearIfSame(value, func() (string, error) { return linuxClipboardRead(ctx) }, func() {
		if path, err := exec.LookPath("wl-copy"); err == nil {
			_ = exec.CommandContext(ctx, path, "--clear").Run()
			return
		}
		if path, err := exec.LookPath("xclip"); err == nil {
			cmd := exec.CommandContext(ctx, path, "-selection", "clipboard", "-in")
			cmd.Stdin = strings.NewReader("")
			_ = cmd.Run()
		}
	})
}

func clearIfSame(value string, read func() (string, error), clear func()) {
	current, err := read()
	if err == nil && current == value {
		clear()
	}
}

// JXA receives all untrusted text as argv; it never interpolates it into code.
const macFormSrc = `ObjC.import('Cocoa');
function run(argv) {
 var prompt=argv[0], title=argv[1], name=argv[2], kind=argv[3], choices=JSON.parse(argv[4]);
 var pattern=new RegExp(argv[5]), category=argv[6], peek=argv[7]==='true';
 var app=$.NSApplication.sharedApplication; app.setActivationPolicy($.NSApplicationActivationPolicyRegular);
 // osascript has no Edit menu by default. Cocoa text fields dispatch these
 // standard actions through the menu's key equivalents.
 var mainMenu=$.NSMenu.alloc.initWithTitle('Main');
 var editRoot=$.NSMenuItem.alloc.initWithTitleActionKeyEquivalent('Edit',null,'');
 var editMenu=$.NSMenu.alloc.initWithTitle('Edit');
 function editItem(menu,title,action,key,modifiers) {
  var item=$.NSMenuItem.alloc.initWithTitleActionKeyEquivalent(title,action,key);
  if(modifiers) item.setKeyEquivalentModifierMask(modifiers);
  menu.addItem(item); return item;
 }
 editItem(editMenu,'Undo','undo:','z'); editItem(editMenu,'Redo','redo:','Z');
 editMenu.addItem($.NSMenuItem.separatorItem);
 editItem(editMenu,'Cut','cut:','x'); editItem(editMenu,'Copy','copy:','c');
 editItem(editMenu,'Paste','paste:','v'); editItem(editMenu,'Select All','selectAll:','a');
 // Control+V is familiar to operators using the browser's keyboard habits.
 editItem(editMenu,'Paste (Ctrl+V)','paste:','v',$.NSEventModifierFlagControl);
 mainMenu.addItem(editRoot); mainMenu.setSubmenuForItem(editMenu,editRoot); app.setMainMenu(mainMenu);
 var alert=$.NSAlert.alloc.init; alert.setMessageText(title);
 alert.setInformativeText(prompt+'\nClipboard history apps and Universal Clipboard may keep a copy.');
 for (var i=0;i<choices.length;i++) alert.addButtonWithTitle(choices[i]);
 var clipIndex=choices.indexOf('Use clipboard'), clipButton=clipIndex<0 ? null : alert.buttons.objectAtIndex(clipIndex);
 var box=$.NSView.alloc.initWithFrame($.NSMakeRect(0,0,480,90));
 var label=$.NSTextField.labelWithString(name); label.setFrame($.NSMakeRect(0,67,480,20)); box.addSubview(label);
 var field=(kind==='secret' ? $.NSSecureTextField : $.NSTextField).alloc.initWithFrame($.NSMakeRect(0,40,480,26)); box.addSubview(field);
 var contextMenu=$.NSMenu.alloc.initWithTitle('Edit');
 editItem(contextMenu,'Cut','cut:',''); editItem(contextMenu,'Copy','copy:','');
 editItem(contextMenu,'Paste','paste:',''); editItem(contextMenu,'Select All','selectAll:','');
 field.setMenu(contextMenu);
 var status=$.NSTextField.labelWithString(''); status.setFrame($.NSMakeRect(110,8,370,20)); box.addSubview(status);
 var show=$.NSButton.alloc.initWithFrame($.NSMakeRect(0,8,100,20));
 var plain=null;
 if(kind==='secret') {
  plain=$.NSTextField.alloc.initWithFrame($.NSMakeRect(0,40,480,26)); plain.setHidden(true); plain.setMenu(contextMenu); box.addSubview(plain);
  ObjC.registerSubclass({name:'BashyReveal', methods:{'toggle:':{types:['void',['id']], implementation:function(sender){
   if (Number(sender.state)===1) { plain.setStringValue(field.stringValue); plain.setHidden(false); field.setHidden(true); plain.becomeFirstResponder(); }
   else { field.setStringValue(plain.stringValue); plain.setHidden(true); field.setHidden(false); field.becomeFirstResponder(); }
  }}}});
  var reveal=$.BashyReveal.alloc.init; show.setButtonType($.NSSwitchButton); show.setTitle('Show'); show.setTarget(reveal); show.setAction('toggle:'); box.addSubview(show);
 }
 function clipboard() { var v=$.NSPasteboard.generalPasteboard.stringForType($.NSPasteboardTypeString); return v ? ObjC.unwrap(v) : ''; }
 function reason(v) { return !v ? 'clipboard is empty' : (!pattern.test(v) ? "clipboard doesn't look like a "+category : ''); }
 function refresh() {
  if(!clipButton) return;
  if(!peek) { clipButton.setEnabled(true); status.setStringValue('Clipboard checked when clicked'); return; }
  var why='clipboard unavailable';
  try { why=reason(clipboard()); } catch(e) {}
  clipButton.setEnabled(why===''); status.setStringValue(why);
 }
 alert.setAccessoryView(box); refresh();
 var lastCount=Number($.NSPasteboard.generalPasteboard.changeCount);
 ObjC.registerSubclass({name:'BashyClipboardWatcher', methods:{'refresh:':{types:['void',['id']], implementation:function(sender){
  var count=Number($.NSPasteboard.generalPasteboard.changeCount);
  if(count!==lastCount) { lastCount=count; refresh(); }
 }}}});
 var watcher=$.BashyClipboardWatcher.alloc.init;
 // A scheduled timer belongs to the default run-loop mode, while NSAlert
 // runs in the modal mode. Register in common and modal modes explicitly.
 var timer=peek ? $.NSTimer.timerWithTimeIntervalTargetSelectorUserInfoRepeats(0.35,watcher,'refresh:',null,true) : null;
 if(timer) {
  var loop=$.NSRunLoop.mainRunLoop;
  loop.addTimerForMode(timer,$.NSRunLoopCommonModes);
  loop.addTimerForMode(timer,$.NSModalPanelRunLoopMode);
 }
 app.activateIgnoringOtherApps(true); alert.window.makeKeyAndOrderFront(null);
 var result={choice:'',value:''};
 while(true) {
  var response=alert.runModal, index=Number(response)-Number($.NSAlertFirstButtonReturn);
  if(index<0 || index>=choices.length) break;
  if(index===clipIndex) {
   try {
    var value=clipboard(), why=reason(value);
    if(why) { status.setStringValue(why); refresh(); continue; }
    if(clipboard()===value) $.NSPasteboard.generalPasteboard.clearContents;
    result={choice:choices[index],value:value}; break;
   } catch(e) { status.setStringValue('clipboard unavailable'); continue; }
  }
  result.choice=choices[index];
  if(index===0) result.value=ObjC.unwrap((kind==='secret' && Number(show.state)===1 ? plain : field).stringValue);
  break;
 }
 if(timer) timer.invalidate;
 return JSON.stringify(result);
}`

const windowsFormSrc = `
param([string]$Text,[string]$Title,[string]$Kind,[string]$Choices,[string]$Pattern,[string]$Category,[string]$Peek)
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$labels = ConvertFrom-Json -InputObject $Choices
$f = New-Object System.Windows.Forms.Form
$f.Text=$Title; $f.Width=800; $f.Height=350; $f.TopMost=$true; $f.StartPosition='CenterScreen'
$l=New-Object System.Windows.Forms.Label; $l.Text=$Text+[Environment]::NewLine+'Clipboard history apps and Universal Clipboard may keep a copy.'; $l.SetBounds(15,15,760,145)
$t=New-Object System.Windows.Forms.TextBox; $t.SetBounds(15,170,760,28)
if($Kind -eq 'secret') { $t.UseSystemPasswordChar=$true }
$show=New-Object System.Windows.Forms.CheckBox; $show.Text='Show'; $show.SetBounds(15,205,80,24)
$show.Add_CheckedChanged({ $t.UseSystemPasswordChar = -not $show.Checked })
$status=New-Object System.Windows.Forms.Label; $status.SetBounds(105,205,670,24)
$f.Controls.AddRange(@($l,$t,$show,$status))
$script:chosen=''; $script:value=''; $script:clipButton=$null
function Read-Clipboard { try { return [System.Windows.Forms.Clipboard]::GetText() } catch { return '' } }
function Clipboard-Reason([string]$v) {
 if([string]::IsNullOrEmpty($v)) { return 'clipboard is empty' }
 if(-not [regex]::IsMatch($v,$Pattern)) { return "clipboard doesn't look like a $Category" }
 return ''
}
function Refresh-Clipboard {
 if($null -eq $script:clipButton) { return }
 if($Peek -eq 'false') { $script:clipButton.Enabled=$true; $status.Text='Clipboard checked when clicked'; return }
 $why=Clipboard-Reason (Read-Clipboard)
 $script:clipButton.Enabled=($why -eq ''); $status.Text=$why
}
for($i=0;$i -lt $labels.Count;$i++) {
 $b=New-Object System.Windows.Forms.Button; $b.Text=$labels[$i]; $b.SetBounds((15+$i*155),245,150,32)
 if($b.Text -eq 'Use clipboard') { $script:clipButton=$b }
 $b.Add_Click({
  if($this.Text -eq 'Use clipboard') {
   $v=Read-Clipboard; $why=Clipboard-Reason $v
   if($why -ne '') { $status.Text=$why; Refresh-Clipboard; return }
   if((Read-Clipboard) -ceq $v) { [System.Windows.Forms.Clipboard]::Clear() }
   $script:value=$v
  } elseif($this.Text -eq $labels[0]) { $script:value=$t.Text }
  $script:chosen=$this.Text; $f.Close()
 })
 $f.Controls.Add($b)
}
$timer=New-Object System.Windows.Forms.Timer; $timer.Interval=350
$timer.Add_Tick({ Refresh-Clipboard }); if($Peek -ne 'false') { $timer.Start() }
$f.Add_Shown({ Refresh-Clipboard; $f.Activate(); $t.Focus() }); [void]$f.ShowDialog(); $timer.Stop(); $timer.Dispose()
@{choice=$script:chosen; value=$script:value} | ConvertTo-Json -Compress
`
