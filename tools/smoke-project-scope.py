#!/usr/bin/env python3
"""Public MCP project scope, personal classification and metadata isolation."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import selectors
import sqlite3
import subprocess
import tempfile


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',required=True,type=Path)
    p.add_argument('--work-dir',required=True,type=Path)
    p.add_argument('--report',required=True,type=Path)
    a=p.parse_args();binary=a.binary.resolve(strict=True)
    with tempfile.TemporaryDirectory(prefix='project-scope-',dir=a.work_dir) as temp:
        base=Path(temp);env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env.update(HOME=str(base),RECALLDOCK_DATA_DIR=str(base/'data'))
        def cli(*args,cwd=base):
            r=subprocess.run([str(binary),*map(str,args)],cwd=cwd,env=env,capture_output=True,text=True,timeout=30);assert r.returncode==0,r.stderr[:400];return r.stdout
        projects=[]
        for name in ['own','foreign']:
            root=base/name;root.mkdir();project=json.loads(cli('projects','create','--name',name))['id'];projects.append((root,project))
            cli('projects','bind',project,'--directory',root)
            cli('save','PERSONAL_'+name,'shared-personal-scope-probe','--scope','personal',cwd=root)
        def call(tool,args,cwd,pinned=False,project=None):
            command=[str(binary),'mcp','--tools=mem_search,mem_context,mem_stats,mem_session_start,mem_session_end,mem_commit_session']
            command += ['--project', project] if project else (['--require-project'] if pinned else [])
            process=subprocess.Popen(command,cwd=cwd,env=env,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,bufsize=1)
            selector=selectors.DefaultSelector();selector.register(process.stdout,selectors.EVENT_READ);seq=0
            try:
                def rpc(method,params):
                    nonlocal seq
                    seq+=1;process.stdin.write(json.dumps(dict(jsonrpc='2.0',id=seq,method=method,params=params))+'\n');process.stdin.flush();assert selector.select(20),'MCP timeout'
                    response=json.loads(process.stdout.readline());assert response.get('id')==seq and 'result' in response,response;return response['result']
                rpc('initialize',dict(protocolVersion='2024-11-05',capabilities={},clientInfo=dict(name='scope-smoke',version='1')))
                return rpc('tools/call',dict(name=tool,arguments=args))
            finally:
                selector.close();process.terminate();process.communicate(timeout=10)
        for tool in ['mem_search','mem_context','mem_stats']:
            args=dict(scope='personal') if tool!='mem_stats' else {}
            if tool=='mem_search':args.update(query='shared-personal-scope-probe',detail='l0')
            if tool=='mem_context':args['mode']='index'
            for pinned in [False,True]:
                result=call(tool,args,projects[0][0],pinned);text=json.dumps(result)
                assert not result.get('isError') and projects[1][1] not in text and 'PERSONAL_foreign' not in text,(tool,pinned,text[:1600])
                if tool=='mem_stats':assert 'Observations: 1' in text
                else:assert 'PERSONAL_own' in text
            broad=dict(args,all_projects=True)
            result=call(tool,broad,base);text=json.dumps(result);assert not result.get('isError') and projects[1][1] in text
            result=call(tool,broad,projects[0][0],True);assert result.get('isError') and projects[1][1] not in json.dumps(result)
            result=call(tool,dict(args,project='not-existing-history'),base);text=json.dumps(result)
            assert result.get('isError') and all(project not in text for _,project in projects)
        own_root,own=projects[0];foreign_root,foreign=projects[1]
        for session,root in [('scope-own-session',own_root),('scope-foreign-session',foreign_root)]:
            result=call('mem_session_start',dict(id=session,directory=str(root)),root)
            assert not result.get('isError'),result
        proposal=dict(session_id='scope-foreign-session',candidates=[dict(category='decision',statement='Keep session writes within their process activation',confidence=0.8)])
        blocked=[('mem_commit_session',proposal),('mem_commit_session',dict(proposal,project=foreign)),
                 ('mem_session_end',dict(id='scope-foreign-session')),
                 ('mem_session_start',dict(id='blocked-start',directory=str(foreign_root))),
                 ('mem_session_start',dict(id='blocked-implicit-start')),
                 ('mem_session_start',dict(id='scope-foreign-session',directory=str(own_root)))]
        for tool,args in blocked:
            result=call(tool,args,foreign_root,project=own)
            assert result.get('isError') and 'project_scope_mismatch' in json.dumps(result),(tool,result)
        with sqlite3.connect(base/'data/recalldock.db') as db:
            assert db.execute('SELECT ended_at FROM sessions WHERE id=?',('scope-foreign-session',)).fetchone()==(None,)
            assert db.execute("SELECT count(*) FROM sessions WHERE id IN ('blocked-start','blocked-implicit-start')").fetchone()==(0,)
            assert db.execute('SELECT count(*) FROM memory_candidates WHERE project=?',(foreign,)).fetchone()==(0,)
        result=call('mem_commit_session',dict(proposal,session_id='scope-own-session'),foreign_root,project=own)
        assert not result.get('isError'),result
        for _ in range(2):
            result=call('mem_session_end',dict(id='scope-own-session'),foreign_root,project=own)
            assert not result.get('isError'),result
            assert json.loads(result['content'][0]['text'])['project']==own,result
        result=call('mem_session_start',dict(id='allowed-start',directory=str(own_root)),foreign_root,project=own)
        assert not result.get('isError'),result
    report=dict(synthetic_only=True,binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),directory_and_process_scope_preserved=True,personal_does_not_broaden_scope=True,statistics_scoped=True,explicit_all_unpinned=True,pinned_all_refused=True,unknown_project_does_not_enumerate_history=True,session_writes_respect_process_scope=True,foreign_session_and_candidates_unchanged=True,matching_session_writes_preserved=True,session_end_reports_stored_owner=True)
    a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
