"""Exercise the shipped shell installer against local release archives."""
import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class Installer(unittest.TestCase):
    def test_release_validation_preserves_existing_install_on_every_refusal(self):
        for scenario in ('valid', 'checksum', 'symlink', 'duplicate', 'missing'):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as scratch:
                root = Path(scratch)
                for name in ('bin', 'fixture', 'commands'):
                    (root / name).mkdir(mode=0o700)
                old = b'previous executable\n'
                payload = b"#!/bin/sh\nprintf 'installed fixture\\n'\n"
                destination = root / 'bin/stavlos'
                destination.write_bytes(old)
                archive = root / 'fixture/archive'
                with tarfile.open(archive, 'w:gz') as output:
                    for _ in range(2 if scenario == 'duplicate' else 1):
                        header = tarfile.TarInfo('another-file' if scenario == 'missing' else 'stavlos')
                        header.mode = 0o755
                        if scenario == 'symlink':
                            header.type, header.linkname = tarfile.SYMTYPE, '/outside'
                            output.addfile(header)
                        else:
                            header.size = len(payload)
                            output.addfile(header, io.BytesIO(payload))
                digest = '0' * 64 if scenario == 'checksum' else hashlib.sha256(archive.read_bytes()).hexdigest()
                (root / 'fixture/checksums').write_text(digest + '  stavlos_Linux_x86_64.tar.gz\n')
                commands = {
                    'uname': '#!/bin/sh\ncase "$1" in -s) echo Linux;; -m) echo x86_64;; *) exit 1;; esac\n',
                    'curl': '''#!/bin/sh
set -eu
output=
url=
while [ "$#" -gt 0 ]; do
 case "$1" in
  --output) output=$2; shift 2 ;;
  --write-out|--retry|--connect-timeout|--max-time|--proto|--proto-redir) shift 2 ;;
  https://*) url=$1; shift ;;
  *) shift ;;
 esac
done
case "$url" in
 https://github.com/nicodes/stavlos/releases/latest) printf 'https://github.com/nicodes/stavlos/releases/tag/v1.2.3' ;;
 https://github.com/nicodes/stavlos/releases/download/v1.2.3/checksums.txt) cp "$INSTALL_FIXTURE/checksums" "$output" ;;
 https://github.com/nicodes/stavlos/releases/download/v1.2.3/stavlos_Linux_x86_64.tar.gz) cp "$INSTALL_FIXTURE/archive" "$output" ;;
 *) exit 1 ;;
esac
''',
                }
                for name, content in commands.items():
                    path = root / 'commands' / name
                    path.write_text(content)
                    path.chmod(0o700)
                env = dict(os.environ, PATH=str(root / 'commands') + os.pathsep + os.environ['PATH'],
                           INSTALL_FIXTURE=str(root / 'fixture'))
                result = subprocess.run(['sh', str(ROOT / 'install.sh'), '--install-dir', str(root / 'bin')],
                                        env=env, capture_output=True, timeout=10)
                self.assertEqual(result.returncode == 0, scenario == 'valid', result.stderr)
                self.assertEqual(destination.read_bytes(), payload if scenario == 'valid' else old)


if __name__ == '__main__':
    unittest.main()
