#!/usr/bin/env python3
"""Isolated release identity and archive integrity regressions."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock
import zipfile


ROOT = Path(__file__).resolve().parents[1]


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace('-', '_'), ROOT / 'tools' / (name + '.py'))
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ReleaseTest(unittest.TestCase):
    def test_builders_reject_unverified_or_mislabeled_versions_before_building(self):
        for script in ('build-release', 'build-release-windows'):
            builder = load(script)
            for flags in ([], ['--development'], ['--publish-local', '--development']):
                with self.subTest(script=script, flags=flags), tempfile.TemporaryDirectory() as tmp:
                    out = Path(tmp) / 'package'
                    with mock.patch.object(sys, 'argv', [script, '--version', '1.2.3', '--output', str(out), *flags]):
                        with mock.patch.object(builder.subprocess, 'check_output') as git:
                            with self.assertRaises(SystemExit):
                                builder.main()
                            git.assert_not_called()
                    self.assertFalse(out.exists())
            with mock.patch.object(sys, 'argv', [script, '--version', '1.2.3-dev.1', '--publish-local', '--output', '/unused']):
                with mock.patch.object(builder.subprocess, 'check_output') as git:
                    with self.assertRaises(SystemExit):
                        builder.main()
                    git.assert_not_called()

    def test_tag_validation_rejects_missing_lightweight_or_wrong_commit(self):
        for script in ('build-release', 'build-release-windows'):
            builder = load(script)
            with mock.patch.object(builder.subprocess, 'check_output', side_effect=['tag\n', 'abc\n']) as git:
                self.assertEqual(builder.require_published_tag(ROOT, '1.2.3', 'abc'), 'v1.2.3')
                self.assertIn('refs/tags/v1.2.3', git.call_args_list[0].args[0])
            for outputs in (['commit\n', 'abc\n'], ['tag\n', 'other\n']):
                with mock.patch.object(builder.subprocess, 'check_output', side_effect=outputs):
                    with self.assertRaises(ValueError):
                        builder.require_published_tag(ROOT, '1.2.3', 'abc')
            with mock.patch.object(builder.subprocess, 'check_output', side_effect=subprocess.CalledProcessError(1, ['git'])):
                with self.assertRaises(ValueError):
                    builder.require_published_tag(ROOT, '1.2.3', 'abc')

    def test_source_verification_rejects_tag_drift_and_dirty_checkout(self):
        verifier = load('verify-release')
        valid = ['tag', 'b' * 40, 'a' * 40, 'a' * 40, '']
        with mock.patch.object(verifier.subprocess, 'check_output', side_effect=valid):
            verifier.verify_source(ROOT, '1.2.3', 'a' * 40, 'b' * 40)
        for index, value in enumerate(['commit', 'c' * 40, 'c' * 40, 'c' * 40, ' M README.md']):
            outputs = valid.copy()
            outputs[index] = value
            with self.subTest(index=index), mock.patch.object(verifier.subprocess, 'check_output', side_effect=outputs):
                with self.assertRaises(ValueError):
                    verifier.verify_source(ROOT, '1.2.3', 'a' * 40, 'b' * 40)

    def fixture(self, base, platform='linux', development=False):
        out = base / platform
        out.mkdir()
        version = '1.2.3-dev.1' if development else '1.2.3'
        name = f'recalldock_{version}_{platform}_amd64'
        metadata = dict(format=1, version=version, source_commit='a' * 40, source_dirty=False,
                        development=development, published=not development, platform=platform, architecture='amd64')
        if not development:
            metadata.update(source_tag='v1.2.3', source_tag_object='b' * 40)
        files = {'runtime/recalldock' + ('.exe' if platform == 'windows' else ''): b'binary',
                 'LICENSE': b'MIT License', 'NOTICE.md': b'Attribution', 'PROVENANCE.md': b'Origins',
                 'README.md': b'Readme', 'README.zh-CN.md': b'Readme', 'docs/RELEASE.md': b'Release'}
        manifest: dict = dict(metadata, files={p: hashlib.sha256(v).hexdigest() for p, v in files.items()})
        archive = out / (name + ('.zip' if platform == 'windows' else '.tar.gz'))
        build: dict = dict(metadata, archive=archive.name)
        return out, name, files, manifest, build

    def write_fixture(self, out, name, files, manifest, build):
        members = dict(files, **{'release.json': (json.dumps(manifest) + '\n').encode()})
        archive = out / build['archive']
        if archive.suffix == '.zip':
            with zipfile.ZipFile(archive, 'w') as zf:
                for path, body in members.items():
                    zf.writestr(name + '/' + path, body)
        else:
            with tarfile.open(archive, 'w:gz') as tf:
                for path, body in members.items():
                    info = tarfile.TarInfo(name + '/' + path)
                    info.size = len(body)
                    tf.addfile(info, io.BytesIO(body))
        build['sha256'] = hashlib.sha256(archive.read_bytes()).hexdigest()
        (out / 'checksums.txt').write_text(build['sha256'] + '  ' + archive.name + '\n')
        (out / 'build.json').write_text(json.dumps(build))

    def verify(self, out, platform, development=False):
        verifier = load('verify-release')
        return verifier.verify_package(out, platform, '1.2.3-dev.1' if development else '1.2.3',
                                       'a' * 40, development, None if development else 'b' * 40)

    def test_valid_formal_and_development_archives(self):
        for platform in ('linux', 'windows'):
            for development in (False, True):
                with self.subTest(platform=platform, development=development), tempfile.TemporaryDirectory() as tmp:
                    fixture = self.fixture(Path(tmp), platform, development)
                    self.write_fixture(*fixture)
                    self.assertEqual(self.verify(fixture[0], platform, development)['platform'], platform)

    def test_mismatched_build_or_release_identity_is_rejected(self):
        changes = {'version': '1.2.4', 'source_commit': 'c' * 40, 'source_dirty': True,
                   'development': True, 'published': False, 'platform': 'other', 'architecture': 'arm64',
                   'source_tag': 'v1.2.4', 'source_tag_object': 'c' * 40}
        for platform in ('linux', 'windows'):
            for target in ('manifest', 'build'):
                for key, value in changes.items():
                    with self.subTest(platform=platform, target=target, field=key), tempfile.TemporaryDirectory() as tmp:
                        out, name, files, manifest, build = self.fixture(Path(tmp), platform)
                        (manifest if target == 'manifest' else build)[key] = value
                        self.write_fixture(out, name, files, manifest, build)
                        with self.assertRaises(ValueError):
                            self.verify(out, platform)

    def test_archive_and_manifest_tampering_is_rejected(self):
        for platform in ('linux', 'windows'):
            for tamper in ('archive', 'checksum', 'manifest_hash', 'extra', 'missing', 'unsafe', 'missing_tag'):
                with self.subTest(platform=platform, tamper=tamper), tempfile.TemporaryDirectory() as tmp:
                    out, name, files, manifest, build = self.fixture(Path(tmp), platform)
                    if tamper == 'manifest_hash':
                        manifest['files']['LICENSE'] = '0' * 64
                    elif tamper == 'extra':
                        files['unlisted.txt'] = b'unlisted'
                    elif tamper == 'missing':
                        del files['LICENSE']
                    elif tamper == 'unsafe':
                        files['../escape'] = b'unsafe'
                    elif tamper == 'missing_tag':
                        del manifest['source_tag']
                    self.write_fixture(out, name, files, manifest, build)
                    if tamper == 'archive':
                        with (out / build['archive']).open('ab') as f:
                            f.write(b'tampered')
                    elif tamper == 'checksum':
                        (out / 'checksums.txt').write_text('0' * 64 + '  ' + build['archive'] + '\n')
                    with self.assertRaises(ValueError):
                        self.verify(out, platform)

    def test_candidate_cannot_pass_formal_verification(self):
        for platform in ('linux', 'windows'):
            with tempfile.TemporaryDirectory() as tmp:
                fixture = self.fixture(Path(tmp), platform, True)
                self.write_fixture(*fixture)
                with self.assertRaises(ValueError):
                    self.verify(fixture[0], platform)


if __name__ == '__main__':
    unittest.main()
