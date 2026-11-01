"""Exercise the packaged Ratatui terminal against an isolated native owner, without model calls."""
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import signal
import sqlite3
import struct
import subprocess
import sys
import tempfile
import termios
import time

ROOT = Path(__file__).resolve().parents[1]
binary = str(Path(sys.argv[1]).resolve())

with tempfile.TemporaryDirectory(prefix='stavlos-terminal-') as scratch:
    root = Path(scratch)
    for name in ('config', 'data', 'cache', 'project'):
        (root / name).mkdir(mode=0o700)
    (root / 'config/stavlos.json').write_text(json.dumps({'reminders': False}))
    (root / 'cache/modelsdev.json').write_bytes((ROOT / 'src/modelsdev-fallback.json').read_bytes())
    env = dict(PATH='/usr/bin:/bin', HOME=scratch, LANG='C.UTF-8', TERM='xterm-256color',
               STAVLOS_CONFIG_DIR=str(root / 'config'), STAVLOS_DATA_DIR=str(root / 'data'),
               STAVLOS_CACHE_DIR=str(root / 'cache'), STAVLOS_SOCKET=str(root / 'owner.sock'))
    owner = subprocess.Popen([binary, 'daemon'], env=env, stdout=subprocess.DEVNULL,
                             stderr=subprocess.PIPE, start_new_session=True)
    master = slave = None
    client = None
    try:
        deadline = time.monotonic() + 15
        while not (root / 'owner.sock').exists():
            if owner.poll() is not None:
                raise AssertionError(owner.stderr.read().decode())
            if time.monotonic() > deadline:
                raise AssertionError('native owner did not become ready')
            time.sleep(0.03)
        channel = subprocess.check_output([binary, 'new', '--no-tui', '--dir', str(root / 'project')],
                                          env=env, text=True, timeout=10).strip()
        master, slave = pty.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 36, 120, 0, 0))
        original = termios.tcgetattr(slave)

        def controlling_terminal():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)

        client = subprocess.Popen([binary, 'open', channel], env=env, stdin=slave, stdout=slave,
                                  stderr=slave, preexec_fn=controlling_terminal)

        def wait_text(expected, seconds=8, initial=b''):
            collected = bytearray(initial)
            deadline = time.monotonic() + seconds
            while expected not in collected:
                if client.poll() is not None:
                    raise AssertionError(f'terminal exited before {expected!r}: {collected!r}')
                if time.monotonic() >= deadline:
                    raise AssertionError(f'terminal did not draw {expected!r}: {collected!r}')
                if select.select([master], [], [], 0.1)[0]:
                    collected.extend(os.read(master, 65536))
            return collected

        wait_text(b'channel chat')
        for command, expected in [(b'/help\r', b'Tab focus'), (b'/providers\r', b'Providers'),
                                  (b'/settings\r', b'project settings'), (b'/tokens system\r', b'tokens over time')]:
            os.write(master, command)
            drawn = wait_text(expected)
            if command.startswith(b'/settings'):
                # The title is drawn while config.list/read are still in flight.
                # Its normal controls replace Working only after the document loads.
                wait_text(b'Ctrl+L', initial=drawn)
            if command.startswith(b'/help'):
                continue
            os.write(master, b'\x1b')
            # Leave the escape prefix isolated from the next command's keys.
            time.sleep(0.2)
        os.write(master, b'\x03')
        wait_text(b'press ctrl+c again')
        assert client.poll() is None, 'the first Ctrl+C must not quit'
        os.write(master, b'\x03')
        assert client.wait(timeout=8) == 0
        restored = termios.tcgetattr(slave)
        mask = termios.ECHO | termios.ICANON
        assert restored[3] & mask == original[3] & mask, 'terminal input modes were not restored'
        with sqlite3.connect(root / 'data/events.db') as database:
            columns = [row[1] for row in database.execute('pragma table_info(events)')]
            kind = 'type' if 'type' in columns else 'kind'
            assert database.execute(f'select count(*) from events where {kind}=?',
                                    ('model.call_started',)).fetchone()[0] == 0
    finally:
        if client is not None and client.poll() is None:
            client.kill()
            client.wait(timeout=5)
        for descriptor in (master, slave):
            if descriptor is not None:
                os.close(descriptor)
        owner.send_signal(signal.SIGTERM)
        try:
            status = owner.wait(timeout=10)
        except subprocess.TimeoutExpired:
            os.killpg(owner.pid, signal.SIGKILL)
            owner.wait()
            raise
    assert status == 0, owner.stderr.read().decode()
print('Native Ratatui command views, two-step quit, terminal restoration and zero model calls verified.')
