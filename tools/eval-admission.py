#!/usr/bin/env python3
"""Evaluate local candidate admission through real MCP using only synthetic bound projects."""
import argparse
import hashlib
import json
import os
from pathlib import Path
from typing import Any
import runpy
import sqlite3
import subprocess
import tempfile


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary', type=Path, required=True)
    p.add_argument('--work-dir', type=Path, required=True)
    p.add_argument('--report', type=Path, required=True)
    p.add_argument('--compare', type=Path)
    p.add_argument('--require-pass', action='store_true', help='exit nonzero if any admission safety/quality case fails')
    a = p.parse_args()
    binary = a.binary.resolve(strict=True)
    MCP = runpy.run_path(str(Path(__file__).with_name('smoke-read-budget.py')))['MCP']
    results = []
    with tempfile.TemporaryDirectory(prefix='candidate-eval-', dir=a.work_dir) as tmp:
        base = Path(tmp)
        root, foreign_root = base / 'project', base / 'foreign'
        root.mkdir()
        foreign_root.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
        env.update(HOME=str(base), RECALLDOCK_DATA_DIR=str(base / 'data'))

        def cli(*args, cwd=root, decode=False, payload=None) -> Any:
            r = subprocess.run([str(binary), *map(str, args)], cwd=cwd, env=env, input=payload, capture_output=True, text=True, timeout=30)
            assert r.returncode == 0, (args, r.stderr)
            return json.loads(r.stdout) if decode else r.stdout

        project = cli('projects', 'create', '--name', 'Synthetic admission project', decode=True)['id']
        foreign_project = cli('projects', 'create', '--name', 'Synthetic foreign project', decode=True)['id']
        for cwd, identity, session in ((root, project, 'admission'), (foreign_root, foreign_project, 'foreign')):
            cli('projects', 'bind', identity, '--directory', cwd, cwd=cwd)
            cli('host-event', 'codex', cwd=cwd, payload=json.dumps({'session_id': session, 'cwd': str(cwd), 'hook_event_name': 'SessionStart'}))
            cli('save', 'Reviewed synthetic source', 'Keep local evidence checks mandatory for durable automatic admission', '--type', 'decision', '--topic', 'source-policy', cwd=cwd)
        exchange = base / 'exchange.json'
        cli('export', exchange)
        sources = json.loads(exchange.read_text())['observations']
        source = next(row for row in sources if row.get('project') == project)
        foreign_source = next(row for row in sources if row.get('project') == foreign_project)
        rpc = MCP(binary, env, root)
        other = MCP(binary, env, foreign_root)
        try:
            def selected(client, row):
                r = client.call('mem_get_observation', {'id': row['id'], 'detail': 'l0'})
                assert not r.get('isError'), r
                state = json.loads(r['content'][0]['text'])
                return {'kind': 'observation', 'ref': state['observation_sync_id'], 'digest': state['snapshot_sha256']}

            ref, foreign_ref = selected(rpc, source), selected(other, foreign_source)
            file = root / 'evidence.txt'
            file.write_text('Reviewed synthetic source bytes for an explicitly registered file\n')
            registered = cli('resources', 'register', '--project', project, '--observation-id', source['id'], '--file', file, decode=True)['identity']
            file_ref = {'kind': 'file', 'ref': registered['source_ref'], 'digest': registered['sha256']}
            pending = other.call('mem_commit_session', {'session_id': 'codex-foreign', 'project': foreign_project, 'candidates': [{'category': 'decision', 'statement': 'Keep the foreign project admission under its own explicit scope', 'confidence': 0.9, 'evidence_refs': [foreign_ref]}]})
            assert not pending.get('isError'), pending
            foreign_candidate = json.loads(pending['content'][0]['text'])['candidate_ids'][0]
            cli('candidates', 'policy', '--all', '--mode', 'tiered')

            def submit(key, statement, refs, confidence=0.99, category='decision', topic='', expected_auto=False, expected_rejected=False):
                candidate = {'category': category, 'statement': statement, 'confidence': confidence, 'evidence_refs': refs}
                if topic:
                    candidate['proposed_topic_key'] = topic
                r = rpc.call('mem_commit_session', {'session_id': 'codex-admission', 'project': project, 'candidates': [candidate]})
                assert not r.get('isError'), r
                value = json.loads(r['content'][0]['text'])
                entry = value['results'][0]
                results.append({'case': key, 'confidence': confidence, 'expected_auto': expected_auto, 'expected_rejected': expected_rejected, 'status': entry['status'], 'reason': entry.get('reason', ''), 'promoted': value['promoted'], 'review': value['review'], 'rejected': value['rejected'], 'duplicate': value['duplicate']})
                return value

            submit('verified-observation', 'Use the selected reviewed source for automatic admission', [ref], confidence=0.9, expected_auto=True)
            with sqlite3.connect(f"file:{base / 'data/recalldock.db'}?mode=ro", uri=True) as db:
                foreign_status = db.execute('SELECT status FROM memory_candidates WHERE sync_id=?', (foreign_candidate,)).fetchone()[0]
            submit('verified-file', 'Keep registered source bytes linked to durable procedures', [file_ref], confidence=0.9, category='procedure', expected_auto=True)
            submit('threshold-eligible', 'Retain current evidence at the configured eligibility boundary', [ref], confidence=0.85, expected_auto=True)
            submit('low-confidence', 'Review uncertain interpretations despite an existing source', [ref], confidence=0.5)
            submit('below-threshold', 'Review candidates below the current explicit tier eligibility floor', [ref], confidence=0.84)
            submit('non-durable-tier', 'Prefer a particular presentation for this local session', [ref], category='user_preference')
            submit('no-evidence', 'Require a source even when the model claims certainty', [])
            submit('claimed-test', 'Require trusted verification rather than self reported test receipts', [{'kind': 'test', 'ref': 'synthetic-self-reported', 'digest': 'a' * 64}])
            submit('wrong-digest', 'Reject source snapshots with an unrelated digest', [dict(ref, digest='0' * 64)])
            submit('foreign-evidence', 'Keep source verification within the selected project', [foreign_ref])
            submit('mixed-evidence', 'Verify every source in a mixed evidence proposal', [ref, {'kind': 'source', 'ref': 'unverified historical note'}])
            submit('missing-source', 'Keep inaccessible historical source claims for review', [{'kind': 'observation', 'ref': 'obs-missing', 'digest': 'a' * 64}])
            submit('topic-conflict', 'Replace the current source policy with a contradictory rule', [ref], topic='source-policy')
            submit('routine-progress', '已完成本次临时测试', [ref], expected_rejected=True)
            submit('sensitive', 'api_key=sk-test-abcdefghijklmnopqrstuvwxyz123456', [ref], expected_rejected=True)
            duplicate = submit('duplicate-existing-candidate', 'Use the selected reviewed source for automatic admission', [ref], confidence=0.9)
            assert duplicate['duplicate'] == 1 and duplicate['promoted'] == 0
            file.write_text('Changed bytes after source registration\n')
            submit('changed-file', 'Review evidence after registered file bytes change', [file_ref])
            patch = base / 'patch.json'
            patch.write_text(json.dumps({'content': 'Updated synthetic source policy'}))
            state = cli('memory', 'show', source['id'], decode=True)
            cli('memory', 'update', source['id'], '--expected', state['snapshot_sha256'], '--input', patch, '--reason', 'Synthetic source revision')
            submit('stale-snapshot', 'Review a candidate whose selected source revision changed', [ref])
            current_ref = selected(rpc, source)
            state = cli('memory', 'show', source['id'], decode=True)
            cli('memory', 'archive', source['id'], '--expected', state['snapshot_sha256'], '--reason', 'Synthetic archived source')
            submit('archived-source', 'Do not trust an archived supporting source merely due to high confidence', [current_ref])
            guarded = [{'category': 'decision', 'statement': 'Keep session and project guards independent from candidate validation', 'confidence': 0.9, 'evidence_refs': [current_ref]}]
            for extra in ({'approval': True}, {'author': 'synthetic'}, {'session': 'forged'}):
                r = rpc.call('mem_commit_session', dict(session_id='codex-admission', project=project, candidates=guarded, **extra))
                assert r.get('isError') and next(iter(extra)) in json.dumps(r), r
            unregistered = rpc.call('mem_commit_session', {'session_id': 'codex-not-registered', 'project': project, 'candidates': guarded})
            assert unregistered.get('isError') and 'session' in json.dumps(unregistered).lower(), unregistered
            wrong_scope = rpc.call('mem_commit_session', {'session_id': 'codex-admission', 'project': foreign_project, 'candidates': guarded})
            assert wrong_scope.get('isError') and 'project' in json.dumps(wrong_scope).lower(), wrong_scope
        finally:
            other.close()
            rpc.close()
        with sqlite3.connect(f"file:{base / 'data/recalldock.db'}?mode=ro", uri=True) as db:
            duplicated_formal = db.execute("SELECT count(*) FROM observations WHERE content='Use the selected reviewed source for automatic admission' AND project=?", (project,)).fetchone()[0]
            conflicts_overwritten = db.execute('SELECT content FROM observations WHERE id=?', (source['id'],)).fetchone()[0] != 'Updated synthetic source policy'
            integrity = db.execute('PRAGMA integrity_check').fetchone()[0]
            fk_violations = len(db.execute('PRAGMA foreign_key_check').fetchall())
        eligible = [r for r in results if r['expected_auto']]
        admitted = [r for r in results if r['promoted']]
        wrong = [r for r in admitted if not r['expected_auto']]
        missing = [r for r in eligible if not r['promoted']]
        correct = sum((r['promoted'] > 0) == r['expected_auto'] and (r['rejected'] > 0) == r['expected_rejected'] for r in results)
        report = {'format': 1, 'synthetic_only': True, 'binary_sha256': hashlib.sha256(binary.read_bytes()).hexdigest(), 'real_mcp_admission_path': True, 'native_host_dispatch_tested': False, 'source_checks_are_not_semantic_truth_verification': True, 'production_policy_changed': False, 'policy_mode': 'tiered', 'tested_current_eligibility_floor': 0.85, 'threshold_is_not_a_calibrated_production_default': True, 'cases': results, 'case_count': len(results), 'automatic_admissions': len(admitted), 'automatic_admission_precision': (len(admitted) - len(wrong)) / len(admitted) if admitted else 1, 'eligible_memory_recall': (len(eligible) - len(missing)) / len(eligible), 'unexpected_admissions': [r['case'] for r in wrong], 'eligible_memory_misses': [r['case'] for r in missing], 'manual_review_count': sum(r['review'] for r in results), 'rejected_count': sum(r['rejected'] for r in results), 'duplicate_formal_records': max(duplicated_formal - 1, 0), 'topic_conflict_overwritten': conflicts_overwritten, 'foreign_pending_promoted_by_other_project_commit': foreign_status == 'promoted', 'model_approval_author_session_cannot_escalate': True, 'unregistered_session_and_wrong_project_denied': True, 'integrity': integrity, 'foreign_key_violations': fk_violations, 'all_passed': correct == len(results) and foreign_status == 'review' and not conflicts_overwritten and duplicated_formal == 1 and integrity == 'ok' and fk_violations == 0}
        if a.compare:
            old = json.loads(a.compare.read_text())
            report['compare'] = {key: {'before': old[key], 'after': report[key]} for key in ('automatic_admission_precision', 'eligible_memory_recall', 'unexpected_admissions', 'manual_review_count', 'foreign_pending_promoted_by_other_project_commit', 'duplicate_formal_records', 'topic_conflict_overwritten', 'all_passed')}
        a.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
        print(json.dumps({k: v for k, v in report.items() if k != 'cases'}, ensure_ascii=False, indent=2))
        if a.require_pass and not report['all_passed']:
            raise SystemExit(1)


if __name__ == '__main__':
    main()
