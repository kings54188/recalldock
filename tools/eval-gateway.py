#!/usr/bin/env python3
"""Measure the fixed gateway and selected MCP paths using isolated synthetic backends."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile

BACKEND = r'''
import json,sys
name,journal=sys.argv[1:3]
def record(method):
 with open(journal,"a") as f:f.write(json.dumps({"backend":name,"method":method})+"\n")
def emit(value):print(json.dumps(value),flush=True)
record("start")
tools=[{"name":f"tool_{i:02d}","annotations":{},"description":"Synthetic selected read; use offset and limit instead of fetching the whole dataset. "*4,"inputSchema":{"type":"object","additionalProperties":False,"required":["message"],"properties":{"message":{"type":"string"},"offset":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":10}}}} for i in range(32)]
for line in sys.stdin:
 r=json.loads(line);i=r.get("id");m=r.get("method")
 if i is None:continue
 record(m)
 if m=="initialize":v={"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":name,"version":"1"}}
 elif m=="tools/list":v={"tools":tools}
 elif m=="tools/call":
  args=r["params"].get("arguments",{});offset=args.get("offset",0);limit=args.get("limit",2)
  payload={"message":args.get("message"),"rows":[{"id":j,"value":f"record-{j}"} for j in range(offset,offset+limit)]}
  v={"content":[{"type":"text","text":json.dumps(payload)}],"structuredContent":payload}
 else:v={}
 emit({"jsonrpc":"2.0","id":i,"result":v})
'''


def packed(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def measure(value):
    text = packed(value)
    ascii_count = sum(ord(c) <= 127 for c in text)
    return {"utf8_bytes": len(text.encode()), "estimated_tokens": 12 + (ascii_count + 3) // 4 + len(text) - ascii_count}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary', type=Path, required=True)
    p.add_argument('--work-dir', type=Path, required=True)
    p.add_argument('--report', type=Path, required=True)
    p.add_argument('--compare', type=Path)
    a = p.parse_args()
    binary = a.binary.resolve(strict=True)
    RPC = runpy.run_path(str(Path(__file__).with_name('smoke-gateway.py')))['RPC']
    with tempfile.TemporaryDirectory(prefix='gateway-eval-', dir=a.work_dir) as tmp:
        root = Path(tmp)
        env = {k: v for k, v in os.environ.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
        env.update(HOME=str(root), RECALLDOCK_DATA_DIR=str(root / 'data'), RECALLDOCK_GATEWAY_REGISTRY=str(root / 'gateway.json'))
        script, journal = root / 'backend.py', root / 'backend-events.jsonl'
        script.write_text(BACKEND)
        sources = root / 'sources.json'
        configurations = {f'catalog{i}': {'command': sys.executable, 'args': ['-I', str(script), f'catalog{i}', str(journal)]} for i in range(3)}
        sources.write_text(json.dumps({'mcpServers': configurations}))

        def cli(*args):
            r = subprocess.run([str(binary), *map(str, args)], cwd=root, env=env, text=True, capture_output=True, timeout=30)
            assert r.returncode == 0, (args, r.stderr)
            return json.loads(r.stdout) if r.stdout.strip() else None

        def events():
            return [json.loads(line) for line in journal.read_text().splitlines()] if journal.exists() else []

        for name in configurations:
            definition = root / f'{name}.json'
            definition.write_text(json.dumps({'description': f'Synthetic {name}', 'source': {'path': str(sources), 'format': 'json', 'keys': ['mcpServers', name]}, 'enabled': True, 'start_allowed': True, 'stateful': True}))
            cli('gateway', 'add', '--server', name, '--input', definition)
        rpc = RPC(binary, env, ('--trust', 'strict'))
        model_calls = []
        try:
            rpc.request('initialize', {'protocolVersion': '2024-11-05', 'capabilities': {}, 'clientInfo': {'name': 'synthetic-evaluation', 'version': '1'}})
            catalog = rpc.request('tools/list', {})
            assert len(catalog['tools']) == 7
            startup = measure(catalog)

            def call(name, args):
                result = rpc.tool(name, args)
                model_calls.append({'tool': name, **measure(result), 'is_error': result.get('isError', False)})
                return result

            found = call('tools_find', {})
            assert not found.get('isError') and not events()
            discovery = measure(found)
            described = call('tools_describe', {'server': 'catalog0', 'tool': 'tool_00', 'refresh': True})
            assert not described.get('isError'), described
            selected = json.loads(described['content'][0]['text'])['data']
            schema = json.loads(selected['schema_json'])
            selected_spec = measure(described)
            before_cached = len(events())
            for _ in range(3):
                repeated = call('tools_describe', {'server': 'catalog0', 'tool': 'tool_00'})
                assert repeated == described
            assert len(events()) == before_cached
            cli('gateway', 'grant', '--server', 'catalog0', '--tool', 'tool_00', '--root', root, '--read-only')
            lease = ''
            valid_sizes = []
            for i in range(3):
                args = {'server': 'catalog0', 'tool': 'tool_00', 'root': str(root), 'expected_schema_hash': selected['schema_hash'], 'arguments': {'message': 'same-task', 'offset': i * 2, 'limit': 2}}
                if lease:
                    args['lease'] = lease
                result = call('tools_call', args)
                assert not result.get('isError'), result
                assert result['structuredContent']['rows'] == [{'id': j, 'value': f'record-{j}'} for j in range(i * 2, i * 2 + 2)]
                assert json.loads(result['content'][0]['text']) == result['structuredContent']
                token = result['_meta']['recalldock_lease']
                assert not lease or token == lease
                lease = token
                valid_sizes.append(measure(result))
            before_invalid = len([e for e in events() if e['method'] == 'tools/call'])
            invalid = call('tools_call', {'server': 'catalog0', 'tool': 'tool_00', 'root': str(root), 'lease': lease, 'expected_schema_hash': selected['schema_hash'], 'arguments': {'message': {'not': 'a string'}, 'offset': 0, 'limit': 2}})
            after_invalid = len([e for e in events() if e['method'] == 'tools/call'])
            invalid_rejected = bool(invalid.get('isError')) and before_invalid == after_invalid
            assert not call('tools_release', {'lease': lease}).get('isError')
            assert not call('work_checkpoint', {'root': str(root), 'lane': 'recovery', 'expected': 0, 'checkpoint': {'goal': 'Synthetic task recovery', 'summary': 'Selected recovery point', 'details': 'Selected detail, never a transcript'}}).get('isError')
            recovery_start = len(model_calls)
            assert not call('work_find', {'root': str(root), 'query': 'Selected recovery point'}).get('isError')
            recovered = call('work_read', {'root': str(root), 'lane': 'recovery'})
            assert not recovered.get('isError') and 'Selected detail' not in packed(recovered)
            recovery_calls = len(model_calls) - recovery_start
            gateway_events = events()
        finally:
            rpc.close()
        direct = []
        for name, config in configurations.items():
            requests = [{'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocolVersion': '2024-11-05', 'capabilities': {}, 'clientInfo': {'name': 'direct-evaluation', 'version': '1'}}}, {'jsonrpc': '2.0', 'id': 2, 'method': 'tools/list', 'params': {}}]
            r = subprocess.run([config['command'], *config['args']], input=''.join(json.dumps(x) + '\n' for x in requests), cwd=root, env=env, capture_output=True, text=True, timeout=30, check=True)
            rows = {x['id']: x['result'] for x in map(json.loads, r.stdout.splitlines())}
            direct.extend(dict(tool, name=name + '__' + tool['name']) for tool in rows[2]['tools'])
            if name == 'catalog0':
                assert rows[2]['tools'][0] == schema, {'direct': rows[2]['tools'][0], 'cached': schema}
        report = {'format': 1, 'synthetic_only': True, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'estimator': 'recalldock-response-estimate-v1: 12 + ceil(ASCII runes/4) + non-ASCII runes; not billing tokens', 'real_host_tokens_or_billing_measured': False, 'fixed_gateway_tools': 7, 'startup_catalog': startup, 'direct_catalog_tools': len(direct), 'direct_catalog': measure({'tools': direct}), 'tools_find': discovery, 'exact_tools_describe': selected_spec, 'cached_describe_starts_backend': False, 'backend_starts': sum(e['method'] == 'start' for e in gateway_events), 'backend_tools_list_calls': sum(e['method'] == 'tools/list' for e in gateway_events), 'backend_tool_calls': sum(e['method'] == 'tools/call' for e in gateway_events), 'schema_validation_rejects_invalid_backend_arguments': invalid_rejected, 'same_grant_reused_without_reapproval': True, 'same_lease_reused': True, 'valid_tool_results': valid_sizes, 'native_content_error_structured_contract_preserved': True, 'task_quality': {'selected_rows_correct': True, 'work_recovery_correct': True}, 'work_recovery_tool_calls': recovery_calls, 'model_tool_calls': model_calls, 'knowledge_database_created': (root / 'data/recalldock.db').exists()}
        if a.compare:
            old = json.loads(a.compare.read_text())
            report['compare'] = {key: {'before': old[key], 'after': report[key]} for key in ('startup_catalog', 'exact_tools_describe', 'backend_starts', 'backend_tools_list_calls', 'schema_validation_rejects_invalid_backend_arguments', 'work_recovery_tool_calls')}
        a.report.write_text(json.dumps(report, indent=2) + '\n')
        print(json.dumps({k: v for k, v in report.items() if k != 'model_tool_calls'}, indent=2))


if __name__ == '__main__':
    main()
