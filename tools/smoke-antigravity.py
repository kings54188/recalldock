#!/usr/bin/env python3
"""Native agy metadata and isolated synthetic hook/MCP acceptance; no model prompts."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import queue
import shutil
import sqlite3
import subprocess
import threading
import tempfile


class RPC:
    def __init__(self, command, env, cwd, stderr_path):
        self.stderr_path = stderr_path
        self.stderr = stderr_path.open('w')
        self.process = subprocess.Popen(command, env=env, cwd=cwd, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=self.stderr, text=True)
        self.responses = queue.Queue()
        self.identifier = 0
        assert self.process.stdout is not None and self.process.stdin is not None

        def read():
            try:
                for line in self.process.stdout:
                    self.responses.put(json.loads(line))
            except (ValueError, OSError) as error:
                self.responses.put(error)
            finally:
                self.responses.put(None)

        threading.Thread(target=read, daemon=True).start()

    def request(self, method, params):
        self.identifier += 1
        self.process.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': self.identifier,
                                            'method': method, 'params': params}) + '\n')
        self.process.stdin.flush()
        while True:
            response = self.responses.get(timeout=20)
            assert isinstance(response, dict), f'gateway exited or emitted invalid JSON: {response}; {self.stderr_path.read_text()[:1000]}'
            if 'id' not in response:
                continue
            assert response['id'] == self.identifier and 'error' not in response, response
            return response['result']

    def tool(self, name, arguments):
        result = self.request('tools/call', {'name': name, 'arguments': arguments})
        assert not result.get('isError'), result
        return json.loads(result['content'][0]['text'])['data']

    def close(self):
        self.process.stdin.close()
        try:
            self.process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.process.terminate()
            self.process.wait(timeout=10)
            raise AssertionError('gateway did not exit on EOF')
        finally:
            self.stderr.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--plugin', type=Path, required=True)
    parser.add_argument('--agy', type=Path, default=shutil.which('agy'))
    parser.add_argument('--work-dir', type=Path, required=True)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    for name in ('binary', 'plugin', 'agy', 'work_dir', 'report'):
        value = getattr(args, name)
        if value is None or not value.is_absolute():
            parser.error(f'{name.replace("_", "-")} must be an absolute path')
    binary, plugin, agy = args.binary.resolve(strict=True), args.plugin.resolve(strict=True), args.agy.resolve(strict=True)
    assert args.work_dir.is_dir() and args.report.parent.is_dir()
    assert binary.is_file() and agy.is_file() and plugin.is_dir()
    manifest = json.loads((plugin / 'plugin.json').read_text())
    assert manifest['name'] == 'recalldock'
    report = {'synthetic_only': True, 'no_model_prompt_or_inference': True,
              'real_authenticated_agy_session_tested': False, 'automatic_host_session_binding_tested': False}
    with tempfile.TemporaryDirectory(prefix='antigravity-smoke-', dir=args.work_dir) as temp:
        base = Path(temp)
        home, data, root, other = base / 'home', base / 'data', base / 'workspace', base / 'other'
        for directory in (home, root, other, home / 'bin'):
            directory.mkdir(mode=0o700)
        (home / 'bin/recalldock').symlink_to(binary)
        env = {'HOME': str(home), 'PATH': str(home / 'bin') + ':/usr/local/bin:/usr/bin:/bin',
               'LANG': 'C.UTF-8', 'XDG_CONFIG_HOME': str(home / 'xdg-config'),
               'XDG_DATA_HOME': str(home / 'xdg-data'), 'RECALLDOCK_DATA_DIR': str(data),
               'RECALLDOCK_GATEWAY_REGISTRY': str(base / 'gateway.json')}

        def run(command, cwd=base, payload=None, success=True):
            result = subprocess.run([str(word) for word in command], cwd=cwd, env=env,
                                    input=payload, text=True, capture_output=True, timeout=30)
            assert (result.returncode == 0) == success, (command, result.returncode, result.stderr[:500])
            return result

        version = run([agy, '--version']).stdout.strip()
        validated = json.loads(run([agy, 'plugin', 'validate', plugin, '--strict', '--json']).stdout)
        assert validated['components']['mcpServers'] == 1
        assert validated['components']['hooks'] == 1 and validated['components']['skills'] == 1
        assert not [finding for finding in validated.get('findings', []) if finding['severity'] in ('error', 'warning')]
        installed_output = run([agy, 'plugin', 'install', plugin]).stdout
        installed = home / '.gemini/config/plugins/recalldock'
        assert installed.is_dir() and str(installed) in installed_output
        listed = json.loads(run([agy, 'plugin', 'list', '--json']).stdout)
        assert len(listed) == 1 and listed[0]['name'] == 'recalldock' and listed[0]['enabled'] is True
        assert listed[0]['version'] == manifest['version'] and Path(listed[0]['path']).resolve() == installed
        assert listed[0]['skills'] == ['selective-memory'] and listed[0]['mcpServers'] == ['recalldock']
        assert listed[0]['hooks'] == ['recalldock-session']
        for name in ('plugin.json', 'mcp_config.json', 'hooks.json', 'skills/selective-memory/SKILL.md'):
            assert hashlib.sha256((installed / name).read_bytes()).digest() == hashlib.sha256((plugin / name).read_bytes()).digest()
        assert not data.exists()
        report.update(agy_version=version, native_strict_validation=True, native_install_list=True,
                      installed_content_matches_source=True, isolated_native_home=True)

        acp = [agy, 'acp', '--disable-telemetry=true', '--include-default-customizations=false',
               '--app-data-dir=smoke-antigravity', '--gemini-dir=.gemini-acp',
               '--cloud_code_endpoint=http://127.0.0.1:9']
        request = {'jsonrpc': '2.0', 'id': 1, 'method': 'initialize',
                   'params': {'protocolVersion': 1, 'clientCapabilities': {},
                              'clientInfo': {'name': 'isolated-adapter-smoke', 'version': '1'}}}
        output = run(acp, payload=json.dumps(request) + '\n').stdout
        responses = {item['id']: item for item in map(json.loads, output.splitlines()) if 'id' in item}
        initialized = responses[1]['result']
        assert initialized['protocolVersion'] == 1 and initialized['agentInfo']['version'] == version
        assert not data.exists()
        report.update(acp_initialize=True, acp_protocol=1, acp_telemetry_disabled=True,
                      acp_model_session_or_mcp_loading_tested=False)

        hooks = json.loads((installed / 'hooks.json').read_text())
        assert set(hooks) == {'recalldock-session'} and set(hooks['recalldock-session']) == {'SessionStart'}
        handlers = hooks['recalldock-session']['SessionStart']
        assert len(handlers) == 1 and handlers[0]['type'] == 'command'
        command = handlers[0]['command']
        assert command == 'recalldock host-event antigravity --event SessionStart'
        conversation = '00000000-0000-4000-8000-000000000021'
        payload = {'conversationId': conversation, 'workspacePaths': [str(root)],
                   'transcriptPath': 'DO_NOT_READ_TRANSCRIPT', 'artifactDirectoryPath': 'DO_NOT_READ_ARTIFACTS',
                   'lastUserInput': 'DO_NOT_STORE_RAW_PROMPT', 'modelName': 'synthetic-metadata-only'}
        ambiguous = dict(payload, conversationId='00000000-0000-4000-8000-000000000022',
                         workspacePaths=[str(root), str(other)])
        rejected = subprocess.run(command, shell=True, cwd=installed, env=env, input=json.dumps(ambiguous),
                                  text=True, capture_output=True, timeout=30)
        assert rejected.returncode != 0 and not data.exists()
        assert not (root / '.recalldock/root.json').exists() and not (other / '.recalldock/root.json').exists()
        unbound = subprocess.run(command, shell=True, cwd=installed, env=env, input=json.dumps(payload),
                                 text=True, capture_output=True, timeout=30)
        assert unbound.returncode == 0 and not unbound.stdout and not data.exists()
        assert not (root / '.recalldock/root.json').exists()
        project = json.loads(run([binary, 'projects', 'create', '--name', 'Synthetic adapter task']).stdout)['id']
        run([binary, 'projects', 'bind', project, '--directory', root])
        report.update(unregistered_start_stays_unbound=True, operator_registered_project=True)
        accepted = subprocess.run(command, shell=True, cwd=installed, env=env, input=json.dumps(payload),
                                  text=True, capture_output=True, timeout=30)
        assert accepted.returncode == 0, accepted.stderr[:500]
        response = json.loads(accepted.stdout)
        session_id = 'antigravity-' + conversation
        assert len(response['injectSteps']) == 1 and session_id in response['injectSteps'][0]['systemMessage']['systemMessage']
        assert 'DO_NOT_' not in accepted.stdout
        with sqlite3.connect((data / 'recalldock.db').as_uri() + '?mode=ro', uri=True) as db:
            origin, directory, summary = db.execute('SELECT origin,directory,summary FROM sessions WHERE id=?', (session_id,)).fetchone()
            assert origin == 'host' and directory == str(root) and not summary
            assert all(db.execute('SELECT count(*) FROM ' + table).fetchone()[0] == 0
                       for table in ('observations', 'memory_candidates', 'user_prompts'))
        with sqlite3.connect((data / 'operations.db').as_uri() + '?mode=ro', uri=True) as db:
            host, directory, ended = db.execute('SELECT host,root,ended_at FROM host_sessions WHERE id=?', (session_id,)).fetchone()
            assert host == 'antigravity' and directory == str(root) and ended is None
        report.update(synthetic_installed_hook_command=True, native_hook_dispatch_tested=False,
                      official_payload_without_event_name=True, multiworkspace_rejected_without_binding=True,
                      host_origin_and_ops_registration=True, raw_prompt_transcript_fields_ignored=True,
                      zero_observations_candidates_prompts=True, no_stop_as_session_end=True)

        server = json.loads((installed / 'mcp_config.json').read_text())['mcpServers']
        assert set(server) == {'recalldock'}
        config = server['recalldock']
        assert config['command'] == 'recalldock' and config['args'] == ['gateway', 'serve', '--host', 'antigravity']
        gateway_command = [config['command'], *config['args']]
        for bound in (False, True):
            extra = ['--host-session', session_id] if bound else []
            rpc = RPC(gateway_command + extra, env, root, base / ('bound-gateway.log' if bound else 'gateway.log'))
            try:
                rpc.request('initialize', {'protocolVersion': '2024-11-05', 'capabilities': {},
                                          'clientInfo': {'name': 'isolated-hook-mcp-smoke', 'version': '1'}})
                tools = rpc.request('tools/list', {})['tools']
                assert len(tools) == 7
                result = rpc.tool('work_checkpoint', {'root': str(root), 'lane': 'agy-acceptance', 'expected': int(bound),
                                                     'checkpoint': {'goal': 'Synthetic Antigravity adapter acceptance',
                                                                    'summary': 'Validated isolated native metadata and synthetic lifecycle'}})
                writer = result['writer']
                assert writer['host'] == 'antigravity' and writer['gateway_session_id'].startswith('gateway-')
                assert writer.get('host_session_id') == (session_id if bound else None)
                read = rpc.tool('work_read', {'root': str(root), 'lane': 'agy-acceptance', 'mode': 'l2'})
                assert read['writer'] == writer and read['generation'] == 1 + int(bound)
            finally:
                rpc.close()
        with sqlite3.connect((data / 'recalldock.db').as_uri() + '?mode=ro', uri=True) as db:
            assert all(db.execute('SELECT count(*) FROM ' + table).fetchone()[0] == 0
                       for table in ('observations', 'memory_candidates', 'user_prompts'))
        report.update(synthetic_installed_mcp_command=True, fixed_gateway_tools=7,
                      gateway_writer_host=True, synthetic_explicit_host_binding=True,
                      normal_gateway_host_session_unknown=True)
    report['ok'] = True
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps(report, ensure_ascii=False))


if __name__ == '__main__':
    main()
