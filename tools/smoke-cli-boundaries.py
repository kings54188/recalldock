#!/usr/bin/env python3
"""Real CLI tests, synthetic data only; no host installers or user database."""
import argparse,json,os,sqlite3,subprocess,tempfile
from pathlib import Path

def main():
 p=argparse.ArgumentParser();p.add_argument('--binary',type=Path,required=True);p.add_argument('--report',type=Path,required=True);a=p.parse_args();binary=a.binary.resolve()
 with tempfile.TemporaryDirectory(prefix='recalldock-cli-') as tmp:
  base=Path(tmp);root=base/'root';deep=root/'a/b/c';deep.mkdir(parents=True);unbound=base/'unbound';unbound.mkdir();data=base/'data'
  env={k:v for k,v in os.environ.items() if not k.startswith('RECALLDOCK_')};env.update(RECALLDOCK_DATA_DIR=str(data),RECALLDOCK_DISABLE_UPDATE_CHECK='1')
  def run(*args,cwd=unbound,ok=True):
   result=subprocess.run([str(binary),*args],cwd=cwd,env=env,capture_output=True,text=True,timeout=20)
   assert (result.returncode==0)==ok,(args,result.returncode,result.stderr[:250])
   return result
  run('tui',ok=False);assert not data.exists()
  for args in [('mcp','--tools'),('mcp','--unknown'),('stats','--all','--project','old'),('search','needle','--limit','bad'),('context','--project'),('save','Title','Body'),('save','Title','Body','--type'),('save','Title','Body','--typo'),('save','Title','Body','--project','--scope','personal')]:
   run(*args,ok=False);assert not data.exists()
  run('tui','--help');assert not data.exists()
  identity=json.loads(run('projects','create','--name','CLI scope').stdout)['id'];child=json.loads(run('projects','create','--name','Nested').stdout)['id']
  run('projects','bind',identity,'--directory',str(root))
  run('save','Needle title','NEEDLE_BODY_PRIVATE','--type','decision',cwd=deep)
  assert json.loads(run('stats','--json',cwd=deep).stdout)['stats']['total_observations']==1
  assert json.loads(run('stats','--json').stdout)['stats']['total_observations']==0
  assert json.loads(run('stats','--all','--json').stdout)['stats']['total_observations']==1
  assert json.loads(run('search','Needle','--json').stdout)['results']==[]
  assert len(json.loads(run('search','Needle','--all','--json').stdout)['results'])==1
  index=run('context','--project',identity).stdout;assert 'Needle title' in index and 'NEEDLE_BODY_PRIVATE' not in index
  assert 'NEEDLE_BODY_PRIVATE' in run('context','--project',identity,'--mode','full').stdout
  run('projects','bind',child,'--directory',str(root/'a/b'))
  assert json.loads(run('stats','--json',cwd=deep).stdout)['stats']['total_observations']==0
  for command in ['search','context','stats']:
   args=[command];args+=['Needle'] if command=='search' else [];args+=['--all','--project',identity];run(*args,ok=False)
  # Failed session deletion must not erase soft-deleted knowledge or cascade.
  with sqlite3.connect(f'file:{data}/recalldock.db?mode=ro',uri=True) as db: obs,session=db.execute('SELECT id,session_id FROM observations').fetchone()
  run('delete',str(obs),cwd=root);refusal=run('delete','session',session,cwd=root,ok=False)
  assert 'including soft-deleted' in refusal.stderr
  with sqlite3.connect(f'file:{data}/recalldock.db?mode=ro',uri=True) as db:
   assert db.execute('SELECT count(*) FROM observations').fetchone()[0]==1
   assert db.execute('PRAGMA integrity_check').fetchone()[0]=='ok'
  # A legacy explicit local declaration must select the same write/read bucket.
  configured=base/'configured';(configured/'.recalldock').mkdir(parents=True)
  (configured/'.recalldock/config.json').write_text(json.dumps({'project_name':'declared-project'}))
  configured_env=dict(env,RECALLDOCK_DATA_DIR=str(base/'configured-data'))
  write=subprocess.run([str(binary),'save','Configured title','Configured durable fixture'],cwd=configured,env=configured_env,capture_output=True,text=True,timeout=20)
  assert write.returncode==0 and 'declared-project' in write.stdout
  read=subprocess.run([str(binary),'stats','--json'],cwd=configured,env=configured_env,capture_output=True,text=True,timeout=20)
  selected=json.loads(read.stdout);assert selected['project']=='declared-project' and selected['stats']['total_observations']==1
  report={'synthetic_only':True,'non_tty_preflight_no_data':True,'invalid_arguments_no_data':True,'unbound_default_zero':True,'explicit_all_total':True,'nested_binding_overrides_parent':True,'historical_context_index_default':True,'explicit_full_operator_context':True,'project_all_mutually_exclusive':True,'soft_deleted_session_refused_no_cascade':True,'sqlite_integrity':'ok','default_save_unbound_refused_before_data':True,'declared_save_and_default_read_match':True}
  a.report.parent.mkdir(parents=True,exist_ok=True);a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))
if __name__=='__main__':main()
