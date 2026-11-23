#!/usr/bin/env python3
"""Registered project CLI acceptance using temporary synthetic data only."""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tempfile


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary', type=Path, required=True)
    p.add_argument('--work-dir', type=Path, required=True)
    p.add_argument('--report', type=Path, required=True)
    args = p.parse_args()
    binary = args.binary.resolve(strict=True)
    env = {k: v for k, v in os.environ.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
    env['RECALLDOCK_DISABLE_UPDATE_CHECK'] = '1'
    with tempfile.TemporaryDirectory(prefix='project-identity-', dir=args.work_dir) as tmp:
        base = Path(tmp)
        env['HOME'] = str(base)
        data = base / 'data'
        root = base / 'project'
        deep = root / 'a/b/c'
        deep.mkdir(parents=True)

        def run(*command, cwd=base, db=data, success=True, json_output=False):
            result = subprocess.run([str(binary), *command], env=dict(env, RECALLDOCK_DATA_DIR=str(db)), cwd=cwd, capture_output=True, text=True, timeout=30)
            if (result.returncode == 0) != success:
                raise AssertionError(f'{command[0]} exit={result.returncode}: {result.stderr[:500]}')
            return json.loads(result.stdout) if json_output else result

        ledger_root = base / 'ledger-project'
        ledger_root.mkdir()
        cp = base / 'checkpoint.json'
        cp.write_text(json.dumps({'goal': 'Synthetic work', 'summary': 'Selected checkpoint', 'next': ['Verify disk state'], 'details': 'DETAIL_ONLY_MARKER'}))
        run('work', 'checkpoint', '--root', str(ledger_root), '--lane', 'test-lane', '--expected', '0', '--input', str(cp))
        assert not (data / 'recalldock.db').exists(), 'work ledger opened the memory database'
        assert (data / 'operations.db').exists(), 'independent work database missing'
        summary = run('work', 'status', '--root', str(ledger_root), '--lane', 'test-lane').stdout
        assert 'DETAIL_ONLY_MARKER' not in summary and len(summary.encode()) <= 2048
        assert 'DETAIL_ONLY_MARKER' in run('work', 'status', '--root', str(ledger_root), '--lane', 'test-lane', '--full').stdout
        run('work', 'checkpoint', '--root', str(ledger_root), '--lane', 'test-lane', '--expected', '0', '--input', str(cp), success=False)
        assert not (data / 'recalldock.db').exists()
        command = [str(binary), 'work', 'checkpoint', '--root', str(ledger_root), '--lane', 'parallel', '--expected', '0', '--input', str(cp)]
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            attempts = list(pool.map(lambda _: subprocess.run(command, env=dict(env, RECALLDOCK_DATA_DIR=str(data)), cwd=base, capture_output=True, timeout=30), range(8)))
        assert sum(attempt.returncode == 0 for attempt in attempts) == 1
        assert not (data / 'recalldock.db').exists()
        identity = run('projects', 'create', '--name', 'Same name', json_output=True)['id']
        second = run('projects', 'create', '--name', 'Same name', json_output=True)['id']
        assert identity != second
        run('projects', 'bind', identity, '--directory', str(root))
        current = run('projects', 'current', '--directory', str(deep), '--json', json_output=True)
        assert current['project_id'] == identity and current['project_path'] == str(root)
        run('save', 'Registered fixture', 'Synthetic project identity fixture', '--type', 'decision', cwd=deep)
        run('save', 'Wrong target', 'Must be rejected', '--project', second, cwd=deep, success=False)
        exported = base / 'exchange.json'
        run('export', str(exported))
        snapshot = json.loads(exported.read_text())
        assert len(snapshot['observations']) == 1 and snapshot['observations'][0]['project'] == identity
        assert len(snapshot['project_identities']) == 2
        assert 'project_bindings' not in snapshot
        moved = base / 'moved'
        root.rename(moved)
        run('projects', 'current', '--directory', str(moved), '--json', success=False)
        run('projects', 'bind', identity, '--directory', str(moved))
        assert run('projects', 'current', '--directory', str(moved / 'a/b/c'), '--json', json_output=True)['project_id'] == identity
        clone = base / 'clone'
        (clone / '.recalldock').mkdir(parents=True)
        shutil.copy2(moved / '.recalldock/root.json', clone / '.recalldock/root.json')
        run('projects', 'current', '--directory', str(clone), '--json', success=False)
        run('projects', 'bind', identity, '--directory', str(clone))
        assert run('projects', 'current', '--directory', str(moved), '--json', json_output=True)['project_id'] == identity
        assert run('projects', 'current', '--directory', str(clone), '--json', json_output=True)['project_id'] == identity
        root.mkdir()
        assert run('projects', 'current', '--directory', str(root), '--json', json_output=True).get('project_id') != identity
        active = run('projects', 'activate', identity, json_output=True)
        assert active['mode'] == 'read_only' and active['loads_memory'] is False
        run('search', 'Synthetic project identity', '--project', identity, '--json')
        fresh_db = base / 'imported'
        run('import', str(exported), db=fresh_db)
        run('import', str(exported), db=fresh_db)
        assert run('projects', 'activate', identity, db=fresh_db, json_output=True)['project_id'] == identity
        run('save', 'Unbound', 'Must not write into imported history', '--project', identity, db=fresh_db, success=False)
        run('projects', 'current', '--directory', str(moved), '--json', db=fresh_db, success=False)
        # Explicit consolidation must fence old writers after a process restart,
        # preserve a similar unselected key, and accept exact manifest replay.
        run('save', 'Retired fixture', 'Synthetic retirement fixture', '--project', 'legacy-retired')
        run('save', 'Separate fixture', 'Synthetic separator fixture', '--project', 'legacy_retired')
        with sqlite3.connect(f'file:{data}/recalldock.db?mode=ro', uri=True) as db:
            store_id = db.execute("SELECT value FROM store_metadata WHERE key='store_id'").fetchone()[0]
            mapping = base / 'mapping.json'
            mapping.write_text(json.dumps({'version': 1, 'store_id': store_id, 'mappings': [
                {'source': 'legacy-retired', 'target': identity},
            ]}))
            run('projects', 'consolidate', '--mapping', str(mapping))
            assert db.execute("SELECT count(*) FROM observations WHERE project='legacy-retired'").fetchone()[0] == 1
            assert db.execute('SELECT count(*) FROM project_key_retirements').fetchone()[0] == 0
            run('projects', 'consolidate', '--mapping', str(mapping), '--apply')
            assert db.execute("SELECT target FROM project_key_retirements WHERE source='legacy-retired'").fetchone()[0] == identity
            assert db.execute("SELECT count(*) FROM observations WHERE project='legacy-retired'").fetchone()[0] == 0
            assert db.execute('SELECT count(*) FROM observations WHERE project=?', (identity,)).fetchone()[0] == 2
            assert db.execute("SELECT count(*) FROM observations WHERE project='legacy_retired'").fetchone()[0] == 1
            refused = run('save', 'Stale writer', 'Must be rejected', '--project', 'legacy-retired', success=False)
            assert 'project key retired' in refused.stderr
            replay = run('projects', 'consolidate', '--mapping', str(mapping), '--apply')
            assert 'Keys retired: 0' in replay.stdout
            run('save', 'Separate still writable', 'Synthetic unselected key', '--project', 'legacy_retired')
            assert db.execute('PRAGMA integrity_check').fetchone()[0] == 'ok'
    report = {'synthetic_only': True, 'same_name_distinct_ids': True, 'deep_inheritance': True, 'write_scope_mismatch_rejected': True, 'move_requires_explicit_rebind': True, 'copied_marker_refused': True, 'explicit_second_binding_preserves_first': True, 'path_reuse_no_inheritance': True, 'read_selection_loads_no_memory': True, 'identity_json_replay': True, 'import_grants_no_directory_write_binding': True, 'foreign_store_marker_rejected': True, 'work_ledger_db_free': True, 'work_summary_bounded': True, 'work_stale_generation_refused': True, 'eight_parallel_cli_writers_one_success': True}
    report.update(consolidation_dry_run_preserves_ownership=True, retired_key_rejected_after_reopen=True,
                  exact_mapping_replay_idempotent=True, similar_unselected_key_preserved=True,
                  consolidation_sqlite_integrity='ok')
    args.report.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
