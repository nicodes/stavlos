"""Bind native executables to the checkout, platform, version and exact bytes."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['record', 'verify'])
    parser.add_argument('path', type=Path)
    parser.add_argument('kind', choices=['cli', 'api'])
    parser.add_argument('platform', choices=['Linux/x86_64', 'Linux/arm64', 'Darwin/x86_64', 'Darwin/arm64'])
    parser.add_argument('version')
    args = parser.parse_args()
    if args.version != 'devel' and not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', args.version):
        raise SystemExit('invalid native artifact version')
    if args.path.is_symlink() or not args.path.is_file():
        raise SystemExit('native artifact must be a regular file')
    raw = args.path.read_bytes()
    if not raw:
        raise SystemExit('native artifact is empty')
    source = subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip()
    expected = dict(schema=1, source=source, sha256=hashlib.sha256(raw).hexdigest(),
                    kind=args.kind, platform=args.platform, version=args.version)
    receipt = args.path.with_name(args.path.name + '.json')
    if args.action == 'record':
        receipt.write_text(json.dumps(expected, sort_keys=True) + '\n')
    elif receipt.is_symlink() or json.loads(receipt.read_text()) != expected:
        raise SystemExit('native artifact source, version, platform or digest mismatch')


if __name__ == '__main__':
    main()
