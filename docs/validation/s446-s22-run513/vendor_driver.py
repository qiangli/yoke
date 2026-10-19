import os,json,sys,time,subprocess,hashlib,signal,uuid
from pathlib import Path
sys.path.insert(0,'$HOME/projects/poc/dhnt/agent-bench/scripts')
from arm_home import seed_claude_keychain
ROOT=Path.cwd(); E=ROOT/'docs/validation/s446-s22-run513'; R=Path('/tmp/s446-run513-vendor'); R.mkdir(mode=0o700)
BIN=Path('$HOME/.local/share/claude/versions/2.1.284'); MODEL='claude-opus-5'
start=time.monotonic(); deadline=start+900
profile=R/'guard.sb'; profile.write_text('(version 1)\n(allow default)\n(deny file-write* (subpath "$HOME") (with send-signal SIGKILL))\n(deny process-exec (literal "/usr/bin/security"))\n')
(E/'guard.sb').write_bytes(profile.read_bytes())
baseargs=[str(BIN),'-p','--max-turns','1','--output-format','stream-json','--verbose','--tools','','--model',MODEL,'--effort','low','--setting-sources','','--strict-mcp-config','--mcp-config','{"mcpServers":{}}','--disable-slash-commands','--no-session-persistence','--system-prompt','']
(E/'vendor-binary.json').write_text(json.dumps(dict(path=str(BIN),sha256=hashlib.sha256(BIN.read_bytes()).hexdigest(),argv=baseargs),indent=2)+'\n')
def canary(label):
 target=Path('$HOME')/('.s446-run513-deny-'+uuid.uuid4().hex)
 p=subprocess.run(['/usr/bin/sandbox-exec','-f',str(profile),'/bin/sh','-c','/usr/bin/touch "$1"','canary',str(target)],capture_output=True,timeout=15)
 record=dict(argv=p.args,exit_code=p.returncode,absent=not target.exists(),stderr=p.stderr.decode(errors='replace'))
 (E/('canary-'+label+'.json')).write_text(json.dumps(record,indent=2)+'\n')
 assert p.returncode in (-9,137) and not target.exists(), 'write deny canary failed'
def quota(dest):
 t=time.monotonic(); p=subprocess.run(['$HOME/.local/bin/bashy','model','quota','--json'],capture_output=True,timeout=60)
 dest.with_suffix('.stdout').write_bytes(p.stdout); dest.with_suffix('.stderr').write_bytes(p.stderr)
 dest.with_suffix('.meta.json').write_text(json.dumps(dict(argv=p.args,exit_code=p.returncode,elapsed=time.monotonic()-t))+'\n')
 assert p.returncode==0
 rows=[i for i in json.loads(p.stdout)['items'] if i.get('label')=='claude']
 assert {r['window'] for r in rows} >= {'5h','week'}
 assert all(r.get('status')=='ok' and not r.get('stale') and r.get('used_percent',100)<95 for r in rows)
 return rows
# Independent auth copies, no shared cache/history/settings; helper strips refresh token.
for arm in ('A','B'):
 a=R/arm; (a/'home/.claude').mkdir(parents=True,mode=0o700); (a/'workspace').mkdir(); (a/'tmp').mkdir()
 seed_claude_keychain(str(a/'home/.claude'),['$HOME'])
 (a/'home/.claude/.claude.json').write_text(json.dumps({'hasCompletedOnboarding':True}))
canary('before')
for arm in ('A','B'):
 a=R/arm; out=E/arm; out.mkdir()
 env={'HOME':str(a/'home'),'CLAUDE_CONFIG_DIR':str(a/'home/.claude'),'PATH':'/usr/bin:/bin','TMPDIR':str(a/'tmp'),'LANG':'C.UTF-8','TERM':'dumb','DISABLE_AUTOUPDATER':'1','CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC':'1'}
 for k,sub in [('XDG_CONFIG_HOME','.config'),('XDG_CACHE_HOME','.cache'),('XDG_DATA_HOME','.local/share'),('XDG_STATE_HOME','.local/state')]: env[k]=str(a/'home'/sub)
 prompt=(ROOT/f'docs/validation/s446-s22-run511/prompt-{arm}.txt').read_bytes(); (out/'prompt.txt').write_bytes(prompt)
 before=quota(out/'quota-before'); assert time.monotonic()<deadline
 argv=['/usr/bin/sandbox-exec','-f',str(profile),*baseargs]
 meta=dict(argv=argv,cwd=str(a/'workspace'),env=env,prompt_sha256=hashlib.sha256(prompt).hexdigest(),attempted=True,model_requested=MODEL)
 (out/'launch.json').write_text(json.dumps(meta,indent=2)+'\n')
 t=time.monotonic(); p=subprocess.Popen(argv,cwd=a/'workspace',env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
 timeout=False
 try: stdout,stderr=p.communicate(prompt,timeout=min(120,deadline-time.monotonic()))
 except subprocess.TimeoutExpired:
  timeout=True; os.killpg(p.pid,signal.SIGKILL); stdout,stderr=p.communicate()
 meta.update(exit_code=p.returncode,latency_seconds=time.monotonic()-t,timed_out=timeout)
 (out/'vendor.stdout').write_bytes(stdout); (out/'vendor.stderr').write_bytes(stderr)
 (out/'launch.json').write_text(json.dumps(meta,indent=2)+'\n')
 after=quota(out/'quota-after'); canary('after-'+arm)
 assert not timeout and p.returncode==0, 'vendor invocation failed'
 events=[json.loads(l) for l in stdout.splitlines() if l.strip()]
 results=[e for e in events if e.get('type')=='result']; assert len(results)==1
 result=results[0]; assert not result.get('is_error') and result.get('usage') and result.get('modelUsage'), 'missing terminal usage'
 assert set(result['modelUsage'])=={MODEL}, 'model mismatch'
 models={e['message']['model'] for e in events if e.get('type')=='assistant'}; assert models=={MODEL}, 'assistant model mismatch'
 payload=json.loads(result['result']); assert set(payload)=={'plan','result'} and all(isinstance(x,str) for x in payload.values()), 'task not delivered'
 (out/'response.json').write_text(json.dumps(payload,indent=2)+'\n')
 (out/'result.bin').write_bytes(payload['result'].encode())
 (out/'usage.json').write_text(json.dumps(result,indent=2)+'\n')
 print(arm,'completed',meta['latency_seconds'],flush=True)
 if arm=='A':
  p=subprocess.run([sys.executable,str(ROOT/'docs/validation/s446-s22-run511/gate.py'),str(out/'result.bin')],capture_output=True)
  (out/'oracle.txt').write_bytes(p.stdout+p.stderr)
  assert p.returncode==0, 'A output failed oracle'
