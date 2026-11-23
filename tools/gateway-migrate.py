#!/usr/bin/env python3
"""Apply an explicit local gateway migration plan with CAS and protected rollback.

Plans contain original configuration references/policy only, never credentials.
Preview is the default. Backups are private and rollback refuses later edits.
"""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile
import tomllib


def digest(body):
    return hashlib.sha256(body).hexdigest()


def validate_parent(path):
    if not path.is_absolute() or path.parent.resolve() != path.parent:
        raise ValueError('configuration parent must be absolute and canonical, without symlinks')


def same_state(current, expected):
    return current['kind'] == expected['kind'] and current['sha256'] == expected['sha256'] and current['mode'] == expected['mode']


def state(path):
    validate_parent(path)
    if path.is_symlink():
        target = os.readlink(path)
        return dict(kind='symlink', target=target, sha256=digest(('symlink:' + target).encode()), mode=0o777), None
    if not path.exists():
        return dict(kind='absent', sha256='absent', mode=0o600), None
    info = path.stat()
    if not stat.S_ISREG(info.st_mode) or info.st_size > 2 << 20:
        raise ValueError('configuration must be a bounded regular file')
    body = path.read_bytes()
    return dict(kind='file', sha256=digest(body), mode=stat.S_IMODE(info.st_mode)), body


def toml_set(text, keys, value):
    """Change one scalar while preserving comments and unrelated tables."""
    parsed = tomllib.loads(text)
    node = parsed
    for key in keys[:-1]:
        node = node.get(key, {})
        if not isinstance(node, dict):
            raise ValueError('TOML parent is not a table')
    header = '[' + '.'.join(json.dumps(k, ensure_ascii=False) for k in keys[:-1]) + ']'
    # Recognize table identity with the TOML parser, including quoted names.
    lines = text.splitlines(keepends=True)
    start, end = None, len(lines)
    for i, line in enumerate(lines):
        stripped = line.strip()
        if not stripped.startswith('['):
            continue
        if start is not None:
            end = i
            break
        try:
            table = tomllib.loads(stripped)
            branch = table
            for key in keys[:-1]:
                branch = branch[key]
            if branch == {}:
                start = i + 1
        except (KeyError, TypeError, tomllib.TOMLDecodeError):
            continue
    literal = json.dumps(value, ensure_ascii=False)
    assignment = json.dumps(keys[-1]) + ' = ' + literal + '\n'
    if start is None:
        if keys[-1] in node:
            raise ValueError('inline/dotted TOML policy needs an explicit reviewed edit')
        text = text.rstrip() + '\n\n' + header + '\n' + assignment
    else:
        changed = False
        for i in range(start, end):
            line = lines[i]
            if '=' not in line or line.lstrip().startswith('#'):
                continue
            try:
                entry = tomllib.loads(line)
            except tomllib.TOMLDecodeError:
                continue
            if keys[-1] in entry:
                lines[i] = assignment
                changed = True
                break
        if not changed:
            if keys[-1] in node:
                raise ValueError('multiline TOML policy needs an explicit reviewed edit')
            lines.insert(end, assignment)
        text = ''.join(lines)
    checked = tomllib.loads(text)
    for key in keys:
        checked = checked[key]
    if checked != value:
        raise ValueError('TOML patch verification failed')
    return text


def render(entry, before):
    kind = entry['format']
    if kind == 'text':
        body = entry['text'].encode()
    elif kind == 'json':
        obj = json.loads(before or b'{}')
        for change in entry['changes']:
            node = obj
            for key in change['keys'][:-1]:
                node = node.setdefault(key, {})
            node[change['keys'][-1]] = change['value']
        body = (json.dumps(obj, ensure_ascii=False, indent=2) + '\n').encode()
    elif kind == 'toml':
        text = (before or b'').decode()
        for change in entry['changes']:
            text = toml_set(text, change['keys'], change['value'])
        body = text.encode()
    elif kind == 'yaml_append':
        text = (before or b'[]\n').decode().rstrip()
        if text == '[]':
            text = ''
        body = (text + '\n' + entry['text'].rstrip() + '\n').encode()
    else:
        raise ValueError('unsupported reviewed edit format')
    if len(body) > 2 << 20:
        raise ValueError('configuration exceeds size limit')
    return body


def atomic_write(path, body, mode):
    validate_parent(path)
    fd, name = tempfile.mkstemp(prefix='.gateway-migrate-', dir=path.parent)
    try:
        os.fchmod(fd, mode)
        with os.fdopen(fd, 'wb') as f:
            f.write(body)
            f.flush()
            os.fsync(f.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


@contextmanager
def locks(paths):
    for path in paths:
        validate_parent(path)
    owned = []
    try:
        for path in sorted(paths):
            name = '.' + path.name + '.gateway-migrate.lock'
            directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
            try:
                fd = os.open(name, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o600, dir_fd=directory)
            except BaseException:
                os.close(directory)
                raise
            owned.append((fd, directory, name))
        yield
    finally:
        for fd, directory, name in reversed(owned):
            os.close(fd)
            try:
                os.unlink(name, dir_fd=directory)
            finally:
                os.close(directory)


def rollback_locked(backup):
    record = json.loads((backup / 'rollback.json').read_text())
    if record.get('version') not in (1,2):
        raise ValueError('unsupported rollback format')
    # Preflight everything before restoring any file.
    for item in record['files']:
        current, _ = state(Path(item['path']))
        installed = dict(kind='file', sha256=item['after_sha256'], mode=item.get('after_mode', current['mode']))
        if item.get('retain_on_rollback') and not same_state(current, installed):
            raise ValueError('rollback refused: retained security policy changed; inspect ' + item['path'])
        if not same_state(current, installed) and not same_state(current, item['before']):
            raise ValueError('rollback refused: later changes at ' + item['path'])
        if item['before']['kind'] == 'file':
            body = (backup / item['backup']).read_bytes()
            if digest(body) != item['before']['sha256']:
                raise ValueError('backup integrity mismatch')
    for item in reversed(record['files']):
        if item.get('retain_on_rollback'):
            continue
        path = Path(item['path'])
        current, _ = state(path)
        before = item['before']
        if same_state(current, before):
            continue
        if before['kind'] == 'absent':
            path.unlink()
        elif before['kind'] == 'symlink':
            temp = path.with_name('.gateway-restore-' + str(os.getpid()))
            temp.symlink_to(before['target'])
            os.replace(temp, path)
        else:
            body = (backup / item['backup']).read_bytes()
            if digest(body) != before['sha256']:
                raise ValueError('backup integrity mismatch')
            atomic_write(path, body, before['mode'])
    retained=sum(bool(item.get('retain_on_rollback')) for item in record['files'])
    result=dict(ok=True, restored=len(record['files'])-retained)
    if retained:result['retained']=retained
    print(json.dumps(result))


def rollback(backup):
    record = json.loads((backup / 'rollback.json').read_text())
    with locks([Path(item['path']) for item in record['files']]):
        rollback_locked(backup)


def apply_locked(prepared, backup):
    # Reconcile under the shared lock before creating the rollback point.
    for entry, before, _, _ in prepared:
        current, _ = state(Path(entry['path']))
        if not same_state(current, before):
            raise ValueError('concurrent edit: ' + entry['path'])
    backup.mkdir(mode=0o700, exist_ok=False)
    records = []
    for i, (entry, before, body, after) in enumerate(prepared):
        name = str(i) + '.original'
        if body is not None:
            atomic_write(backup / name, body, 0o600)
        record=dict(path=entry['path'], before=before, after_sha256=digest(after), after_mode=entry.get('mode', before['mode'] if before['kind'] == 'file' else 0o600), backup=name)
        if entry.get('retain_on_rollback') is True:record['retain_on_rollback']=True
        records.append(record)
    version=2 if any(item.get('retain_on_rollback') for item in records) else 1
    atomic_write(backup / 'rollback.json', (json.dumps(dict(version=version, files=records), indent=2) + '\n').encode(), 0o600)
    for entry, before, _, after in prepared:
        path = Path(entry['path'])
        current, _ = state(path)
        if not same_state(current, before):
            raise ValueError('concurrent edit; preserve backup and inspect ' + str(path))
        atomic_write(path, after, entry.get('mode', before['mode'] if before['kind'] == 'file' else 0o600))
    print(json.dumps(dict(ok=True, applied=len(prepared), backup=str(backup))))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--plan', type=Path)
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--expected-sha256')
    parser.add_argument('--backup', type=Path)
    parser.add_argument('--rollback', action='store_true')
    args = parser.parse_args()
    if args.rollback:
        if args.apply or not args.backup or not args.backup.is_absolute():
            parser.error('--rollback requires only absolute --backup')
        rollback(args.backup)
        return
    if not args.plan or not args.plan.is_absolute():
        parser.error('absolute --plan required')
    raw = args.plan.read_bytes()
    plan = json.loads(raw)
    if plan['version'] != 1 or not 1 <= len(plan['files']) <= 100:
        raise ValueError('unsupported or oversized plan')
    prepared, seen = [], set()
    for entry in plan['files']:
        if not isinstance(entry.get('retain_on_rollback',False),bool):
            raise ValueError('retain_on_rollback must be a boolean')
        path = Path(entry['path'])
        if not path.is_absolute() or path in seen or not path.parent.is_dir():
            raise ValueError('unique absolute paths with existing parents required')
        seen.add(path)
        before, body = state(path)
        if before['sha256'] != entry['expected_sha256']:
            raise ValueError('stale plan: ' + str(path))
        if before['kind'] == 'symlink' and not entry.get('replace_symlink', False):
            raise ValueError('symlink replacement needs explicit plan selection')
        after = render(entry, body)
        prepared.append((entry, before, body, after))
    preview = dict(plan_sha256=digest(raw), files=[dict(path=e['path'], before=b['sha256'], after=digest(a), format=e['format']) for e, b, _, a in prepared], notes=plan.get('notes', []))
    if not args.apply:
        print(json.dumps(preview, ensure_ascii=False, indent=2))
        return
    if args.expected_sha256 != digest(raw) or not args.backup or not args.backup.is_absolute():
        raise ValueError('apply requires matching --expected-sha256 and new absolute --backup')
    with locks([Path(e['path']) for e, _, _, _ in prepared]):
        apply_locked(prepared, args.backup)


if __name__ == '__main__':
    main()
