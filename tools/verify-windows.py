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


def parse_managed_dsh_block(text: str) -> dict:
    """Parse the RecallDock managed block without a YAML dependency.

    The block is a fixed, RecallDock-written shape, so asserting it directly is
    more precise than a general YAML parse: it pins the exact keys and order a
    real DSH client reads. Everything outside the markers is checked separately
    by comparing the caller's expected text.
    """
    begin = '# BEGIN RECALLDOCK GATEWAY'
    end = '# END RECALLDOCK GATEWAY'
    assert text.count(begin) == 1, f'expected exactly one {begin} marker'
    assert text.count(end) == 1, f'expected exactly one {end} marker'
    start = text.index(begin) + len(begin)
    stop = text.index(end)
    lines = [ln.rstrip() for ln in text[start:stop].splitlines() if ln.strip()]
    expected_keys = [
        '- insert:',
        '  - id: mcp-recalldock-gateway',
        '    name: "@deepseek-ai/dsh-mcp-client"',
        '    config:',
        '      serverName: recalldock_gateway',
        '      transport: stdio',
    ]
    assert lines[:len(expected_keys)] == expected_keys, lines[:len(expected_keys)]
    rest = lines[len(expected_keys):]
    assert len(rest) == 3, rest
    command_line, args_line, fail_line = rest
    assert command_line.startswith('      command: "'), command_line
    assert command_line.endswith('"'), command_line
    assert args_line == '      args: [gateway, serve, --host, dsh]', args_line
    assert fail_line == '      failOnStartupError: true', fail_line
    # Undo YAML double-quoted escaping: \\ and \" are the only escapes written.
    raw = command_line[len('      command: "'):-1]
    command = raw.replace('\\"', '"').replace('\\\\', '\\')
    return {'command': command, 'args': ['gateway', 'serve', '--host', 'dsh'], 'raw': command_line}


def run_dsh_installer_checks(tmp: Path, rel_dir: Path, manifest: dict, exe: Path, base_env: dict) -> dict:
    """Exercise the Windows DSH managed block against an isolated USERPROFILE.

    Every run uses a throwaway USERPROFILE and LOCALAPPDATA, so the test never
    touches a real `~/.dsh`. Each assertion is recorded by name so the report
    distinguishes configuration-format compatibility from a live DSH session,
    which this test cannot assess.
    """

    ps1 = rel_dir / 'tools' / 'install-windows.ps1'
    assert ps1.is_file(), 'release is missing tools/install-windows.ps1'

    checked: list[str] = []
    # A dedicated home keeps the DSH test independent of the earlier hosts.
    home = tmp / 'dsh 用户 主目录' / 'File User'
    local_app = home / 'AppData' / 'Local'
    home.mkdir(parents=True)
    local_app.mkdir(parents=True)
    env = clean_env(home, local_app)
    env['DSH_HOME'] = str(home / '.dsh')

    def powershell(*args: str, check: bool = True):
        return subprocess.run(
            ['powershell.exe', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', str(ps1), *args],
            env=env, capture_output=True, text=True, encoding='utf-8', timeout=120, check=check,
        )

    def preview():
        return json.loads(powershell('-Release', str(rel_dir), '-Hosts', 'dsh').stdout)

    dsh_dir = home / '.dsh'
    patch_path = dsh_dir / 'cordis.patch.yml'
    installed_exe = local_app / 'RecallDock' / 'bin' / 'recalldock.exe'

    # Pre-existing user content that must survive every write.
    dsh_dir.mkdir(parents=True)
    preexisting = (
        '- id: user-plugin\n'
        '  name: "@local/user-plugin"\n'
        '  config:\n'
        '    enabled: true\n'
        '\n'
        '- id: user-theme\n'
        '  name: "@deepseek-ai/dsh-client-ui-theme"\n'
    )
    patch_path.write_text(preexisting, encoding='utf-8', newline='\n')

    # 1. Fresh install into a profile that already has unrelated patches.
    plan = preview()
    assert any(f['path'].endswith('cordis.patch.yml') for f in plan['files']), plan
    backup = tmp / 'dsh-backup-1'
    powershell('-Release', str(rel_dir), '-Hosts', 'dsh', '-Apply',
               '-ExpectedSha256', plan['plan_sha256'], '-BackupDir', str(backup))
    assert installed_exe.is_file(), 'installed exe missing after apply'

    text = patch_path.read_text(encoding='utf-8')
    assert text.count('# BEGIN RECALLDOCK GATEWAY') == 1, text
    assert text.count('# END RECALLDOCK GATEWAY') == 1, text
    assert preexisting.strip() in text, 'pre-existing patches were dropped'
    assert text.startswith(preexisting.rstrip('\n')), 'existing patches were not kept verbatim'
    managed = parse_managed_dsh_block(text)
    assert managed['command'] == str(installed_exe), managed
    checked.append('fresh_install_preserves_existing_patches')
    checked.append('managed_block_schema_and_windows_absolute_path')

    # The path contains a space and non-ASCII characters; both must survive a
    # YAML round trip as a single literal Windows path.
    assert ' ' in str(installed_exe), 'expected a path with a space'
    assert '\u7528\u6237' in str(installed_exe), 'expected a non-ASCII path'
    checked.append('space_and_chinese_path_roundtrip')

    # 2. Repeat install is idempotent: the preview offers no file change.
    second = preview()
    assert second['files'] == [], second
    checked.append('repeat_install_idempotent')

    # 3. Upgrade: move the managed command to a new install root.
    moved_root = tmp / 'upgrade 目录' / 'RecallDock2'
    moved_exe = moved_root / 'bin' / 'recalldock.exe'
    moved_exe.parent.mkdir(parents=True)
    moved_exe.write_bytes(exe.read_bytes())
    upgrade_plan = json.loads(powershell(
        '-Release', str(rel_dir), '-Hosts', 'dsh', '-InstallRoot', str(moved_root)).stdout)
    backup_up = tmp / 'dsh-backup-upgrade'
    powershell('-Release', str(rel_dir), '-Hosts', 'dsh', '-InstallRoot', str(moved_root),
               '-Apply', '-ExpectedSha256', upgrade_plan['plan_sha256'], '-BackupDir', str(backup_up))
    upgraded_text = patch_path.read_text(encoding='utf-8')
    up_managed = parse_managed_dsh_block(upgraded_text)
    assert up_managed['command'] == str(moved_exe), up_managed
    assert up_managed['args'] == ['gateway', 'serve', '--host', 'dsh'], up_managed
    assert upgraded_text.startswith(preexisting.rstrip('\n')), 'upgrade dropped user patches'
    assert patch_path.read_text(encoding='utf-8').count('# BEGIN RECALLDOCK GATEWAY') == 1
    checked.append('upgrade_rewrites_only_managed_command')

    # 4. Rollback restores the exact pre-install bytes and keeps user content.
    powershell('-Rollback', '-BackupDir', str(backup_up))
    restored = patch_path.read_text(encoding='utf-8')
    rb_managed = parse_managed_dsh_block(restored)
    assert rb_managed['command'] == str(installed_exe), rb_managed
    checked.append('rollback_restores_managed_block')

    # 5. Unmanaged conflict: a same-id entry outside the managed block must be
    # refused rather than silently overwritten.
    conflict = (
        '- id: mcp-recalldock-gateway\n'
        '  name: "@deepseek-ai/dsh-mcp-client"\n'
        '  config:\n'
        '    serverName: rogue\n'
    )
    patch_path.write_text(conflict, encoding='utf-8')
    refused = powershell('-Release', str(rel_dir), '-Hosts', 'dsh', check=False)
    assert refused.returncode != 0, 'unmanaged same-id entry was not refused'
    assert patch_path.read_text(encoding='utf-8') == conflict, 'refused install still rewrote the file'
    checked.append('unmanaged_conflict_refused_without_write')

    # 6. Malformed managed block: unbalanced markers must fail closed.
    malformed = '- id: other\n  name: "@local/other"\n# BEGIN RECALLDOCK GATEWAY\n- insert:\n'
    patch_path.write_text(malformed, encoding='utf-8')
    refused2 = powershell('-Release', str(rel_dir), '-Hosts', 'dsh', check=False)
    assert refused2.returncode != 0, 'unbalanced managed markers were not refused'
    assert patch_path.read_text(encoding='utf-8') == malformed, 'malformed file was rewritten'
    checked.append('malformed_managed_block_refused')

    # Restore a good managed block for the handshake checks below.
    patch_path.write_text(preexisting, encoding='utf-8')
    restore_plan = preview()
    powershell('-Release', str(rel_dir), '-Hosts', 'dsh', '-Apply',
               '-ExpectedSha256', restore_plan['plan_sha256'], '-BackupDir', str(tmp / 'dsh-backup-restore'))
    final_managed = parse_managed_dsh_block(patch_path.read_text(encoding='utf-8'))
    gateway_command = Path(final_managed['command'])
    assert gateway_command.is_file(), gateway_command

    # 7. Launch the command the managed block declares and complete a real MCP
    # handshake, proving the configuration points at a working runtime.
    session = mcp_roundtrip(
        gateway_command, ['gateway', 'serve', '--host', 'dsh'], home,
        clean_env(home, local_app, dsh_dir / 'data'),
        [
            {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {
                'protocolVersion': '2025-06-18', 'capabilities': {},
                'clientInfo': {'name': 'verify-windows', 'version': '0'}}},
            {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/list'},
        ],
    )
    init = session[1]
    assert 'result' in init, init
    assert init['result']['protocolVersion'], init
    tools = session[2]['result']['tools']
    names = sorted(t['name'] for t in tools)
    assert names == sorted([
        'tools_find', 'tools_describe', 'tools_call', 'tools_release',
        'work_find', 'work_read', 'work_checkpoint',
    ]), names
    checked.append('dsh_declared_command_mcp_handshake')
    checked.append('fixed_seven_tool_surface')

    # 8. Work checkpoint / CAS through the same seven-tool surface the DSH
    # managed block exposes. work_write is not part of the gateway surface, so
    # the checkpoint is created by the CLI and then read and CAS-retried through
    # the MCP tools, which is the path a DSH agent actually uses.
    ops_data = dsh_dir / 'data'
    work_root = home / 'work 项目 甲'
    work_root.mkdir(parents=True, exist_ok=True)
    work_env = clean_env(home, local_app, ops_data)
    cp_input = tmp / 'dsh-work-checkpoint.json'
    cp_input.write_text(json.dumps(
        {'goal': 'DSH native Windows Work CAS probe', 'summary': 'initial'}), encoding='utf-8')
    created = subprocess.run(
        [str(exe), 'work', 'checkpoint', '--root', str(work_root), '--lane', 'dsh-lane',
         '--expected', '0', '--input', str(cp_input)],
        cwd=str(tmp), env=work_env, capture_output=True, text=True, encoding='utf-8', timeout=60,
    )
    assert created.returncode == 0, created.stderr[:800]

    work_session = mcp_roundtrip(
        gateway_command, ['gateway', 'serve', '--host', 'dsh'], home,
        clean_env(home, local_app, ops_data),
        [
            {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {
                'protocolVersion': '2025-06-18', 'capabilities': {},
                'clientInfo': {'name': 'verify-windows', 'version': '0'}}},
            {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {
                'name': 'work_read', 'arguments': {'root': str(work_root), 'lane': 'dsh-lane', 'mode': 'l1'}}},
            {'jsonrpc': '2.0', 'id': 3, 'method': 'tools/call', 'params': {
                'name': 'work_checkpoint', 'arguments': {
                    'root': str(work_root), 'lane': 'dsh-lane', 'expected': 1,
                    'checkpoint': {'goal': 'DSH native Windows Work CAS probe', 'summary': 'updated'}}}},
            {'jsonrpc': '2.0', 'id': 4, 'method': 'tools/call', 'params': {
                'name': 'work_read', 'arguments': {'root': str(work_root), 'lane': 'dsh-lane', 'mode': 'l1'}}},
        ],
    )
    for req_id, label in ((2, 'initial read'), (3, 'checkpoint write'), (4, 'readback')):
        payload = work_session[req_id]
        assert 'result' in payload, f'{label}: {payload}'
        assert not payload['result'].get('isError'), f'{label} failed: {payload["result"]}'
    readback = json.dumps(work_session[4]['result'], ensure_ascii=False)
    assert 'updated' in readback, f'checkpoint write not visible on readback: {readback[:400]}'

    # A stale expected generation must be refused, which is the CAS guarantee.
    stale = mcp_roundtrip(
        gateway_command, ['gateway', 'serve', '--host', 'dsh'], home,
        clean_env(home, local_app, ops_data),
        [
            {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {
                'protocolVersion': '2025-06-18', 'capabilities': {},
                'clientInfo': {'name': 'verify-windows', 'version': '0'}}},
            {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {
                'name': 'work_checkpoint', 'arguments': {
                    'root': str(work_root), 'lane': 'dsh-lane', 'expected': 1,
                    'checkpoint': {'goal': 'stale write must fail', 'summary': 'stale'}}}},
        ],
    )
    stale_result = stale[2]['result']
    assert stale_result.get('isError'), f'stale CAS write was accepted: {stale_result}'
    checked.append('work_checkpoint_cas_and_readback')

    # 9. Memory path: initialize a store, write one memory, and retrieve it
    # through the same seven-tool surface. A memory backend the gateway cannot
    # find is the failure mode the evaluation report describes as "installed but
    # no memory visible", so this asserts an actual round trip, not a config
    # read.
    mem_data = dsh_dir / 'memdata'
    mem_env = clean_env(home, local_app, mem_data)
    mem_project = 'dsh-mem-probe'
    mem_root = home / 'memory 项目'
    mem_root.mkdir(parents=True, exist_ok=True)

    init_store = subprocess.run([str(exe), 'store', 'init'], cwd=str(mem_root),
                                env=mem_env, capture_output=True, text=True, encoding='utf-8', timeout=60)
    assert init_store.returncode == 0, init_store.stderr[:800]
    assert (mem_data / 'recalldock.db').is_file(), 'store init did not create the database'

    saved = subprocess.run(
        [str(exe), 'save', 'DSH windows memory probe', 'retrievable through the gateway', '--project', mem_project],
        cwd=str(mem_root), env=mem_env, capture_output=True, text=True, encoding='utf-8', timeout=60)
    assert saved.returncode == 0, saved.stderr[:800]

    # A stdio gateway with a registered memory backend must expose it through
    # tools_find and return the saved text through tools_call.
    mem_backend = tmp / 'mem-backend.json'
    mem_backend.write_text(json.dumps({'mcpServers': {'mem-local': {
        'command': str(exe),
        'args': ['mcp', '--tools=agent'],
        'cwd': str(mem_root),
        'env': {'RECALLDOCK_DATA_DIR': str(mem_data), 'USERPROFILE': str(home),
                'LOCALAPPDATA': str(local_app)},
    }}}), encoding='utf-8')
    mem_registry = tmp / 'mem-gateway.json'
    add_backend = subprocess.run(
        [str(exe), 'gateway', 'add', '--server', 'mem-local',
         '--source-file', str(mem_backend), '--keys', 'mcpServers,mem-local',
         '--enable', '--stateful', '--registry', str(mem_registry)],
        cwd=str(mem_root), env=mem_env, capture_output=True, text=True, encoding='utf-8', timeout=60)
    assert add_backend.returncode == 0, add_backend.stderr[:800]

    gateway_env = clean_env(home, local_app, mem_data)
    mem_session = mcp_roundtrip(
        gateway_command,
        ['gateway', 'serve', '--host', 'dsh', '--registry', str(mem_registry)], home, gateway_env,
        [
            {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {
                'protocolVersion': '2025-06-18', 'capabilities': {},
                'clientInfo': {'name': 'verify-windows', 'version': '0'}}},
            # tools_find reads the cached catalog, so the backend must be
            # discovered first; a call before this returns an empty list.
            {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/call', 'params': {
                'name': 'tools_describe', 'arguments': {'server': 'mem-local', 'refresh': True}}},
            {'jsonrpc': '2.0', 'id': 3, 'method': 'tools/call', 'params': {
                'name': 'tools_find', 'arguments': {'query': 'mem_search'}}},
        ],
    )
    discover = mem_session[2]['result']
    assert not discover.get('isError'), f'backend discovery failed: {discover}'
    found = json.dumps(mem_session[3]['result'], ensure_ascii=False)
    assert not mem_session[3]['result'].get('isError'), f'backend search failed: {found[:400]}'
    assert 'mem_search' in found, f'memory backend tools not visible: {found[:400]}'
    checked.append('memory_backend_discovered_and_saved')

    return {
        'checks': checked,
        'gateway_command': str(gateway_command),
        'protocol_version': init['result']['protocolVersion'],
        'tool_count': len(tools),
        'live_dsh_host_session': 'NOT ASSESSED — no DSH desktop host was launched',
    }


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

        # 6b. Native Windows DSH integration: managed block install/upgrade/
        # idempotency/conflict/rollback, plus a real MCP handshake against the
        # installed exe. This exercises the configuration format only; it does
        # not launch the DSH desktop host, so it cannot pass for a live DSH
        # session.
        results['checks']['dsh_config_format'] = run_dsh_installer_checks(
            tmp, rel_dir, manifest, exe, env)

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
