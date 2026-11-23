#!/usr/bin/env python3
"""Exercise one registered local LSP through a real gateway with isolated grants.

No host sessions are restarted, no production registry is written, and only the
gateway's own process tree is observed. This is protocol QA, not native-host UI QA.
"""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

from importlib.util import module_from_spec, spec_from_file_location


def children(pid):
    parents = {}
    for entry in Path('/proc').iterdir():
        if not entry.name.isdecimal():
            continue
        try:
            stat = (entry / 'stat').read_text().rsplit(')', 1)[1].split()
            parents[int(entry.name)] = (int(stat[1]), stat[19])
        except (OSError, IndexError, ValueError):
            continue
    owned = {pid}
    for _ in range(16):
        found = {p for p, (parent, _) in parents.items() if parent in owned}
        if found <= owned:
            break
        owned |= found
    return {p: parents[p][1] for p in owned - {pid} if p in parents}


def still_running(owned):
    # A leaked grandchild can be reparented and disappear from the gateway's
    # current tree. Check recorded process identities too, without signaling.
    found = []
    for pid, started in owned.items():
        try:
            stat = (Path('/proc') / str(pid) / 'stat').read_text().rsplit(')', 1)[1].split()
            if stat[19] == started and stat[0] != 'Z':
                found.append(pid)
        except (OSError, IndexError):
            pass
    return found


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--registry', type=Path, required=True)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--host', choices=('codex', 'dsh'), required=True)
    parser.add_argument('--server', required=True)
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--expected-error', help='explicit negative regression before configuration repair')
    args = parser.parse_args()
    source = Path(__file__).resolve().parents[1]
    spec = spec_from_file_location('gateway_smoke', source / 'tools/smoke-gateway.py')
    module = module_from_spec(spec)
    spec.loader.exec_module(module)
    raw = args.registry.read_bytes()
    original = json.loads(raw)['servers'][args.server]
    assert original['host'] == args.host and original['stateful']
    report = dict(host=args.host, server=args.server, native_host_ui=False)
    # Match GUI DSH's restricted environment; backend declarations must provide
    # their own toolchain path rather than depending on our login shell.
    env = dict(os.environ, PATH=f"{Path.home() / '.local/bin'}:/usr/local/bin:/usr/bin:/bin", PYTHONDONTWRITEBYTECODE='1')
    with tempfile.TemporaryDirectory(prefix='.lsp-smoke-', dir=args.report.parent) as tmp:
        directory = Path(tmp)
        env['RECALLDOCK_DATA_DIR'] = str(directory / 'data')
        registry = directory / 'gateway.json'
        binary = args.binary.resolve(strict=True)

        def cli(*parts):
            r = subprocess.run([str(binary), 'gateway', *map(str, parts), '--registry', str(registry)],
                               env=env, capture_output=True, text=True, timeout=45)
            assert r.returncode == 0, r.stderr[:300]
            return json.loads(r.stdout) if r.stdout.strip() else None

        definition = directory / 'definition.json'
        definition.write_text(json.dumps({k: original[k] for k in ('description', 'source', 'host', 'enabled', 'start_allowed', 'stateful')}))
        cli('add', '--server', args.server, '--input', definition)
        rpc = module.RPC(binary, env, ('--registry', str(registry), '--host', args.host))
        lease = None
        try:
            rpc.request('initialize', dict(protocolVersion='2024-11-05', capabilities={}, clientInfo=dict(name='lsp-qa', version='1')))
            assert len(rpc.request('tools/list', {})['tools']) == 7
            session = json.loads(rpc.tool('tools_find', {})['content'][0]['text'])['data']['session']
            assert not children(rpc.p.pid), 'find started backend'
            cli('discover', '--server', args.server)
            described = rpc.tool('tools_describe', dict(server=args.server, tool='lsp_hover'))
            record = json.loads(described['content'][0]['text'])['data']
            arguments = dict(file=str(source / 'internal/gateway/pool.go'), symbol='NewPool')
            call = dict(server=args.server, tool='lsp_hover', root=str(args.root.resolve(strict=True)),
                        expected_schema_hash=record['schema_hash'], arguments=arguments)
            denied = rpc.tool('tools_call', call)
            assert denied.get('isError') and 'approval_required' in json.dumps(denied)
            assert not children(rpc.p.pid), 'denied call started backend'
            parameters = directory / 'arguments.json'
            parameters.write_text(json.dumps(arguments))
            expiry = (datetime.now(timezone.utc) + timedelta(minutes=4)).isoformat().replace('+00:00', 'Z')
            cli('grant', '--server', args.server, '--tool', 'lsp_hover', '--root', args.root,
                '--session', session, '--arguments', parameters, '--expires', expiry)
            result = rpc.tool('tools_call', call)
            control = json.loads(result['content'][-1]['text'])['recalldock_gateway']
            lease = control['lease']
            owned = children(rpc.p.pid)
            assert owned, 'no real backend process'
            text = '\n'.join(c.get('text', '') for c in result['content'][:-1])
            if args.expected_error:
                assert args.expected_error in text, 'expected backend failure not reproduced'
                report['expected_failure_reproduced'] = args.expected_error
            else:
                assert not result.get('isError') and 'func NewPool(registryPath string, ops *opsstore.Store) *Pool' in text, text[:300]
                again = rpc.tool('tools_call', dict(call, lease=lease))
                assert not again.get('isError')
                assert json.loads(again['content'][-1]['text'])['recalldock_gateway']['lease'] == lease
                assert children(rpc.p.pid) == owned, 'lease did not reuse the same backend processes'
                report['go_signature_and_reuse_verified'] = True
            assert not rpc.tool('tools_release', dict(lease=lease)).get('isError')
            lease = None
            deadline = time.monotonic() + 5
            while (children(rpc.p.pid) or still_running(owned)) and time.monotonic() < deadline:
                time.sleep(.05)
            assert not children(rpc.p.pid) and not still_running(owned), 'release leaked owned subprocesses'
            assert rpc.tool('tools_call', dict(call, lease=control['lease'])).get('isError')
            report.update(model_visible_lease=True, release_and_reap_verified=True, recorded_process_identities_checked=True, restricted_parent_path=True,
                          seven_tools=True, default_denial=True, find_no_start=True)
        finally:
            try:
                if lease is not None:
                    rpc.tool('tools_release', dict(lease=lease))
            finally:
                try:
                    cli('revoke', '--server', args.server, '--tool', 'lsp_hover')
                finally:
                    rpc.close()
    assert args.registry.read_bytes() == raw, 'production registry changed concurrently; inspect without overwriting'
    report.update(ok=True, production_registry_unchanged=True, production_registry_sha256=hashlib.sha256(raw).hexdigest())
    args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps(report, ensure_ascii=False))


if __name__ == '__main__':
    main()
