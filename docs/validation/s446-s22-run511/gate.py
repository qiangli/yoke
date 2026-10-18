#!/usr/bin/env python3
"""Independent byte oracle; no model or shell execution. Optional candidate file."""
import hashlib
from pathlib import Path
import sys

root = Path(__file__).resolve().parent
expected = b'assignment-proof\n'
assert (root / 'expected.stdout').read_bytes() == expected
for row in (root / 'frozen.sha256').read_text().splitlines():
    digest, name = row.split('  ', 1)
    assert hashlib.sha256((root / name).read_bytes()).hexdigest() == digest, name

def correct(data):
    return data == expected

assert correct(expected)
for wrong in (b'', b'assignment-proof', b'assignment-proof\r\n', b'echo assignment-proof\n', expected + b'extra\n'):
    assert not correct(wrong)
if len(sys.argv) == 2:
    assert correct(Path(sys.argv[1]).read_bytes()), 'output mismatch'
elif len(sys.argv) != 1:
    raise SystemExit('usage: gate.py [candidate-stdout-file]')
print('PASS: frozen hashes and independent exact-byte oracle; no economic acceptance')
