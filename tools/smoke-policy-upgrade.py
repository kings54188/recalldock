#!/usr/bin/env python3
"""Keep a real legacy MCP runtime alive across a versioned policy transition."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import selectors
import subprocess
import tempfile


class MCP:
    def __init__(self,binary,env,root):
        self.p=subprocess.Popen([str(binary),'mcp','--tools=agent','--require-project'],cwd=root,env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,bufsize=1)
        self.selector=selectors.DefaultSelector();self.selector.register(self.p.stdout,selectors.EVENT_READ);self.seq=0
        self.rpc('initialize',dict(protocolVersion='2024-11-05',capabilities={},clientInfo=dict(name='synthetic-policy-upgrade',version='1')))
    def rpc(self,method,params):
        self.seq+=1;self.p.stdin.write(json.dumps(dict(jsonrpc='2.0',id=self.seq,method=method,params=params))+'\n');self.p.stdin.flush()
        assert self.selector.select(20),'MCP response timeout'
        response=json.loads(self.p.stdout.readline());assert response.get('id')==self.seq and 'result' in response,response;return response['result']
    def submit(self,project,statement,refs,confidence=0.9):
        r=self.rpc('tools/call',dict(name='mem_commit_session',arguments=dict(project=project,session_id='codex-synthetic-upgrade',candidates=[dict(category='decision',statement=statement,confidence=confidence,evidence_refs=refs)])))
        assert not r.get('isError'),r;return json.loads(r['content'][0]['text'])
    def close(self):
        self.selector.close();self.p.terminate();self.p.communicate(timeout=10)


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',type=Path,required=True)
    p.add_argument('--legacy-binary',type=Path,required=True)
    p.add_argument('--work-dir',type=Path,required=True)
    p.add_argument('--report',type=Path,required=True)
    p.add_argument('--mode',choices=['checked','tiered'],default='checked')
    a=p.parse_args();binary=a.binary.resolve(strict=True);legacy=a.legacy_binary.resolve(strict=True)
    with tempfile.TemporaryDirectory(prefix='live-policy-upgrade-',dir=a.work_dir) as temp:
        base=Path(temp);root=base/'project';root.mkdir();data=base/'data'
        env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env.update(HOME=str(base),RECALLDOCK_DATA_DIR=str(data))
        def cli(*args,exe=binary,decode=False,input=None):
            r=subprocess.run([str(exe),*map(str,args)],cwd=root,env=env,input=input,capture_output=True,text=True,timeout=30);assert r.returncode==0,r.stderr[:400];return json.loads(r.stdout) if decode else r.stdout
        project=cli('projects','create','--name','Synthetic policy upgrade',exe=legacy,decode=True)['id']
        cli('projects','bind',project,'--directory',root,exe=legacy)
        cli('host-event','codex',exe=legacy,input=json.dumps(dict(session_id='synthetic-upgrade',cwd=str(root),hook_event_name='SessionStart')))
        legacy_scope=['--all'] if a.mode=='tiered' else []
        cli('candidates','policy',*legacy_scope,'--mode',a.mode,exe=legacy)
        old=MCP(legacy,env,root);new=None
        try:
            claimed=[dict(kind='test',ref='synthetic-unverified-test-claim',digest='a'*64)]
            before=old.submit(project,'Retain bounded synthetic legacy statements for compatibility checks',claimed,confidence=0.99)
            assert before['promoted']==1,'selected legacy runtime did not reproduce the old automatic policy'
            # The new operator command also exercises a real legacy DB upgrade.
            cli('candidates','policy','--all','--mode',a.mode)
            policy=json.loads((data/'candidate-admission.json').read_text());assert policy==dict(version=4,mode='tiered-v4') if a.mode=='tiered' else policy==dict(version=2,mode='checked-v2')
            after=old.submit(project,'Keep later unverified legacy statements pending after the upgrade',claimed)
            assert after['promoted']==0 and after['review']==1 and old.p.poll() is None
            cli('save','Reviewed current source','Use current local snapshots for admission verification')
            exchange=base/'exchange.json';cli('export',exchange);records=json.loads(exchange.read_text())['observations'];assert len(records)==2
            source=next(row for row in records if row['title']=='Reviewed current source')
            snapshot=cli('memory','show',source['id'],decode=True)
            new=MCP(binary,env,root)
            accepted=new.submit(project,'Preserve explicit provenance for newly admitted local decisions',[dict(kind='observation',ref=source['sync_id'],digest=snapshot['snapshot_sha256'])])
            assert accepted['promoted']==1,'new runtime silently lost checked automation'
            cli('export',exchange);assert len(json.loads(exchange.read_text())['observations'])==3
        finally:
            if new:new.close()
            old.close()
    report=dict(synthetic_only=True,binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),legacy_binary_sha256=hashlib.sha256(legacy.read_bytes()).hexdigest(),legacy_vulnerable_behavior_reproduced=True,legacy_process_remained_alive=True,versioned_policy_closed_legacy_auto_admission=True,current_engine_automatic_admission_still_operates=True,policy_mode=a.mode,no_legacy_post_upgrade_unverified_fact=True,existing_records_preserved=True)
    a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
