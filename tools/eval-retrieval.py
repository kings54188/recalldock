#!/usr/bin/env python3
"""Fixed bilingual lexical retrieval evaluation on isolated synthetic CLI data."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import statistics
import subprocess
import tempfile
from typing import Any


def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',type=Path,required=True)
    p.add_argument('--work-dir',type=Path,required=True)
    p.add_argument('--report',type=Path,required=True)
    p.add_argument('--fixture',type=Path,default=Path(__file__).resolve().parents[1]/'testdata/retrieval/bilingual-v1.json')
    p.add_argument('--compare',type=Path,default=None,help='previous report JSON to compute deltas against (same fixture only)')
    a=p.parse_args();binary=a.binary.resolve(strict=True);raw=a.fixture.read_bytes();fixture=json.loads(raw)
    report: dict[str, Any]=dict(format=1,synthetic_only=True,fixture_sha256=hashlib.sha256(raw).hexdigest(),binary_sha256=hashlib.sha256(binary.read_bytes()).hexdigest(),semantic_translation_tested=False,actual_host_context_or_billing_measured=False)
    with tempfile.TemporaryDirectory(prefix='retrieval-eval-',dir=a.work_dir) as temp:
        root=Path(temp);env={k:v for k,v in os.environ.items() if not k.startswith(('RECALLDOCK_','ENGRAM_'))};env.update(HOME=str(root),RECALLDOCK_DATA_DIR=str(root/'data'))
        def run(*args,decode=True) -> Any:
            r=subprocess.run([str(binary),*map(str,args)],cwd=root,env=env,capture_output=True,text=True,timeout=30)
            if r.returncode:raise AssertionError((args[0],r.stderr[:400]))
            return json.loads(r.stdout) if decode else r.stdout
        for item in fixture['records']:
            run('save',item['title'],item['content']+' BODY_NOT_IN_L0','--project','eval-fixture','--type','decision','--topic','eval/'+item['key'],decode=False)
        # An identical lexical match in another project must never enter results.
        run('save','consistent backup foreign','Foreign scope fixture','--project','other-fixture',decode=False)
        export=root/'export.json';run('export',export,decode=False)
        before=json.loads(export.read_text());ids={row['topic_key'].removeprefix('eval/'):row['id'] for row in before['observations'] if row.get('project')=='eval-fixture'}
        config=root/'.recalldock';config.mkdir(exist_ok=True);(config/'config.json').write_text(json.dumps(dict(project_name='eval-fixture')))
        for item in fixture['records']:
            if item.get('archive'):
                selected=run('memory','show',ids[item['key']])
                run('memory','archive',ids[item['key']],'--expected',selected['snapshot_sha256'],'--reason','Fixed synthetic retired item')
        details=[];checks=[]
        for case in fixture['queries']:
            for detail in ['l0','l1']:
                r=run('search',case['query'],'--project','eval-fixture','--detail',detail,'--limit','3','--token-budget','800','--match-mode',case.get('match_mode','all'),'--json')
                retrieved=[row['id'] for row in r['results']];expected={ids[k] for k in case['expected']};hits=len(expected.intersection(retrieved))
                recall=hits/len(expected) if expected else float(not retrieved)
                precision=hits/len(retrieved) if retrieved else float(not expected)
                packed=json.dumps(r,ensure_ascii=False)
                budget_ok=r['budget']['used']<=r['budget']['limit']==800
                text_present=all(isinstance(row.get('selected_content'),str) and row['selected_content'] for row in r['results'])
                body_safe=detail!='l0' or 'BODY_NOT_IN_L0' not in packed
                suggestion_ok=True
                if 'suggested_query' in case:
                    suggestion_ok=r.get('did_you_mean')==dict(query=case['suggested_query'],id=ids[case['suggested_key']])
                if case.get('no_suggestion'):
                    suggestion_ok=r.get('did_you_mean') is None
                raw_score_ok=all(isinstance(row.get('raw_bm25'),(int,float)) and row['raw_bm25']<=0 for row in r['results'] if row.get('score_source')=='fts5_bm25')
                passed=recall==1 and precision==1 and budget_ok and text_present and body_safe and suggestion_ok and raw_score_ok
                checks.append(passed)
                details.append(dict(query=case['query'],match_mode=case.get('match_mode','all'),language=case['language'],detail=detail,recall_at_3=recall,precision_of_returned_top_3=precision,selected_estimated_tokens=r['budget']['used'],response_utf8_bytes=len(packed.encode()),latency_ms=r['latency_ms'],passed=passed))
        run('export',export,decode=False);after=json.loads(export.read_text())
        report.update(corpus_records=len(fixture['records']),query_cases=len(fixture['queries']),search_calls=len(details),recall_at_3=statistics.mean(r['recall_at_3'] for r in details),precision_of_returned_top_3=statistics.mean(r['precision_of_returned_top_3'] for r in details),mean_selected_estimated_tokens=statistics.mean(r['selected_estimated_tokens'] for r in details),mean_response_utf8_bytes=statistics.mean(r['response_utf8_bytes'] for r in details),search_created_knowledge_records=len(after['observations'])-len(before['observations']),all_passed=all(checks) and len(after['observations'])==len(before['observations']),cases=details)
    if a.compare is not None:
        previous=json.loads(a.compare.read_text())
        if previous.get('fixture_sha256')!=report['fixture_sha256']:
            raise SystemExit('compare refused: fixtures differ')
        keys=('recall_at_3','precision_of_returned_top_3','mean_selected_estimated_tokens','mean_response_utf8_bytes','search_calls','all_passed')
        report['compare']=dict(previous_binary_sha256=previous.get('binary_sha256',''),deltas={k:report[k]-previous[k] for k in keys if isinstance(report.get(k),(int,float)) and isinstance(previous.get(k),(int,float))},previous_all_passed=previous.get('all_passed'),fixture_matches=True)
        old_cases={(c['query'],c['detail'],c.get('match_mode','all')):c for c in previous.get('cases',[])}
        report['compare']['changed_cases']=[dict(query=c['query'],detail=c['detail'],was=old_cases[(c['query'],c['detail'],c.get('match_mode','all'))]['passed'],now=c['passed']) for c in details if (c['query'],c['detail'],c.get('match_mode','all')) in old_cases and old_cases[(c['query'],c['detail'],c.get('match_mode','all'))]['passed']!=c['passed']]
    a.report.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
    print(json.dumps({k:v for k,v in report.items() if k!='cases'},ensure_ascii=False))
    if not report['all_passed']:raise SystemExit(1)


if __name__=='__main__':main()
