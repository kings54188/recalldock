#!/usr/bin/env python3
"""Observe real local servers with stale cloud/release configuration. Synthetic only."""
import argparse,http.server,json,os,selectors,socket,subprocess,tempfile,threading,time,urllib.request
from pathlib import Path

def main():
 p=argparse.ArgumentParser();p.add_argument('--binary',required=True,type=Path);p.add_argument('--work-dir',required=True,type=Path);p.add_argument('--report',required=True,type=Path);a=p.parse_args();binary=a.binary.resolve()
 hits=[]
 class Observer(http.server.BaseHTTPRequestHandler):
  def do_GET(self):hits.append(('GET',self.path));self.send_response(200);self.end_headers();self.wfile.write(b'{}')
  def do_POST(self):hits.append(('POST',self.path));self.send_response(200);self.end_headers();self.wfile.write(b'{}')
  def log_message(self,*args):pass
 observer=http.server.ThreadingHTTPServer(('127.0.0.1',0),Observer);thread=threading.Thread(target=observer.serve_forever,daemon=True);thread.start()
 processes=[]
 try:
  with tempfile.TemporaryDirectory(prefix='offline-',dir=a.work_dir) as tmp:
   root=Path(tmp);data=root/'data';out=f'http://127.0.0.1:{observer.server_port}'
   env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env.update(RECALLDOCK_DATA_DIR=str(data),RECALLDOCK_CLOUD_AUTOSYNC='1',RECALLDOCK_CLOUD_TOKEN='synthetic-token',RECALLDOCK_CLOUD_SERVER=out,RECALLDOCK_RELEASE_API_URL=out,RECALLDOCK_AGENT_CLI='must-not-execute')
   env['HOME']=str(root)
   def run(*args,ok=True):
    r=subprocess.run([str(binary),*args],env=env,cwd=root,capture_output=True,text=True,timeout=15);assert (r.returncode==0)==ok,(args,r.stderr[:180]);return r
   for args in [('cloud','status'),('cloud','serve'),('sync','--cloud','--project','p'),('conflicts','scan','--semantic'),('sync','--','--cloud'),('conflicts','scan','--','--semantic')]:
    r=run(*args,ok=False);assert 'local-only' in r.stderr and not data.exists()
   run('version');run('cloud','--help');run('cloud','serve','--help');assert not data.exists()
   # A fake runner would leave this marker if any private snippet were delegated.
   runner=root/'must-not-execute';marker=root/'runner-invoked';runner.write_text('#!/bin/sh\ntouch "'+str(marker)+'"\n');runner.chmod(0o700);env['PATH']=str(root)+os.pathsep+env.get('PATH','')
   run('store','init','--json');assert (data/'recalldock.db').is_file()
   with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
   http_process=subprocess.Popen([str(binary),'serve',str(port)],env=env,cwd=root,stdout=subprocess.PIPE,stderr=subprocess.PIPE);processes.append(http_process)
   base=f'http://127.0.0.1:{port}';deadline=time.monotonic()+15
   while True:
    try:
     with urllib.request.urlopen(base+'/health',timeout=1) as response:assert response.status==200;break
    except OSError:
     if time.monotonic()>deadline:raise AssertionError('local HTTP startup timeout')
     time.sleep(.05)
   with urllib.request.urlopen(base+'/sync/status',timeout=5) as response:
    status=json.load(response);assert status['enabled'] is False and status['reason_code']=='local_only'
   mcp=subprocess.Popen([str(binary),'mcp'],env=env,cwd=root,stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE);processes.append(mcp)
   selector=selectors.DefaultSelector();selector.register(mcp.stdout,selectors.EVENT_READ)
   mcp.stdin.write((json.dumps({'jsonrpc':'2.0','id':1,'method':'initialize','params':{'protocolVersion':'2024-11-05','capabilities':{},'clientInfo':{'name':'offline-smoke','version':'1'}}})+'\n').encode());mcp.stdin.flush();assert selector.select(10);assert 'result' in json.loads(mcp.stdout.readline())
   run('save','Local synthetic fact','Do not send memory to cloud','--project','synthetic')
   time.sleep(1.25)
   assert not hits,hits;assert not marker.exists()
   report={'synthetic_only':True,'blocked_commands_no_data_or_requests':6,'stale_release_environment_ignored':True,'stale_cloud_environment_no_upload_polling_observed':True,'real_servers':2,'local_sync_status_explicit':True,'external_runner_not_executed':True,'observer_http_requests':len(hits),'observation_seconds_after_local_write':1.25,'limits':'finite observation plus production source/handler regression, not an OS-level network sandbox'}
   selector.close()
 finally:
  for process in processes:
   process.terminate()
   try:process.communicate(timeout=5)
   except subprocess.TimeoutExpired:process.kill();process.communicate(timeout=5)
  observer.shutdown();observer.server_close();thread.join(timeout=3)
 a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))
if __name__=='__main__':main()
