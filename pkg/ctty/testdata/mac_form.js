ObjC.import('Cocoa');
function run(argv) {
 var prompt=argv[0], title=argv[1], name=argv[2], kind=argv[3], choices=JSON.parse(argv[4]);
 var pattern=new RegExp(argv[5]), category=argv[6], peek=argv[7]==='true';
 var app=$.NSApplication.sharedApplication; app.setActivationPolicy($.NSApplicationActivationPolicyAccessory);
 var alert=$.NSAlert.alloc.init; alert.setMessageText(title);
 alert.setInformativeText(prompt+'\nClipboard history apps and Universal Clipboard may keep a copy.');
 for (var i=0;i<choices.length;i++) alert.addButtonWithTitle(choices[i]);
 var clipIndex=choices.indexOf('Use clipboard'), clipButton=clipIndex<0 ? null : alert.buttons.objectAtIndex(clipIndex);
 var box=$.NSView.alloc.initWithFrame($.NSMakeRect(0,0,480,90));
 var label=$.NSTextField.labelWithString(name); label.setFrame($.NSMakeRect(0,67,480,20)); box.addSubview(label);
 var field=(kind==='secret' ? $.NSSecureTextField : $.NSTextField).alloc.initWithFrame($.NSMakeRect(0,40,480,26)); box.addSubview(field);
 var status=$.NSTextField.labelWithString(''); status.setFrame($.NSMakeRect(110,8,370,20)); box.addSubview(status);
 var show=$.NSButton.alloc.initWithFrame($.NSMakeRect(0,8,100,20));
 var plain=null;
 if(kind==='secret') {
  plain=$.NSTextField.alloc.initWithFrame($.NSMakeRect(0,40,480,26)); plain.setHidden(true); box.addSubview(plain);
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
 var timer=peek ? $.NSTimer.scheduledTimerWithTimeIntervalTargetSelectorUserInfoRepeats(0.35,watcher,'refresh:',null,true) : null;
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
}