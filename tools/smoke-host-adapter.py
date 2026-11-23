#!/usr/bin/env python3
"""Exercise host adapter against only synthetic, isolated local project data."""
import argparse
import json
import os
from pathlib import Path
import selectors
import shutil
import subprocess
import tempfile


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary', type=Path, required=True)
    p.add_argument('--work-dir', type=Path, required=True)
    a = p.parse_args()
    binary = str(a.binary.resolve(strict=True))
    env = {k: v for k, v in os.environ.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
    with tempfile.TemporaryDirectory(prefix='host-adapter-', dir=a.work_dir) as temp:
        base = Path(temp)
        root = base / 'project'
        deep = root / 'child'
        deep.mkdir(parents=True)
        env['RECALLDOCK_DATA_DIR'] = str(base / 'data')
        env['HOME'] = str(base)
        def run(*args, cwd=base, data=None, ok=True):
            r = subprocess.run([binary, *args], cwd=cwd, env=env, input=data, capture_output=True, text=True, timeout=30)
            assert (r.returncode == 0) == ok, (args, r.returncode, r.stderr[:500])
            return r
        unbound = run('mcp', '--require-project', cwd=deep, ok=False)
        assert 'store_not_initialized' in unbound.stderr and not (base / 'data').exists()
        run('store', 'init')
        unbound = run('mcp', '--require-project', cwd=deep, ok=False)
        assert 'not bound' in unbound.stderr
        # New host mode provisions distinct projects from same-named directories,
        # even when several host processes start the same workspace together.
        auto1 = base / 'first' / 'workspace'
        auto2 = base / 'second' / 'workspace'
        auto1.mkdir(parents=True)
        auto2.mkdir(parents=True)
        payload = lambda sid, cwd: json.dumps({'session_id': sid, 'cwd': str(cwd), 'hook_event_name': 'SessionStart'})
        processes = [subprocess.Popen([binary, 'host-event', 'codex', '--auto-project'], cwd=auto1, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True) for _ in range(6)]
        outputs = [p.communicate(payload('auto-' + str(i), auto1), timeout=30) for i, p in enumerate(processes)]
        assert all(p.returncode == 0 for p in processes), outputs
        def marker(root): return json.loads((root / '.recalldock/root.json').read_text())['project_id']
        first_id = marker(auto1)
        run('host-event', 'claude-code', '--auto-project', cwd=auto2, data=payload('auto-second', auto2))
        assert first_id != marker(auto2)
        run('save', 'unique-private-marker', 'unique-private-body', '--type', 'decision', cwd=auto1)
        mcp_auto = subprocess.Popen([binary, 'mcp', '--tools=agent', '--auto-project'], cwd=auto1, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        requests = [
            {'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'smoke','version':'1'}}},
            {'jsonrpc':'2.0','id':2,'method':'tools/list','params':{}},
            {'jsonrpc':'2.0','id':3,'method':'tools/call','params':{'name':'mem_search','arguments':{'query':'unique-private-marker'}}},
        ]
        out, err = mcp_auto.communicate(''.join(json.dumps(req) + '\n' for req in requests), timeout=30)
        assert mcp_auto.returncode == 0, (out, err)
        responses = {x['id']: x for x in map(json.loads, out.splitlines()) if 'id' in x}
        tools_auto = {x['name'] for x in responses[2]['result']['tools']}
        assert 'mem_search' in tools_auto and 'mem_session_start' not in tools_auto
        assert 'unique-private-marker' in json.dumps(responses[3])
        mcp_other = subprocess.Popen([binary, 'mcp', '--tools=agent', '--auto-project'], cwd=auto2, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        out, err = mcp_other.communicate(''.join(json.dumps(req) + '\n' for req in requests), timeout=30)
        assert mcp_other.returncode == 0, err
        other_responses = {x['id']: x for x in map(json.loads, out.splitlines()) if 'id' in x}
        other_search = json.loads(other_responses[3]['result']['content'][0]['text'])
        assert other_search['results'] == [] and 'unique-private-body' not in json.dumps(other_search)
        project = json.loads(run('projects', 'create', '--name', 'Synthetic').stdout)['id']
        run('projects', 'bind', project, '--directory', str(root))
        run('host-event','codex','--auto-project',data=payload('inherited-deep',deep),cwd=deep)
        inherited=json.loads(run('projects','current','--directory',str(deep),'--json').stdout)
        assert inherited['project_id']==project and not (deep/'.recalldock/root.json').exists(), 'host hook shadowed the registered parent scope'
        git_env = dict(env, GIT_AUTHOR_NAME='Synthetic', GIT_AUTHOR_EMAIL='fixture@example.invalid', GIT_COMMITTER_NAME='Synthetic', GIT_COMMITTER_EMAIL='fixture@example.invalid')
        def git(cwd, *args):
            subprocess.run(['git', '-C', str(cwd), *map(str, args)], env=git_env, capture_output=True, text=True, timeout=30, check=True)
        git(root, 'init', '-q')
        git(root, 'commit', '-q', '--allow-empty', '-m', 'Synthetic host worktree')
        linked = base / 'linked'
        git(root, 'worktree', 'add', '-q', '--detach', linked, 'HEAD')
        for auto in (False, True):
            options = ['--auto-project'] if auto else []
            run('host-event', 'codex', *options, cwd=linked, data=payload('synthetic-linked-' + str(auto), linked))
            current = json.loads(run('projects', 'current', '--json', cwd=linked).stdout)
            assert current['project_id'] == project and current['project_source'] == 'worktree_main'
            assert not (linked / '.recalldock/root.json').exists()
        linked_mcp = subprocess.Popen([binary, 'mcp', '--tools=agent', '--auto-project'], cwd=linked, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        out, err = linked_mcp.communicate(''.join(json.dumps(req) + '\n' for req in requests), timeout=30)
        assert linked_mcp.returncode == 0 and not (linked / '.recalldock/root.json').exists(), (out, err)
        linked_responses = {row['id']: row for row in map(json.loads, out.splitlines()) if 'id' in row}
        assert json.loads(linked_responses[3]['result']['content'][0]['text'])['results'] == []
        unbound_main = base / 'unbound-main'; unbound_main.mkdir()
        git(unbound_main, 'init', '-q')
        git(unbound_main, 'commit', '-q', '--allow-empty', '-m', 'Synthetic unbound main')
        unbound_linked = base / 'unbound-linked'
        git(unbound_main, 'worktree', 'add', '-q', '--detach', unbound_linked, 'HEAD')
        refused = run('host-event', 'codex', '--auto-project', cwd=unbound_linked, data=payload('synthetic-unbound-linked', unbound_linked), ok=False)
        assert 'bound main checkout' in refused.stderr and not (unbound_linked / '.recalldock/root.json').exists()
        # A copied marker must not silently activate the original project's memory.
        clone = base / 'clone'
        (clone / '.recalldock').mkdir(parents=True)
        shutil.copy2(root / '.recalldock/root.json', clone / '.recalldock/root.json')
        run('mcp', '--require-project', cwd=clone, ok=False)
        run('save', 'synthetic marker', 'synthetic body', '--type', 'decision', cwd=deep)
        start = json.dumps({'session_id': 'synthetic-123', 'cwd': str(deep), 'hook_event_name': 'SessionStart', 'prompt': 'DO_NOT_STORE_RAW', 'transcript_path': 'DO_NOT_STORE_TRANSCRIPT'})
        hook = run('host-event', 'codex', data=start, cwd=deep)
        assert 'DO_NOT_STORE' not in hook.stdout and 'codex-synthetic-123' in hook.stdout
        mcp = subprocess.Popen([binary, 'mcp', '--tools=agent', '--require-project'], cwd=deep, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)
        try:
            select = selectors.DefaultSelector()
            select.register(mcp.stdout, selectors.EVENT_READ)
            n = 0
            def rpc(method, params):
                nonlocal n
                n += 1
                mcp.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': n, 'method': method, 'params': params}) + '\n')
                mcp.stdin.flush()
                assert select.select(15), f'timeout {method}'
                response = json.loads(mcp.stdout.readline())
                assert response.get('id') == n and 'result' in response, response
                return response['result']
            rpc('initialize', {'protocolVersion':'2024-11-05', 'capabilities':{}, 'clientInfo':{'name':'isolated-synthetic-test', 'version':'1'}})
            identity = rpc('tools/call', {'name':'mem_current_project','arguments':{}})
            identity = json.loads(identity['content'][0]['text'])
            assert Path(identity['data_dir']).resolve() == (base / 'data').resolve()
            tools = {x['name'] for x in rpc('tools/list', {})['tools']}
            assert 'mem_search' in tools and 'mem_session_start' not in tools and 'mem_session_end' not in tools
            def call(name, args): return rpc('tools/call', {'name':name, 'arguments':args})
            search = call('mem_search', {'query':'synthetic marker'})
            assert 'synthetic marker' in json.dumps(search)
            foreign = call('mem_search', {'query':'synthetic marker', 'all_projects':True})
            assert foreign.get('isError') is True and 'synthetic body' not in json.dumps(foreign)
            proposal = call('mem_commit_session', {'session_id':'not-registered', 'project':project, 'candidates':[]})
            assert proposal.get('isError') is True
        finally:
            mcp.terminate()
            mcp.communicate(timeout=10)
        end = json.dumps({'session_id':'synthetic-123', 'cwd':str(deep), 'hook_event_name':'SessionEnd', 'prompt':'DO_NOT_STORE_RAW'})
        run('host-event', 'codex', data=end, cwd=deep)
        run('host-event', 'codex', data=start, cwd=deep)
        print(json.dumps({'synthetic_only': True, 'unbound_fails_closed': True, 'nested_binding': True, 'linked_worktree_identity': True, 'unbound_main_fail_closed': True, 'native_host_dispatch_tested': False, 'lifecycle_metadata_only': True, 'no_model_lifecycle_tools': True, 'no_unregistered_proposal': True, 'all_projects_denied': True, 'resume': True}))


if __name__ == '__main__':
    main()
