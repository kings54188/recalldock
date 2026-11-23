#!/usr/bin/env python3
"""Real stdio gateway acceptance using only temporary synthetic data/backends."""
import argparse
import json
import os
from pathlib import Path
import queue
import sqlite3
import subprocess
import sys
import tempfile
import threading

BACKEND = r'''
import json,sys,os
with open(sys.argv[1],"a") as f:f.write("start\n")
def emit(x):print(json.dumps(x),flush=True)
for line in sys.stdin:
 r=json.loads(line);i=r.get("id");m=r.get("method")
 if i is None:continue
 if m=="initialize":v={"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"synthetic","version":"1"}}
 elif m=="tools/list":v={"tools":[{"name":"echo","description":"Synthetic echo","inputSchema":{"type":"object","properties":{"message":{"type":"string"}}}},{"name":"image","inputSchema":{"type":"object"}}]}
 elif m=="tools/call":
  p=r["params"]
  if "progressToken" in p.get("_meta",{}):emit({"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":p["_meta"]["progressToken"],"progress":1,"total":1}})
  v={"content":[{"type":"image","data":"c3ludGhldGlj","mimeType":"image/png"}],"isError":True,"structuredContent":{"synthetic":True}} if p["name"]=="image" else {"content":[{"type":"text","text":p.get("arguments",{}).get("message","")} ]}
 else:v={}
 emit({"jsonrpc":"2.0","id":i,"result":v})
'''


class RPC:
    def __init__(self, binary, env, extra=()):
        self.p = subprocess.Popen([str(binary), 'gateway', 'serve', *extra], env=env,
                                  stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                  stderr=subprocess.DEVNULL, text=True)
        self.q = queue.Queue()
        self.i = 0
        self.notifications = []

        def reader():
            for line in self.p.stdout:
                self.q.put(json.loads(line))
            self.q.put(None)

        threading.Thread(target=reader, daemon=True).start()

    def request(self, method, params):
        self.i += 1
        self.p.stdin.write(json.dumps(dict(jsonrpc='2.0', id=self.i, method=method, params=params)) + '\n')
        self.p.stdin.flush()
        while True:
            r = self.q.get(timeout=15)
            assert r is not None, 'gateway exited early'
            if 'id' not in r:
                self.notifications.append(r)
                continue
            assert r['id'] == self.i and 'error' not in r, r
            return r['result']

    def tool(self, name, args, progress=None):
        p = dict(name=name, arguments=args)
        if progress is not None:
            p['_meta'] = dict(progressToken=progress)
        return self.request('tools/call', p)

    def close(self):
        self.p.stdin.close()
        try:
            self.p.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.p.terminate()
            self.p.wait(timeout=5)
            raise AssertionError('gateway did not exit on EOF')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    checks = []
    with tempfile.TemporaryDirectory(prefix='recalldock-gateway-') as tmp:
        root = Path(tmp)
        data = root / 'data'
        data.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
        env.update(HOME=str(root), RECALLDOCK_DATA_DIR=str(data), RECALLDOCK_GATEWAY_REGISTRY=str(root / 'gateway.json'))

        def cli(*parts):
            r = subprocess.run([str(binary), *map(str, parts)], env=env, capture_output=True, text=True, timeout=20)
            assert r.returncode == 0, (parts[0:2], r.stderr[:300])
            return json.loads(r.stdout) if r.stdout.strip() else None

        backend = root / 'backend.py'
        backend.write_text(BACKEND)
        starts = root / 'starts'
        original = root / 'original.json'
        original.write_text(json.dumps(dict(mcpServers=dict(test=dict(command=sys.executable, args=[str(backend), str(starts)])))))
        definition = root / 'definition.json'
        definition.write_text(json.dumps(dict(description='Synthetic backend', source=dict(path=str(original), format='json', keys=['mcpServers', 'test']), enabled=True, start_allowed=True, stateful=True)))
        cli('gateway', 'add', '--server', 'test', '--input', definition)
        rpc = RPC(binary, env, ('--trust', 'strict'))
        try:
            rpc.request('initialize', dict(protocolVersion='2024-11-05', capabilities={}, clientInfo=dict(name='acceptance', version='1')))
            tools = rpc.request('tools/list', {})['tools']
            assert len(tools) == 7
            assert not starts.exists()
            found = rpc.tool('tools_find', {})
            assert not found.get('isError') and not starts.exists()
            checks.append('seven fixed tools; idle/find start no backend')
            cp = dict(root=str(root), lane='test', expected=0, checkpoint=dict(goal='Synthetic task', summary='Checked files', details='SELECTED-DETAIL'))
            assert not rpc.tool('work_checkpoint', cp).get('isError')
            assert rpc.tool('work_checkpoint', cp).get('isError')
            r = rpc.tool('work_read', dict(root=str(root), lane='test'))
            assert 'SELECTED-DETAIL' not in json.dumps(r)
            r = rpc.tool('work_read', dict(root=str(root), lane='test', mode='l2', generation=1))
            assert 'SELECTED-DETAIL' in json.dumps(r)
            assert not (data / 'recalldock.db').exists()
            checks.append('progress CAS/L1/L2 independent from knowledge database')
            described_builtin = rpc.tool('tools_describe', dict(server='recalldock_gateway'))
            builtin = json.loads(described_builtin['content'][0]['text'])['data']
            assert len(builtin['tools']) == 7 and not builtin['has_more']
            assert len(described_builtin['content'][0]['text'].encode()) <= 2048
            seen_builtin, builtin_after = set(), ''
            for _ in range(8):
                result = rpc.tool('tools_find', dict(query='work', builtin_after=builtin_after))
                assert not result.get('isError') and len(result['content'][0]['text'].encode()) <= 2048
                found_builtin = json.loads(result['content'][0]['text'])['data']
                seen_builtin.update(item['name'] for item in found_builtin['builtin_tools'])
                if not found_builtin['builtin_has_more']:
                    break
                next_after = found_builtin['builtin_after']
                assert next_after and next_after != builtin_after
                builtin_after = next_after
            assert {'work_find', 'work_read', 'work_checkpoint'} <= seen_builtin
            event = dict(root=str(root), lane='test', expected=1,
                         event=dict(id='attempt', input_fingerprint='synthetic-v1', kind='attempt',
                                    status='reported', summary='26/26 event checks', details='SELECTED-EVENT'),
                         patch=dict(summary='26/26 recovery checks'))
            applied = rpc.tool('work_checkpoint', event)
            assert not applied.get('isError')
            applied_data = json.loads(applied['content'][0]['text'])['data']
            writer = applied_data['writer']
            assert writer['gateway_session_id'].startswith('gateway-') and not writer.get('host_session_id')
            repeated = rpc.tool('work_checkpoint', event)
            repeated_data = json.loads(repeated['content'][0]['text'])['data']
            assert repeated_data['replayed'] and repeated_data['generation'] == 2 and repeated_data['writer'] == writer
            selected = rpc.tool('work_read', dict(root=str(root), lane='test', view='event', mode='l2', generation=2))
            assert not selected.get('isError') and 'SELECTED-EVENT' in json.dumps(selected)
            history = rpc.tool('work_read', dict(root=str(root), lane='test', view='history', limit=1))
            history_data = json.loads(history['content'][0]['text'])['data']
            assert history_data['has_more'] and history_data['total'] == 2 and history_data['before'] == 2
            search = rpc.tool('work_find', dict(root=str(root), query='26/26', limit=20))
            search_data = json.loads(search['content'][0]['text'])['data']
            assert [task['lane'] for task in search_data['tasks']] == ['test'] and not search_data['has_more']
            assert 'SELECTED-EVENT' not in json.dumps(search)
            assert rpc.tool('work_checkpoint', dict(event, author='model-claim')).get('isError')
            assert not starts.exists()
            checks.append('complete bounded discovery; progress search/history/event/replayed/server-owned writer')
            budget_scope = dict(root=str(root), lane='event-budget')
            assert not rpc.tool('work_checkpoint', dict(budget_scope, expected=0, checkpoint=dict(goal='Bounded event budget'))).get('isError')
            oversized = dict(id='escaped', input_fingerprint='escaped-v1', status='started', summary='s' * 600, details='\t' * 6000)
            rejected = rpc.tool('work_checkpoint', dict(budget_scope, expected=1, event=oversized))
            assert rejected.get('isError') and 'budget' in rejected['content'][0]['text']
            unchanged = json.loads(rpc.tool('work_read', budget_scope)['content'][0]['text'])['data']
            assert unchanged['generation'] == 1 and not unchanged.get('needs_check', False)
            with sqlite3.connect(f"file:{data / 'operations.db'}?mode=ro", uri=True) as database:
                for table in ('events', 'operations'):
                    assert database.execute(f"SELECT count(*) FROM {table} WHERE lane='event-budget'").fetchone()[0] == 0
            checks.append('unreadable event envelope rejected without generation/event/operation side effects')
            r = rpc.tool('tools_describe', dict(server='test', refresh=True))
            assert not r.get('isError'), r
            assert starts.read_text().count('start') == 1
            described = rpc.tool('tools_describe', dict(server='test', tool='echo'))
            record = json.loads(described['content'][0]['text'])['data']
            call = dict(server='test', tool='echo', root=str(root), expected_schema_hash=record['schema_hash'], arguments=dict(message='synthetic'))
            assert rpc.tool('tools_call', dict(call, approval=True)).get('isError')
            assert starts.read_text().count('start') == 1
            checks.append('strict denial occurs before backend start; model approval ineffective')
            cli('gateway', 'grant', '--server', 'test', '--tool', 'echo', '--root', root, '--read-only')
            r = rpc.tool('tools_call', call, progress='host-token')
            assert not r.get('isError'), r
            # Model-only hosts discard _meta. Use the visible control block for
            # reuse/release and merely check metadata compatibility separately.
            control = json.loads(r['content'][-1]['text'])['recalldock_gateway']
            lease = control['lease']
            assert control['idle_ttl_seconds'] == 300 and r['_meta']['recalldock_lease'] == lease
            rpc.tool('tools_find', {})
            assert any(n['params'].get('progressToken') == 'host-token' for n in rpc.notifications)
            r = rpc.tool('tools_call', dict(call, lease=lease))
            assert not r.get('isError') and starts.read_text().count('start') == 2
            assert not rpc.tool('tools_release', dict(lease=lease)).get('isError')
            assert rpc.tool('tools_call', dict(call, lease=lease)).get('isError')
            checks.append('model-visible lease reuse/release and progress token forwarding')
            cli('gateway', 'revoke', '--server', 'test', '--tool', 'echo')
            assert rpc.tool('tools_call', call).get('isError')
            cli('gateway', 'grant', '--server', 'test', '--tool', 'image', '--root', root, '--read-only')
            record = json.loads(rpc.tool('tools_describe', dict(server='test', tool='image'))['content'][0]['text'])['data']
            r = rpc.tool('tools_call', dict(call, tool='image', expected_schema_hash=record['schema_hash']))
            assert r['isError'] and r['content'][0]['type'] == 'image' and r['structuredContent']['synthetic']
            lease = json.loads(r['content'][-1]['text'])['recalldock_gateway']['lease']
            assert not rpc.tool('tools_release', dict(lease=lease)).get('isError')
            cli('gateway', 'disable', '--server', 'test')
            assert rpc.tool('tools_call', call).get('isError')
            cli('gateway', 'remove', '--server', 'test')
            assert original.exists()
            checks.append('live CLI revoke/disable/remove; native image/error/structured result preserved')
        finally:
            rpc.close()
        escaped_backend = root / 'escaped-backend.py'
        escaped_backend.write_text(r'''
import json,sys
for line in sys.stdin:
 r=json.loads(line);i=r.get("id");m=r.get("method")
 if i is None:continue
 if m=="initialize":v={"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"escaped-synthetic","version":"1"}}
 elif m=="tools/list":v={"tools":[{"name":name,"description":"work"+"\""*116,"inputSchema":{"type":"object"}} for name in ["\""*200,"\""*199+"a"]]}
 else:v={}
 print(json.dumps({"jsonrpc":"2.0","id":i,"result":v}),flush=True)
''')
        ids = ['a' * 80, 'b' * 80]
        escaped_original = root / 'escaped-original.json'
        escaped_original.write_text(json.dumps(dict(mcpServers={name: dict(command=sys.executable, args=['-I', str(escaped_backend)]) for name in ids})))
        for name in ids:
            definition.write_text(json.dumps(dict(description='work' + 'x' * 116,
                                                  source=dict(path=str(escaped_original), format='json', keys=['mcpServers', name]),
                                                  enabled=True, start_allowed=True)))
            cli('gateway', 'add', '--server', name, '--input', definition)
            cli('gateway', 'discover', '--server', name)
        escaped_rpc = RPC(binary, env, ('--trust', 'strict'))
        try:
            escaped_rpc.request('initialize', dict(protocolVersion='2024-11-05', capabilities={}, clientInfo=dict(name='escaped-acceptance', version='1')))
            after, seen, summaries_truncated = '', set(), False
            for _ in range(8):
                result = escaped_rpc.tool('tools_find', dict(query='work', tool_after=after))
                assert not result.get('isError'), result
                assert len(result['content'][0]['text'].encode()) <= 2048
                page = json.loads(result['content'][0]['text'])['data']
                summaries_truncated |= page.get('summaries_truncated', False)
                for item in page['tools']:
                    key = (item['server'], item['name'])
                    assert key not in seen
                    seen.add(key)
                if not page['tool_has_more']:
                    break
                assert page['tool_after'] and page['tool_after'] != after
                after = page['tool_after']
            assert len(seen) == 4 and summaries_truncated
            checks.append('accepted escaped tool metadata remains discoverable within 2 KiB')
        finally:
            escaped_rpc.close()
    report = dict(ok=True, checks=checks, fixed_tools=len(tools), schema_bytes=len(json.dumps(tools).encode()))
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps(report, ensure_ascii=False))


if __name__ == '__main__':
    main()
