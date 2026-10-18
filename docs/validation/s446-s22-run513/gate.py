#!/usr/bin/env python3
"""Offline evidence gate; never launches a model or native worker."""
import base64
import hashlib
import json
from pathlib import Path
import subprocess
import sys

root = Path(__file__).resolve().parent
frozen = root.parent / 's446-s22-run511'

def read(path):
    return json.loads((root / path).read_text())

def sha(data):
    return hashlib.sha256(data).hexdigest()

for arm in ('A', 'B'):
    assert (root / arm / 'prompt.txt').read_bytes() == (frozen / f'prompt-{arm}.txt').read_bytes()
    launch = read(f'{arm}/launch.json')
    assert launch['exit_code'] == 0 and not launch['timed_out']
    assert launch['latency_seconds'] < 120
    result = read(f'{arm}/usage.json')
    events = [json.loads(line) for line in (root / arm / 'vendor.stdout').read_text().splitlines()]
    assert [e for e in events if e['type'] == 'result'] == [result]
    assert not result['is_error'] and result['terminal_reason'] == 'completed'
    assert set(result['modelUsage']) == {'claude-opus-5'}
    assert {e['message']['model'] for e in events if e['type'] == 'assistant'} == {'claude-opus-5'}
    assert any(e.get('request_id') for e in events)
    usage = result['usage']
    model = result['modelUsage']['claude-opus-5']
    for a, b in [('input_tokens', 'inputTokens'), ('output_tokens', 'outputTokens'),
                 ('cache_read_input_tokens', 'cacheReadInputTokens'),
                 ('cache_creation_input_tokens', 'cacheCreationInputTokens')]:
        assert usage[a] == model[b]
    assert json.loads(result['result']) == read(f'{arm}/response.json')
    for side in ('before', 'after'):
        assert read(f'{arm}/quota-{side}.meta.json')['exit_code'] == 0
        rows = [r for r in read(f'{arm}/quota-{side}.stdout')['items'] if r.get('label') == 'claude']
        assert {r['window'] for r in rows} >= {'5h', 'week'}
        assert all(r['status'] == 'ok' and not r['stale'] and r['used_percent'] < 95 for r in rows)

assert read('A/launch.json')['argv'] == read('B/launch.json')['argv']
for key in ('HOME', 'CLAUDE_CONFIG_DIR', 'TMPDIR'):
    assert read('A/launch.json')['env'][key] != read('B/launch.json')['env'][key]
for label in ('before', 'after-A', 'after-B'):
    canary = read(f'canary-{label}.json')
    assert canary['exit_code'] in (-9, 137) and canary['absent']

native = root / 'B/native'
capture = read('B/native/capture/capture.json')
run = read('B/native/native-run.json')
body = (native / 'task.json').read_bytes()
task = json.loads(body)
script = base64.b64decode(task['artifact']['script'])
assert task['artifact']['digest'] == capture['artifact_digest'] == 'sha256:' + sha(script)
assert capture['body_sha256'] == sha(body)
assert (native / 'capture/task.json').read_bytes() == body == run['body'].encode() == run['script_assignment']['body'].encode()
assert str(run['id']) == capture['run']['WEAVE_ISSUE']
assert Path(run['workspace']).resolve() == Path(capture['cwd']).resolve()
assert run['exit_code'] == capture['exit_code'] == 0 and run['state'] == 'no-op'
assert not capture['timed_out'] and run['finished_at']
assert sha((native / 'capture/stdout.bin').read_bytes()) == capture['stdout_sha256']
assert (native / 'delta-replay.jsonl').read_bytes() == b''
assert '"type":"lifecycle"' in (native / 'delta.jsonl').read_text()
assert 'sentinel absent' in (native / 'admission.txt').read_text()
for candidate in (root / 'A/result.bin', native / 'capture/stdout.bin'):
    subprocess.run([sys.executable, str(frozen / 'gate.py'), str(candidate)], check=True)
assert read('metrics.json')['A']['premium_tokens_per_success'] == 360
assert read('metrics.json')['B']['premium_tokens_per_success'] == 410
print('PASS: paired model/usage/quota, raw byte oracles, artifact/body/run correlation and durable deltas')
