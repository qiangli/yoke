ObjC.import('Cocoa');
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
}