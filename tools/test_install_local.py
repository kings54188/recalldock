#!/usr/bin/env python3
"""Isolated release install/upgrade/recovery tests; no real host state."""
import hashlib
import importlib.util
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import tomllib
import unittest
from unittest import mock

SCRIPT = Path(__file__).with_name('install-local.py')
spec = importlib.util.spec_from_file_location('installer', SCRIPT)
i = importlib.util.module_from_spec(spec)
spec.loader.exec_module(i)


class InstallerTest(unittest.TestCase):
    def test_codex_managed_tools_get_native_approval_without_resetting_operator_policy(self):
        for policy in [None, 'prompt', 'writes']:
            with self.subTest(policy=policy), tempfile.TemporaryDirectory(prefix='recalldock-codex-native-policy-') as tmp:
                base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir()
                path = home / '.codex/config.toml'; path.parent.mkdir()
                original = '[mcp_servers.recalldock_gateway]\ncommand="/old/runtime"\nargs=["gateway","serve","--trust","strict"]\n'
                if policy is not None:
                    original += 'default_tools_approval_mode=' + json.dumps(policy) + '\n'
                path.write_text(original)
                plan = i.prepare(release, home, ['codex'])
                entry = next(e for e in plan['files'] if e['path'] == str(path))
                value = tomllib.loads(i.m.render(entry, original.encode()).decode())['mcp_servers']['recalldock_gateway']
                self.assertEqual(value['args'], ['gateway', 'serve', '--trust', 'strict'])
                if policy is None:
                    self.assertEqual(set(value['tools']), {'tools_find', 'tools_describe', 'tools_call', 'tools_release', 'work_find', 'work_read', 'work_checkpoint'})
                    self.assertTrue(all(v['approval_mode'] == 'approve' for v in value['tools'].values()))
                else:
                    self.assertEqual(value['default_tools_approval_mode'], policy)
                    self.assertNotIn('tools', value)
                self.assertEqual(path.read_text(), original)

    def test_local_publication_requires_matching_annotated_tag(self):
        spec = importlib.util.spec_from_file_location('release_builder', Path(__file__).with_name('build-release.py'))
        builder = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(builder)
        root = Path('/synthetic/source')
        with mock.patch.object(builder.subprocess, 'check_output', side_effect=['tag\n', 'abc\n']):
            self.assertEqual(builder.require_published_tag(root, '0.3.22', 'abc'), 'v0.3.22')
        for outputs in [['commit\n', 'abc\n'], ['tag\n', 'other\n']]:
            with self.subTest(outputs=outputs), mock.patch.object(builder.subprocess, 'check_output', side_effect=outputs):
                with self.assertRaisesRegex(ValueError, 'matching annotated tag'):
                    builder.require_published_tag(root, '0.3.22', 'abc')
        with mock.patch.object(builder.subprocess, 'check_output', side_effect=subprocess.CalledProcessError(1, ['git'])):
            with self.assertRaisesRegex(ValueError, 'matching annotated tag'):
                builder.require_published_tag(root, '0.3.22', 'abc')

    def test_gateway_change_during_prepare_is_not_overwritten(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-prepare-race-') as tmp:
            base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir()
            path = home / '.codex/config.toml'; path.parent.mkdir()
            original = '[mcp_servers.recalldock_gateway]\ncommand="/old/runtime"\nargs=["gateway","serve","--trust","strict"]\n'
            path.write_text(original)
            state = i.m.state
            changed = []
            def edit_between_reads(selected):
                before, body = state(selected)
                if selected == path and not changed:
                    path.write_text(original + '# later operator edit\n')
                    changed.append(True)
                return before, body
            with mock.patch.object(i.m, 'state', side_effect=edit_between_reads):
                with self.assertRaisesRegex(ValueError, 'changed while preparing'):
                    i.prepare(release, home, ['codex'])
            self.assertEqual(path.read_text(), original + '# later operator edit\n')
            self.assertFalse((home / '.local').exists())

    def test_existing_gateway_execution_configuration_is_not_reset(self):
        for host in ['codex', 'claude-code']:
            with self.subTest(host=host), tempfile.TemporaryDirectory(prefix='recalldock-preserve-trust-') as tmp:
                base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir()
                server = dict(command='/old/runtime', args=['gateway', 'serve', '--host', host, '--trust', 'strict', '--host-session', host + '-real', '--registry', '/selected/registry.json'], enabled=False, cwd='/selected/workspace', env={'OPTION': 'synthetic'})
                if host == 'codex':
                    path = home / '.codex/config.toml'; path.parent.mkdir()
                    original = '[mcp_servers.recalldock_gateway]\n' + ''.join(key + '=' + json.dumps(value) + '\n' for key, value in server.items() if key != 'env') + 'env={OPTION="synthetic"}\n'
                    path.write_text(original)
                else:
                    path = home / '.recalldock/claude-gateway.json'; path.parent.mkdir()
                    original = json.dumps({'mcpServers': {'recalldock_gateway': server, 'other': {'command': 'other'}}})
                    path.write_text(original)
                plan = i.prepare(release, home, [host])
                entry = next(e for e in plan['files'] if e['path'] == str(path))
                text = i.m.render(entry, original.encode()).decode()
                actual = tomllib.loads(text)['mcp_servers']['recalldock_gateway'] if host == 'codex' else json.loads(text)['mcpServers']['recalldock_gateway']
                self.assertEqual(actual, dict(server, command=str(release / 'runtime/recalldock')))
                self.assertEqual(path.read_text(), original)

    def test_existing_dsh_block_only_changes_runtime_command(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-dsh-trust-') as tmp:
            base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir()
            path = home / '.dsh/cordis.patch.yml'; path.parent.mkdir()
            original = '# outside\n# BEGIN RECALLDOCK GATEWAY\n- insert:\n  - id: mcp-recalldock-gateway\n    name: "@deepseek-ai/dsh-mcp-client"\n    config:\n      serverName: recalldock_gateway\n      transport: stdio\n      command: "/old/runtime"\n      args: [gateway, serve, --host, dsh, --trust, strict]\n      failOnStartupError: false\n# END RECALLDOCK GATEWAY\n# after\n'
            path.write_text(original)
            plan = i.prepare(release, home, ['dsh'])
            entry = next(e for e in plan['files'] if e['path'] == str(path))
            rendered = i.m.render(entry, original.encode()).decode()
            self.assertEqual(rendered, original.replace('"/old/runtime"', json.dumps(str(release / 'runtime/recalldock'))))

    def test_antigravity_selection_only_prepares_native_plugin_install(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-agy-selection-') as tmp:
            base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir()
            plugin = release / 'marketplace/plugins/antigravity'; plugin.mkdir(parents=True)
            (plugin / 'plugin.json').write_text(json.dumps({'name': 'recalldock', 'version': '0.3.21-test'}))
            manifest = json.loads((release / 'release.json').read_text())
            manifest['files']['marketplace/plugins/antigravity/plugin.json'] = hashlib.sha256((plugin / 'plugin.json').read_bytes()).hexdigest()
            (release / 'release.json').write_text(json.dumps(manifest))
            plan = i.prepare(release, home, ['antigravity'])
            self.assertEqual([e['path'] for e in plan['files']], [str(home / '.local/bin/recalldock')])
            self.assertTrue(any('agy plugin install' in note and str(plugin) in note for note in plan['notes']))
            self.assertFalse((home / '.gemini').exists())
            self.assertFalse((home / '.local').exists())

    def test_native_host_entrypoints_are_never_managed_by_recalldock_install(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-native-entrypoints-') as tmp:
            base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir()
            native = home / '.local/bin'; native.mkdir(parents=True)
            originals = {}
            for name in ('codex', 'claude', 'agy', 'dsh'):
                path = native / name
                path.write_text('#!/bin/sh\nprintf "%s\\n" "native-original"\n'); path.chmod(0o700)
                originals[name] = (path.read_bytes(), path.stat().st_mode)
            command = [sys.executable, str(SCRIPT), '--release', str(release), '--home', str(home), '--host', 'codex', '--host', 'claude-code', '--host', 'dsh']
            preview = subprocess.run(command, capture_output=True, text=True, check=True)
            plan = json.loads(preview.stdout)
            self.assertFalse(any(e['path'] == str(native / name) for e in plan['files'] for name in originals))
            subprocess.run(command + ['--apply', '--expected-sha256', plan['plan_sha256'], '--backup', str(base / 'backup')], capture_output=True, text=True, check=True)
            for name, before in originals.items():
                path = native / name
                self.assertEqual((path.read_bytes(), path.stat().st_mode), before)
            self.assertFalse((home / '.gemini').exists())

    def test_claude_wrapper_terminates_variadic_config_before_user_arguments(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-claude-argv-') as tmp:
            base = Path(tmp)
            release = self.release(base)
            home = base / 'home'; home.mkdir()
            plan = i.prepare(release, home, ['claude-code'])
            entry = next(e for e in plan['files'] if e['path'].endswith('/recalldock-claude'))
            wrapper = base / 'wrapper'; wrapper.write_bytes(i.m.render(entry, None)); wrapper.chmod(0o755)
            fake = base / 'claude'; fake.write_text('#!/bin/sh\nfor arg do printf \'%s\\n\' "$arg"; done\n'); fake.chmod(0o755)
            result = subprocess.run([str(wrapper), 'mcp', 'get', 'recalldock_gateway'], env={'PATH': str(base)}, capture_output=True, text=True, check=True)
            self.assertEqual(result.stdout.splitlines(), ['--mcp-config', str(home / '.recalldock/claude-gateway.json'), '--strict-mcp-config', 'mcp', 'get', 'recalldock_gateway'])

    def test_unknown_or_malformed_policy_is_never_enabled(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-policy-reject-') as tmp:
            base=Path(tmp);release=self.release(base);home=base/'home';home.mkdir();data=base/'data';data.mkdir()
            policy=data/'candidate-admission.json'
            for raw in ['{"mode":"manual"}', '{"version":null,"mode":"checked"}', '{"mode":"checked","unknown":true}', '{"version":3,"mode":"checked"}', 'malformed']:
                with self.subTest(raw=raw):
                    policy.write_text(raw)
                    plan=i.prepare(release,home,[],data)
                    self.assertFalse(any(e['path']==str(policy) for e in plan['files']))
                    self.assertEqual(policy.read_text(),raw)

    def test_existing_automatic_policy_is_hardened_first_and_retained(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-policy-install-') as tmp:
            base=Path(tmp);release=self.release(base);home=base/'home';home.mkdir();data=base/'custom-data';data.mkdir()
            policy=data/'candidate-admission.json';original=json.dumps(dict(mode='checked'));policy.write_text(original)
            command=[sys.executable,str(SCRIPT),'--release',str(release),'--home',str(home),'--data-dir',str(data),'--host','codex']
            def run(*args):return subprocess.run(command+list(map(str,args)),capture_output=True,text=True,timeout=10)
            preview=run();self.assertEqual(preview.returncode,0,preview.stderr);plan=json.loads(preview.stdout)
            self.assertEqual(plan['files'][0]['path'],str(policy))
            self.assertTrue(plan['files'][0]['retain_on_rollback'])
            self.assertEqual(policy.read_text(),original)
            backup=base/'backup';applied=run('--apply','--expected-sha256',plan['plan_sha256'],'--backup',backup);self.assertEqual(applied.returncode,0,applied.stderr)
            self.assertEqual(json.loads(policy.read_text()),dict(mode='checked-v2',version=2))
            self.assertEqual(json.loads(run().stdout)['files'],[])
            rollback=subprocess.run([sys.executable,str(SCRIPT),'--rollback','--backup',str(backup)],capture_output=True,text=True,timeout=10)
            self.assertEqual(rollback.returncode,0,rollback.stderr)
            self.assertEqual(json.loads(rollback.stdout)['retained'],1)
            self.assertEqual(json.loads(policy.read_text()),dict(mode='checked-v2',version=2))
            self.assertFalse((home/'.local/bin/recalldock').exists())
            self.assertFalse((data/'recalldock.db').exists(),'configuration migration opened knowledge')

    def test_tiered_evidence_policy_is_hardened_without_enabling_unknown_formats(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-tiered-upgrade-') as tmp:
            base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir(); data = base / 'data'; data.mkdir()
            policy = data / 'candidate-admission.json'
            original = json.dumps(dict(version=3, mode='tiered-v3'))
            policy.write_text(original)
            command = [sys.executable, str(SCRIPT), '--release', str(release), '--home', str(home), '--data-dir', str(data)]
            preview = subprocess.run(command, capture_output=True, text=True, check=True)
            plan = json.loads(preview.stdout)
            self.assertEqual(plan['files'][0]['path'], str(policy))
            self.assertTrue(plan['files'][0]['retain_on_rollback'])
            self.assertEqual(policy.read_text(), original)
            backup = base / 'backup'
            subprocess.run(command + ['--apply', '--expected-sha256', plan['plan_sha256'], '--backup', str(backup)], capture_output=True, text=True, check=True)
            self.assertEqual(json.loads(policy.read_text()), dict(version=4, mode='tiered-v4'))
            subprocess.run([sys.executable, str(SCRIPT), '--rollback', '--backup', str(backup)], capture_output=True, text=True, check=True)
            self.assertEqual(json.loads(policy.read_text()), dict(version=4, mode='tiered-v4'))
            self.assertFalse((data / 'recalldock.db').exists())
            for obj in [dict(version=3, mode='tiered-v3', approval=True), dict(version=4, mode='tiered-v4'), dict(version=3, mode='unknown')]:
                policy.write_text(json.dumps(obj))
                plan = i.prepare(release, home, [], data)
                self.assertFalse(any(e['path'] == str(policy) for e in plan['files']))
                self.assertEqual(json.loads(policy.read_text()), obj)

    def release(self, base):
        root = base / 'release'
        (root / 'runtime').mkdir(parents=True)
        (root / 'tools').mkdir()
        binary = root / 'runtime/recalldock'
        binary.write_text('#!/bin/sh\nexit 0\n')
        binary.chmod(0o755)
        for name in ['install-local.py', 'gateway-migrate.py']:
            shutil.copy2(SCRIPT.parent / name, root / 'tools' / name)
        files = {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest() for p in root.rglob('*') if p.is_file()}
        (root / 'release.json').write_text(json.dumps(dict(format=1, version='0.3.0-test', files=files)))
        return root

    def test_preview_apply_idempotence_and_guarded_rollback(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-install-') as tmp:
            base = Path(tmp)
            release = self.release(base)
            home = base / 'home'; home.mkdir()
            (home / '.codex').mkdir()
            config = home / '.codex/config.toml'
            original = '# preserve comment\n[mcp_servers.other]\ncommand="original"\nenv={TOKEN="synthetic"}\n'
            config.write_text(original)
            command = [sys.executable, str(SCRIPT), '--release', str(release), '--home', str(home)]
            for host in ['codex', 'claude-code', 'dsh']:
                command += ['--host', host]
            def run(*args):
                return subprocess.run(command + list(map(str, args)), capture_output=True, text=True, timeout=10)
            preview = run();self.assertEqual(preview.returncode, 0, preview.stderr)
            digest = json.loads(preview.stdout)['plan_sha256']
            self.assertEqual(config.read_text(), original)
            self.assertFalse((home / '.local').exists())
            backup = base / 'backup'
            config.write_text(original + '# later\n')
            self.assertNotEqual(run('--apply', '--expected-sha256', digest, '--backup', backup).returncode, 0)
            self.assertFalse(backup.exists())
            config.write_text(original)
            applied = run('--apply', '--expected-sha256', digest, '--backup', backup)
            self.assertEqual(applied.returncode, 0, applied.stderr)
            current = config.read_bytes()
            parsed = tomllib.loads(current.decode())
            self.assertEqual(parsed['mcp_servers']['other']['env'], dict(TOKEN='synthetic'))
            self.assertIn('# preserve comment', current.decode())
            self.assertEqual(json.loads(run().stdout)['files'], [])
            self.assertFalse((home / '.recalldock/recalldock.db').exists())
            rollback = [sys.executable, str(SCRIPT), '--rollback', '--backup', str(backup)]
            config.write_text('later edit')
            self.assertNotEqual(subprocess.run(rollback, capture_output=True).returncode, 0)
            self.assertTrue((home / '.local/bin/recalldock').exists())
            config.write_bytes(current)
            restored = subprocess.run(rollback, capture_output=True, text=True)
            self.assertEqual(restored.returncode, 0, restored.stderr)
            self.assertEqual(config.read_text(), original)
            self.assertFalse((home / '.local/bin/recalldock').exists())

    def test_mode_only_install_preview_cas_and_rollback(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-install-mode-') as tmp:
            base = Path(tmp); release = self.release(base); home = base / 'home'; home.mkdir()
            launcher = home / '.local/bin/recalldock'; launcher.parent.mkdir(parents=True)
            original = '#!/bin/sh\nexec ' + str(release / 'runtime/recalldock') + ' "$@"\n'
            launcher.write_text(original); launcher.chmod(0o600)
            command = [sys.executable, '-I', str(SCRIPT), '--release', str(release), '--home', str(home)]
            def run(*args):
                return subprocess.run(command + list(map(str, args)), capture_output=True, text=True, timeout=10)
            preview = run(); self.assertEqual(preview.returncode, 0, preview.stderr)
            plan = json.loads(preview.stdout)
            self.assertEqual(len(plan['files']), 1, 'non-executable launcher was reported as installed')
            self.assertEqual(plan['files'][0]['before_mode'], 0o600)
            self.assertEqual(plan['files'][0]['after_mode'], 0o755)
            backup = base / 'backup'
            launcher.chmod(0o640)
            self.assertNotEqual(run('--apply', '--expected-sha256', plan['plan_sha256'], '--backup', backup).returncode, 0)
            self.assertFalse(backup.exists())
            launcher.chmod(0o600)
            applied = run('--apply', '--expected-sha256', plan['plan_sha256'], '--backup', backup)
            self.assertEqual(applied.returncode, 0, applied.stderr)
            self.assertEqual(launcher.stat().st_mode & 0o777, 0o755)
            self.assertEqual(launcher.read_text(), original)
            self.assertEqual(json.loads(run().stdout)['files'], [])
            rollback = [sys.executable, '-I', str(SCRIPT), '--rollback', '--backup', str(backup)]
            launcher.chmod(0o700)
            self.assertNotEqual(subprocess.run(rollback, capture_output=True).returncode, 0)
            self.assertEqual(launcher.stat().st_mode & 0o777, 0o700)
            launcher.chmod(0o755)
            restored = subprocess.run(rollback, capture_output=True, text=True)
            self.assertEqual(restored.returncode, 0, restored.stderr)
            self.assertEqual(launcher.stat().st_mode & 0o777, 0o600)
            self.assertEqual(launcher.read_text(), original)
            self.assertEqual(subprocess.run(rollback, capture_output=True).returncode, 0)

    def test_integrity_and_unmanaged_dsh_and_parent_symlink(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-install-') as tmp:
            base = Path(tmp);release = self.release(base);home = base / 'home';home.mkdir()
            (home / '.dsh').mkdir()
            (home / '.dsh/cordis.patch.yml').write_text('- id: mcp-recalldock-gateway\n')
            with self.assertRaisesRegex(ValueError, 'unmanaged'):
                i.prepare(release, home, ['dsh'])
            (release / 'runtime/recalldock').write_text('tampered')
            with self.assertRaisesRegex(ValueError, 'integrity'):
                i.prepare(release, home, [])
        with tempfile.TemporaryDirectory(prefix='recalldock-install-') as tmp:
            base=Path(tmp);release=self.release(base);home=base/'home';home.mkdir()
            outside=base/'outside';outside.mkdir();(home/'.local').symlink_to(outside)
            with self.assertRaisesRegex(ValueError,'symlink'):
                i.prepare(release,home,[])

    def test_interactive_install_requires_tty_and_confirms_before_apply(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-interactive-') as tmp:
            base = Path(tmp)
            release = self.release(base)
            home = base / 'home'
            home.mkdir()
            (home / '.codex').mkdir()
            codex_cfg = home / '.codex/config.toml'
            original = '[mcp_servers.other]\ncommand = "other"\n'
            codex_cfg.write_text(original)

            # Non-TTY subprocess must refuse --interactive fail-closed without changing files.
            non_tty = subprocess.run(
                [sys.executable, str(SCRIPT), '--release', str(release), '--home', str(home), '--interactive'],
                input='y\n', capture_output=True, text=True,
            )
            self.assertNotEqual(non_tty.returncode, 0)
            self.assertIn('interactive', non_tty.stderr.lower())
            self.assertEqual(codex_cfg.read_text(), original)

            # Declining confirmation ('n') leaves all files untouched.
            with mock.patch.object(sys, 'argv', [str(SCRIPT), '--release', str(release), '--home', str(home), '--interactive']), \
                 mock.patch.object(sys.stdin, 'isatty', return_value=True), \
                 mock.patch('builtins.input', return_value='n'):
                with self.assertRaises(SystemExit) as cm:
                    i.main()
                self.assertEqual(cm.exception.code, 1)
            self.assertEqual(codex_cfg.read_text(), original)
            self.assertFalse((home / '.local/bin/recalldock').exists())

            # Accepting confirmation ('y') runs locked digest-CAS install + automatic backup + rollback works.
            with mock.patch.object(sys, 'argv', [str(SCRIPT), '--release', str(release), '--home', str(home), '--interactive']), \
                 mock.patch.object(sys.stdin, 'isatty', return_value=True), \
                 mock.patch('builtins.input', return_value='y'):
                i.main()
            self.assertTrue((home / '.local/bin/recalldock').is_file())
            self.assertIn('recalldock_gateway', codex_cfg.read_text())
            backups = list((home / '.local/state/recalldock/backups').iterdir())
            self.assertEqual(len(backups), 1)
            rollback = subprocess.run(
                [sys.executable, str(SCRIPT), '--rollback', '--backup', str(backups[0])],
                capture_output=True, text=True, check=True,
            )
            self.assertEqual(codex_cfg.read_text(), original)
            self.assertFalse((home / '.local/bin/recalldock').exists())

    def test_release_and_home_default_when_inside_extracted_release(self):
        with tempfile.TemporaryDirectory(prefix='recalldock-rel-default-') as tmp:
            base = Path(tmp)
            release = self.release(base)
            home = base / 'home'
            home.mkdir()
            script_in_rel = release / 'tools/install-local.py'
            res = subprocess.run(
                [sys.executable, str(script_in_rel)],
                env={'HOME': str(home), 'PATH': '/usr/bin:/bin'},
                capture_output=True, text=True, check=True,
            )
            plan = json.loads(res.stdout)
            self.assertEqual(plan['release_version'], '0.3.0-test')
            self.assertEqual(plan['files'][0]['path'], str(home / '.local/bin/recalldock'))


if __name__ == '__main__':
    unittest.main()
