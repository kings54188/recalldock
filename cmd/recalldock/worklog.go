package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"recalldock.local/recalldock/internal/opsstore"
)

func cmdWorklog(args []string) {
	if err := runWork(args); err != nil {
		fatal(err)
	}
}

func runWork(args []string) error {
	if len(args) == 0 {
		return errors.New("work requires find, read, checkpoint, append, history, import-ledger, export-ledger, doctor or backup")
	}
	sub := args[0]
	allowed := map[string]bool{}
	for _, key := range []string{"--root", "--lane", "--mode", "--input", "--expected", "--generation", "--before", "--limit", "--query", "--after", "--order", "--status", "--recorded-root", "--expected-sha256", "--output"} {
		allowed[key] = true
	}
	values := map[string]string{}
	flags := map[string]bool{}
	for i := 1; i < len(args); i++ {
		key := args[i]
		if _, ok := values[key]; ok {
			return fmt.Errorf("duplicate work option %s", key)
		}
		if flags[key] {
			return fmt.Errorf("duplicate work option %s", key)
		}
		if key == "--full" || key == "--apply" || key == "--event" {
			flags[key] = true
			continue
		}
		if !allowed[key] || i+1 >= len(args) {
			return fmt.Errorf("unknown work option or missing value: %s", key)
		}
		i++
		values[key] = args[i]
	}
	valid := map[string]string{
		"find": "--root --query --after --limit --order --status", "status": "--root --lane --mode --full --generation", "read": "--root --lane --mode --generation --full --event",
		"checkpoint": "--root --lane --expected --input", "append": "--root --lane --expected --input",
		"history": "--root --lane --before --limit", "import-ledger": "--root --lane --input --recorded-root --expected-sha256 --apply",
		"export-ledger": "--root --lane --output", "doctor": "", "backup": "",
	}
	keys, ok := valid[sub]
	if !ok {
		return fmt.Errorf("unknown work subcommand %s", sub)
	}
	for key := range values {
		if !containsOption(keys, key) {
			return fmt.Errorf("%s does not accept %s", sub, key)
		}
	}
	for key := range flags {
		if !containsOption(keys, key) {
			return fmt.Errorf("%s does not accept %s", sub, key)
		}
	}
	order := "recent"
	if value, found := values["--order"]; found {
		if value == "" {
			return errors.New("order must be recent or lane")
		}
		order = value
	}
	if value, found := values["--status"]; found && value == "" {
		return errors.New("status must be active, blocked, completed, archived or all")
	}
	root, lane := values["--root"], values["--lane"]
	if sub != "doctor" && sub != "backup" {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("--root must be an absolute path: got %q", root)
		}
		selectedLane := lane
		if sub == "find" {
			selectedLane = "index"
		}
		if err := opsstore.ValidateSelection(root, selectedLane); err != nil {
			return err
		}
	}
	mode := values["--mode"]
	if mode == "" {
		mode = "l1"
	}
	if flags["--full"] {
		if values["--mode"] != "" {
			return errors.New("choose --mode or --full, not both")
		}
		mode = "l2"
	}
	if mode != "l0" && mode != "l1" && mode != "l2" {
		return errors.New("mode must be l0, l1 or l2")
	}
	numbers := map[string]int64{}
	for _, key := range []string{"--expected", "--generation", "--before", "--limit"} {
		if value, found := values[key]; found {
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return fmt.Errorf("invalid nonnegative integer for %s", key)
			}
			numbers[key] = n
		}
	}
	if limit, found := numbers["--limit"]; found && (limit < 1 || limit > 20) {
		return errors.New("limit must be 1..20")
	}
	if flags["--event"] && (numbers["--generation"] < 1 || mode != "l2") {
		return errors.New("selected event requires --generation N --mode l2")
	}
	findOptions := opsstore.FindOptions{Query: values["--query"], After: values["--after"], Order: order, Status: values["--status"], Limit: int(numbers["--limit"])}
	if sub == "find" {
		if err := opsstore.ValidateFindOptions(findOptions); err != nil {
			return err
		}
	}
	var cp opsstore.Checkpoint
	var appendInput struct {
		Event opsstore.Event `json:"event"`
		Patch opsstore.Patch `json:"patch"`
	}
	var ledger opsstore.LedgerImport
	if sub == "checkpoint" || sub == "append" {
		if values["--input"] == "" {
			return errors.New("explicit --input JSON_FILE required")
		}
		if _, ok := numbers["--expected"]; !ok {
			return errors.New("explicit --expected GENERATION required")
		}
		if sub == "checkpoint" {
			if err := opsstore.ReadInput(values["--input"], &cp); err != nil {
				return err
			}
			if err := opsstore.ValidateCheckpoint(root, lane, cp); err != nil {
				return err
			}
		} else {
			if err := opsstore.ReadInput(values["--input"], &appendInput); err != nil {
				return err
			}
		}
	}
	if sub == "import-ledger" {
		recorded := values["--recorded-root"]
		if recorded == "" {
			recorded = root
		}
		var err error
		ledger, err = opsstore.InspectLedger(values["--input"], recorded)
		if err != nil {
			return err
		}
		if !flags["--apply"] {
			body, err := opsstore.Encode(map[string]any{"root": root, "lane": lane, "recorded_root": recorded, "generation": ledger.Generation, "sha256": ledger.SHA256, "goal": ledger.Checkpoint.Goal, "apply": false}, "l0", opsstore.SummaryLimit)
			if err == nil {
				fmt.Print(string(body))
			}
			return err
		}
		if values["--expected-sha256"] != ledger.SHA256 {
			return errors.New("explicit matching --expected-sha256 required")
		}
	}
	if sub == "export-ledger" && values["--output"] == "" {
		return errors.New("explicit --output ABS_PATH required")
	}
	dir, err := opsstore.DataDir()
	if err != nil {
		return err
	}
	write := sub == "checkpoint" || sub == "append" || (sub == "import-ledger" && flags["--apply"])
	s, err := opsstore.Open(dir, write)
	if opsstore.IsMissing(err) && sub == "find" {
		body, err := opsstore.EncodeTasks(opsstore.TaskPage{Tasks: []opsstore.Task{}, Order: order}, false)
		if err == nil {
			fmt.Print(string(body))
		}
		return err
	}
	if err != nil {
		return err
	}
	defer s.Close()
	var body []byte
	var snap opsstore.Snapshot
	switch sub {
	case "find", "history":
		var page opsstore.TaskPage
		if sub == "find" {
			page, err = s.Find(root, findOptions)
		} else {
			page, err = s.History(root, lane, numbers["--before"], int(numbers["--limit"]))
		}
		if err == nil {
			body, err = opsstore.EncodeTasks(page, sub == "history")
		}
	case "read", "status":
		if flags["--event"] {
			var event json.RawMessage
			event, err = s.Event(root, lane, numbers["--generation"])
			if err == nil {
				body, err = opsstore.Encode(event, "l2", opsstore.DetailLimit)
			}
		} else {
			snap, err = s.Read(root, lane, numbers["--generation"])
			if err == nil {
				body, err = opsstore.EncodeSnapshot(snap, mode)
			}
		}
	case "checkpoint":
		snap, err = s.Save(root, lane, numbers["--expected"], cp)
	case "append":
		snap, err = s.Append(root, lane, numbers["--expected"], appendInput.Event, appendInput.Patch)
	case "import-ledger":
		snap, err = s.ImportLedger(root, lane, ledger, values["--expected-sha256"])
	case "export-ledger":
		var hash string
		hash, err = s.ExportLedger(root, lane, values["--output"])
		if err == nil {
			body, err = opsstore.Encode(map[string]string{"path": values["--output"], "sha256": hash}, "l0", opsstore.SummaryLimit)
		}
	case "doctor":
		var report map[string]any
		report, err = s.Doctor()
		if err == nil {
			body, err = opsstore.Encode(report, "l0", opsstore.SummaryLimit)
		}
	case "backup":
		var backup opsstore.Backup
		backup, err = s.Backup()
		if err == nil {
			body, err = opsstore.Encode(backup, "l0", opsstore.SummaryLimit)
		}
	}
	if err != nil {
		return err
	}
	if body == nil {
		body, err = opsstore.EncodeSnapshot(snap, "l0")
	}
	if err != nil {
		return err
	}
	fmt.Print(string(body))
	return nil
}

func containsOption(keys, key string) bool {
	for _, value := range strings.Fields(keys) {
		if value == key {
			return true
		}
	}
	return false
}
