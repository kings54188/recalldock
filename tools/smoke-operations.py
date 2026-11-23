#!/usr/bin/env python3
"""Synthetic process-level acceptance for the independent operations database."""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True)
    parser.add_argument("--report", required=True)
    args = parser.parse_args()
    binary = str(Path(args.binary).resolve())
    with tempfile.TemporaryDirectory(prefix="recalldock-operations-") as temp:
        base = Path(temp)
        root = base / "workspace"
        root.mkdir()
        data = base / "data"
        env = {**os.environ, "RECALLDOCK_DATA_DIR": str(data), "HOME": str(base / "home")}
        Path(env["HOME"]).mkdir()

        def call(*words, success=True, error_contains=()):
            result = subprocess.run([binary, "work", *map(str, words)], env=env, cwd=root, text=True, capture_output=True, timeout=30)
            if success and result.returncode:
                raise AssertionError(result.stderr)
            if not success:
                assert result.returncode, "unexpected success"
                for word in error_contains:
                    assert word in result.stderr, result.stderr
                return result.stderr, 0
            return json.loads(result.stdout), len(result.stdout.encode())

        scope = ["--root", str(root), "--lane", "acceptance"]
        for order in ("recent", "lane"):
            empty, _ = call("find", "--root", root, "--order", order, "--status", "all", "--limit", 20)
            assert empty["data"]["tasks"] == [] and empty["data"]["total"] == 0
            assert empty["data"]["has_more"] is False and empty["data"]["order"] == order
            assert not data.exists() and not (root / ".recalldock").exists()
        for option, value in (("--order", "updated"), ("--order", ""), ("--status", "delivered"), ("--status", "")):
            call("find", "--root", root, option, value, success=False)
            assert not data.exists()
        missing_error, _ = call("find", "--root", base / "missing-root", success=False, error_contains=("root",))
        assert "lstat " not in missing_error, missing_error
        cp = {"goal": "Synthetic operations acceptance", "summary": "Recovery summary 26/26", "next": ["Verify state"], "details": "first-detail " * 200}
        source = base / "input.json"
        source.write_text(json.dumps(cp))
        first, _ = call("checkpoint", *scope, "--expected", 0, "--input", source)
        assert first["data"]["generation"] == 1 and not first["data"].get("replayed", False)
        assert not (data / "recalldock.db").exists()
        for query in ("26/26", "first-detail"):
            found, _ = call("find", "--root", root, "--query", query)
            assert [task["lane"] for task in found["data"]["tasks"]] == ["acceptance"]
            assert found["data"]["total"] == 1 and found["data"]["has_more"] is False
            assert "first-detail" not in json.dumps(found)
        sizes = {}
        for mode in ("l0", "l1", "l2"):
            body, length = call("read", *scope, "--mode", mode)
            sizes[mode] = length
            assert length <= (12288 if mode == "l2" else 2048)
            assert ("first-detail" in json.dumps(body)) == (mode == "l2")
        cp["details"] = "second-detail"
        source.write_text(json.dumps(cp))

        def writer(_):
            return subprocess.run([binary, "work", "checkpoint", *scope, "--expected", "1", "--input", str(source)], cwd=root, env=env, text=True, capture_output=True, timeout=30)

        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(writer, range(8)))
        assert sum(r.returncode == 0 for r in results) == 1
        assert all(r.returncode == 0 or "generation mismatch" in r.stderr for r in results)
        assert all(r.returncode == 0 or ("expected=1" in r.stderr and "current=2" in r.stderr) for r in results)
        old, _ = call("read", *scope, "--mode", "l2", "--generation", 1)
        assert "first-detail" in old["data"]["checkpoint"]["details"]
        event = {"event": {"id": "attempt", "input_fingerprint": "synthetic-v1", "kind": "attempt", "status": "started", "summary": "Outcome must be checked", "details": "Selected failure reproduction"}}
        event_file = base / "event.json"
        event_file.write_text(json.dumps(event))
        applied, _ = call("append", *scope, "--expected", 2, "--input", event_file)
        assert applied["data"]["generation"] == 3 and applied["data"]["needs_check"]
        assert not applied["data"].get("replayed", False)
        repeated, _ = call("append", *scope, "--expected", 2, "--input", event_file)
        assert repeated["data"]["generation"] == 3 and repeated["data"]["replayed"] is True
        detail, _ = call("read", *scope, "--mode", "l2", "--event", "--generation", 3)
        assert detail["data"]["event"] == event["event"]
        history, _ = call("history", *scope, "--limit", 2)
        assert [task["generation"] for task in history["data"]["tasks"]] == [3, 2]
        assert history["data"]["has_more"] is True and history["data"]["total"] == 3
        older, _ = call("history", *scope, "--limit", 2, "--before", history["data"]["before"])
        assert [task["generation"] for task in older["data"]["tasks"]] == [1]
        assert older["data"]["has_more"] is False and not older["data"].get("before")
        call("read", *scope, "--mode", "l1", "--event", "--generation", 3, success=False)
        call("read", "--root", root, "--lane", "unknown", success=False, error_contains=("work_find",))
        event["event"]["input_fingerprint"] = "different-input"
        event_file.write_text(json.dumps(event))
        call("append", *scope, "--expected", 3, "--input", event_file, success=False)
        output = base / "projection.md"
        call("export-ledger", *scope, "--output", output)
        output.write_text("manual edit")
        call("export-ledger", *scope, "--output", output, success=False)
        assert output.read_text() == "manual edit"
        backup, _ = call("backup")
        backup = backup["data"]
        assert hashlib.sha256(Path(backup["path"]).read_bytes()).hexdigest() == backup["sha256"]
        with sqlite3.connect(f"file:{backup['path']}?mode=ro", uri=True) as db:
            assert db.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
            assert db.execute("SELECT count(*) FROM checkpoints").fetchone()[0] == 3
        generation = 3
        for i in range(12):
            event_file.write_text(json.dumps({'event': {'id': f'report-{i}', 'input_fingerprint': f'report-input-{i}', 'status': 'reported', 'summary': 'Unrelated bounded progress'}}))
            current, _ = call('append', *scope, '--expected', generation, '--input', event_file)
            generation += 1
            assert current['data']['generation'] == generation and current['data']['needs_check']
        pending, _ = call('read', *scope, '--mode', 'l2')
        assert pending['data']['needs_check'] and len(pending['data']['checkpoint']['operations']) == 12
        source.write_text(json.dumps(cp))
        current, _ = call('checkpoint', *scope, '--expected', generation, '--input', source)
        generation += 1
        assert current['data']['needs_check']
        event_file.write_text(json.dumps({'event': {'id': 'attempt', 'input_fingerprint': 'synthetic-v1', 'status': 'verified', 'summary': 'Explicitly checked outcome', 'evidence': 'selected-evidence.json'}}))
        resolved, _ = call('append', *scope, '--expected', generation, '--input', event_file)
        assert not resolved['data'].get('needs_check', False)
        old_pending, _ = call('read', *scope, '--mode', 'l0', '--generation', 3)
        assert old_pending['data']['needs_check']
        for lane, status in (("alpha", "completed"), ("zeta", "blocked"), ("probe", "archived")):
            source.write_text(json.dumps({"goal": f"Synthetic {lane}", "summary": f"Selected {lane} state", "status": status}))
            call("checkpoint", "--root", root, "--lane", lane, "--expected", 0, "--input", source)
        for order, expected in (("recent", ["zeta", "alpha", "acceptance"]), ("lane", ["acceptance", "alpha", "zeta"])):
            after = ""
            seen = []
            for _ in expected:
                words = ["find", "--root", root, "--order", order, "--limit", 1]
                if after:
                    words.extend(["--after", after])
                page, size = call(*words)
                assert size <= 2048 and page["data"]["total"] == len(expected)
                assert len(page["data"]["tasks"]) == 1
                seen.extend(task["lane"] for task in page["data"]["tasks"])
                if page["data"]["has_more"]:
                    assert page["data"].get("after") and page["data"]["after"] != after
                    after = page["data"]["after"]
                else:
                    assert not page["data"].get("after")
                    break
            assert seen == expected, (order, seen)
        recent, _ = call("find", "--root", root, "--limit", 20)
        assert recent["data"]["order"] == "recent"
        assert [task["lane"] for task in recent["data"]["tasks"]] == ["zeta", "alpha", "acceptance"]
        for status, lane in (("active", "acceptance"), ("blocked", "zeta"), ("completed", "alpha"), ("archived", "probe")):
            page, _ = call("find", "--root", root, "--status", status, "--limit", 20)
            assert [task["lane"] for task in page["data"]["tasks"]] == [lane]
            assert page["data"]["total"] == 1 and page["data"]["has_more"] is False
        all_tasks, _ = call("find", "--root", root, "--status", "all", "--limit", 20)
        assert all_tasks["data"]["total"] == 4 and len(all_tasks["data"]["tasks"]) == 4
        assert all_tasks["data"]["has_more"] is False
        assert not (data / "recalldock.db").exists()
        moved = base / "moved"
        root.rename(moved)
        root.mkdir()
        call("read", *scope, success=False)
        call("read", "--root", moved, "--lane", "acceptance", success=False)
        report = {
            "synthetic_only": True,
            "knowledge_database_created": False,
            "read_and_invalid_options_do_not_initialize_store": True,
            "summary_and_selected_details_search": True,
            "default_recent_and_lane_order": True,
            "archived_hidden_and_status_filters": True,
            "transparent_terminal_pagination": True,
            "history_pagination_and_event_details": True,
            "eight_process_CAS_winners": 1,
            "CAS_expected_current_diagnostics": True,
            "actionable_missing_root_and_lane_errors": True,
            "immutable_history": True,
            "append_retry_idempotent": True,
            "append_retry_replayed_marker": True,
            "operation_conflict_rejected": True,
            "uncertain_work_requires_check": True,
            "pending_survives_display_window_and_checkpoint_replacement": True,
            "explicit_resolution_clears_only_current_reminder": True,
            "projection_hand_edits_preserved": True,
            "backup_integrity": "ok",
            "moved_and_recreated_roots_rejected": True,
            "response_bytes": sizes,
        }
    Path(args.report).write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report))


if __name__ == "__main__":
    main()
