#!/usr/bin/env python3
"""Build a deterministic Windows x64 RecallDock ZIP release without mutating host state."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import zipfile


def require_published_tag(root, version, commit):
    tag = 'v' + version
    ref = 'refs/tags/' + tag
    try:
        kind = subprocess.check_output(['git', 'cat-file', '-t', ref], cwd=root, text=True, stderr=subprocess.DEVNULL).strip()
        target = subprocess.check_output(['git', 'rev-parse', ref + '^{commit}'], cwd=root, text=True, stderr=subprocess.DEVNULL).strip()
    except subprocess.CalledProcessError as err:
        raise ValueError('local publication requires a matching annotated tag ' + tag) from err
    if kind != 'tag' or target != commit:
        raise ValueError('local publication requires a matching annotated tag ' + tag)
    return tag


def main():
    root = Path(__file__).resolve().parents[1]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--output', required=True, type=Path)
    parser.add_argument('--development', action='store_true')
    parser.add_argument('--publish-local', action='store_true')
    a = parser.parse_args()
    if not a.development and not a.publish_local:
        parser.error('formal packages require --publish-local and a matching annotated tag')
    if a.publish_local and (a.development or not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', a.version)):
        parser.error('local publication requires a final version and clean formal source')
    if a.development and '-' not in a.version:
        parser.error('development packages require a prerelease version')
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:-[a-z0-9.]+)?', a.version):
        parser.error('invalid semantic version')
    if not a.output.is_absolute() or a.output.exists() or a.output.resolve().is_relative_to(root):
        parser.error('new absolute output directory outside the source checkout required')
    commit = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip()
    dirty = bool(subprocess.check_output(['git', 'status', '--porcelain'], cwd=root, text=True).strip())
    if dirty and not a.development:
        parser.error('formal packages require clean committed source')
    source_tag = require_published_tag(root, a.version, commit) if a.publish_local else None
    tag_object = subprocess.check_output(['git', 'rev-parse', 'refs/tags/' + source_tag], cwd=root, text=True).strip() if source_tag else None

    epoch = int(subprocess.check_output(['git', 'show', '-s', '--format=%ct', 'HEAD'], cwd=root, text=True).strip())
    a.output.mkdir(mode=0o700, parents=False, exist_ok=False)
    name = f'recalldock_{a.version}_windows_amd64'
    pkg = a.output / name
    pkg.mkdir()
    (pkg / 'runtime').mkdir()

    env = os.environ.copy()
    env['CGO_ENABLED'] = '0'
    env['GOOS'] = 'windows'
    env['GOARCH'] = 'amd64'
    subprocess.run(
        ['go', 'build', '-trimpath', f'-ldflags=-X main.version={a.version}', '-o', str(pkg / 'runtime/recalldock.exe'), './cmd/recalldock'],
        cwd=root, env=env, check=True,
    )

    market = pkg / 'marketplace'
    shutil.copytree(root / 'integrations/host-plugins', market, symlinks=False)
    for rel in ['plugins/codex/.codex-plugin/plugin.json', 'plugins/claude-code/.claude-plugin/plugin.json', 'plugins/antigravity/plugin.json']:
        p = market / rel
        obj = json.loads(p.read_text(encoding='utf-8'))
        obj['version'] = a.version
        p.write_text(json.dumps(obj, indent=2, ensure_ascii=False) + '\n', encoding='utf-8')
    mp = market / '.claude-plugin/marketplace.json'
    mobj = json.loads(mp.read_text(encoding='utf-8'))
    mobj['plugins'][0]['version'] = a.version
    mp.write_text(json.dumps(mobj, indent=2, ensure_ascii=False) + '\n', encoding='utf-8')

    # Rewrite Windows-native plugin entrypoints so they never invoke Unix sh -c or $HOME/.local/bin.
    for host in ('codex', 'claude-code'):
        (market / f'plugins/{host}/.mcp.json').write_text(json.dumps({
            'mcpServers': {
                'recalldock': {
                    'command': 'recalldock.exe',
                    'args': ['gateway', 'serve', '--host', host],
                }
            }
        }, indent=2) + '\n', encoding='utf-8')

    (market / 'plugins/codex/hooks/hooks.json').write_text(json.dumps({
        'hooks': {
            ev: [{'hooks': [{'type': 'command', 'command': 'recalldock.exe host-event codex', 'timeout': 3 if ev == 'SessionEnd' else 5}]}]
            for ev in ('SessionStart', 'SubagentStart', 'SessionEnd')
        }
    }, indent=2) + '\n', encoding='utf-8')
    (market / 'plugins/claude-code/hooks/hooks.json').write_text(json.dumps({
        'hooks': {
            ev: [{'hooks': [{'type': 'command', 'command': 'recalldock.exe host-event claude-code', 'timeout': 5}]}]
            for ev in ('SessionStart', 'SessionEnd')
        }
    }, indent=2) + '\n', encoding='utf-8')
    (market / 'plugins/antigravity/mcp_config.json').write_text(json.dumps({
        'mcpServers': {
            'recalldock': {
                'command': 'recalldock.exe',
                'args': ['gateway', 'serve', '--host', 'antigravity'],
            }
        }
    }, indent=2) + '\n', encoding='utf-8')
    (market / 'plugins/antigravity/hooks.json').write_text(json.dumps({
        'recalldock-session': {
            'SessionStart': [{'type': 'command', 'command': 'recalldock.exe host-event antigravity --event SessionStart', 'timeout': 5}]
        }
    }, indent=2) + '\n', encoding='utf-8')

    (pkg / 'tools').mkdir()
    shutil.copy2(root / 'tools/install-windows.ps1', pkg / 'tools/install-windows.ps1')
    for fn in ('LICENSE', 'NOTICE.md', 'PROVENANCE.md', 'README.md', 'README.zh-CN.md'):
        src = root / fn
        if src.is_file():
            shutil.copy2(src, pkg / fn)
    (pkg / 'docs').mkdir()
    for fn in ('WINDOWS.md', 'INSTALLATION.md', 'GATEWAY-MCP.md', 'RELEASE.md', 'LOCAL-QUICKSTART.md', 'GATEWAY-AUTH.md', 'HOST-ADAPTERS.md'):
        src = root / 'docs' / fn
        if src.is_file():
            shutil.copy2(src, pkg / 'docs' / fn)

    files = {
        path.relative_to(pkg).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in sorted(pkg.rglob('*')) if path.is_file()
    }
    manifest: dict = dict(
        format=1,
        version=a.version,
        platform='windows',
        architecture='amd64',
        source_commit=commit,
        source_dirty=dirty,
        development=a.development,
        published=a.publish_local,
        files=files,
    )
    if source_tag:
        manifest.update(source_tag=source_tag, source_tag_object=tag_object)
    (pkg / 'release.json').write_text(json.dumps(manifest, sort_keys=True, indent=2) + '\n', encoding='utf-8')

    if not a.development and (subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root, text=True).strip() != commit or subprocess.check_output(['git', 'status', '--porcelain'], cwd=root, text=True).strip()):
        raise ValueError('source changed during release build; preserve outputs and rebuild from a clean commit')
    if source_tag:
        require_published_tag(root, a.version, commit)
        if subprocess.check_output(['git', 'rev-parse', 'refs/tags/' + source_tag], cwd=root, text=True).strip() != tag_object:
            raise ValueError('annotated tag changed during release build')
    archive = a.output / f'{name}.zip'
    import datetime
    dt = datetime.datetime.fromtimestamp(epoch, datetime.timezone.utc).timetuple()[:6]
    with zipfile.ZipFile(archive, 'w', compression=zipfile.ZIP_DEFLATED, compresslevel=6) as zf:
        for path in sorted(pkg.rglob('*')):
            arc = path.relative_to(a.output).as_posix()
            if path.is_symlink():
                raise ValueError('Windows release package must not contain symlinks')
            if path.is_dir():
                zi = zipfile.ZipInfo(arc + '/', date_time=dt)
                zi.external_attr = (0o755 << 16)
                zf.writestr(zi, b'')
            elif path.is_file():
                zi = zipfile.ZipInfo(arc, date_time=dt)
                zi.compress_type = zipfile.ZIP_DEFLATED
                zi.external_attr = ((0o755 if path.suffix == '.exe' else 0o644) << 16)
                zf.writestr(zi, path.read_bytes())

    sha = hashlib.sha256(archive.read_bytes()).hexdigest()
    (a.output / 'checksums.txt').write_text(f'{sha}  {archive.name}\n', encoding='utf-8')
    report: dict = dict(
        format=1,
        version=a.version,
        source_commit=commit,
        source_dirty=dirty,
        development=a.development,
        platform='windows',
        architecture='amd64',
        archive=archive.name,
        sha256=sha,
        published=a.publish_local,
    )
    if source_tag is not None:
        report['source_tag'] = source_tag
        report['source_tag_object'] = tag_object
        report['publication_target'] = 'local-offline-artifact'
    (a.output / 'build.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
