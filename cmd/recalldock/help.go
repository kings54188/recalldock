package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Help is routed before configuration, orphan recovery, update checks, or any
// command handler. Entries describe this checkout, not a different release.
var commandHelp = map[string]string{
	"projects bindings":                  "projects bindings PROJECT_ID [--limit 1..20] [--after BINDING_ID] (bounded binding metadata, including retired roots)",
	"projects unbind":                    "projects unbind PROJECT_ID --directory ABS_PATH --binding EXPECTED_BINDING_ID (retire exact binding; marker remains a fail-closed fence)",
	"ui":                                 "ui [--port 7438] [--project PROJECT] [--open] [--read-only] (loopback operator web UI; prints a one-time token URL; separate from serve/MCP)",
	"gateway":                            "gateway <serve|list|check|doctor|add|discover|grant|revoke|enable|disable|remove> [--registry ABS_PATH] (registry: flag > RECALLDOCK_GATEWAY_REGISTRY > ~/.recalldock/gateway.json; independent of RECALLDOCK_DATA_DIR)",
	"gateway check":                      "gateway check [--registry ABS_PATH] (resolve source references only; no backend starts)",
	"gateway doctor":                     "gateway doctor [--registry ABS_PATH] (list grants whose schema_hash no longer matches the catalog; no backend starts)",
	"gateway serve":                      "gateway serve [--registry ABS_PATH] [--host codex|claude-code|dsh|antigravity] [--host-session REAL_REGISTERED_ID] [--trust agent|strict] (optional trusted host binding; stdio; fixed 7 tools, no backend autostart)",
	"gateway list":                       "gateway list [--registry ABS_PATH] (metadata only; no database creation)",
	"gateway add":                        "gateway add --server ID --input SERVER_JSON [--registry ABS_PATH] (original configuration reference; no grants)",
	"gateway discover":                   "gateway discover --server ID [--registry ABS_PATH] (approved selected backend only; closes owned connection)",
	"gateway grant":                      "gateway grant --server ID --tool NAME --root ABS_PATH (--read-only | --session GATEWAY_SESSION --expires RFC3339 --arguments ABS_JSON) [--registry ABS_PATH] (local operator permission)",
	"gateway revoke":                     "gateway revoke --server ID --tool NAME [--registry ABS_PATH]",
	"gateway enable":                     "gateway enable --server ID [--registry ABS_PATH]",
	"gateway disable":                    "gateway disable --server ID [--registry ABS_PATH]",
	"gateway remove":                     "gateway remove --server ID [--registry ABS_PATH] (catalog and definition only; original configuration preserved)",
	"memory":                             "memory <show|history|version|update|cooldown|archive|reactivate|archived|similar|consolidate|sources> (local operator only; JSON default)",
	"memory show":                        "memory show ID [--full] [--json] (default metadata + selected snapshot digest)",
	"memory history":                     "memory history ID [--limit 1..20] [--before SEQUENCE] [--include-deleted] (L0 only)",
	"memory version":                     "memory version ID SEQUENCE [--full] [--include-deleted] (selected historical state only)",
	"memory update":                      "memory update ID --expected SNAPSHOT_SHA256 --input PATCH_JSON --reason TEXT (registered project requires matching root)",
	"memory cooldown":                    "memory cooldown --project PROJECT | --all [--min-age 30d] [--dry-run] (marks rarely-recalled memories for bounded review; never deletes; pinned/protected skipped)",
	"memory archive":                     "memory archive ID --expected SNAPSHOT_SHA256 --reason TEXT (preserves knowledge/history; default recall excludes archived; protected memories refused)",
	"memory reactivate":                  "memory reactivate ID --expected SNAPSHOT_SHA256 --reason TEXT (explicitly returns an archived memory to active recall)",
	"memory archived":                    "memory archived --project PROJECT [--limit 1..20] [--before ID] (bounded metadata index; no knowledge bodies)",
	"memory similar":                     "memory similar --project PROJECT [--threshold 0.6] [--limit 1..50] [--json] (read-only lexical consolidation hints; never edits)",
	"memory consolidate":                 "memory consolidate TARGET_ID --source ID [--source ID] --reason TEXT (archive sources into a refined target; sources leave recall/search/review but stay readable and linked)",
	"memory sources":                     "memory sources TARGET_ID [--json] (list archived sources folded into a target; no automatic content merge)",
	"resources":                          "resources <register|check> (file content-hash identity; local operator only)",
	"resources register":                 "resources register --project PROJECT --observation-id ID --file PATH (creates file://path@sha256 identity and binds the memory to it)",
	"resources check":                    "resources check --file PATH [--project PROJECT] (recomputes identity: unchanged|changed|missing|unregistered)",
	"backup":                             "backup [--json] (existing selected store only; online verified private snapshot, no restore)",
	"serve":                              "serve [port]",
	"mcp":                                "mcp [--tools=agent|admin|all|TOOL[,PROFILE|TOOL...]] [--project NAME] (agent default; admin is a deferred read-only add-on set; tool selection never grants writes)",
	"tui":                                "tui",
	"search":                             "search <query> [--type TYPE] [--project PROJECT | --all] [--scope SCOPE] [--limit N] [--match-mode all|any] [--profile PROFILE] [--since DATE] [--until DATE] [--strategy flat|hierarchical|auto] [--detail l0|l1] [--token-budget N] [--root-path PATH] [--max-depth N] [--trace] [--explain-empty] [--include-stale] [--json]",
	"save":                               "save <title> <content> [--type TYPE] [--project PROJECT] [--scope SCOPE] [--topic TOPIC_KEY] [--memory-class CLASS] [--memory-path PATH] [--abstract TEXT] [--overview TEXT] (create-only; existing topic changes require memory update; default requires root/config or explicit --project)",
	"delete":                             "delete <observation_id> [--hard] | delete <session|prompt|project> <id-or-name>",
	"delete session":                     "delete session <id> (session must have no observations, including soft-deleted; no cascade)",
	"delete prompt":                      "delete prompt <id> (permanent)",
	"delete project":                     "delete project <name-or-ID> [--hard --confirm] (matching trusted directory/config required)",
	"timeline":                           "timeline <observation_id> [--before 0..50] [--after 0..50] [--json]",
	"context":                            "context [project | --project PROJECT | --all] [--scope SCOPE] [--mode index|l0|l1|full] [--token-budget 1..32000] (default: cwd project, index)",
	"stats":                              "stats [--project PROJECT | --all] [--json] (default: cwd project)",
	"export":                             "export [--project PROJECT] [file.json]",
	"import":                             "import <file.json>",
	"sync":                               "sync [--import | --status] [--project PROJECT | --all] (local chunks only; cloud disabled)",
	"sync cleanup":                       "sync cleanup --non-enrolled [--dry-run | --apply --all] [--json]",
	"projects":                           "projects <create|adopt|delete|bind|activate|budget|registry|current|list|consolidate|prune>",
	"projects create":                    "projects create --name NAME (creates an unbound stable identity)",
	"projects adopt":                     "projects adopt --name LEGACY_NAME (register an existing name-keyed bucket; no binding, no guessing)",
	"projects delete":                    "projects delete --project PROJECT_ID --confirm (delete an empty identity with no bindings/observations/sessions/candidates; irreversible)",
	"projects bind":                      "projects bind PROJECT_ID --directory ABS_PATH (explicit local binding)",
	"projects activate":                  "projects activate PROJECT_ID [--token-budget 1..32000] (read-only selector + shared budget; loads no memory)",
	"projects budget":                    "projects budget READ_BUDGET_ID (operator-only accounting status; does not reset)",
	"projects registry":                  "projects registry [--json] (JSON default; registered identities, independent of cwd)",
	"projects current":                   "projects current [--directory PATH] [--json]",
	"projects list":                      "projects list [--json] (projects with sessions, observations or prompts; registered names and separate count definitions)",
	"projects consolidate":               "projects consolidate [--all] [--dry-run] | projects consolidate --mapping FILE [--apply]",
	"projects prune":                     "projects prune --all [--dry-run] | projects prune --project ID [--apply] (registered identities need the --project path; interactive list protects them)",
	"store":                              "store <current|init> [options]",
	"store init":                         "store init [--json] (explicitly initialize the selected data directory)",
	"store current":                      "store current [--directory PATH] [--json]",
	"doctor":                             "doctor [--json] [--project PROJECT] [--check CODE] [--silence-warning CODE]",
	"doctor repair":                      "doctor repair --project PROJECT --check CODE (--plan|--dry-run|--apply)",
	"conflicts":                          "conflicts <list|show|stats|scan|deferred>",
	"conflicts list":                     "conflicts list [--project PROJECT] [--status pending|judged|orphaned|ignored] [--since RFC3339] [--limit 1..500] [--json]",
	"conflicts show":                     "conflicts show <relation_id> [--json]",
	"conflicts stats":                    "conflicts stats [--project PROJECT] [--json]",
	"conflicts scan":                     "conflicts scan [--project PROJECT] [--since RFC3339] [--dry-run | --apply] [--max-insert 1..10000] [--json] (non-archived lexical scan; inspected counts successes; failures report nonzero exit)",
	"conflicts deferred":                 "conflicts deferred [--status deferred|dead] [--limit N] [--inspect SYNC_ID] [--replay --all] [--json]",
	"review":                             "review <list|mark|link|dependencies|process|resource-link|resource-dependencies|resource-changed|resource-refresh> [options]",
	"review list":                        "review list [--project PROJECT | --all] [--status active|stale] [--limit 1..20] [--json] (state filter precedes limit; unknown or duplicate options are errors)",
	"review mark":                        "review mark <observation_id> [--json]",
	"review link":                        "review link <source_observation_id> <dependent_observation_id> [--reason TEXT] [--json]",
	"review dependencies":                "review dependencies [--project PROJECT | --all] [--status active|stale] [--source SYNC_ID] [--dependent SYNC_ID] [--limit 1..500] [--json]",
	"review process":                     "review process --all [--limit N] [--json]",
	"review resource-link":               "review resource-link <source_ref> <dependent_observation_id> --project PROJECT [--reason TEXT] [--json]",
	"review resource-dependencies":       "review resource-dependencies [--project PROJECT | --all] [--status active|stale] [--source-ref REF] [--dependent SYNC_ID] [--limit 1..500] [--json]",
	"review resource-changed":            "review resource-changed <source_ref> --project PROJECT [--replacement-source-ref REF] [--reason TEXT] [--json]",
	"review resource-refresh":            "review resource-refresh <resource_dependency_sync_id> --project PROJECT [--source-ref REF] [--reason TEXT] [--json]",
	"cloud":                              "cloud <status|enroll|config|serve|upgrade|repair|bootstrap> [options]",
	"cloud status":                       "cloud status",
	"cloud enroll":                       "cloud enroll <project>",
	"cloud config":                       "cloud config [options]",
	"cloud serve":                        "cloud serve [options]",
	"cloud upgrade":                      "cloud upgrade <doctor|repair|bootstrap|status|rollback> --project PROJECT [options]",
	"cloud upgrade doctor":               "cloud upgrade doctor --project PROJECT",
	"cloud upgrade repair":               "cloud upgrade repair --project PROJECT [options]",
	"cloud upgrade bootstrap":            "cloud upgrade bootstrap --project PROJECT [options]",
	"cloud upgrade status":               "cloud upgrade status --project PROJECT",
	"cloud upgrade rollback":             "cloud upgrade rollback --project PROJECT [options]",
	"cloud repair":                       "cloud repair materialize-mutations --project PROJECT (--dry-run|--apply)",
	"cloud repair materialize-mutations": "cloud repair materialize-mutations --project PROJECT (--dry-run|--apply)",
	"cloud bootstrap":                    "cloud bootstrap [options]",
	"setup":                              "setup --release ABS_RELEASE --home ABS_HOME [--data-dir ABS_DATA] [--host codex|claude-code|dsh|antigravity] [--apply --expected-sha256 PREVIEW_SHA --backup NEW_ABS_DIR]; setup --rollback --backup ABS_DIR (offline; Python 3.11+; no knowledge DB; hardened policy retained on rollback)",
	"protocol-mode":                      "protocol-mode <slug>",
	"obsidian-export":                    "obsidian-export --vault PATH [--project PROJECT] [--attachment-root ABS_PATH] [--force] [--graph-config preserve|force|skip] [--watch] [--interval DURATION] (attachments opt-in, registered SHA256 files only)",
	"candidate-hook":                     "candidate-hook (reads structured candidate input from stdin)",
	"candidates":                         "candidates <list|show|approve|reject|revive|sweep|policy> (local operator interface, not an agent permission)",
	"candidates list":                    "candidates list --project PROJECT | --all [--status review|expired|promoted|rejected|all] [--limit 1..20] [--json] (JSON default; default review, 5; preview only)",
	"candidates show":                    "candidates show CANDIDATE_ID (selected snapshot and unverified evidence claims)",
	"candidates approve":                 "candidates approve CANDIDATE_ID --expected SNAPSHOT_SHA256 --reason TEXT",
	"candidates reject":                  "candidates reject CANDIDATE_ID --expected SNAPSHOT_SHA256 --reason TEXT",
	"candidates revive":                  "candidates revive CANDIDATE_ID --expected SNAPSHOT_SHA256 --reason TEXT (expired/overdue review body is retained; extends its review window)",
	"candidates sweep":                   "candidates sweep --all (apply the configured automatic policy to all pending reviews; uncertain entries stay in review)",
	"candidates policy":                  "candidates policy --all --mode manual|duplicates|checked|tiered (checked/tiered verify current same-project observation snapshots or registered file hashes; tiered auto-admits high-confidence durable categories, other claims remain pending)",
	"work":                               "work <find|read|status|checkpoint|append|history|import-ledger|export-ledger|doctor|backup> (independent operations.db; no long-term knowledge writes)",
	"work find":                          "work find --root ABS_PATH [--query TEXT] [--after CURSOR] [--limit 1..20] [--order recent|lane] [--status active|blocked|completed|archived|all] (default recent, exclude archived; L0 has_more/total; reads never initialize a database)",
	"work read":                          "work read --root ABS_PATH --lane EXACT_LANE [--mode l0|l1|l2] [--generation N] [--event] (default L1; event requires explicit generation and L2)",
	"work status":                        "work status --root ABS_PATH --lane EXACT_LANE [--full | --mode l0|l1|l2] (default L1 <=2 KiB; L2 <=12 KiB)",
	"work checkpoint":                    "work checkpoint --root ABS_PATH --lane EXACT_LANE --expected GENERATION --input JSON_FILE (new task expected=0; generation CAS)",
	"work append":                        "work append --root ABS_PATH --lane EXACT_LANE --expected GENERATION --input EVENT_JSON (event + optional patch; retries deduplicated)",
	"work history":                       "work history --root ABS_PATH --lane EXACT_LANE [--before GENERATION] [--limit 1..20] (immutable L0 snapshot index with has_more/total)",
	"work import-ledger":                 "work import-ledger --root ABS_PATH --lane EXACT_LANE --input MD_FILE [--recorded-root OLD_ABS_PATH] [--apply --expected-sha256 SHA256] (preview by default; preserves source and generation)",
	"work export-ledger":                 "work export-ledger --root ABS_PATH --lane EXACT_LANE --output ABS_PATH (create-only Markdown projection; never overwrites hand edits)",
	"work doctor":                        "work doctor (operations database identity and integrity; no content)",
	"work backup":                        "work backup (existing operations database only; consistent verified SQLite snapshot)",
	"version":                            "version",
}

func helpChildren(command string) []string {
	prefix := command + " "
	var children []string
	for key := range commandHelp {
		if strings.HasPrefix(key, prefix) && !strings.Contains(strings.TrimPrefix(key, prefix), " ") {
			children = append(children, key)
		}
	}
	sort.Strings(children)
	return children
}

func handleHelpCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	var target []string
	requested := false
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		target, requested = args[1:], true
	} else {
		for i, arg := range args {
			if arg == "--" {
				break // Everything after -- is literal input, including --help.
			}
			if arg == "--help" || arg == "-h" {
				target, requested = args[:i], true
				break
			}
		}
	}
	if !requested {
		return false
	}
	if len(target) == 0 {
		printUsage()
		return true
	}
	if target[0] == "cloud" {
		fmt.Println("usage: recalldock cloud (disabled in local-only build). Cloud commands are disabled; use local JSON/Markdown interchange. No configuration or network request is performed.")
		return true
	}
	command := target[0]
	_, known := commandHelp[command]
	for i := 1; known && i < len(target) && !strings.HasPrefix(target[i], "-") && len(helpChildren(command)) > 0; i++ {
		command += " " + target[i]
		_, known = commandHelp[command]
	}
	if !known {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", command)
		exitFunc(1)
		return true
	}
	fmt.Printf("usage: recalldock %s\n", commandHelp[command])
	for _, child := range helpChildren(command) {
		fmt.Printf("  recalldock %s\n", commandHelp[child])
	}
	if command == "search" {
		fmt.Println("Use -- before a literal query that contains help flags: recalldock search -- --help")
	}
	if command == "setup" {
		fmt.Println("Only a complete verified release layout supports setup; a bare source checkout is refused. No upstream plugin will be installed.")
	}
	return true
}
