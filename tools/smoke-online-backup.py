#!/usr/bin/env python3
"""WAL-safe online snapshots under independent writers; restore only to temp copies."""
import argparse,concurrent.futures,hashlib,json,os,shutil,socket,sqlite3,subprocess,tempfile,time,urllib.request
from pathlib import Path

def main():
 p=argparse.ArgumentParser();p.add_argument('--binary',required=True,type=Path);p.add_argument('--work-dir',required=True,type=Path);p.add_argument('--report',required=True,type=Path);a=p.parse_args();binary=a.binary.resolve()
 with tempfile.TemporaryDirectory(prefix='backup-',dir=a.work_dir) as tmp:
  root=Path(tmp);data=root/'data';env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env['RECALLDOCK_DATA_DIR']=str(data);env['HOME']=str(root)
  def run(*args,environment=env,ok=True):
   r=subprocess.run([str(binary),*args],cwd=root,env=environment,capture_output=True,text=True,timeout=30);assert (r.returncode==0)==ok,(args[:1],r.stderr[:250]);return r
  run('backup',ok=False);assert not data.exists();run('backup','--help');assert not data.exists()
  run('store','init','--json');assert (data/'recalldock.db').is_file()
  with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
  server=subprocess.Popen([str(binary),'serve',str(port)],cwd=root,env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
  try:
   deadline=time.monotonic()+15
   while True:
    try:
     with urllib.request.urlopen(f'http://127.0.0.1:{port}/health',timeout=1) as r:assert r.status==200;break
    except OSError:
     if time.monotonic()>deadline:raise AssertionError('backup server startup timeout')
     time.sleep(.05)
   for i in range(4):run('save',f'WAL seed {i}',f'WAL safety fixture {i}','--project','backup-project')
   with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
    jobs=[pool.submit(run,'save',f'Concurrent {i}',f'WAL safety concurrent fixture {i}','--project','backup-project') for i in range(12)]
    backups=[pool.submit(run,'backup','--json') for _ in range(3)]
    for job in jobs:job.result()
    snapshots=[json.loads(job.result().stdout) for job in backups]
   snapshots.append(json.loads(run('backup','--json').stdout))
   assert len({x['path'] for x in snapshots})==4
   counts=[]
   for i,snapshot in enumerate(snapshots):
    path=Path(snapshot['path']);assert path.stat().st_mode&0o777==0o600;assert snapshot['integrity']=='ok';assert hashlib.sha256(path.read_bytes()).hexdigest()==snapshot['sha256'];assert path.stat().st_size==snapshot['bytes']
    with sqlite3.connect(f'file:{path}?mode=ro',uri=True) as db:
     assert db.execute('PRAGMA integrity_check').fetchone()[0]=='ok';assert db.execute('PRAGMA foreign_key_check').fetchall()==[]
     count=db.execute('SELECT count(*) FROM observations').fetchone()[0];assert 4<=count<=16;counts.append(count)
     assert db.execute("SELECT value FROM store_metadata WHERE key='store_id'").fetchone()[0]==snapshot['store_id']
    restored=root/f'restored-{i}';restored.mkdir();shutil.copyfile(path,restored/'recalldock.db')
    restored_env=dict(env,RECALLDOCK_DATA_DIR=str(restored),RECALLDOCK_EXPECT_STORE_ID=snapshot['store_id'])
    stats=json.loads(run('stats','--all','--json',environment=restored_env).stdout);assert stats['stats']['total_observations']==count
    assert len(json.loads(run('search','WAL safety','--all','--json','--limit','20',environment=restored_env).stdout)['results'])==count
   assert counts[-1]==16
   assert not list((data/'backups').glob('*.partial'))
   report={'synthetic_only':True,'live_server':True,'independent_writer_processes':12,'concurrent_backup_processes':3,'snapshots_verified_and_restored':4,'final_observations':16,'snapshot_counts':counts,'hash_and_store_identity_verified':True,'integrity_and_foreign_keys':'ok','restored_cli_stats_and_fts_search':True,'private_file_permissions':True,'no_partial_files':True,'missing_store_not_initialized':True,'no_original_data_accessed':True}
  finally:
   server.terminate()
   try:server.communicate(timeout=5)
   except subprocess.TimeoutExpired:server.kill();server.communicate(timeout=5)
 a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))
if __name__=='__main__':main()
