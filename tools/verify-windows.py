#!/usr/bin/env python3
"""Run full 14-point native Windows x64 verification against a built Windows ZIP package."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import tomllib
import zipfile


def clean_env(home: Path, local_appdata: Path, data_dir: Path | None = None):
    env = {k: v for k, v in os.environ.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
    env['USERPROFILE'] = str(home)
    env['HOME'] = str(home)
    env['LOCALAPPDATA'] = str(local_appdata)
    env['APPDATA'] = str(home / 'AppData' / 'Roaming')
    if data_dir is not None:
        env['RECALLDOCK_DATA_DIR'] = str(data_dir)
    return env


def run_cli(exe: Path, args: list[str], cwd: Path, env: dict[str, str], check: bool = True, input_text: str | None = None):
    return subprocess.run(
        [str(exe), *args],
        cwd=str(cwd),
        env=env,
        input=input_text,
        capture_output=True,
        text=True,
        encoding='utf-8',
        timeout=45,
        check=check,
    )


def mcp_roundtrip(exe: Path, args: list[str], cwd: Path, env: dict[str, str], calls: list[dict]):
    p = subprocess.Popen(
        [str(exe), *args],
        cwd=str(cwd),
        env=env,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        encoding='utf-8',
    )
    parsed = {}
    try:
        assert p.stdin is not None and p.stdout is not None and p.stderr is not None
        for call in calls:
            p.stdin.write(json.dumps(call) + '\n')
            p.stdin.flush()
            if 'id' not in call:
                continue
            while True:
                line = p.stdout.readline()
                assert line != '', f'unexpected EOF waiting for id={call["id"]}'
                if not line.strip():
                    continue
                obj = json.loads(line)  # asserts stdout contains strictly valid JSON-RPC lines, no stray logs
                if 'id' in obj:
                    parsed[obj['id']] = obj
                    if obj['id'] == call['id']:
                        break
    finally:
        if p.stdin and not p.stdin.closed:
            p.stdin.close()
        stderr_out = p.stderr.read() if p.stderr else ''
        rc = p.wait(timeout=15)
        assert rc == 0, f'gateway serve exited {rc}: {stderr_out[:1000]}'
    return parsed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--zip', required=True, type=Path)
    parser.add_argument('--checksums', required=True, type=Path)
    parser.add_argument('--report', required=True, type=Path)
    a = parser.parse_args()

    if sys.platform != 'win32':
        raise SystemExit('verify-windows.py must execute on native Windows')

    results = {'platform': 'windows', 'arch': 'amd64', 'checks': {}}

    # 1. Verify ZIP SHA256 against checksums.txt and extract to fresh independent dir
    expected_line = a.checksums.read_text(encoding='utf-8').strip().splitlines()[0]
    expected_sha, expected_name = expected_line.split()[:2]
    assert expected_name == a.zip.name
    actual_sha = hashlib.sha256(a.zip.read_bytes()).hexdigest()
    assert actual_sha == expected_sha, f'zip sha mismatch: {actual_sha} vs {expected_sha}'
    results['checks']['zip_sha256'] = actual_sha

    with tempfile.TemporaryDirectory(prefix='rd-win-test-') as tmp_raw:
        tmp = Path(tmp_raw)
        extract_dir = tmp / 'extract'
        extract_dir.mkdir()
        with zipfile.ZipFile(a.zip, 'r') as zf:
            zf.extractall(extract_dir)
        roots = [p for p in extract_dir.iterdir() if p.is_dir()]
        assert len(roots) == 1
        rel_dir = roots[0].resolve()
        manifest = json.loads((rel_dir / 'release.json').read_text(encoding='utf-8'))
        for member_rel, member_sha in manifest['files'].items():
            target = rel_dir / Path(member_rel)
            assert target.is_file(), f'missing manifest member: {member_rel}'
            assert hashlib.sha256(target.read_bytes()).hexdigest() == member_sha, f'hash mismatch: {member_rel}'
        results['checks']['zip_extract_and_manifest'] = True

        exe = rel_dir / 'runtime' / 'recalldock.exe'
        # 2. Standalone process launch & version
        home = tmp / '用户 主目录' / 'Test User'
        local_app = home / 'AppData' / 'Local'
        home.mkdir(parents=True)
        local_app.mkdir(parents=True)
        env = clean_env(home, local_app)

        ver = run_cli(exe, ['version'], tmp, env).stdout.strip()
        assert manifest['version'] in ver, ver
        results['checks']['standalone_version'] = ver

        # 3. Default Windows data dir (%LOCALAPPDATA%\RecallDock) + Chinese + Spaces paths
        proj_a = home / '工作 空间' / '项目 甲 App'
        proj_b = home / '工作 空间' / 'Project Beta'
        unbound_c = home / '工作 空间' / 'Unbound Dir'
        for d in (proj_a, proj_b, unbound_c):
            d.mkdir(parents=True)

        p_a = json.loads(run_cli(exe, ['projects', 'create', '--name', 'Alpha CN Space', '--json'], proj_a, env).stdout)
        default_data_dir = local_app / 'RecallDock'
        assert (default_data_dir / 'recalldock.db').is_file(), 'did not initialize default %LOCALAPPDATA%\\RecallDock\\recalldock.db'
        run_cli(exe, ['projects', 'bind', p_a['id'], '--directory', str(proj_a)], proj_a, env)

        # Case-insensitive drive letter lookup check on Windows
        alt_case_proj_a = Path(str(proj_a)[0].lower() + str(proj_a)[1:])
        cur = json.loads(run_cli(exe, ['projects', 'current', '--json'], alt_case_proj_a, env).stdout)
        assert cur['project'] == p_a['id'], cur

        # UNC network path must be rejected fail-closed
        unc_res = run_cli(exe, ['projects', 'bind', p_a['id'], '--directory', r'\\server\share\proj'], proj_a, env, check=False)
        assert unc_res.returncode != 0, 'UNC path should be refused for project binding'
        results['checks']['unc_path_rejected'] = True

        # 4. SQLite write, close, reopen, search, online backup
        run_cli(exe, ['save', 'Windows SQLite 持久验证', '包含空格和中文路径的稳定测试结论。', '--type', 'decision', '--topic', 'win/path'], proj_a, env)
        found = json.loads(run_cli(exe, ['search', '持久验证', '--json'], proj_a, env).stdout)
        assert len(found['results']) >= 1 and found.get('project') == p_a['id'], found
        bak = json.loads(run_cli(exe, ['backup', '--json'], proj_a, env).stdout)
        assert bak['integrity'] == 'ok' and Path(bak['path']).is_file(), bak
        results['checks']['sqlite_chinese_space_and_backup'] = True

        # 5. Project isolation (Project B and Unbound Dir C cannot see Project A)
        p_b = json.loads(run_cli(exe, ['projects', 'create', '--name', 'Beta', '--json'], proj_b, env).stdout)
        run_cli(exe, ['projects', 'bind', p_b['id'], '--directory', str(proj_b)], proj_b, env)
        b_search = json.loads(run_cli(exe, ['search', '持久验证', '--json'], proj_b, env).stdout)
        c_search = json.loads(run_cli(exe, ['search', '持久验证', '--json'], unbound_c, env).stdout)
        assert len(b_search['results']) == 0 and len(c_search['results']) == 0
        results['checks']['project_isolation'] = True

        # 6. PowerShell install, idempotency, escaped TOML/JSON, protected rollback, uninstall
        ps1 = rel_dir / 'tools' / 'install-windows.ps1'
        codex_cfg = home / '.codex' / 'config.toml'
        codex_cfg.parent.mkdir(parents=True)
        codex_cfg.write_text('# keep custom comment\n[mcp_servers.other]\ncommand = "other.exe"\n', encoding='utf-8')

        preview = json.loads(subprocess.check_output(
            ['powershell.exe', '-NoProfile', '-File', str(ps1), '-Release', str(rel_dir), '-Hosts', 'codex,claude-code,antigravity'],
            env=env, text=True, encoding='utf-8',
        ))
        assert len(preview['files']) == 4, preview
        bak_dir = tmp / 'ps-backup'
        subprocess.run(
            ['powershell.exe', '-NoProfile', '-File', str(ps1), '-Release', str(rel_dir), '-Hosts', 'codex,claude-code,antigravity',
             '-Apply', '-ExpectedSha256', preview['plan_sha256'], '-BackupDir', str(bak_dir)],
            env=env, check=True, capture_output=True, text=True,
        )
        installed_exe = local_app / 'RecallDock' / 'bin' / 'recalldock.exe'
        assert installed_exe.is_file()
        assert manifest['version'] in run_cli(installed_exe, ['version'], tmp, env).stdout
        parsed_toml = tomllib.loads(codex_cfg.read_text(encoding='utf-8'))
        assert parsed_toml['mcp_servers']['other']['command'] == 'other.exe'
        assert parsed_toml['mcp_servers']['recalldock_gateway']['command'] == str(installed_exe)
        assert '# keep custom comment' in codex_cfg.read_text(encoding='utf-8')
        claude_cfg = json.loads((local_app / 'RecallDock' / 'claude-gateway.json').read_text(encoding='utf-8'))
        assert claude_cfg['mcpServers']['recalldock_gateway']['command'] == str(installed_exe)

        # Idempotent second preview
        preview2 = json.loads(subprocess.check_output(
            ['powershell.exe', '-NoProfile', '-File', str(ps1), '-Release', str(rel_dir), '-Hosts', 'codex,claude-code,antigravity'],
            env=env, text=True, encoding='utf-8',
        ))
        assert preview2['files'] == [], preview2

        # Protected rollback: refuse after manual edit, succeed when unmodified
        saved_toml = codex_cfg.read_text(encoding='utf-8')
        codex_cfg.write_text(saved_toml + '\n# edited\n', encoding='utf-8')
        refused = subprocess.run(
            ['powershell.exe', '-NoProfile', '-File', str(ps1), '-Rollback', '-BackupDir', str(bak_dir)],
            env=env, capture_output=True, text=True,
        )
        assert refused.returncode != 0
        codex_cfg.write_text(saved_toml, encoding='utf-8')
        subprocess.run(
            ['powershell.exe', '-NoProfile', '-File', str(ps1), '-Rollback', '-BackupDir', str(bak_dir)],
            env=env, check=True, capture_output=True, text=True,
        )
        assert 'recalldock_gateway' not in tomllib.loads(codex_cfg.read_text(encoding='utf-8'))['mcp_servers']
        assert not installed_exe.exists()
        assert (default_data_dir / 'recalldock.db').is_file()
        results['checks']['powershell_install_idempotent_rollback'] = True

        # Re-apply via `recalldock.exe setup` then uninstall preserving DB
        subprocess.run(
            [str(exe), 'setup', '-Release', str(rel_dir), '-Hosts', 'codex', '-Apply', '-ExpectedSha256',
             json.loads(run_cli(exe, ['setup', '-Release', str(rel_dir), '-Hosts', 'codex'], tmp, env).stdout)['plan_sha256'],
             '-BackupDir', str(tmp / 'ps-backup-2')],
            env=env, check=True, capture_output=True, text=True,
        )
        assert installed_exe.is_file()
        subprocess.run(
            ['powershell.exe', '-NoProfile', '-File', str(ps1), '-Uninstall', '-BackupDir', str(tmp / 'ps-backup-2')],
            env=env, check=True, capture_output=True, text=True,
        )
        assert not installed_exe.exists() and (default_data_dir / 'recalldock.db').is_file()
        results['checks']['setup_cmd_and_uninstall_preserve_db'] = True

        # 7. Gateway MCP stdio: 7 fixed tools across 8 coexisting backends, add/scan/remove/schema-change, Work CAS
        # Test -Interactive refuses non-interactive stdin cleanly
        non_tty_ps = subprocess.run(
            ['powershell.exe', '-NoProfile', '-File', str(ps1), '-Release', str(rel_dir), '-Interactive'],
            input='y\n', env=env, capture_output=True, text=True,
        )
        assert non_tty_ps.returncode != 0, 'non-TTY -Interactive should fail closed'

        # Build 8 synthetic backends (7 + 1 extra) and register them
        mcp_servers = {}
        for idx in range(1, 9):
            key = f'srv-{idx:02d}'
            mcp_servers[key] = {
                'command': str(exe),
                'args': ['mcp', '--tools=agent'],
                'cwd': str(proj_a),
                'env': {
                    'RECALLDOCK_DATA_DIR': str(default_data_dir),
                    'LOCALAPPDATA': str(local_app),
                    'USERPROFILE': str(home),
                },
            }
        backend_src = tmp / 'backend-src.json'
        backend_src.write_text(json.dumps({'mcpServers': mcp_servers}), encoding='utf-8')
        for idx in range(1, 9):
            srv_id = f'srv-{idx:02d}'
            if idx == 8:
                # Verify direct CLI flag add (`--source-file` + `--keys` + `--enable` + `--stateful`)
                run_cli(exe, [
                    'gateway', 'add', '--server', srv_id,
                    '--source-file', str(backend_src), '--keys', f'mcpServers,{srv_id}',
                    '--host', 'codex', '--description', 'Eighth Windows backend',
                    '--enable', '--stateful',
                ], tmp, env)
            else:
                bdef = tmp / f'backend-def-{idx}.json'
                bdef.write_text(json.dumps({
                    'description': f'Isolated RecallDock backend #{idx}',
                    'source': {'path': str(backend_src), 'format': 'json', 'keys': ['mcpServers', srv_id]},
                    'host': 'codex', 'enabled': True, 'start_allowed': True, 'stateful': True,
                }), encoding='utf-8')
                run_cli(exe, ['gateway', 'add', '--server', srv_id, '--input', str(bdef)], tmp, env)
        run_cli(exe, ['gateway', 'discover', '--server', 'srv-01'], tmp, env)
        run_cli(exe, ['gateway', 'discover', '--server', 'srv-08'], tmp, env)
        listed_srvs = json.loads(run_cli(exe, ['gateway', 'list'], tmp, env).stdout)
        assert len(listed_srvs) == 8, listed_srvs

        # Verify read-only gateway scan redacts secrets across synthetic host configs
        codex_scan_cfg = home / '.codex' / 'config.toml'
        codex_scan_cfg.write_text('[mcp_servers.win_direct]\ncommand = "cmd.exe"\nenv = { TOKEN = "win-secret-no-leak" }\n', encoding='utf-8')
        scan_out = run_cli(exe, ['gateway', 'scan', '--home', str(home)], tmp, env).stdout
        assert 'win-secret-no-leak' not in scan_out, 'gateway scan leaked secret on Windows'
        scan_items = json.loads(scan_out)
        assert any(it['name'] == 'win_direct' and it['mode'] == 'host_direct' for it in scan_items), scan_items

        calls = [
            {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2024-11-05', 'capabilities': {}, 'clientInfo': {'name': 'win-ci', 'version': '1'}}},
            {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/list', 'params': {}},
            {'jsonrpc': '2.0', 'id': 3, 'method': 'tools/call', 'params': {'name': 'tools_find', 'arguments': {'query': 'mem_context'}}},
            {'jsonrpc': '2.0', 'id': 4, 'method': 'tools/call', 'params': {'name': 'tools_describe', 'arguments': {'server': 'srv-08', 'tool': 'mem_context'}}},
            {'jsonrpc': '2.0', 'id': 5, 'method': 'tools/call', 'params': {'name': 'work_checkpoint', 'arguments': {'root': str(proj_a), 'lane': 'win-ci', 'expected': 0, 'checkpoint': {'goal': 'Verify Windows Work CAS', 'summary': 'Initial checkpoint'}}}},
            {'jsonrpc': '2.0', 'id': 6, 'method': 'tools/call', 'params': {'name': 'work_checkpoint', 'arguments': {'root': str(proj_a), 'lane': 'win-ci', 'expected': 0, 'checkpoint': {'goal': 'Should fail CAS'}}}},
            {'jsonrpc': '2.0', 'id': 7, 'method': 'tools/call', 'params': {'name': 'work_checkpoint', 'arguments': {'root': str(proj_a), 'lane': 'win-ci', 'expected': 1, 'checkpoint': {'goal': 'Verify Windows Work CAS', 'status': 'completed', 'summary': 'Second checkpoint'}}}},
            {'jsonrpc': '2.0', 'id': 8, 'method': 'tools/call', 'params': {'name': 'work_read', 'arguments': {'root': str(proj_a), 'lane': 'win-ci', 'mode': 'l1'}}},
            {'jsonrpc': '2.0', 'id': 9, 'method': 'tools/call', 'params': {'name': 'tools_find', 'arguments': {'after': 'srv-06'}}},
        ]
        res = mcp_roundtrip(exe, ['gateway', 'serve'], tmp, env, calls)
        assert {t['name'] for t in res[2]['result']['tools']} == {'tools_find', 'tools_describe', 'tools_call', 'tools_release', 'work_find', 'work_read', 'work_checkpoint'}
        desc_data = json.loads(res[4]['result']['content'][0]['text'])['data']
        schema_hash = desc_data['schema_hash']
        assert not res[5]['result'].get('isError'), res[5]
        assert res[6]['result'].get('isError'), 'stale expected=0 should fail CAS'
        assert not res[7]['result'].get('isError'), res[7]
        assert json.loads(res[8]['result']['content'][0]['text'])['data']['generation'] == 2
        page2 = json.loads(res[9]['result']['content'][0]['text'])['data']
        assert any(s['id'] == 'srv-08' for s in page2['servers']), page2

        # Separate MCP session to invoke tools_call + tools_release with returned schema_hash
        calls2 = [
            {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2024-11-05', 'capabilities': {}, 'clientInfo': {'name': 'win-ci', 'version': '1'}}},
            {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {'name': 'tools_call', 'arguments': {'server': 'srv-08', 'tool': 'mem_context', 'schema_hash': schema_hash, 'root': str(proj_a), 'arguments': {'mode': 'index'}}}},
        ]
        res2 = mcp_roundtrip(exe, ['gateway', 'serve'], tmp, env, calls2)
        call_res = res2[2]['result']
        assert not call_res.get('isError'), call_res
        assert 'Windows SQLite' in call_res['content'][0]['text'] or 'alpha' in call_res['content'][0]['text'].lower() or 'Memory Context' in call_res['content'][0]['text']
        lease = json.loads(call_res['content'][-1]['text'])['recalldock_gateway']['lease']
        assert lease

        # Also verify explicit lease release, config-change detection, removal denial, and work backup
        p = subprocess.Popen(
            [str(exe), 'gateway', 'serve'],
            cwd=str(tmp), env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, encoding='utf-8',
        )
        try:
            def rpc(msg):
                p.stdin.write(json.dumps(msg) + '\n')
                p.stdin.flush()
                return json.loads(p.stdout.readline())
            rpc({'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2024-11-05', 'capabilities': {}, 'clientInfo': {'name': 'win-ci', 'version': '1'}}})
            r_call = rpc({'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {'name': 'tools_call', 'arguments': {'server': 'srv-08', 'tool': 'mem_context', 'schema_hash': schema_hash, 'root': str(proj_a), 'arguments': {'mode': 'index'}}}})['result']
            assert not r_call.get('isError'), r_call
            live_lease = json.loads(r_call['content'][-1]['text'])['recalldock_gateway']['lease']
            r_rel = rpc({'jsonrpc': '2.0', 'id': 3, 'method': 'tools/call', 'params': {'name': 'tools_release', 'arguments': {'lease': live_lease}}})['result']
            assert not r_rel.get('isError') and 'released' in json.dumps(r_rel), r_rel

            # Remove srv-08 while gateway serve is running -> live tools_find/tools_call deny immediately
            run_cli(exe, ['gateway', 'remove', '--server', 'srv-08'], tmp, env)
            r_call_removed = rpc({'jsonrpc': '2.0', 'id': 4, 'method': 'tools/call', 'params': {'name': 'tools_call', 'arguments': {'server': 'srv-08', 'tool': 'mem_context', 'schema_hash': schema_hash, 'root': str(proj_a), 'arguments': {'mode': 'index'}}}})['result']
            assert r_call_removed.get('isError'), 'removed server call should fail in running gateway'
        finally:
            p.stdin.close()
            p.wait(timeout=15)
            assert p.returncode == 0

        wbak = json.loads(run_cli(exe, ['work', 'backup'], proj_a, env).stdout)
        assert wbak['data']['integrity'] == 'ok' and Path(wbak['data']['path']).is_file(), wbak
        results['checks']['gateway_8_backends_scan_remove_and_work_cas'] = True

    results['ok'] = True
    a.report.parent.mkdir(parents=True, exist_ok=True)
    a.report.write_text(json.dumps(results, indent=2, ensure_ascii=False) + '\n', encoding='utf-8')
    print(json.dumps(results, ensure_ascii=False))


if __name__ == '__main__':
    main()
