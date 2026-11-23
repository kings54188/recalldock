#!/usr/bin/env python3
"""Exercise real CLI knowledge links and hash-selected local attachments."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',required=True,type=Path)
    p.add_argument('--work-dir',required=True,type=Path)
    p.add_argument('--report',required=True,type=Path)
    a=p.parse_args();binary=a.binary.resolve(strict=True)
    with tempfile.TemporaryDirectory(prefix='obsidian-graph-',dir=a.work_dir) as temp:
        root=Path(temp);env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env.update(HOME=str(root),RECALLDOCK_DATA_DIR=str(root/'data'))
        def run(*args,ok=True,decode=False):
            r=subprocess.run([str(binary),*map(str,args)],cwd=root,env=env,capture_output=True,text=True,timeout=30)
            assert (r.returncode==0)==ok,(args[0],r.stderr[:400])
            return json.loads(r.stdout) if decode else r.stdout
        config=root/'.recalldock';config.mkdir();(config/'config.json').write_text(json.dumps({'project_name':'graph'}))
        for title in ['Source','Target']:
            run('save',title,'Synthetic graph fixture '+title,'--project','graph')
        exchange=root/'exchange.json';run('export',exchange);rows=json.loads(exchange.read_text())['observations'];source=next(x for x in rows if x['title']=='Source');target=next(x for x in rows if x['title']=='Target')
        selected=run('memory','show',source['id'],decode=True);patch=root/'patch.json';patch.write_text(json.dumps(dict(content='Follow [[memory:'+target['sync_id']+'|target evidence]] and [[User note]].')))
        run('memory','update',source['id'],'--expected',selected['snapshot_sha256'],'--input',patch,'--reason','Synthetic stable body reference')
        run('review','link',source['id'],target['id'],'--reason','Synthetic derived fact')
        assets=root/'assets';assets.mkdir();file=assets/'diagram.png';body=b'Synthetic attachment bytes';file.write_bytes(body)
        run('resources','register','--project','graph','--observation-id',source['id'],'--file',file,decode=True)
        vault=root/'vault';vault.mkdir();(vault/'User note.md').write_text('Existing user note')
        args=['obsidian-export','--vault',vault,'--project','graph','--attachment-root',assets]
        run(*args)
        managed=vault/'recalldock';notes=list(managed.rglob('*.md'));links=0
        for note in notes:
            text=note.read_text();assert '[[memory:' not in text
            for match in re.findall(r'\[\[([^\]]+)\]\]',text):
                destination=match.split('|',1)[0].split('#',1)[0];path=vault/destination
                if not path.suffix:path=path.with_suffix('.md')
                assert path.is_file(),('broken projected link',destination)
                links+=1
        assert links>=6
        copied=managed/'_attachments'/(hashlib.sha256(body).hexdigest()+'.png');assert copied.read_bytes()==body
        previous={p:p.read_bytes() for p in notes};file.write_bytes(b'changed synthetic source')
        run(*args,ok=False);assert all(p.read_bytes()==text for p,text in previous.items())
        file.write_bytes(body);copied.write_bytes(b'user changed attachment');run(*args,ok=False);assert copied.read_bytes()==b'user changed attachment'
        run('export',exchange);assert len(json.loads(exchange.read_text())['observations'])==2
        assert (vault/'User note.md').read_text()=='Existing user note'
    report=dict(synthetic_only=True,binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),real_cli_stable_body_links=True,knowledge_dependencies=True,registered_attachment_bytes=True,all_generated_links_resolve=True,source_hash_change_refused=True,manual_attachment_preserved=True,native_user_link_preserved=True,no_export_created_knowledge=True)
    a.report.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report))


if __name__=='__main__':main()
