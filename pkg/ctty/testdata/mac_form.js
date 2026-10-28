ObjC.import('Cocoa');
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
}