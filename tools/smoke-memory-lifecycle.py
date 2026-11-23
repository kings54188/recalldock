#!/usr/bin/env python3
"""Archive/reactivation and schema-upgrade acceptance, isolated synthetic data."""
import argparse
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--upgrade-from', type=Path, required=True)
    parser.add_argument('--work-dir', type=Path, required=True)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    old = args.upgrade_from.resolve(strict=True)
    report = dict(synthetic_only=True)
    with tempfile.TemporaryDirectory(prefix='memory-lifecycle-', dir=args.work_dir) as tmp:
        root = Path(tmp)
        data = root / 'data'
        env = {k: v for k, v in os.environ.items() if not k.startswith(('RECALLDOCK_', 'ENGRAM_'))}
        env.update(HOME=str(root), RECALLDOCK_DATA_DIR=str(data))

        def run(*parts, executable=binary, ok=True, chosen_env=env):
            r = subprocess.run([str(executable), *map(str, parts)], env=chosen_env, cwd=root,
                               capture_output=True, text=True, timeout=30)
            assert (r.returncode == 0) == ok, (parts[:2], r.returncode, r.stderr[:400])
            return r.stdout

        for command in ('archive', 'reactivate', 'archived'):
            run('memory', command, '--help')
        run('memory', 'archive', '1', '--expected', 'z' * 64, '--reason', 'Invalid digest', ok=False)
        assert not data.exists(), 'invalid lifecycle input or help initialized data'
        config = root / '.recalldock'
        config.mkdir()
        (config / 'config.json').write_text(json.dumps({'project_name': 'lifecycle'}))
        run('save', 'Lifecycle fixture', 'durable-lifecycle-needle BODY_SELECTED_ONLY', '--project', 'lifecycle', executable=old)
        exchange = root / 'before.json'
        run('export', exchange, executable=old)
        oid = json.loads(exchange.read_text())['observations'][0]['id']
        # This opens and migrates an actual DB created by the installed old CLI.
        selected = json.loads(run('memory', 'show', oid))
        archived = json.loads(run('memory', 'archive', oid, '--expected', selected['snapshot_sha256'], '--reason', 'Reviewed obsolete knowledge'))
        assert archived['lifecycle_state'] == 'archived' and 'state' not in archived
        index = json.loads(run('memory', 'archived', '--project', 'lifecycle'))
        assert len(index) == 1 and index[0]['id'] == oid
        assert 'BODY_SELECTED_ONLY' not in json.dumps(index)
        results = json.loads(run('search', 'durable-lifecycle-needle', '--project', 'lifecycle', '--json'))
        assert not results['results'], 'archive still appears in default recall'
        run('memory', 'reactivate', oid, '--expected', selected['snapshot_sha256'], '--reason', 'Stale selection', ok=False)
        full = json.loads(run('memory', 'show', oid, '--full'))
        assert full['state']['content'].endswith('BODY_SELECTED_ONLY') and full['state']['archived_at']
        archive_exchange = root / 'archived.json'
        run('export', archive_exchange)
        imported = dict(env, RECALLDOCK_DATA_DIR=str(root / 'imported'))
        run('import', archive_exchange, chosen_env=imported)
        run('import', archive_exchange, chosen_env=imported)
        assert len(json.loads(run('memory', 'archived', '--project', 'lifecycle', chosen_env=imported))) == 1
        restored = json.loads(run('memory', 'reactivate', oid, '--expected', archived['snapshot_sha256'], '--reason', 'Reviewed current evidence'))
        assert restored['lifecycle_state'] == 'active'
        results = json.loads(run('search', 'durable-lifecycle-needle', '--project', 'lifecycle', '--json'))
        assert len(results['results']) == 1
        history = json.loads(run('memory', 'history', oid))
        assert len(history) == 3
        with sqlite3.connect(f'file:{data / "recalldock.db"}?mode=ro', uri=True) as db:
            assert db.execute('PRAGMA integrity_check').fetchone()[0] == 'ok'
            assert db.execute('SELECT count(*) FROM observations').fetchone()[0] == 1
            assert db.execute('SELECT count(*) FROM observation_edit_audits').fetchone()[0] == 2
        report.update(old_schema_upgrade=True, archive_excluded_from_recall=True, body_preserved=True,
                      bounded_archive_index=True, stale_reactivation_refused=True, explicit_reactivation=True,
                      archive_interchange_replay=True, lifecycle_history_and_audits=True, integrity='ok')
    args.report.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
