"""Exercise fail-closed paths in the actual shell gate without network scans."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class GateFailures(unittest.TestCase):
    def run_gate(self, group='lint', **overrides):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / 'scripts').mkdir()
            (root / 'bin').mkdir()
            shutil.copy(ROOT / 'scripts/check.sh', root / 'scripts/check.sh')
            shutil.copy(ROOT / 'scripts/gocyclo-baseline.txt', root / 'scripts/gocyclo-baseline.txt')
            for name in ('dirname', 'mktemp', 'rm', 'cat', 'awk', 'sort', 'head', 'tail', 'mkdir'):
                (root / 'bin' / name).symlink_to(shutil.which(name))
            go = root / 'bin/go'
            go.write_text('''#!/usr/bin/python3
import os, sys
from pathlib import Path
args = sys.argv[1:]
if args[:2] == ['tool', 'govulncheck']:
    sys.exit(int(os.environ.get('SCAN_EXIT', '0')))
if args[:2] == ['tool', 'deadcode']:
    sys.exit(int(os.environ.get('DEADCODE_EXIT', '0')))
if args[:2] == ['tool', 'gocyclo']:
    if os.environ.get('CYCLO_ERROR'):
        print('scanner unavailable', file=sys.stderr)
        sys.exit(1)
    if args[3] == '25':
        print(Path('scripts/gocyclo-baseline.txt').read_text(), end='')
        sys.exit(1)  # Normal gocyclo threshold result, allowed by the ratchet.
sys.exit(0)
''')
            go.chmod(0o755)
            gofmt = root / 'bin/gofmt'
            gofmt.write_text('#!/bin/bash\nexit "${FMT_EXIT:-0}"\n')
            gofmt.chmod(0o755)
            env = dict(os.environ, PATH=str(root / 'bin'), **overrides)
            return subprocess.run(['/bin/bash', str(root / 'scripts/check.sh'), group],
                                  env=env, text=True, capture_output=True)

    def test_successful_scanner_and_allowed_complexity(self):
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_findings_fail(self):
        self.assertNotEqual(self.run_gate(SCAN_EXIT='3').returncode, 0)

    def test_scanner_error_fails(self):
        self.assertNotEqual(self.run_gate(SCAN_EXIT='1').returncode, 0)

    def test_deadcode_execution_error_fails(self):
        self.assertNotEqual(self.run_gate(DEADCODE_EXIT='1').returncode, 0)

    def test_complexity_execution_error_fails(self):
        self.assertNotEqual(self.run_gate(CYCLO_ERROR='1').returncode, 0)

    def test_formatter_execution_error_fails(self):
        self.assertNotEqual(self.run_gate(FMT_EXIT='1').returncode, 0)

    def test_missing_npm_fails_build(self):
        result = self.run_gate('build')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('npm-missing', result.stdout)


if __name__ == '__main__':
    unittest.main()
