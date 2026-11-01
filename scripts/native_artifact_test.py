"""Reject changed executables and mismatched native build receipts before assembly."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class NativeArtifact(unittest.TestCase):
    def test_source_digest_platform_and_version_are_all_bound(self):
        with tempfile.TemporaryDirectory() as scratch:
            binary = Path(scratch) / 'stavlos'
            binary.write_bytes(b'local native artifact fixture')
            base = [sys.executable, str(ROOT / 'scripts/native-artifact.py')]
            args = [str(binary), 'cli', 'Linux/x86_64', 'v1.2.3']
            subprocess.run(base + ['record'] + args, cwd=ROOT, check=True)
            receipt = binary.with_name(binary.name + '.json')
            original = receipt.read_bytes()
            self.assertEqual(subprocess.run(base + ['verify'] + args, cwd=ROOT).returncode, 0)
            for field, value in [('source', '0' * 40), ('platform', 'Linux/arm64'), ('version', 'v1.2.4'),
                                 ('kind', 'api'), ('sha256', '0' * 64)]:
                with self.subTest(field=field):
                    document = json.loads(original)
                    document[field] = value
                    receipt.write_text(json.dumps(document))
                    result = subprocess.run(base + ['verify'] + args, cwd=ROOT, capture_output=True)
                    self.assertNotEqual(result.returncode, 0)
            receipt.write_bytes(original)
            binary.write_bytes(b'changed executable')
            self.assertNotEqual(subprocess.run(base + ['verify'] + args, cwd=ROOT, capture_output=True).returncode, 0)


if __name__ == '__main__':
    unittest.main()
