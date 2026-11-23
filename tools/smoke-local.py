#!/usr/bin/env python3
"""Synthetic local CLI acceptance; never opens the user's memory database."""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    report = {"synthetic_only": True}
    with tempfile.TemporaryDirectory(prefix="recalldock-smoke-", dir=args.work_dir) as tmp:
        root = Path(tmp)
        env_home = str(root)
        data = root / "source"
        env = dict(os.environ, HOME=env_home, RECALLDOCK_DATA_DIR=str(data), RECALLDOCK_DISABLE_UPDATE_CHECK="1")
        # The smoke must not inherit host DB pinning, auth or model runner settings.
        for key in list(env):
            if key.startswith(("RECALLDOCK_", "ENGRAM_")) and key not in {"RECALLDOCK_DATA_DIR", "RECALLDOCK_DISABLE_UPDATE_CHECK"}:
                del env[key]

        def run(*command, directory=data, success=True):
            proc = subprocess.run([str(binary), *command], env=dict(env, RECALLDOCK_DATA_DIR=str(directory)), cwd=root, capture_output=True, text=True, timeout=30)
            if (proc.returncode == 0) != success:
                raise AssertionError(f"{command[0]} unexpected exit={proc.returncode}; stderr={proc.stderr[:600]}")
            return proc

        for command in [("--help",), ("save", "--help"), ("search", "--help"), ("delete", "session", "--help"), ("projects", "current", "--help"), ("obsidian-export", "--help"), ("context", "--help")]:
            run(*command)
        assert not data.exists(), "help created a database/data directory"
        report["help_without_data_side_effects"] = 7
        run("save", "Seed", "Synthetic local smoke", "--project", "smoke", "--type", "decision", "--topic", "design/seed")
        with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
            def writer(index):
                for item in range(5):
                    run("save", f"Writer {index}-{item}", f"Synthetic unique fixture {index}-{item}", "--project", "smoke", "--type", "decision", "--topic", f"design/{index}-{item}")
            list(pool.map(writer, range(3)))

        def count(directory):
            with sqlite3.connect(f"file:{directory / 'recalldock.db'}?mode=ro", uri=True) as db:
                assert db.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
                return db.execute("SELECT COUNT(*) FROM observations").fetchone()[0]

        assert count(data) == 16
        report["three_process_observations"] = 16
        exported = root / "exchange.json"
        run("export", str(exported))
        assert len(json.loads(exported.read_text())["observations"]) == 16
        imported = root / "imported"
        run("import", str(exported), directory=imported)
        run("import", str(exported), directory=imported)
        assert count(imported) == 16
        report["json_replay_count"] = 16
        conflict = json.loads(exported.read_text())
        conflict["observations"][0]["content"] = "Conflicting identity must be refused"
        conflict_file = root / "conflict.json"
        conflict_file.write_text(json.dumps(conflict))
        run("import", str(conflict_file), directory=imported, success=False)
        assert count(imported) == 16
        report["json_conflict_rejected"] = True
        vault = root / "vault"
        (vault / ".obsidian").mkdir(parents=True)
        graph = vault / ".obsidian/graph.json"
        graph.write_text('{"user_setting":"preserve"}')
        legacy = vault / "engram/user-note.md"
        legacy.parent.mkdir()
        legacy.write_text("Existing unrelated note")
        run("obsidian-export", "--vault", str(vault))
        run("obsidian-export", "--vault", str(vault))
        assert graph.read_text() == '{"user_setting":"preserve"}'
        assert legacy.read_text() == "Existing unrelated note"
        managed = vault / "recalldock"
        link_count = 0
        for note in managed.rglob("*.md"):
            for target in re.findall(r"\[\[([^\]|]+)", note.read_text()):
                assert target.startswith("recalldock/")
                assert (vault / (target + ".md")).is_file(), f"unresolved {target}"
                link_count += 1
        assert link_count > 16
        report.update(vault_resolved_links=link_count, existing_graph_preserved=True, legacy_notes_preserved=True)
        run("obsidian-export", "--vault", str(vault), "--force", directory=imported, success=False)
        report["different_database_refused"] = True
        note = next((managed / "smoke/decision").glob("*.md"))
        note.write_text("Manual annotation must survive")
        run("obsidian-export", "--vault", str(vault), "--force", success=False)
        assert note.read_text() == "Manual annotation must survive"
        report["manual_edit_preserved_and_exit_nonzero"] = True
        run("obsidian-export", "--vault", str(root / "partial"), "--limit", "1", success=False)
        assert not (root / "partial").exists()
        report["unsupported_partial_graph_fails_without_vault_writes"] = True
        report["sqlite_integrity"] = "ok"
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, ensure_ascii=False))


if __name__ == "__main__":
    main()
