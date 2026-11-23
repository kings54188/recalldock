#!/usr/bin/env python3
"""Offline reviewed local install: preview, digest-CAS apply, protected rollback.

Requires Python 3.11+. Does not download, start hosts, change knowledge databases,
grant backend tools, or disable unrelated MCP entries. Host hooks remain subject
to the host's normal plugin installation/trust workflow.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import stat
import sys
import time
import tomllib

# Keep versioned release directories immutable when loading the bundled helper.
sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location('migration', Path(__file__).with_name('gateway-migrate.py'))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


def verify_release(release):
    if not release.is_absolute() or release.is_symlink():
        raise ValueError('release must be an absolute, real, immutable directory')
    release = release.resolve(strict=True)
    raw = (release / 'release.json').read_bytes()
    manifest = json.loads(raw)
    if manifest.get('format') != 1 or not re.fullmatch(r'\d+\.\d+\.\d+(?:-[a-z0-9.]+)?', manifest.get('version', '')):
        raise ValueError('unsupported release manifest')
    required = {'runtime/recalldock', 'tools/install-local.py', 'tools/gateway-migrate.py'}
    if not required.issubset(manifest['files']):
        raise ValueError('incomplete release')
    actual = {str(p.relative_to(release)) for p in release.rglob('*') if p.is_file()}
    if actual != set(manifest['files']) | {'release.json'}:
        raise ValueError('release contains unlisted or missing files')
    for name, expected in manifest['files'].items():
        rel = Path(name)
        path = release / rel
        if rel.is_absolute() or '..' in rel.parts or not path.is_file() or path.resolve() != path or path.is_symlink():
            raise ValueError('unsafe release member')
        if m.digest(path.read_bytes()) != expected:
            raise ValueError('release integrity mismatch: ' + name)
    binary = release / 'runtime/recalldock'
    if not os.access(binary, os.X_OK):
        raise ValueError('release runtime is not executable')
    return release, m.digest(raw), manifest


def prepare(release, home, hosts, data_dir=None):
    release, release_hash, manifest = verify_release(release)
    if not home.is_absolute() or not home.is_dir() or home.resolve() != home:
        raise ValueError('home must be an existing absolute real directory')
    data_dir=data_dir if data_dir is not None else home/'.recalldock'
    if not data_dir.is_absolute() or data_dir.resolve()!=data_dir or (data_dir.exists() and not data_dir.is_dir()):
        raise ValueError('data directory must be an absolute canonical directory (or a new path)')
    binary = str(release / 'runtime/recalldock')
    files = []
    policy_notes=[]
    host_notes=[]

    def safe_parent(path,boundary):
        if not path.is_relative_to(boundary):
            raise ValueError('configuration path escapes its selected root')
        # No parent symlink may redirect the selected home scope elsewhere.
        for parent in path.parents:
            if parent.is_symlink():
                raise ValueError('symlink configuration parent is not supported')
            if parent == boundary:
                break

    def add(path, kind, boundary=home, expected_state=None, **changes):
        safe_parent(path,boundary)
        before, body = m.state(path)
        if expected_state is not None and not m.same_state(before, expected_state):
            raise ValueError('configuration changed while preparing installation')
        if before['kind'] == 'symlink':
            raise ValueError('preserve existing symlink; select an explicit migration instead')
        entry = dict(path=str(path), format=kind, expected_sha256=before['sha256'], **changes)
        after = m.render(entry, body)
        after_mode = entry.get('mode', before['mode'] if before['kind'] == 'file' else 0o600)
        entry['expected_mode'] = before['mode']
        if before['sha256'] != m.digest(after) or before['mode'] != after_mode:
            files.append(entry)

    launcher = home / '.local/bin/recalldock'
    add(launcher, 'text', text='#!/bin/sh\nexec ' + shlex.quote(binary) + ' "$@"\n', mode=0o755)
    for host in hosts:
        server = dict(command=binary, args=['gateway', 'serve', '--host', host])
        if host == 'codex':
            path = home / '.codex/config.toml'
            before, body = m.state(path)
            configured = tomllib.loads(body.decode()) if body is not None else {}
            servers = configured.get('mcp_servers', {})
            if not isinstance(servers, dict):
                raise ValueError('Codex MCP servers must be an object')
            existing = servers.get('recalldock_gateway')
            if 'recalldock_gateway' in servers and not isinstance(existing, dict):
                raise ValueError('existing Codex gateway must be an object')
            changes = [dict(keys=['mcp_servers', 'recalldock_gateway', 'command'], value=binary)] if existing is not None else [dict(keys=['mcp_servers', 'recalldock_gateway', key], value=value) for key, value in server.items()] + [dict(keys=['mcp_servers', 'recalldock_gateway', 'enabled'], value=True)]
            if existing is None or (existing.get('enabled', True) and not any(key in existing for key in ('default_tools_approval_mode', 'tools', 'enabled_tools', 'disabled_tools'))):
                for tool in ('tools_find', 'tools_describe', 'tools_call', 'tools_release', 'work_find', 'work_read', 'work_checkpoint'):
                    changes.append(dict(keys=['mcp_servers', 'recalldock_gateway', 'tools', tool, 'approval_mode'], value='approve'))
            add(path, 'toml', expected_state=before, changes=changes)
        elif host == 'claude-code':
            path = home / '.recalldock/claude-gateway.json'
            before, body = m.state(path)
            configured = json.loads(body) if body is not None else {}
            if not isinstance(configured, dict):
                raise ValueError('Claude MCP configuration must be an object')
            servers = configured.get('mcpServers', {})
            if not isinstance(servers, dict):
                raise ValueError('Claude MCP servers must be an object')
            existing = servers.get('recalldock_gateway')
            if 'recalldock_gateway' in servers and not isinstance(existing, dict):
                raise ValueError('existing Claude gateway must be an object')
            changes = [dict(keys=['mcpServers', 'recalldock_gateway', 'command'], value=binary)] if existing is not None else [dict(keys=['mcpServers', 'recalldock_gateway'], value=server)]
            add(path, 'json', expected_state=before, changes=changes)
            wrapper = '#!/bin/sh\nexec claude --mcp-config ' + shlex.quote(str(home / '.recalldock/claude-gateway.json')) + ' --strict-mcp-config "$@"\n'
            add(home / '.local/bin/recalldock-claude', 'text', text=wrapper, mode=0o755)
        elif host == 'dsh':
            # Managed block only: all other patches, bundles and auth retained.
            path = home / '.dsh/cordis.patch.yml'
            before, body = m.state(path)
            text = (body or b'[]\n').decode()
            begin, end = '# BEGIN RECALLDOCK GATEWAY\n', '# END RECALLDOCK GATEWAY\n'
            if text.count(begin) != text.count(end) or text.count(begin) > 1:
                raise ValueError('malformed managed DSH block')
            block = begin + '- insert:\n  - id: mcp-recalldock-gateway\n    name: "@deepseek-ai/dsh-mcp-client"\n    config:\n      serverName: recalldock_gateway\n      transport: stdio\n      command: ' + json.dumps(binary) + '\n      args: [gateway, serve, --host, dsh]\n      failOnStartupError: true\n' + end
            if begin in text:
                start, stop = text.index(begin), text.index(end) + len(end)
                if stop <= start:
                    raise ValueError('malformed DSH block ordering')
                outside = text[:start] + text[stop:]
                if re.search(r'\bid:\s*[\'\"]?mcp-recalldock-gateway\b', outside):
                    raise ValueError('unmanaged DSH gateway exists; use an explicit reviewed migration')
                current_block = text[start:stop]
                command_line = re.compile(r'(?m)^([ \t]+command:)[^\n]*$')
                if len(command_line.findall(current_block)) != 1:
                    raise ValueError('managed DSH block requires exactly one runtime command')
                updated_block = command_line.sub(lambda match: match.group(1) + ' ' + json.dumps(binary), current_block)
                text = text[:start] + updated_block + text[stop:]
            else:
                if re.search(r'\bid:\s*[\'\"]?mcp-recalldock-gateway\b', text):
                    raise ValueError('unmanaged DSH gateway exists; preserve and migrate explicitly')
                text = ('' if text.strip() == '[]' else text.rstrip() + '\n') + block
            add(path, 'text', expected_state=before, text=text)
        elif host == 'antigravity':
            plugin = release / 'marketplace/plugins/antigravity'
            if not (plugin / 'plugin.json').is_file():
                raise ValueError('selected release has no native Antigravity plugin')
            host_notes.append('Antigravity uses its native plugin manager: agy plugin install ' + shlex.quote(str(plugin)) + '. No native settings or hook trust are fabricated; activate only after this runtime is installed.')
        else:
            raise ValueError('unsupported selected host')
    policy=data_dir/'candidate-admission.json'
    safe_parent(policy,data_dir)
    if policy.exists() and not policy.is_symlink():
        info=policy.stat()
        if stat.S_ISREG(info.st_mode) and info.st_size<=512:
            try:obj=json.loads(policy.read_bytes())
            except (ValueError,UnicodeError):obj=None
            # Only exact recognized policies are hardened; unknown formats stay fail-closed.
            if isinstance(obj,dict) and set(obj)=={'mode'} and obj['mode'] in ('checked','duplicates'):
                add(policy,'json',boundary=data_dir,changes=[dict(keys=['version'],value=2),dict(keys=['mode'],value=obj['mode']+'-v2')],retain_on_rollback=True)
                files.insert(0,files.pop())
                policy_notes.append('Versioned automatic-admission policy is applied first and retained on rollback; older runtimes require manual review. Original policy remains in the private backup.')
            elif isinstance(obj,dict) and set(obj)=={'version','mode'} and obj==dict(version=3,mode='tiered-v3'):
                add(policy,'json',boundary=data_dir,changes=[dict(keys=['version'],value=4),dict(keys=['mode'],value='tiered-v4')],retain_on_rollback=True)
                files.insert(0,files.pop())
                policy_notes.append('Tiered evidence verification is hardened first and retained on rollback; runtimes without tiered-v4 stay manual. The original policy is privately backed up.')
    return dict(version=1, release=str(release), release_sha256=release_hash, release_version=manifest['version'], home=str(home),data_dir=str(data_dir), hosts=hosts, files=files, notes=[
        'No knowledge DB, registry grants or unrelated tool configuration changes.',
        'Host plugins/hooks/skills require the host normal trust workflow against release/marketplace.',
        'Other MCP entries remain enabled; this is not a whole-host tool security boundary.',
        'Restart/new host sessions are required to validate actual loading.',
    ]+policy_notes+host_notes)


def detect_installed_hosts(home, release=None):
    hosts = []
    if (home / '.codex').is_dir():
        hosts.append('codex')
    if (home / '.claude').exists() or (home / '.claude.json').is_file():
        hosts.append('claude-code')
    # Only auto-enable Antigravity from global PATH when installing into the live user HOME,
    # and only when the selected release actually bundles the native Antigravity plugin.
    agy_detected = (home / '.gemini').is_dir() or (home == Path.home().resolve() and bool(shutil.which('agy')))
    agy_bundled = release is None or (release / 'marketplace/plugins/antigravity/plugin.json').is_file()
    if agy_detected and agy_bundled:
        hosts.append('antigravity')
    if (home / '.dsh').is_dir():
        hosts.append('dsh')
    return hosts


def default_backup_dir(home, version):
    base = home / '.local/state/recalldock/backups'
    stamp = version + '-' + time.strftime('%Y%m%d-%H%M%S')
    candidate = base / stamp
    suffix = 2
    while candidate.exists():
        candidate = base / f'{stamp}-{suffix}'
        suffix += 1
    return candidate


def print_human_plan(preview, hosts, backup_dir=None):
    print(f"RecallDock {preview['release_version']} local install plan")
    print(f"  plan_sha256: {preview['plan_sha256']}")
    print(f"  hosts: {', '.join(hosts) if hosts else '(CLI binary only)'}")
    if backup_dir is not None:
        print(f"  backup: {backup_dir}")
    if not preview['files']:
        print('  files: none (already up to date)')
    else:
        print('  files to update:')
        for f in preview['files']:
            tag = 'new' if f['before'] == 'absent' else 'update'
            extra = ' [retain_on_rollback]' if f.get('retain_on_rollback') else ''
            print(f"    - [{tag}] {f['path']}{extra}")
    for note in preview['notes']:
        print(f'  note: {note}')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--release', type=Path, help='path to extracted release directory (defaults to script parent)')
    parser.add_argument('--home', type=Path, help='target user home directory (defaults to current HOME)')
    parser.add_argument('--data-dir', type=Path, help='explicit existing data directory for policy hardening; defaults to HOME/.recalldock; no database reads')
    parser.add_argument('--host', action='append', choices=['codex', 'claude-code', 'dsh', 'antigravity'], default=[])
    parser.add_argument('--interactive', action='store_true', help='review planned changes and confirm before applying with automatic private backup')
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--expected-sha256')
    parser.add_argument('--backup', type=Path)
    parser.add_argument('--rollback', action='store_true')
    a = parser.parse_args()
    if a.rollback:
        if a.apply or a.interactive or a.release or a.home or a.data_dir or a.host or a.expected_sha256 or not a.backup or not a.backup.is_absolute():
            parser.error('--rollback requires only absolute --backup')
        m.rollback(a.backup)
        return
    if a.release is None:
        candidate = Path(__file__).resolve().parents[1]
        if (candidate / 'release.json').is_file():
            a.release = candidate
        else:
            parser.error('explicit --release required when not run from inside an extracted release')
    if a.home is None:
        a.home = Path.home().resolve()
    if len(set(a.host)) != len(a.host):
        parser.error('duplicate --host selection is not allowed')
    if a.interactive:
        if a.apply or a.expected_sha256:
            parser.error('--interactive cannot be combined with --apply or --expected-sha256')
        if not sys.stdin.isatty():
            raise ValueError('--interactive requires a live interactive terminal; use preview + --apply --expected-sha256 in scripts')
        if not a.host:
            a.host = detect_installed_hosts(a.home, a.release)
    plan = prepare(a.release, a.home, a.host, a.data_dir)
    raw = (json.dumps(plan, sort_keys=True, ensure_ascii=False, separators=(',', ':')) + '\n').encode()
    fingerprint = m.digest(raw)
    preview = dict(plan_sha256=fingerprint, release_version=plan['release_version'], release_sha256=plan['release_sha256'], files=[dict(path=e['path'], before=e['expected_sha256'], before_mode=e['expected_mode'], after=m.digest(m.render(e, m.state(Path(e['path']))[1])), after_mode=e.get('mode', e['expected_mode'] if e['expected_sha256'] != 'absent' else 0o600), **({'retain_on_rollback': True} if e.get('retain_on_rollback') else {})) for e in plan['files']], notes=plan['notes'])
    if a.interactive:
        backup = a.backup if a.backup is not None else default_backup_dir(a.home, plan['release_version'])
        if not backup.is_absolute() or backup.exists():
            raise ValueError('backup directory must be a non-existing absolute path')
        print_human_plan(preview, a.host, backup if plan['files'] else None)
        if not plan['files']:
            print(json.dumps(dict(ok=True, applied=0, notes=['already installed'])))
            return
        reply = input('Apply these changes? [y/N]: ')
        if reply.strip().lower() not in ('y', 'yes'):
            print('Cancelled. No files were changed.', file=sys.stderr)
            sys.exit(1)
        a.apply = True
        a.expected_sha256 = fingerprint
        a.backup = backup
    elif not a.apply:
        if a.expected_sha256 or a.backup:
            parser.error('backup/expected digest require --apply')
        print(json.dumps(preview, ensure_ascii=False, indent=2))
        return
    if a.expected_sha256 != fingerprint or not a.backup or not a.backup.is_absolute() or a.backup.exists():
        raise ValueError('apply requires matching preview digest and new absolute backup directory')
    if not plan['files']:
        print(json.dumps(dict(ok=True, applied=0, notes=['already installed'])))
        return
    # Directory creation is confined to selected home/data paths. Rollback retains
    # empty parent directories; files are restored with guarded preconditions.
    a.backup.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    for entry in plan['files']:
        Path(entry['path']).parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    prepared = []
    with m.locks([Path(e['path']) for e in plan['files']]):
        verify_release(a.release)
        for entry in plan['files']:
            before, body = m.state(Path(entry['path']))
            if before['sha256'] != entry['expected_sha256'] or before['mode'] != entry['expected_mode']:
                raise ValueError('configuration changed since preview')
            prepared.append((entry, before, body, m.render(entry, body)))
        m.apply_locked(prepared, a.backup)
    if a.interactive:
        print(f"Rollback command:\n  ~/.local/bin/recalldock setup --rollback --backup {shlex.quote(str(a.backup))}")


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, KeyError, TypeError) as error:
        print('install refused: ' + str(error), file=sys.stderr)
        sys.exit(1)
