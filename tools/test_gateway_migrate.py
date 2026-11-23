#!/usr/bin/env python3
"""Synthetic migration/rollback tests; never touch real host configuration."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import tomllib
import unittest

SCRIPT = Path(__file__).with_name('gateway-migrate.py')
spec = importlib.util.spec_from_file_location('migration', SCRIPT)
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)


class MigrationTest(unittest.TestCase):
    def test_rollback_retains_security_policy_and_refuses_unsafe_reversion(self):
        with tempfile.TemporaryDirectory(prefix='security-policy-migration-') as tmp:
            root=Path(tmp);policy=root/'policy.json';launcher=root/'launcher'
            original=json.dumps(dict(mode='checked'));policy.write_text(original);launcher.write_text('old launcher')
            plan=root/'plan.json';backup=root/'backup'
            plan.write_text(json.dumps(dict(version=1,files=[dict(path=str(policy),expected_sha256=m.state(policy)[0]['sha256'],format='json',changes=[dict(keys=['version'],value=2),dict(keys=['mode'],value='checked-v2')],retain_on_rollback=True),dict(path=str(launcher),expected_sha256=m.state(launcher)[0]['sha256'],format='text',text='new launcher')])))
            def run(*args):return subprocess.run([sys.executable,str(SCRIPT),*map(str,args)],capture_output=True,text=True,timeout=10)
            applied=run('--plan',plan,'--apply','--expected-sha256',m.digest(plan.read_bytes()),'--backup',backup);self.assertEqual(applied.returncode,0,applied.stderr)
            hardened=policy.read_bytes()
            policy.write_text(original)
            refused=run('--rollback','--backup',backup)
            self.assertNotEqual(refused.returncode,0,'unsafe legacy policy allowed an automatic downgrade')
            self.assertEqual(launcher.read_text(),'new launcher','all preconditions must be checked before any restoration')
            policy.write_bytes(hardened)
            restored=run('--rollback','--backup',backup);self.assertEqual(restored.returncode,0,restored.stderr)
            self.assertEqual(launcher.read_text(),'old launcher')
            self.assertEqual(policy.read_bytes(),hardened,'rollback re-enabled the legacy automatic policy')
            self.assertEqual(json.loads(restored.stdout)['retained'],1)
            record=json.loads((backup/'rollback.json').read_text())
            selected=next(x for x in record['files'] if x['path']==str(policy))
            self.assertEqual((backup/selected['backup']).read_text(),original,'operator recovery original was lost')

    def test_apply_reconciles_modes_before_any_backup_or_write(self):
        with tempfile.TemporaryDirectory(prefix='gateway-mode-cas-') as tmp:
            root = Path(tmp); config = root / 'config'; config.write_text('original'); config.chmod(0o600)
            before, body = m.state(config)
            entry = dict(path=str(config), format='text', text='installed', mode=0o755)
            config.chmod(0o640)
            with self.assertRaisesRegex(ValueError, 'concurrent'):
                with m.locks([config]):
                    m.apply_locked([(entry, before, body, b'installed')], root / 'backup')
            self.assertFalse((root / 'backup').exists())
            self.assertEqual(config.read_bytes(), body)
            self.assertEqual(config.stat().st_mode & 0o777, 0o640)

    def test_parent_symlink_refused_before_locks_apply_and_rollback(self):
        for operation in ['apply', 'rollback']:
            with self.subTest(operation=operation), tempfile.TemporaryDirectory(prefix='gateway-parent-') as tmp:
                root = Path(tmp); home = root / 'home'; home.mkdir(); directory = home / '.codex'; directory.mkdir()
                config = directory / 'config.toml'; config.write_text('original config')
                other = home / 'other.json'; other.write_text('original other')
                entries = [dict(path=str(path), expected_sha256=m.state(path)[0]['sha256'], format='text', text='installed ' + path.name) for path in [config, other]]
                plan = root / 'plan.json'; plan.write_text(json.dumps(dict(version=1, files=entries)))
                backup = root / 'backup'
                command = [sys.executable, '-I', str(SCRIPT)]
                apply = ['--plan', str(plan), '--apply', '--expected-sha256', m.digest(plan.read_bytes()), '--backup', str(backup)]
                if operation == 'rollback':
                    result = subprocess.run(command + apply, capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, 0, result.stderr)
                outside = root / 'outside'; outside.mkdir()
                external = outside / config.name; external.write_bytes(config.read_bytes())
                external_before = external.read_bytes(); other_before = other.read_bytes()
                directory.rename(home / '.codex-saved'); directory.symlink_to(outside, target_is_directory=True)
                args = apply if operation == 'apply' else ['--rollback', '--backup', str(backup)]
                result = subprocess.run(command + args, capture_output=True, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0, 'symlink parent redirected a protected ' + operation)
                self.assertEqual(external.read_bytes(), external_before)
                self.assertEqual(other.read_bytes(), other_before, 'preflight must precede all changes')
                self.assertEqual(sorted(p.name for p in outside.iterdir()), [config.name], 'created lock outside selected scope')
                if operation == 'apply':
                    self.assertFalse(backup.exists())

    def test_parent_swapped_after_prepare_is_refused_without_partial_apply(self):
        with tempfile.TemporaryDirectory(prefix='gateway-parent-race-') as tmp:
            root = Path(tmp); directory = root / 'config'; directory.mkdir(); config = directory / 'file'
            config.write_text('original'); other = root / 'other'; other.write_text('original other')
            prepared = []
            for path in [other, config]:
                before, body = m.state(path)
                prepared.append((dict(path=str(path), format='text', text='installed'), before, body, b'installed'))
            outside = root / 'outside'; outside.mkdir(); external = outside / 'file'; external.write_text('original')
            with m.locks([other, config]):
                directory.rename(root / 'config-saved'); directory.symlink_to(outside, target_is_directory=True)
                with self.assertRaisesRegex(ValueError, 'parent|symlink|canonical'):
                    m.apply_locked(prepared, root / 'backup')
            self.assertEqual(external.read_text(), 'original')
            self.assertEqual(other.read_text(), 'original other')
            self.assertFalse((root / 'backup').exists())

    def test_legacy_rollback_records_still_readable(self):
        for version in [1, 2]:
            with self.subTest(version=version), tempfile.TemporaryDirectory(prefix='gateway-legacy-') as tmp:
                root = Path(tmp); path = root / 'config'; path.write_text('original'); path.chmod(0o640)
                before, body = m.state(path)
                backup = root / 'backup'; backup.mkdir(); (backup / '0.original').write_bytes(body)
                path.write_text('installed'); path.chmod(0o600)
                record = dict(path=str(path), before=before, after_sha256=m.digest(b'installed'), backup='0.original')
                (backup / 'rollback.json').write_text(json.dumps(dict(version=version, files=[record])))
                result = subprocess.run([sys.executable, '-I', str(SCRIPT), '--rollback', '--backup', str(backup)], capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(path.read_bytes(), body)
                self.assertEqual(path.stat().st_mode & 0o777, 0o640)

    def test_toml_preserves_auth_plugin_and_comments(self):
        text = '# retain comment\n[plugins."sample@local"]\nenabled = true\n[mcp_servers.old]\ncommand = "original"\nenv = { AUTH = "synthetic" }\n'
        text = m.toml_set(text, ['mcp_servers', 'old', 'enabled'], False)
        text = m.toml_set(text, ['plugins', 'sample@local', 'mcp_servers', 'old', 'enabled'], False)
        text = m.toml_set(text, ['mcp_servers', 'gateway', 'command'], '/absolute/binary')
        parsed = tomllib.loads(text)
        self.assertIn('# retain comment', text)
        self.assertEqual(parsed['mcp_servers']['old']['env'], dict(AUTH='synthetic'))
        self.assertTrue(parsed['plugins']['sample@local']['enabled'])
        self.assertFalse(parsed['plugins']['sample@local']['mcp_servers']['old']['enabled'])

    def test_preview_cas_apply_and_protected_rollback(self):
        with tempfile.TemporaryDirectory(prefix='gateway-migration-') as tmp:
            root = Path(tmp)
            config = root / 'config.json'
            original = dict(auth='synthetic-only', hooks=dict(retained=True))
            config.write_text(json.dumps(original))
            exe = root / 'native'
            exe.write_text('synthetic executable')
            launcher = root / 'launcher'
            launcher.symlink_to(exe)
            plan = root / 'plan.json'
            entries = [dict(path=str(config), expected_sha256=m.state(config)[0]['sha256'], format='json', changes=[dict(keys=['mcpServers', 'gateway'], value=dict(command='/absolute/gateway'))]), dict(path=str(launcher), expected_sha256=m.state(launcher)[0]['sha256'], replace_symlink=True, format='text', text='#!/bin/sh\nexec /native "$@"\n', mode=0o755)]
            plan.write_text(json.dumps(dict(version=1, files=entries)))
            raw = plan.read_bytes()

            def run(*args):
                return subprocess.run([sys.executable, str(SCRIPT), *map(str, args)], capture_output=True, text=True, timeout=10)

            preview = run('--plan', plan)
            self.assertEqual(preview.returncode, 0, preview.stderr)
            self.assertEqual(json.loads(config.read_text()), original)
            backup = root / 'backup'
            args = ['--plan', plan, '--apply', '--expected-sha256', m.digest(raw), '--backup', backup]
            config.write_text('concurrent edit')
            self.assertNotEqual(run(*args).returncode, 0)
            self.assertFalse(backup.exists())
            config.write_text(json.dumps(original))
            applied = run(*args)
            self.assertEqual(applied.returncode, 0, applied.stderr)
            changed = config.read_bytes()
            self.assertEqual(json.loads(changed)['auth'], original['auth'])
            self.assertFalse(launcher.is_symlink())
            config.write_text('later user change')
            rejected = run('--rollback', '--backup', backup)
            self.assertNotEqual(rejected.returncode, 0)
            self.assertEqual(config.read_text(), 'later user change')
            self.assertFalse(launcher.is_symlink(), 'rollback preflight must precede all changes')
            config.write_bytes(changed)
            restored = run('--rollback', '--backup', backup)
            self.assertEqual(restored.returncode, 0, restored.stderr)
            self.assertEqual(json.loads(config.read_text()), original)
            self.assertTrue(launcher.is_symlink())
            self.assertEqual(run('--rollback', '--backup', backup).returncode, 0)


if __name__ == '__main__':
    unittest.main()
