#!/usr/bin/env python3
"""Build the SSO-only release from a published, fixed production baseline."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile

BASE = 'db652bc663a459470f322f6bbadc875f65733075'
REVIEWED_MAIN = '21ce076d96326b854e4354b99827a7c8dd92a7d4'
APPLICATION_FILES = [
    'controller/community_sso.go', 'controller/studio_sso.go',
    'middleware/auth_origin.go', 'web/src/routes/community-authorize.tsx',
    'web/src/features/auth/lib/community-sso-search.ts',
]
TEST_FILES = [
    'controller/community_sso_test.go', 'controller/studio_sso_test.go',
    'web/src/features/auth/lib/__tests__/community-sso-origins.test.ts',
]
ROOT = Path(__file__).resolve().parent.parent

def git(*args):
    return subprocess.check_output(['git', '-C', str(ROOT), *args])

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--release', required=True)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    release = git('rev-parse', '--verify', args.release+'^{commit}').decode().strip()
    if release != args.release or git('rev-parse', 'origin/main').decode().strip() != release:
        raise SystemExit('Release must be the full verified origin/main commit.')
    if args.output.exists():
        raise SystemExit('Output already exists; preserved unchanged.')
    for name in APPLICATION_FILES:
        if git('rev-parse', BASE+':'+name) != git('rev-parse', REVIEWED_MAIN+':'+name):
            raise SystemExit('Unaudited baseline mismatch: '+name)
    # Read committed objects only. All dependencies and non-SSO code retain the production baseline.
    source = git('archive', BASE)
    with tarfile.open(fileobj=io.BytesIO(source)) as archive:
        archive.extractall(args.output, filter='data')
    manifest = {'release': release, 'base': BASE, 'files': {}}
    for name in APPLICATION_FILES + TEST_FILES:
        content = git('show', release+':'+name)
        target = args.output/name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(content)
        manifest['files'][name] = hashlib.sha256(content).hexdigest()
    (args.output/'VERSION').write_text('sso-'+release[:12]+'-base-'+BASE[:12], encoding='utf-8')
    (args.output/'sso-release.json').write_text(json.dumps(manifest, indent=2)+'\n', encoding='utf-8')
    print(json.dumps({'release': release, 'base': BASE, 'output': str(args.output)}))

if __name__ == '__main__':
    main()
