const test = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const source = fs.readFileSync(__dirname + '/page.js', 'utf8');
function environment() {
  const sockets = [], timers = new Map(); let seq = 0;
  class Socket {
    constructor(url) { this.url=url; this.readyState=0; this.sent=[]; sockets.push(this); }
    send(message) { this.sent.push(JSON.parse(message)); }
    open() { this.readyState=1; this.onopen(); }
    message(command) { this.onmessage({data:JSON.stringify({command})}); }
    close() { if(this.readyState===3)return; this.readyState=3; this.onclose?.(); }
  }
  const scope={WebSocket:Socket,URL,TextEncoder,location:{host:'example.test',protocol:'https:',href:'https://example.test/proxy/app/'},document:{baseURI:'https://example.test/proxy/app/'},setTimeout:fn=>{timers.set(++seq,fn);return seq},clearTimeout:id=>timers.delete(id),setInterval:fn=>{timers.set(++seq,fn);return seq},clearInterval:id=>timers.delete(id)};
  vm.createContext(scope);vm.runInContext(source,scope);
  return {api:scope.BashyPage,sockets,timers};
}
const settle=()=>new Promise(resolve=>setImmediate(resolve));
test('shared handlers await results, reject input and unregister',async()=>{
  const {api}=environment();let username='';const submit=async()=>{if(!username)throw Error('Username required');await Promise.resolve();return {username}};
  const remove=api.register('form.submit',{description:'Submit form',run:submit});
  await assert.rejects(api.invoke('form.submit'),/Username required/);
  username='john';assert.deepEqual(await api.invoke('form.submit'),await submit());
  assert.throws(()=>api.register('form.submit',{run:()=>null}),/already registered/);
  remove();await assert.rejects(api.invoke('form.submit'),/not mounted/);
});
test('mount-relative URL, metadata and command completion use one handler',async()=>{
  const {api,sockets,timers}=environment();let calls=0;
  const off=api.register('page.search',{description:'Search',run:async input=>{calls++;return {query:input}}});
  const stop=api.connect({url:'api/page/events',label:'Apps'});const ws=sockets[0];
  assert.equal(ws.url,'wss://example.test/proxy/app/api/page/events');ws.open();
  assert.deepEqual(ws.sent[0],{actions:[{name:'page.search',description:'Search'}],label:'Apps'});
  const command={id:'one',action:'page.search',input:'john'};ws.message(command);ws.message(command);await settle();
  assert.equal(calls,1);assert.deepEqual(ws.sent[1],{id:'one',result:{query:'john'}});
  off();assert.deepEqual(ws.sent.at(-1).actions,[]);stop();assert.equal(timers.size,0);
});
test('disconnect prevents queued work, late results and replay',async()=>{
  const {api,sockets,timers}=environment();let calls=0,finish;
  api.register('page.search',{run:()=>{calls++;return new Promise(resolve=>finish=resolve)}});
  api.connect({url:'api/page/events'});const ws=sockets[0];ws.open();
  ws.message({id:'queued',action:'page.search'});api.disconnect();await settle();assert.equal(calls,0);
  api.connect({url:'api/page/events'});const second=sockets[1];second.open();second.message({id:'running',action:'page.search'});await settle();
  assert.equal(calls,1);second.close();finish('done');await settle();assert.equal(second.sent.length,1);
  // Reconnect advertises handlers; it never resends an action or its old result.
  const retry=[...timers.values()][0];retry();const third=sockets[2];third.open();
  assert.equal(calls,1);assert.equal(third.sent.length,1);
  const scheduled=timers.size;second.onclose();assert.equal(timers.size,scheduled,'stale socket changed reconnect timers');api.disconnect();
});
test('identity replacement isolates old socket events and callbacks',async()=>{
  const {api,sockets}=environment();let calls=0;
  api.register('page.search',{run:()=>calls++});const oldStop=api.connect({url:'api/page/events'});const old=sockets[0];old.open();
  api.connect({url:'api/page/events'});const current=sockets[1];current.open();oldStop();
  old.message({id:'stale',action:'page.search'});current.message({id:'new',action:'page.search'});await settle();
  assert.equal(calls,1);assert.equal(current.readyState,1);api.disconnect();
});
test('rejects foreign endpoints and safely reports handler and serialization errors',async()=>{
  const {api,sockets}=environment();assert.throws(()=>api.connect({url:'https://foreign.test/events'}),/same-origin/);
  api.register('page.fail',{run:()=>{throw Error('validation failed')}});
  api.register('page.cycle',{run:()=>{const x={};x.x=x;return x}});
  api.connect({url:'api/page/events'});const ws=sockets[0];ws.open();
  ws.message({id:'bad',action:'page.fail'});ws.message({id:'cycle',action:'page.cycle'});ws.message({id:'missing',action:'unknown'});await settle();
  const errors=Object.fromEntries(ws.sent.filter(x=>x.id).map(x=>[x.id,x.error]));
  assert.equal(errors.bad,'validation failed');assert.match(errors.cycle,/not JSON serializable/);assert.match(errors.missing,/not mounted/);api.disconnect();
});
