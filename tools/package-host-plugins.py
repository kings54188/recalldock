#!/usr/bin/env python3
"""Build a new local Linux release without changing installed host state."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--version', required=True, help='new immutable version, e.g. 0.1.1')
parser.add_argument('--output', required=True, type=Path, help='new, non-existing release directory')
parser.add_argument('--require-clean', action='store_true', help='refuse formal packages without a clean committed checkout')
args = parser.parse_args()
if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9.]+)?', args.version):
    parser.error('version must be a simple semantic version')
out = args.output.absolute()
commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=root, text=True).strip())
if args.require_clean and dirty:
    raise SystemExit('formal packages require a clean committed checkout')
out.mkdir(mode=0o700, parents=False, exist_ok=False)
market = out / 'marketplace'
shutil.copytree(root / 'integrations/host-plugins', market, symlinks=False)
for rel in ['plugins/codex/.codex-plugin/plugin.json', 'plugins/claude-code/.claude-plugin/plugin.json', 'plugins/antigravity/plugin.json']:
    path = market / rel
    obj = json.loads(path.read_text())
    obj['version'] = args.version
    path.write_text(json.dumps(obj, indent=2, ensure_ascii=False) + '\n')
path = market / '.claude-plugin/marketplace.json'
obj = json.loads(path.read_text())
obj['plugins'][0]['version'] = args.version
path.write_text(json.dumps(obj, indent=2, ensure_ascii=False) + '\n')
(out / 'runtime').mkdir()
env = os.environ.copy()
env['CGO_ENABLED'] = '0'
if subprocess.check_output(['go', 'env', 'GOOS'], cwd=root, text=True).strip() != 'linux':
    raise SystemExit('automatic project provisioning is currently supported only on Linux')
if subprocess.check_output(['go', 'env', 'GOARCH'], cwd=root, text=True).strip() != subprocess.check_output(['go', 'env', 'GOHOSTARCH'], cwd=root, text=True).strip():
    raise SystemExit('complete packages require native-architecture runtime validation')
subprocess.run(['go', 'build', '-trimpath', '-ldflags=-X main.version=' + args.version,
                '-o', str(out / 'runtime/recalldock'), './cmd/recalldock'], cwd=root, env=env, check=True)
# A package must launch the small gateway, preserve hooks, and never initialize
# long-term knowledge just to expose its fixed tool list.
for host in ('codex', 'claude-code'):
    plugin = market / 'plugins' / host
    servers = json.loads((plugin / '.mcp.json').read_text())['mcpServers']
    if len(servers) != 1 or 'gateway serve --host ' + host not in next(iter(servers.values()))['args'][-1]:
        raise SystemExit('host package must expose the selected-host gateway only')
    if not (plugin / 'hooks/hooks.json').is_file():
        raise SystemExit('host lifecycle hooks are missing')
antigravity_plugin = market / 'plugins/antigravity'
ag_servers = json.loads((antigravity_plugin / 'mcp_config.json').read_text())['mcpServers']
if len(ag_servers) != 1 or next(iter(ag_servers.values())) != {'command': 'recalldock', 'args': ['gateway', 'serve', '--host', 'antigravity']}:
    raise SystemExit('antigravity host package must expose the selected-host gateway only')
if not (antigravity_plugin / 'hooks.json').is_file():
    raise SystemExit('antigravity host lifecycle hooks are missing')
with tempfile.TemporaryDirectory(prefix='recalldock-package-check-') as tmp:
    check_env = {k: v for k, v in env.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
    check_env.update(HOME=tmp, RECALLDOCK_DATA_DIR=str(Path(tmp) / 'data'))
    requests = [dict(jsonrpc='2.0', id=1, method='initialize', params=dict(protocolVersion='2024-11-05', capabilities={}, clientInfo=dict(name='package-check', version='1'))),
                dict(jsonrpc='2.0', id=2, method='tools/list', params={})]
    result = subprocess.run([str(out / 'runtime/recalldock'), 'gateway', 'serve'],
                            cwd=tmp, env=check_env, input=''.join(json.dumps(r) + '\n' for r in requests),
                            capture_output=True, text=True, timeout=15, check=True)
    responses = {r['id']: r for r in map(json.loads, result.stdout.splitlines()) if 'id' in r}
    names = {t['name'] for t in responses[2]['result']['tools']}
    if names != {'tools_find', 'tools_describe', 'tools_call', 'tools_release', 'work_find', 'work_read', 'work_checkpoint'}:
        raise SystemExit('packaged gateway tool surface differs from the fixed contract')
    if (Path(tmp) / 'data/recalldock.db').exists():
        raise SystemExit('packaged gateway unexpectedly initialized long-term knowledge')
(out / 'tools').mkdir()
for name in ['install-local.py', 'gateway-migrate.py']:
    shutil.copy2(root / 'tools' / name, out / 'tools' / name)
for name in ['LICENSE', 'NOTICE.md', 'PROVENANCE.md', 'README.md', 'README.zh-CN.md']:
    if (root / name).is_file():
        shutil.copy2(root / name, out / name)
(out / 'docs').mkdir()
for name in ['WINDOWS.md', 'INSTALLATION.md', 'GATEWAY-MCP.md', 'RELEASE.md', 'LOCAL-QUICKSTART.md', 'GATEWAY-AUTH.md', 'HOST-ADAPTERS.md']:
    if (root / 'docs' / name).is_file():
        shutil.copy2(root / 'docs' / name, out / 'docs' / name)
files = {str(path.relative_to(out)): hashlib.sha256(path.read_bytes()).hexdigest()
         for path in sorted(out.rglob('*')) if path.is_file()}
if args.require_clean and (subprocess.check_output(['git','rev-parse','HEAD'],cwd=root,text=True).strip()!=commit or subprocess.check_output(['git','status','--porcelain'],cwd=root,text=True).strip()):
    raise SystemExit('source changed during packaging; inspect candidate and rebuild from a clean commit')
manifest = dict(format=1, version=args.version, platform='linux',
                architecture=subprocess.check_output(['go', 'env', 'GOARCH'], cwd=root, text=True).strip(),
                source_commit=commit, source_dirty=dirty, files=files)
(out / 'release.json').write_text(json.dumps(manifest, sort_keys=True, indent=2) + '\n')
print(out)
