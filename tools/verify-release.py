#!/usr/bin/env python3
"""Verify release source identity, manifests and archive checksums before publication."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import tarfile
import zipfile


ROOT = Path(__file__).resolve().parents[1]


def verify_source(root, version, commit, tag_object):
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', version):
        raise ValueError('formal release requires a final semantic version')
    ref = 'refs/tags/v' + version
    def git(*args):
        return subprocess.check_output(['git', *args], cwd=root, text=True).strip()
    if git('cat-file', '-t', ref) != 'tag':
        raise ValueError('formal release requires an annotated tag')
    if git('rev-parse', ref) != tag_object or git('rev-parse', ref + '^{commit}') != commit:
        raise ValueError('annotated tag identity changed or points to a different commit')
    if git('rev-parse', 'HEAD') != commit or git('status', '--porcelain'):
        raise ValueError('release checkout must be clean and match the resolved source commit')


def archive_files(archive, prefix):
    files = {}
    seen = set()
    def member_path(name):
        path = PurePosixPath(name)
        if '\\' in name or path.is_absolute() or '..' in path.parts or not path.parts or path.parts[0] != prefix:
            raise ValueError('unsafe or unexpected archive member: ' + name)
        rel = path.relative_to(prefix).as_posix()
        if rel in seen:
            raise ValueError('duplicate archive member: ' + name)
        seen.add(rel)
        return rel
    if archive.suffix == '.zip':
        with zipfile.ZipFile(archive) as zf:
            for member in zf.infolist():
                rel = member_path(member.filename)
                mode = member.external_attr >> 16
                if stat.S_ISLNK(mode):
                    raise ValueError('archive symlinks are forbidden')
                if not member.is_dir():
                    files[rel] = zf.read(member)
    else:
        with tarfile.open(archive, 'r:gz') as tf:
            for member in tf:
                rel = member_path(member.name)
                if not member.isfile() and not member.isdir():
                    raise ValueError('archive links and special files are forbidden')
                if member.isfile():
                    stream = tf.extractfile(member)
                    assert stream is not None
                    files[rel] = stream.read()
    return files


def verify_package(directory, platform, version, commit, development, tag_object):
    if development:
        if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+-[a-z0-9.]+', version):
            raise ValueError('development verification requires a prerelease version')
    elif not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', version) or not tag_object:
        raise ValueError('formal verification requires a final version and annotated tag object')
    name = f'recalldock_{version}_{platform}_amd64'
    archive_name = name + ('.zip' if platform == 'windows' else '.tar.gz')
    build = json.loads((directory / 'build.json').read_text(encoding='utf-8'))
    expected = dict(format=1, version=version, source_commit=commit, source_dirty=False,
                    development=development, published=not development, platform=platform, architecture='amd64')
    if not development:
        expected.update(source_tag='v' + version, source_tag_object=tag_object)
    for key, value in expected.items():
        if key not in build or type(build[key]) is not type(value) or build[key] != value:
            raise ValueError(f'{platform} build.json mismatch: {key}')
    if build.get('archive') != archive_name:
        raise ValueError('archive name does not match release identity')
    archive = directory / archive_name
    sha = hashlib.sha256(archive.read_bytes()).hexdigest()
    lines = (directory / 'checksums.txt').read_text(encoding='utf-8').splitlines()
    if build.get('sha256') != sha or len(lines) != 1 or lines[0].split() != [sha, archive_name]:
        raise ValueError(f'{platform} archive SHA256/build.json/checksums.txt mismatch')
    members = archive_files(archive, name)
    if 'release.json' not in members:
        raise ValueError('release.json missing from archive')
    manifest = json.loads(members.pop('release.json'))
    for key, value in expected.items():
        if key not in manifest or type(manifest[key]) is not type(value) or manifest[key] != value:
            raise ValueError(f'{platform} release.json mismatch: {key}')
    hashes = manifest.get('files')
    if not isinstance(hashes, dict) or set(hashes) != set(members):
        raise ValueError('archive files differ from release.json inventory')
    required = {'LICENSE', 'NOTICE.md', 'PROVENANCE.md', 'README.md', 'README.zh-CN.md', 'docs/RELEASE.md',
                'runtime/recalldock' + ('.exe' if platform == 'windows' else '')}
    if not required.issubset(members):
        raise ValueError('release is missing runtime or attribution/documentation files')
    for path, body in members.items():
        if hashlib.sha256(body).hexdigest() != hashes[path]:
            raise ValueError('release.json member SHA256 mismatch: ' + path)
    return dict(platform=platform, archive=archive_name, sha256=sha, files=len(members))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--source-commit', required=True)
    parser.add_argument('--tag-object')
    parser.add_argument('--development', action='store_true')
    parser.add_argument('--source-only', action='store_true')
    parser.add_argument('--linux', type=Path)
    parser.add_argument('--windows', type=Path)
    parser.add_argument('--checksums-output', type=Path)
    parser.add_argument('--report', type=Path)
    a = parser.parse_args()
    if a.source_only and a.development:
        parser.error('source-only verification requires a formal tag')
    if not a.development:
        verify_source(ROOT, a.version, a.source_commit, a.tag_object)
    if not a.source_only and not (a.linux or a.windows):
        parser.error('at least one package directory is required')
    packages = [verify_package(directory, platform, a.version, a.source_commit, a.development, a.tag_object)
                for platform, directory in (('linux', a.linux), ('windows', a.windows)) if directory]
    report = dict(ok=True, version=a.version, source_commit=a.source_commit, development=a.development,
                  source_tag=None if a.development else 'v' + a.version, source_tag_object=a.tag_object, packages=packages)
    if a.checksums_output:
        if len(packages) != 2:
            parser.error('combined checksums require both verified platforms')
        a.checksums_output.write_text(''.join(f"{p['sha256']}  {p['archive']}\n" for p in packages), encoding='utf-8')
    if a.report:
        a.report.write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
