#!/usr/bin/env python3
"""CLI/MCP regression acceptance on disposable synthetic projects and data."""
import argparse
import json
import os
from pathlib import Path
import runpy
import sqlite3
import subprocess
import tempfile

MCP = runpy.run_path(str(Path(__file__).with_name("smoke-read-budget.py")))["MCP"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    binary = args.binary.resolve(strict=True)
    report: dict[str, bool | int | str] = {"synthetic_only": True}
    with tempfile.TemporaryDirectory(prefix="defects-", dir=args.work_dir) as tmp:
        base = Path(tmp)
        a, b, outside = base / "project-a", base / "project-b", base / "outside"
        for directory in (a, b, outside):
            directory.mkdir()
        data = base / "data"
        env = {k: v for k, v in os.environ.items() if not k.startswith(("RECALLDOCK_", "ENGRAM_"))}
        env.update(HOME=str(base), RECALLDOCK_DATA_DIR=str(data))

        def cli(*command, cwd=outside, ok=True, environment=env):
            result = subprocess.run([str(binary), *map(str, command)], env=environment, cwd=cwd,
                                    capture_output=True, text=True, timeout=30)
            assert (result.returncode == 0) == ok, (command[:3], result.returncode, result.stderr[:400])
            return result

        project_a = json.loads(cli("projects", "create", "--name", "Synthetic A").stdout)["id"]
        project_b = json.loads(cli("projects", "create", "--name", "Synthetic B").stdout)["id"]
        cli("projects", "bind", project_a, "--directory", a)
        cli("projects", "bind", project_b, "--directory", b)
        titles = ['Shared a"b/c', 'Shared "x"y#z', 'Shared foo"bar*baz', 'Shared literal"value `/path` #2093 *']
        for title in titles:
            cli("save", title, "Synthetic literal fixture", "--type", "decision", cwd=a)
        cli("save", "Foreign marker", "Synthetic foreign fact", "--type", "decision", cwd=b)
        exchange = base / "exchange.json"
        cli("export", exchange)
        rows = json.loads(exchange.read_text())["observations"]
        ids = {row["title"]: row["id"] for row in rows}
        enum_cases = 0
        for key, value in (("--scope", "bogus"), ("--type", "bogus_type"), ("--strategy", "bogus"),
                           ("--detail", "bogus"), ("--profile", "bogus"), ("--match-mode", "bogus"), ("--root-path", "../bad")):
            for all_projects in (False, True):
                command = ["search", "Shared", key, value, "--json"]
                if all_projects:
                    command.append("--all")
                result = cli(*command, ok=False)
                assert not result.stdout and "invalid_arguments:" not in result.stderr, (command, result.stderr)
                enum_cases += 1
        dry_text = cli("conflicts", "scan", "--project", project_a, "--dry-run", "--max-insert", "1").stdout
        assert "larger --max-insert" in dry_text and "does not persist progress" in dry_text and "Re-run to continue" not in dry_text
        report.update(cli_enum_cases=enum_cases, dry_run_nonresumable_warning=True)
        for query in ['a"b/c', '"x"y#z', 'foo"bar*baz', '"" "', 'literal"value `/path` #2093 *']:
            for mode in ("all", "any"):
                cli("search", query, "--all", "--match-mode", mode, "--json")
        complete = json.loads(cli("conflicts", "scan", "--project", project_a, "--json").stdout)
        assert complete["failed"] == 0 and complete["inspected"] == len(titles), complete
        preview = json.loads(cli("conflicts", "scan", "--project", project_a, "--max-insert", "3", "--json").stdout)
        assert preview["failed"] == 0 and preview["inspected"] > 0, preview
        assert preview["would_insert"] == 3 and preview["capped"], preview
        applied = json.loads(cli("conflicts", "scan", "--project", project_a, "--max-insert", "3", "--apply", "--json").stdout)
        assert applied["inserted"] == preview["would_insert"] and applied["relation_errors"] == 0, applied
        relations = json.loads(cli("conflicts", "list", "--project", project_a, "--json").stdout)
        assert relations["total"] == applied["inserted"] and len(relations["relations"]) == 3
        relation_id = relations["relations"][0]["id"]
        for command in (("timeline", ids[titles[0]], "--before", "0", "--after", "0", "--json"),
                        ("conflicts", "show", relation_id, "--json"),
                        ("conflicts", "stats", "--project", project_a, "--json"),
                        ("conflicts", "deferred", "--json"),
                        ("candidates", "list", "--all", "--json"),
                        ("projects", "registry", "--json")):
            json.loads(cli(*command).stdout)
        report["json_matrix_cases"] = 6
        report.update(literal_cli_queries=10, scan_inspected=len(titles), preview_apply_inserted=3)

        for command in (("review", "list", "--porject", project_a),
                        ("review", "list", "--limit", "0"), ("review", "list", "--limit", "abc"),
                        ("review", "list", "--limit", "5000"), ("review", "list", "--project"),
                        ("review", "list", "--status", "bogus"),
                        ("review", "dependencies", "--porject", project_a),
                        ("review", "resource-dependencies", "--porject", project_a),
                        ("conflicts", "list", "--porject", project_a),
                        ("conflicts", "scan", "--apply", "--dry-run"),
                        ("conflicts", "scan", "--dry-run", "--apply"),
                        ("projects", "list", "--bogus")):
            result = cli(*command, cwd=a, ok=False)
            assert not result.stdout, (command, result.stdout)
        report["strict_cli_cases"] = 12
        projects = json.loads(cli("projects", "list", "--json").stdout)
        assert {p["name"] for p in projects["projects"]} == {"Synthetic A", "Synthetic B"}, projects
        assert projects["registered_count"] == 2 and "count_basis" in projects
        assert "projects_basis" in json.loads(cli("stats", "--all", "--json").stdout)
        registry = cli("projects", "registry")
        assert len(json.loads(registry.stdout)) == 2 and "registered identities" in registry.stderr
        report["json_lists_and_count_definitions"] = True

        source, dependent = ids[titles[0]], ids[titles[1]]
        cli("review", "link", source, dependent, "--reason", "Synthetic derivation", cwd=a)
        snapshot = json.loads(cli("memory", "show", source).stdout)["snapshot_sha256"]
        patch = base / "patch.json"
        patch.write_text(json.dumps({"content": "Revised synthetic source"}))
        cli("memory", "update", source, "--expected", snapshot, "--input", patch,
            "--reason", "Verified synthetic revision", cwd=a)
        stale = json.loads(cli("review", "list", "--project=" + project_a, "--status=stale", "--limit=1", "--json").stdout)
        assert [row["id"] for row in stale["observations"]] == [dependent], stale
        assert json.loads(cli("review", "list", "--project", project_a, "--status", "active", "--json").stdout)["observations"] == []
        report["review_state_filtered_before_limit"] = True

        clients = []
        try:
            unbound = MCP(binary, env, outside)
            clients.append(unbound)
            tools = unbound.rpc("tools/list", {})["tools"]
            for tool in tools:
                schema = tool["inputSchema"]
                assert schema["additionalProperties"] is False, tool["name"]
                result = unbound.call(tool["name"], {"bogus_key": 1})
                assert result.get("isError") and "invalid_arguments" in json.dumps(result), result
            schemas = {tool["name"]: tool["inputSchema"] for tool in tools}
            for key in ("match_mode", "profile", "strategy", "detail"):
                assert schemas["mem_search"]["properties"][key]["enum"], key
                for all_projects in (False, True):
                    fields: dict[str, str | bool] = {"query": "Shared", key: "bogus"}
                    if all_projects:
                        fields["all_projects"] = True
                    result = unbound.call("mem_search", fields)
                    payload = json.loads(result["content"][0]["text"])
                    assert result.get("isError") and payload["error_code"] == "invalid_arguments", payload
                    assert not payload["message"].startswith("invalid_arguments:"), payload
            for fields, code in (({"query": "的", "all_projects": True}, "query_has_no_terms"),
                                 ({"query": "Shared", "all_projects": True, "token_budget": 1}, "budget_too_small"),
                                 ({"query": 'foo"bar*baz', "all_projects": True, "raw_fts": True}, "invalid_query")):
                result = unbound.call("mem_search", fields)
                payload = json.loads(result["content"][0]["text"])
                assert result.get("isError") and payload["error_code"] == code and not payload["message"].startswith(code+":"), payload
            report.update(mcp_enum_cases=8, clean_structured_errors=True)
            for query in ['a"b/c', '"x"y#z', 'foo"bar*baz', '"" "']:
                result = unbound.call("mem_search", {"query": query, "all_projects": True, "match_mode": "any"})
                assert not result.get("isError"), result
            invalid = [("mem_context", {"all_projects": True, "limit": 5}),
                       ("mem_context", {"all_projects": True, "token_budget": 1.9}),
                       ("mem_review", {"action": "no_such_action"}),
                       ("mem_get_observation", {"id": source + 0.9}),
                       ("mem_get_observation", {"id": source + 0.0000001}),
                       ("mem_get_observation", {"id": 9007199254740992})]
            for name, fields in invalid:
                result = unbound.call(name, fields)
                assert result.get("isError") and "invalid_arguments" in json.dumps(result), result
            for query in ['foo"bar*baz', "missingcolumn:marker"]:
                result = unbound.call("mem_search", {"query": query, "all_projects": True, "raw_fts": True})
                assert result.get("isError") and "invalid_query" in json.dumps(result) and "SQL logic error" not in json.dumps(result), result
            for options in ({"strategy": "bogus"}, {"detail": "l2"}, {"root_path": "../bad"}):
                result = unbound.call("mem_search", dict(options, query="marker", all_projects=True))
                assert result.get("isError") and "invalid_arguments" in json.dumps(result), result
            pinned = MCP(binary, env, a)
            clients.append(pinned)
            result = pinned.call("mem_review", {"action": "list", "status": "stale"})
            assert not result.get("isError") and "Foreign marker" not in json.dumps(result), result
            payload = json.loads(result["content"][0]["text"])
            assert [row["id"] for row in payload["observations"]] == [dependent], payload
            report.update(mcp_strict_tools=len(tools), mcp_invalid_integer_and_arguments=len(invalid),
                          raw_fts_classified=2, actionable_option_errors=3, mcp_review_scope=True)
        finally:
            for client in clients:
                client.close()

        source_config = base / "backend.json"
        source_config.write_text(json.dumps({"mcpServers": {"synthetic": {"command": "/bin/false"}}}))
        definition = base / "server.json"
        definition.write_text(json.dumps({"description": "Synthetic metadata only", "source": {"path": str(source_config), "format": "json", "keys": ["mcpServers", "synthetic"]}, "enabled": False, "start_allowed": False, "stateful": False}))
        cli("gateway", "add", "--server", "synthetic", "--input", definition)
        selected_registry = base / ".recalldock" / "gateway.json"
        alternate_data = base / "alternate-data"
        alternate_data.mkdir()
        (alternate_data / "gateway.json").write_text("invalid registry must be ignored")
        alternate_env = dict(env, RECALLDOCK_DATA_DIR=str(alternate_data))
        assert json.loads(cli("gateway", "list", environment=alternate_env).stdout)[0]["id"] == "synthetic"
        missing = cli("gateway", "grant", "--server", "missing", "--tool", "test", "--root", a,
                      "--read-only", environment=alternate_env, ok=False)
        assert str(selected_registry) in missing.stderr, missing.stderr
        empty_registry = base / "empty-registry.json"
        empty_registry.write_text('{"version":1,"servers":{}}')
        empty_registry.chmod(0o600)
        overridden_env = dict(alternate_env, RECALLDOCK_GATEWAY_REGISTRY=str(empty_registry))
        assert json.loads(cli("gateway", "list", environment=overridden_env).stdout) == []
        assert len(json.loads(cli("gateway", "list", "--registry", selected_registry, environment=overridden_env).stdout)) == 1
        report["gateway_data_independent_env_and_flag_precedence"] = True
        with sqlite3.connect(f"file:{data / 'recalldock.db'}?mode=ro", uri=True) as db:
            assert db.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
            assert db.execute("PRAGMA foreign_key_check").fetchall() == []
        report["sqlite_integrity"] = "ok"
    args.report.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report))


if __name__ == "__main__":
    main()
