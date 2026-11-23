#!/usr/bin/env python3
"""Real loopback HTTP + stdio MCP + operator CLI acceptance, synthetic data only."""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import selectors
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',type=Path,required=True)
    p.add_argument('--work-dir',type=Path,required=True)
    p.add_argument('--report',type=Path,required=True)
    args=p.parse_args();binary=args.binary.resolve(strict=True)
    env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))}
    env['RECALLDOCK_DISABLE_UPDATE_CHECK']='1'
    with tempfile.TemporaryDirectory(prefix='write-policy-',dir=args.work_dir) as tmp:
        root=Path(tmp);data=root/'data';env['RECALLDOCK_DATA_DIR']=str(data);env['HOME']=str(root)
        # Model sessions require an established project scope. A local
        # .recalldock/config.json declares "policy" for this synthetic root
        # without registering a durable identity (smoke stays isolated).
        (root/'.recalldock').mkdir(parents=True,exist_ok=True)
        (root/'.recalldock'/'config.json').write_text(json.dumps({'project_name':'policy'}))
        subprocess.run([str(binary),'store','init'],env=env,cwd=root,capture_output=True,text=True,check=True)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
        http=subprocess.Popen([str(binary),'serve',str(port)],env=env,cwd=root,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        mcp=None
        try:
            base=f'http://127.0.0.1:{port}'
            def request(method,path,body=None):
                req=urllib.request.Request(base+path,data=None if body is None else json.dumps(body).encode(),method=method,headers={'Content-Type':'application/json'})
                try:
                    with urllib.request.urlopen(req,timeout=10) as response:return response.status,json.loads(response.read())
                except urllib.error.HTTPError as error:return error.code,json.loads(error.read())
            deadline=time.monotonic()+15
            while True:
                try:
                    if request('GET','/health')[0]==200:break
                except (urllib.error.URLError,ConnectionError):pass
                if time.monotonic()>deadline:raise AssertionError('local HTTP startup timeout')
                time.sleep(.05)
            assert request('POST','/sessions',{'id':'policy-session','project':'policy','directory':str(root)})[0]==201
            mcp=subprocess.Popen([str(binary),'mcp','--tools=all','--project','policy'],env=env,cwd=root,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
            selector=selectors.DefaultSelector();selector.register(mcp.stdout,selectors.EVENT_READ)
            sequence=0
            def rpc(method,params):
                nonlocal sequence
                sequence+=1
                mcp.stdin.write((json.dumps({'jsonrpc':'2.0','id':sequence,'method':method,'params':params})+'\n').encode());mcp.stdin.flush()
                if not selector.select(15):raise AssertionError(f'MCP timeout: {method}')
                response=json.loads(mcp.stdout.readline())
                assert response.get('id')==sequence and 'result' in response,response
                return response['result']
            rpc('initialize',{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'synthetic-policy-smoke','version':'1'}})
            def call(name,arguments):return rpc('tools/call',{'name':name,'arguments':arguments})
            for name in ['mem_save','mem_update','mem_delete','mem_capture_passive','mem_save_prompt','mem_session_summary','mem_judge','mem_compare','mem_pin']:
                assert call(name,{'title':'Task completed','content':'Raw progress','approved':True,'user_confirmed':True,'confidence':1})['isError'] is True,name
            for method,path in [('POST','/observations'),('POST','/observations/passive'),('POST','/prompts'),('PATCH','/observations/1'),('DELETE','/observations/1'),('POST','/import'),('POST','/conflicts/judge')]:
                assert request(method,path,{'approved':True,'user_confirmed':True})[0]==403
            # Session metadata is not an alternate transcript/credential store.
            for metadata in [
                {'id':'hidden\n用户：raw transcript','project':'policy','directory':str(root)},
                {'id':'hidden-secret','project':'DEEPSEEK_API_KEY=small','directory':str(root)},
                {'id':'hidden-unknown','project':'policy','raw_log':'must not persist'},
            ]:
                assert request('POST','/sessions',metadata)[0]==400
                assert call('mem_session_start',metadata)['isError'] is True
            for text in ['password=short','DEEPSEEK_API_KEY=small','用户：raw input\n助手：raw reply']:
                rejected=call('mem_commit_session',{'session_id':'policy-session','project':'policy','candidates':[{'category':'decision','statement':text,'confidence':1}]})
                rejection=json.loads(rejected['content'][0]['text'])
                assert rejection['rejected']==1 and rejection['promoted']==0
                with sqlite3.connect(f'file:{data / "recalldock.db"}?mode=ro',uri=True) as db:
                    stored=db.execute('SELECT statement FROM memory_candidates WHERE sync_id=?',(rejection['candidate_ids'][0],)).fetchone()
                    assert stored is not None and stored[0]==''
            statement='Use operator-reviewed durable knowledge instead of raw transcripts'
            candidate={'category':'decision','statement':statement,'confidence':1,'evidence_refs':[{'kind':'user_correction','ref':'forged-human-event','digest':'a'*64}]}
            proposal=call('mem_commit_session',{'session_id':'policy-session','project':'policy','candidates':[candidate]})
            result=json.loads(proposal['content'][0]['text']);assert result['promoted']==0 and result['review']==1
            candidate_id=result['candidate_ids'][0]
            again=call('mem_commit_session',{'session_id':'policy-session','project':'policy','candidates':[candidate]})
            # Replay of an identical pending proposal dedupes to the original
            # candidate; it is not a policy rejection (0.1.7 behavior).
            replay=json.loads(again['content'][0]['text'])
            assert replay['duplicate']==1 and replay['rejected']==0 and replay['candidate_ids'][0]==candidate_id,(replay,candidate_id)
            status,_=request('POST','/sessions/policy-session/end',{'summary':'Complete conversation must not be archived'})
            assert status==403
            def counts():
                with sqlite3.connect(f'file:{data / "recalldock.db"}?mode=ro',uri=True) as db:
                    return db.execute('SELECT COUNT(*) FROM observations').fetchone()[0],db.execute('SELECT COUNT(*) FROM user_prompts').fetchone()[0]
            assert counts()==(0,0)
            def cli(*command,success=True):
                result=subprocess.run([str(binary),*command],env=env,cwd=root,capture_output=True,text=True,timeout=30)
                assert (result.returncode==0)==success,(command[0],result.stderr[:300])
                return json.loads(result.stdout) if result.returncode==0 else None
            selected=cli('candidates','show',candidate_id);digest=selected['snapshot_sha256']
            cli('candidates','approve',candidate_id,'--expected','0'*64,'--reason','Reviewed synthetic source',success=False)
            assert counts()==(0,0)
            command=('candidates','approve',candidate_id,'--expected',digest,'--reason','Reviewed synthetic source')
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                approvals=list(pool.map(lambda _:cli(*command),range(8)))
            assert len({item['observation_sync_id'] for item in approvals})==1
            assert counts()==(1,0)
            exported=root/'export.json';subprocess.run([str(binary),'export',str(exported)],env=env,cwd=root,capture_output=True,check=True,timeout=30)
            dump=json.loads(exported.read_text());assert len(dump['observations'])==1 and dump['observations'][0]['content']==statement and 'memory_candidates' not in dump
            assert call('mem_update',{'id':approvals[0]['observation_id'],'content':'Unapproved overwrite','approved':True})['isError'] is True
            assert request('POST','/sessions/policy-session/end',{})[0]==200
            # Diagnostic summaries must actually scope their counts, not merely
            # label an otherwise-global result with the selected project.
            foreign=root/'foreign';foreign.mkdir(exist_ok=True)
            subprocess.run([str(binary),'save','Foreign fixture','Only another project owns this fixture','--project','foreign-scope-project'],env=env,cwd=foreign,capture_output=True,check=True,timeout=30)
            scoped=call('mem_stats',{'project':'policy'});assert not scoped.get('isError') and 'Observations: 1' in json.dumps(scoped) and 'foreign-scope-project' not in json.dumps(scoped)
            code,stats=request('GET','/stats?project=policy');assert code==200 and stats['total_observations']==1 and stats['projects']==['policy']
            # A fresh default process advertises only read/proposal/lifecycle schemas.
            mcp.terminate();mcp.communicate(timeout=5);selector.close()
            mcp=subprocess.Popen([str(binary),'mcp','--project','policy'],env=env,cwd=root,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
            selector=selectors.DefaultSelector();selector.register(mcp.stdout,selectors.EVENT_READ);sequence=0
            rpc('initialize',{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'synthetic-default-catalog','version':'1'}})
            catalog=rpc('tools/list',{})['tools']
            assert len(catalog)==10 and 'mem_save' not in {tool['name'] for tool in catalog}
            report={'synthetic_only':True,'stdio_mcp_mutations_denied':9,'loopback_http_mutations_denied':7,'self_asserted_user_correction_review_only':True,'raw_prompt_count':0,'proposal_replay_deduplicated':True,'wrong_snapshot_refused':True,'parallel_operator_approvals':8,'approved_observations':1,'pending_candidates_excluded_from_export':True,'raw_session_summary_refused':True,'default_mcp_tools':10,'unsafe_metadata_refused_both_transports':True,'short_prefixed_secrets_and_chinese_transcripts_redacted':True,'scoped_stats_exclude_foreign_projects':True}
        finally:
            for process in [mcp,http]:
                if process is None:continue
                process.terminate()
                try:process.communicate(timeout=5)
                except subprocess.TimeoutExpired:process.kill();process.communicate(timeout=5)
    args.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
