"""Check a packaged CLI's Rust API with an isolated authenticated browser session."""
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
binary = str(Path(sys.argv[1]).resolve())
with tempfile.TemporaryDirectory(prefix='stv-smoke-') as scratch:
    root = Path(scratch)
    for name in ('config', 'data', 'cache', 'tmp'):
        (root / name).mkdir(mode=0o700)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    (root / 'config/stavlos.json').write_text(json.dumps({'web': {'port': port}, 'reminders': False}))
    (root / 'cache/models.json').write_bytes((ROOT / 'internal/modelsdev/fallback.json').read_bytes())
    env = {
        'PATH': '/usr/bin:/bin', 'HOME': scratch, 'TMPDIR': str(root / 'tmp'),
        'STAVLOS_CONFIG_DIR': str(root / 'config'), 'STAVLOS_DATA_DIR': str(root / 'data'),
        'STAVLOS_CACHE_DIR': str(root / 'cache'), 'STAVLOS_SOCKET': str(root / 'socket'),
        'LANG': 'C.UTF-8',
    }
    process = subprocess.Popen([binary, 'daemon'], env=env, stdout=subprocess.DEVNULL,
                               stderr=subprocess.PIPE, start_new_session=True)
    try:
        deadline = time.monotonic() + 15
        while not (root / 'socket').exists():
            if process.poll() is not None:
                raise AssertionError(process.stderr.read().decode())
            if time.monotonic() > deadline:
                raise AssertionError('packaged daemon did not become ready')
            time.sleep(0.03)
        opened = subprocess.check_output([binary, 'web', 'on', '--print'], env=env, text=True, timeout=10).strip()
        parsed = urllib.parse.urlsplit(opened)
        code = urllib.parse.parse_qs(parsed.fragment)['code'][0]
        base = f'http://127.0.0.1:{port}'
        request = urllib.request.Request(base + '/api/session',
                                         data=json.dumps({'code': code}).encode(),
                                         headers={'Origin': base, 'Content-Type': 'application/json'})
        with urllib.request.urlopen(request, timeout=5) as response:
            cookie = response.headers['Set-Cookie'].split(';')[0]
            page_key = json.load(response)['key']
        request = urllib.request.Request(base + '/api/session', headers={'Cookie': cookie, 'X-Stavlos-Key': page_key})
        with urllib.request.urlopen(request, timeout=5) as response:
            assert response.status == 204
        with urllib.request.urlopen(base + '/', timeout=5) as response:
            assert response.status == 200 and b'<!doctype html' in response.read().lower()
        assert not list((root / 'tmp').glob('stavlos-api-*')), 'launcher left an extracted API executable'
        request = urllib.request.Request(base + '/api/session', method='DELETE', headers={'Origin': base, 'Cookie': cookie})
        with urllib.request.urlopen(request, timeout=5) as response:
            assert response.status == 204
        subprocess.run([binary, 'web', 'off'], env=env, stdout=subprocess.DEVNULL, check=True, timeout=10)
        with socket.socket() as probe:
            assert probe.connect_ex(('127.0.0.1', port)) != 0
    finally:
        process.send_signal(signal.SIGTERM)
        try:
            code = process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            raise
    assert code == 0, process.stderr.read().decode()
