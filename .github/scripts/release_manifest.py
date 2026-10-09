"""Record build inputs; this is not a signed provenance attestation."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys


def command(argv, cwd):
    return subprocess.check_output(argv, cwd=cwd, text=True, encoding='utf-8').strip()


def record_checkout(path, requested_ref=None, expected_sha=None):
    path = Path(path)
    sha = command(['git', 'rev-parse', 'HEAD'], path)
    if not re.fullmatch('[0-9a-f]{40}', sha):
        raise ValueError('checkout did not resolve to a full commit SHA')
    if expected_sha and expected_sha != sha:
        raise ValueError('checkout changed after its immutable input was recorded')
    locks = {name: hashlib.sha256((path / name).read_bytes()).hexdigest()
             for name in ['go.mod', 'go.sum', 'package.json', 'pnpm-lock.yaml']
             if (path / name).is_file()}
    dirty = bool(command(['git', 'status', '--porcelain', '--untracked-files=no'], path))
    return {'commit_sha': sha, 'requested_ref': requested_ref, 'tracked_tree_dirty': dirty, 'lock_sha256': locks}


def main():
    root = Path.cwd()
    result = {'schema': 1, 'integrity_model': 'Unsigned build-input record; checksums are not publisher signatures.',
              'source_repository': os.environ.get('GITHUB_REPOSITORY'),
              'source': record_checkout(root, os.environ.get('GITHUB_REF'), os.environ.get('GITHUB_SHA')),
              'toolchain': {}}
    for name, env in [('frontend', 'FRONTEND'), ('agent', 'AGENT')]:
        path = root / ('.frontend' if name == 'frontend' else '.agent')
        if path.exists():
            expected = os.environ.get(env + '_SHA')
            if not expected:
                raise ValueError('missing immutable ' + name + ' SHA')
            result[name] = record_checkout(path, os.environ.get(env + '_REF'), expected)
    tools = []
    if (root / 'go.mod').exists(): tools.append(('go', os.environ.get('GO_BINARY', 'go'), 'version'))
    if (root / 'package.json').exists() or (root / '.frontend').exists():
        tools.extend([('node', os.environ.get('NODE_BINARY', 'node'), '--version'), ('pnpm', os.environ.get('PNPM_BINARY', 'pnpm'), '--version')])
    for name, exe, flag in tools:
        result['toolchain'][name] = command([exe, flag], root)
    out = Path(sys.argv[1])
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(result, indent=2) + '\n', encoding='utf-8')

if __name__ == '__main__':
    main()
