#!/usr/bin/env python3
"""Real packaged-runtime setup/launcher/upgrade rollback smoke in isolated HOME."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import tomllib


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--release',type=Path,required=True)
    p.add_argument('--work-dir',type=Path,required=True)
    p.add_argument('--report',type=Path,required=True)
    a=p.parse_args();release=a.release.resolve(strict=True);binary=release/'runtime/recalldock'
    hashes=lambda:{str(x.relative_to(release)):hashlib.sha256(x.read_bytes()).hexdigest() for x in release.rglob('*') if x.is_file()}
    before=hashes()
    with tempfile.TemporaryDirectory(prefix='install-e2e-',dir=a.work_dir) as tmp:
        root=Path(tmp);home=root/'home';home.mkdir();(home/'.codex').mkdir()
        config=home/'.codex/config.toml';original='# retain local policy\n[mcp_servers.original]\ncommand="original"\nenabled=true\n';config.write_text(original)
        env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env.update(HOME=str(home),RECALLDOCK_DATA_DIR=str(root/'knowledge-must-not-exist'))
        def run(*args,ok=True):
            r=subprocess.run([str(binary),*map(str,args)],cwd=root,env=env,capture_output=True,text=True,timeout=30)
            assert (r.returncode==0)==ok,(args[0],r.stderr[:400]);return r.stdout
        selection=['setup','--release',release,'--home',home,'--host','codex','--host','claude-code','--host','dsh']
        preview=json.loads(run(*selection));assert len(preview['files'])==5 and config.read_text()==original
        assert not (home/'.local').exists()
        backup=root/'private-backup'
        run(*selection,'--apply','--expected-sha256','0'*64,'--backup',backup,ok=False)
        assert not backup.exists()
        run(*selection,'--apply','--expected-sha256',preview['plan_sha256'],'--backup',backup)
        installed=config.read_bytes()
        assert '# retain local policy' in installed.decode() and tomllib.loads(installed.decode())['mcp_servers']['original']['enabled'] is True
        launched=subprocess.run([str(home/'.local/bin/recalldock'),'version'],cwd=root,env=env,capture_output=True,text=True,check=True)
        assert json.loads((release/'release.json').read_text())['version'] in launched.stdout
        assert json.loads(run(*selection))['files']==[]
        assert not (root/'knowledge-must-not-exist').exists()
        config.write_text('later user edit')
        run('setup','--rollback','--backup',backup,ok=False)
        assert config.read_text()=='later user edit' and (home/'.local/bin/recalldock').exists()
        config.write_bytes(installed)
        run('setup','--rollback','--backup',backup)
        assert config.read_text()==original and not (home/'.local/bin/recalldock').exists()
        assert (backup.stat().st_mode & 0o777)==0o700
        # Upgrade an existing automatic policy through the packaged entry point.
        # Retain the stronger policy when rolling back launchers/configuration.
        data=root/'existing-data';data.mkdir();policy=data/'candidate-admission.json'
        legacy_policy='{"mode":"checked"}\n';policy.write_text(legacy_policy)
        upgrade=[*selection,'--data-dir',data]
        preview=json.loads(run(*upgrade))
        assert preview['files'][0]['path']==str(policy) and preview['files'][0]['retain_on_rollback']
        assert policy.read_text()==legacy_policy
        upgrade_backup=root/'upgrade-backup'
        run(*upgrade,'--apply','--expected-sha256',preview['plan_sha256'],'--backup',upgrade_backup)
        hardened=policy.read_bytes();assert json.loads(hardened)==dict(version=2,mode='checked-v2')
        assert json.loads(run(*upgrade))['files']==[]
        rollback=json.loads(run('setup','--rollback','--backup',upgrade_backup))
        assert rollback['retained']==1 and policy.read_bytes()==hardened
        assert config.read_text()==original and not (home/'.local/bin/recalldock').exists()
        assert sorted(x.name for x in data.iterdir())==['candidate-admission.json']
        saved=json.loads((upgrade_backup/'rollback.json').read_text())
        selected=next(x for x in saved['files'] if x['path']==str(policy))
        assert (upgrade_backup/selected['backup']).read_text()==legacy_policy
        assert hashes()==before,'setup mutated the immutable release (including bytecode)'
    report=dict(synthetic_only=True,real_packaged_runtime=True,preview_no_writes=True,stale_plan_refused=True,unrelated_config_and_comments_preserved=True,launcher_version_matches=True,idempotent=True,no_knowledge_or_operations_db=True,guarded_rollback=True,private_backup=True,release_immutable=True,legacy_policy_hardened_first=True,security_policy_retained_on_rollback=True,original_policy_backed_up=True)
    a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
