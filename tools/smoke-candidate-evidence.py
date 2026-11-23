#!/usr/bin/env python3
"""Real operator/MCP checked-admission flow using isolated synthetic evidence."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import selectors
import subprocess
import tempfile


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',required=True,type=Path)
    p.add_argument('--work-dir',required=True,type=Path)
    p.add_argument('--report',required=True,type=Path)
    a=p.parse_args();binary=a.binary.resolve(strict=True)
    with tempfile.TemporaryDirectory(prefix='candidate-evidence-',dir=a.work_dir) as temp:
        base=Path(temp);root=base/'project';root.mkdir()
        env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env.update(HOME=str(base),RECALLDOCK_DATA_DIR=str(base/'data'))
        def run(*args,decode=False,input=None):
            r=subprocess.run([str(binary),*map(str,args)],cwd=root,env=env,input=input,capture_output=True,text=True,timeout=30)
            assert r.returncode==0,(args[0],r.stderr[:400]);return json.loads(r.stdout) if decode else r.stdout
        project=run('projects','create','--name','Synthetic evidence project',decode=True)['id']
        run('projects','bind',project,'--directory',root)
        hook=run('host-event','codex',input=json.dumps(dict(session_id='synthetic-evidence',cwd=str(root),hook_event_name='SessionStart')))
        assert 'codex-synthetic-evidence' in hook
        run('save','Registration owner','Reviewed synthetic source registration BODY_ONLY_NOT_IN_L0')
        exchange=base/'exchange.json';run('export',exchange);owner=json.loads(exchange.read_text())['observations'][0]
        file=root/'source.txt';file.write_text('Synthetic versioned evidence bytes')
        identity=run('resources','register','--project',project,'--observation-id',owner['id'],'--file',file,decode=True)['identity']
        run('candidates','policy','--all','--mode','checked')
        process=subprocess.Popen([str(binary),'mcp','--tools=agent','--require-project'],cwd=root,env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,bufsize=1)
        selector=selectors.DefaultSelector();selector.register(process.stdout,selectors.EVENT_READ);seq=0
        try:
            def rpc(method,params):
                nonlocal seq
                seq+=1;process.stdin.write(json.dumps(dict(jsonrpc='2.0',id=seq,method=method,params=params))+'\n');process.stdin.flush()
                assert selector.select(20),'MCP response timeout'
                result=json.loads(process.stdout.readline());assert result.get('id')==seq and 'result' in result,result
                return result['result']
            rpc('initialize',dict(protocolVersion='2024-11-05',capabilities={},clientInfo=dict(name='synthetic-evidence-smoke',version='1')))
            view=rpc('tools/call',dict(name='mem_get_observation',arguments=dict(id=owner['id'],detail='l0')))
            assert not view.get('isError') and 'BODY_ONLY_NOT_IN_L0' not in json.dumps(view)
            snapshot=json.loads(view['content'][0]['text'])
            assert len(snapshot['snapshot_sha256'])==64 and snapshot['observation_sync_id']==owner['sync_id']
            def submit(statement,evidence):
                r=rpc('tools/call',dict(name='mem_commit_session',arguments=dict(session_id='codex-synthetic-evidence',project=project,candidates=[dict(category='decision',statement=statement,confidence=0.9,evidence_refs=evidence)])))
                assert not r.get('isError'),r
                return json.loads(r['content'][0]['text'])
            false=submit('Keep self reported command outcomes pending for review',[dict(kind='test',ref='claimed-test-pass',digest='a'*64)])
            assert false['promoted']==0 and false['review']==1 and false['results'][0]['reason']=='evidence_not_verified'
            ref=dict(kind='file',ref=identity['source_ref'],digest=identity['sha256'])
            verified=submit('Preserve registered source snapshots for bounded verification',[ref]);assert verified['promoted']==1
            observation=submit('Retain selected knowledge snapshots as explicit provenance',[dict(kind='observation',ref=snapshot['observation_sync_id'],digest=snapshot['snapshot_sha256'])]);assert observation['promoted']==1
            file.write_text('Changed version after registration')
            changed=submit('Refresh stale source claims before automatic admission',[ref]);assert changed['promoted']==0 and changed['review']==1
        finally:
            selector.close();process.terminate();process.communicate(timeout=10)
        run('export',exchange);exported=json.loads(exchange.read_text())
        assert len(exported['observations'])==3 and len(exported['resource_dependencies'])==2 and len(exported['dependencies'])==1
        assert not any(row['content'].startswith('Keep self reported') or row['content'].startswith('Refresh stale') for row in exported['observations'])
    report=dict(synthetic_only=True,binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),real_mcp_checked_policy=True,untrusted_test_claim_pending=True,registered_bound_file_admitted=True,selected_snapshot_metadata_without_body=True,selected_observation_snapshot_admitted=True,changed_file_pending=True,resource_and_memory_dependencies_preserved=True,no_unverified_fact_persisted=True,source_verification_is_not_semantic_truth=True)
    a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
