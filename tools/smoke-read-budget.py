#!/usr/bin/env python3
"""Shared SQLite read budget across two MCP processes and one HTTP process."""
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


class MCP:
    def __init__(self,binary,env,cwd):
        self.p=subprocess.Popen([str(binary),'mcp'],env=env,cwd=cwd,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        self.selector=selectors.DefaultSelector();self.selector.register(self.p.stdout,selectors.EVENT_READ);self.sequence=0
        initialized=self.rpc('initialize',{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'budget-smoke','version':'1'}})
        assert 'BODY_ONLY_' not in json.dumps(initialized)
        self.rpc('tools/list',{})
    def rpc(self,method,params):
        self.sequence+=1;self.p.stdin.write((json.dumps({'jsonrpc':'2.0','id':self.sequence,'method':method,'params':params})+'\n').encode());self.p.stdin.flush()
        if not self.selector.select(15):raise AssertionError('MCP timeout')
        response=json.loads(self.p.stdout.readline());assert response.get('id')==self.sequence and 'result' in response,response
        return response['result']
    def call(self,name,args):return self.rpc('tools/call',{'name':name,'arguments':args})
    def close(self):
        self.p.terminate()
        try:self.p.communicate(timeout=5)
        except subprocess.TimeoutExpired:self.p.kill();self.p.communicate(timeout=5)
        self.selector.close()


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--binary',type=Path,required=True);p.add_argument('--work-dir',type=Path,required=True);p.add_argument('--report',type=Path,required=True);args=p.parse_args()
    binary=args.binary.resolve(strict=True);env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env['RECALLDOCK_DISABLE_UPDATE_CHECK']='1'
    with tempfile.TemporaryDirectory(prefix='shared-read-',dir=args.work_dir) as tmp:
        base=Path(tmp);root=base/'project';root.mkdir();outside=base/'unbound';outside.mkdir();data=base/'data';env['RECALLDOCK_DATA_DIR']=str(data)
        def cli(*command,cwd=root):
            result=subprocess.run([str(binary),*command],env=env,cwd=cwd,capture_output=True,text=True,timeout=30);assert result.returncode==0,result.stderr[:500]
            return result.stdout
        project=json.loads(cli('projects','create','--name','Synthetic read budget'))['id'];cli('projects','bind',project,'--directory',str(root))
        for i in range(16):cli('save',f'Budget fact {i}',f'BODY_ONLY_{i} '+('synthetic local data '*200),'--type','decision','--topic',f'budget/{i}')
        exported=base/'seed.json';cli('export',str(exported));observations=json.loads(exported.read_text())['observations'];ids=[item['id'] for item in observations]
        # Strict schemas enlarge both catalog receipts before the concurrent phase.
        active=json.loads(cli('projects','activate',project,'--token-budget','20000'));budget_id=active['read_budget_id'];assert active['loads_memory'] is False
        env['RECALLDOCK_READ_BUDGET_ID']=budget_id
        clients=[];http=None
        def state():
            with sqlite3.connect(f'file:{data / "recalldock.db"}?mode=ro',uri=True) as db:
                used,limit=db.execute('SELECT used,token_limit FROM read_budgets WHERE id=?',(budget_id,)).fetchone()
                count=db.execute('SELECT COUNT(*) FROM read_budgets').fetchone()[0]
                return used,limit,count
        try:
            a=MCP(binary,env,outside);clients.append(a);after_first=state()[0]
            b=MCP(binary,env,outside);clients.append(b);after_second=state()[0]
            assert after_first>0 and after_second>after_first
            with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
            http=subprocess.Popen([str(binary),'serve',str(port)],env=env,cwd=outside,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
            def get(path):
                try:
                    with urllib.request.urlopen(f'http://127.0.0.1:{port}'+path,timeout=10) as response:return response.status,response.read().decode()
                except urllib.error.HTTPError as error:return error.code,error.read().decode()
            deadline=time.monotonic()+15
            while True:
                try:
                    if get('/health')[0]==200:break
                except (urllib.error.URLError,ConnectionError):pass
                if time.monotonic()>deadline:raise AssertionError('HTTP timeout')
                time.sleep(.05)
            search=a.call('mem_search',{'query':'Budget fact','project':project,'limit':2})
            assert not search.get('isError') and 'BODY_ONLY_' not in json.dumps(search)
            first=a.call('mem_get_observation',{'id':ids[0]});assert not first.get('isError') and 'BODY_ONLY_' in json.dumps(first)
            reset=b.call('mem_get_observation',{'id':ids[0],'session_id':'attempted-reset'})
            assert reset.get('isError') and 'invalid_arguments' in json.dumps(reset) and 'BODY_ONLY_' not in json.dumps(reset)
            duplicate=b.call('mem_get_observation',{'id':ids[0]})
            assert not duplicate.get('isError') and 'already_loaded' in json.dumps(duplicate) and 'BODY_ONLY_' not in json.dumps(duplicate)
            status,body=get(f'/observations/{ids[0]}');assert status==200 and 'already_loaded' in body and 'BODY_ONLY_' not in body
            # An operator revision is a new receipt key, not an incorrectly cached old body.
            selected=json.loads(cli('memory','show',str(ids[0])))
            patch=base/'revision-patch.json';patch.write_text(json.dumps({'content':'BODY_REVISION_TWO '+('synthetic local data '*200)}))
            cli('memory','update',str(ids[0]),'--expected',selected['snapshot_sha256'],'--input',str(patch),'--reason','Reviewed synthetic revision')
            revised=b.call('mem_get_observation',{'id':ids[0]});assert not revised.get('isError') and 'BODY_REVISION_TWO' in json.dumps(revised)
            with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
                calls=[pool.submit(a.call,'mem_get_observation',{'id':ids[1]}),pool.submit(b.call,'mem_get_observation',{'id':ids[2]}),pool.submit(get,f'/observations/{ids[3]}')]
                results=[call.result() for call in calls]
            assert not results[0].get('isError') and not results[1].get('isError') and results[2][0]==200, ([(r.get('isError'), json.dumps(r)[:300]) for r in results[:2]], results[2][0], state())
            exhausted_id=None
            for id_ in ids[4:]:
                result=a.call('mem_get_observation',{'id':id_})
                if result.get('isError'):
                    assert 'BODY_ONLY_' not in json.dumps(result);exhausted_id=id_;break
            assert exhausted_id is not None,'fixture did not reach the shared payload limit'
            bypass=b.call('mem_get_observation',{'id':exhausted_id,'session_id':'another-session','read_budget_id':'fabricated','token_budget':1000000})
            assert bypass.get('isError') and 'BODY_ONLY_' not in json.dumps(bypass)
            status,body=get(f'/observations/{exhausted_id}?read_budget_id=fabricated&session_id=another');assert status==400 and 'BODY_ONLY_' not in body
            status,body=get(f'/observations/{exhausted_id}');assert status==429 and 'BODY_ONLY_' not in body
            used,limit,count=state();assert 0<used<=limit and count==1
            report={'synthetic_only':True,'cooperating_processes':3,'catalog_accounted_per_mcp_instance':True,'default_search_index_no_full_body':True,'cross_process_cross_transport_dedup':True,'new_revision_reactivated':True,'concurrent_reads_within_limit':True,'model_parameters_cannot_reset':True,'read_budget_rows':count,'estimated_used':used,'estimated_limit':limit,'unknown_or_exhausted_payload_not_returned':True}
        finally:
            for client in clients:client.close()
            if http is not None:
                http.terminate()
                try:http.communicate(timeout=5)
                except subprocess.TimeoutExpired:http.kill();http.communicate(timeout=5)
    args.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
