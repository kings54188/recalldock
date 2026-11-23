#!/usr/bin/env python3
"""Operator revision loop with independent processes; synthetic temp data only."""
import argparse,concurrent.futures,json,os,shutil,sqlite3,subprocess,tempfile
from pathlib import Path

def main():
 p=argparse.ArgumentParser();p.add_argument('--binary',required=True,type=Path);p.add_argument('--work-dir',required=True,type=Path);p.add_argument('--report',required=True,type=Path);a=p.parse_args();binary=a.binary.resolve()
 with tempfile.TemporaryDirectory(prefix='revisions-',dir=a.work_dir) as tmp:
  base=Path(tmp);root=base/'project';root.mkdir();data=base/'data';outside=base/'outside';outside.mkdir()
  env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env['RECALLDOCK_DATA_DIR']=str(data)
  def run(*args,cwd=root,ok=True,environment=env):
   result=subprocess.run([str(binary),*args],env=environment,cwd=cwd,capture_output=True,text=True,timeout=30);assert (result.returncode==0)==ok,(args[:2],result.stderr[:300]);return result
  project=json.loads(run('projects','create','--name','Revision fixture').stdout)['id'];run('projects','bind',project,'--directory',str(root))
  seed=('save','Durable fact','ORIGINAL_BODY_ONLY','--type','decision','--topic','design/fixture');run(*seed)
  exchange=base/'exchange.json';run('export',str(exchange));id_=json.loads(exchange.read_text())['observations'][0]['id']
  selected=json.loads(run('memory','show',str(id_)).stdout);assert 'state' not in selected
  assert 'ORIGINAL_BODY_ONLY' in run('memory','show',str(id_),'--full').stdout
  run(*seed);assert len(json.loads(run('memory','history',str(id_)).stdout))==1
  run('save','Durable fact','UNREVIEWED_OVERWRITE','--type','decision','--topic','design/fixture',ok=False)
  patch=base/'patch.json';patch.write_text(json.dumps({'content':'UPDATED_BODY_ONLY'}));command=('memory','update',str(id_),'--expected',selected['snapshot_sha256'],'--input',str(patch),'--reason','Reviewed synthetic source')
  run(*command,cwd=outside,ok=False)
  with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
   results=list(pool.map(lambda _:subprocess.run([str(binary),*command],cwd=root,env=env,capture_output=True,text=True,timeout=30),range(8)))
  assert sum(r.returncode==0 for r in results)==1
  assert all('snapshot changed' in r.stderr for r in results if r.returncode!=0)
  current=json.loads(run('memory','show',str(id_)).stdout);assert current['revision_count']==2
  no_op=('memory','update',str(id_),'--expected',current['snapshot_sha256'],'--input',str(patch),'--reason','Confirm same state')
  run(*no_op);assert len(json.loads(run('memory','history',str(id_)).stdout))==2
  assert 'ORIGINAL_BODY_ONLY' not in run('memory','version',str(id_),'1').stdout
  assert 'ORIGINAL_BODY_ONLY' in run('memory','version',str(id_),'1','--full').stdout
  assert json.loads(run('search','ORIGINAL_BODY_ONLY','--project',project,'--json').stdout)['results']==[]
  run('export',str(exchange));assert 'ORIGINAL_BODY_ONLY' not in exchange.read_text()
  backup=json.loads(run('backup','--json').stdout);restored=base/'restored';restored.mkdir();shutil.copyfile(backup['path'],restored/'recalldock.db')
  restored_env=dict(env,RECALLDOCK_DATA_DIR=str(restored),RECALLDOCK_EXPECT_STORE_ID=backup['store_id'])
  assert 'ORIGINAL_BODY_ONLY' in run('memory','version',str(id_),'1','--full',environment=restored_env).stdout
  run('delete',str(id_));run('memory','history',str(id_),ok=False)
  assert len(json.loads(run('memory','history',str(id_),'--include-deleted').stdout))==3
  run('delete',str(id_),'--hard')
  with sqlite3.connect(f'file:{data}/recalldock.db?mode=ro',uri=True) as db:
   assert db.execute('SELECT count(*) FROM observation_versions WHERE observation_id=?',(id_,)).fetchone()[0]==0
   assert db.execute('SELECT count(*) FROM observation_edit_audits WHERE observation_id=?',(id_,)).fetchone()[0]==0
   assert db.execute('PRAGMA integrity_check').fetchone()[0]=='ok'
  report={'synthetic_only':True,'operator_create_no_upsert':True,'duplicate_create_no_version_noise':True,'history_metadata_default_full_explicit':True,'registered_edit_requires_binding':True,'parallel_cli_updates':8,'cas_successes':1,'stale_rejections':7,'no_op_no_extra_revision':True,'old_versions_not_current_search_or_export':True,'online_backup_preserves_versions':True,'soft_delete_hidden_by_default':True,'explicit_hard_delete_purges_versions_and_edit_audits':True,'sqlite_integrity':'ok'}
 a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))
if __name__=='__main__':main()
