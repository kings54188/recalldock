#!/usr/bin/env python3
"""Fail-closed public export and audit tool driven by public-export-manifest.json."""
from __future__ import annotations

import argparse
from fnmatch import fnmatchcase
import json
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys

# Disallowed files if present in exported target regardless of manifest patterns.
DENYLIST_NAMES = {
    'AGENTS.md',
    'CLAUDE.md',
    'DOCS.md',
    'MAINTENANCE.md',
    'PLAN-CHECKLIST.md',
}

DENYLIST_REL_PATHS = {
    'docs/AGENT-SETUP.md',
    'docs/ARCHITECTURE.md',
    'docs/DOCTOR.md',
    'docs/PLUGINS.md',
    'docs/TEAM-USAGE.md',
    'docs/intended-usage.md',
}

DENYLIST_PREFIXES = (
    'docs/codebase/',
    'docs/diagrams/',
    'docs/engram-cloud/',
    'docs/beta/',
    'skills/',
    'openspec/',
    '.windsurf/',
    '.devcontainer/',
)

FORBIDDEN_CONTENT_PATTERNS = [
    ('developer_home_path', re.compile(r'/home/k(?:/|\b)')),
    ('private_work_tag', re.compile(r'<local-work>|private-work')),
    ('excluded_maintenance_doc', re.compile(r'\bMAINTENANCE\.md\b')),
    ('excluded_plan_checklist', re.compile(r'\bPLAN-CHECKLIST\.md\b')),
    ('excluded_core_completion', re.compile(r'\bCORE-COMPLETION\.md\b')),
    ('excluded_codebase_guide', re.compile(r'\bCODEBASE-GUIDE\.md\b')),
    ('excluded_acceptance_doc', re.compile(r'\bACCEPTANCE-0323\.md\b')),
    ('private_model_id', re.compile(r'\bgpt-6-luna\b')),
    ('github_pat', re.compile(r'(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}')),
    ('gitlab_pat', re.compile(r'glpat-[A-Za-z0-9\-_]{20,}')),
    ('anthropic_key', re.compile(r'sk-ant-[A-Za-z0-9\-_]{20,}')),
    ('aws_key', re.compile(r'\bAKIA[0-9A-Z]{16}\b')),
    ('private_key_pem', re.compile(r'-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----')),
]

# Skip self-match inside export-public.py and narrow single-rule synthetic test/manifest exemptions.
CONTENT_SCAN_SELF_EXEMPT = {'tools/export-public.py'}
EXCLUDED_DOC_NAME_EXEMPT = {'public-export-manifest.json'}
SYNTHETIC_PEM_FIXTURE_EXEMPT = {'internal/opsstore/store_test.go'}

BINARY_DB_EXTENSIONS = {'.db', '.sqlite', '.sqlite3', '.db-wal', '.db-shm', '.wal', '.shm'}
MARKDOWN_LINK_RE = re.compile(r'\[[^\]]*\]\(([^)]+)\)')


def match_pattern(rel: str, pattern: str) -> bool:
    if pattern.endswith('/**'):
        prefix = pattern[:-3]
        return rel == prefix or rel.startswith(prefix + '/')
    return fnmatchcase(rel, pattern)


def matches_any(rel: str, patterns: list[str]) -> bool:
    return any(match_pattern(rel, pat) for pat in patterns)


def load_manifest(path: Path) -> dict:
    data = json.loads(path.read_text(encoding='utf-8'))
    if data.get('format') != 1:
        raise ValueError(f'unsupported manifest format in {path}')
    if not isinstance(data.get('include'), list) or not isinstance(data.get('exclude'), list):
        raise ValueError('manifest must contain include and exclude lists')
    return data


def list_source_files(source: Path) -> list[str]:
    if (source / '.git').exists():
        out = subprocess.check_output(
            ['git', '-C', str(source), 'ls-files', '--cached', '--others', '--exclude-standard', '-z'],
        )
        items = [p.decode('utf-8') for p in out.split(b'\x00') if p]
        return sorted(items)
    result = []
    for p in source.rglob('*'):
        if p.is_file() or p.is_symlink():
            rel = p.relative_to(source).as_posix()
            if rel.startswith('.git/') or '__pycache__' in Path(rel).parts or rel.endswith('.pyc'):
                continue
            result.append(rel)
    return sorted(result)


def classify_files(files: list[str], manifest: dict) -> tuple[list[str], list[str]]:
    included = []
    excluded = []
    unclassified = []
    ambiguous = []
    inc = manifest['include']
    exc = manifest['exclude']
    for rel in files:
        if '__pycache__' in PurePosixPath(rel).parts or rel.endswith('.pyc'):
            continue
        in_inc = matches_any(rel, inc)
        in_exc = matches_any(rel, exc)
        if in_inc and in_exc:
            ambiguous.append(rel)
        elif in_inc:
            included.append(rel)
        elif in_exc:
            excluded.append(rel)
        else:
            unclassified.append(rel)
    if ambiguous:
        raise ValueError(f'files match both include and exclude: {ambiguous[:10]}')
    if unclassified:
        raise ValueError(f'unclassified source files (fail-closed): {unclassified[:20]}')
    return included, excluded


def audit_target(target: Path, manifest: dict) -> list[str]:
    errors: list[str] = []
    if not target.is_dir():
        return [f'target directory does not exist: {target}']

    target_root = target.resolve()
    present_files: set[str] = set()
    for p in target.rglob('*'):
        rel = p.relative_to(target).as_posix()
        parts = PurePosixPath(rel).parts
        if parts[0] == '.git' or '__pycache__' in parts or rel.endswith('.pyc'):
            continue
        if p.is_symlink():
            errors.append(f'symlink forbidden in public export: {rel}')
            continue
        if not p.is_file():
            continue
        present_files.add(rel)

        if Path(rel).name in DENYLIST_NAMES and rel not in ('tools/export-public.py',):
            errors.append(f'denylisted filename present: {rel}')
        if rel in DENYLIST_REL_PATHS:
            errors.append(f'obsolete doc present: {rel}')
        if any(rel.startswith(px) for px in DENYLIST_PREFIXES):
            errors.append(f'denylisted directory present: {rel}')
        if matches_any(rel, manifest['exclude']) or not matches_any(rel, manifest['include']):
            errors.append(f'file outside public manifest allowlist: {rel}')
        if any(rel.endswith(ext) for ext in BINARY_DB_EXTENSIONS):
            errors.append(f'database artifact forbidden in public export: {rel}')

        if rel in CONTENT_SCAN_SELF_EXEMPT:
            continue
        try:
            raw = p.read_bytes()
        except OSError as exc:
            errors.append(f'cannot read {rel}: {exc}')
            continue
        if b'\x00' in raw:
            continue
        text = raw.decode('utf-8', errors='replace')
        for label, pat in FORBIDDEN_CONTENT_PATTERNS:
            if label.startswith('excluded_') and rel in EXCLUDED_DOC_NAME_EXEMPT:
                continue
            if label == 'private_key_pem' and rel in SYNTHETIC_PEM_FIXTURE_EXEMPT:
                continue
            m = pat.search(text)
            if m:
                line_no = text[: m.start()].count('\n') + 1
                errors.append(f'forbidden pattern {label} at {rel}:{line_no}')

        if rel.endswith('.md'):
            for line_no, line in enumerate(text.splitlines(), 1):
                for m in MARKDOWN_LINK_RE.finditer(line):
                    link = m.group(1).strip()
                    if not link or link.startswith(('http://', 'https://', 'mailto:', '#')):
                        continue
                    path_part = link.split('#', 1)[0].split('?', 1)[0]
                    if not path_part:
                        continue
                    resolved = (p.parent / path_part).resolve()
                    try:
                        rel_target = resolved.relative_to(target_root).as_posix()
                    except ValueError:
                        errors.append(f'broken outside-repo link at {rel}:{line_no} -> {link}')
                        continue
                    if not resolved.exists() and rel_target not in present_files:
                        errors.append(f'broken markdown link at {rel}:{line_no} -> {link}')
    return errors


def export_tree(source: Path, output: Path, manifest: dict) -> dict:
    files = list_source_files(source)
    included, excluded = classify_files(files, manifest)
    if output.exists():
        # Preserve output/.git if refreshing an existing clean preview clone, remove everything else.
        for child in list(output.iterdir()):
            if child.name == '.git':
                continue
            if child.is_dir() and not child.is_symlink():
                shutil.rmtree(child)
            else:
                child.unlink()
    else:
        output.mkdir(parents=True, exist_ok=True)

    for rel in included:
        src = source / rel
        if src.is_symlink():
            raise ValueError(f'refusing to export symlink: {rel}')
        dst = output / rel
        dst.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(src, dst)

    errors = audit_target(output, manifest)
    if errors:
        raise ValueError('post-export audit failed:\n  ' + '\n  '.join(errors))
    return {'included': len(included), 'excluded': len(excluded), 'output': str(output)}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument('--manifest', type=Path)
    parser.add_argument('--output', type=Path)
    parser.add_argument('--check', action='store_true', help='audit --target against manifest and leak rules')
    parser.add_argument('--target', type=Path)
    args = parser.parse_args()

    source = args.source.resolve()
    manifest_path = (args.manifest or (source / 'public-export-manifest.json')).resolve()
    manifest = load_manifest(manifest_path)

    if args.check:
        target = (args.target or source).resolve()
        errors = audit_target(target, manifest)
        if errors:
            print(json.dumps({'ok': False, 'errors': errors}, indent=2, ensure_ascii=False), file=sys.stderr)
            return 1
        print(json.dumps({'ok': True, 'target': str(target)}, ensure_ascii=False))
        return 0

    if not args.output:
        parser.error('--output is required unless --check is set')
    report = export_tree(source, args.output.resolve(), manifest)
    print(json.dumps({'ok': True, **report}, ensure_ascii=False))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
