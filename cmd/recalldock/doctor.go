package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"recalldock.local/recalldock/internal/diagnostic"
	"recalldock.local/recalldock/internal/store"
)

func cmdDoctor(cfg store.Config) {
	if len(os.Args) > 2 && os.Args[2] == "repair" {
		cmdDoctorRepair(cfg)
		return
	}
	jsonOut := false
	project := ""
	check := ""
	var silence []string
	for _, code := range strings.Split(os.Getenv("RECALLDOCK_DOCTOR_SILENCE_WARNINGS"), ",") {
		if code = strings.TrimSpace(code); code != "" {
			silence = append(silence, code)
		}
	}
	for i := 2; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--json":
			jsonOut = true
		case "--project":
			if i+1 >= len(os.Args) {
				fmt.Fprintln(os.Stderr, "error: --project requires a value")
				exitFunc(1)
				return
			}
			project = os.Args[i+1]
			i++
		case "--check":
			if i+1 >= len(os.Args) {
				fmt.Fprintln(os.Stderr, "error: --check requires a value")
				exitFunc(1)
				return
			}
			check = os.Args[i+1]
			i++
		case "--silence-warning":
			if i+1 >= len(os.Args) {
				fmt.Fprintln(os.Stderr, "error: --silence-warning requires a check code")
				exitFunc(1)
				return
			}
			silence = append(silence, strings.TrimSpace(os.Args[i+1]))
			i++
		case "--help", "-h", "help":
			printDoctorUsage()
			return
		default:
			fmt.Fprintf(os.Stderr, "error: unknown doctor argument %q\n", os.Args[i])
			printDoctorUsage()
			exitFunc(1)
			return
		}
	}

	project, _ = store.NormalizeProject(project)
	for _, code := range silence {
		if _, err := diagnostic.DefaultRegistry().Lookup(code); err != nil {
			if jsonOut {
				writeDoctorJSON(diagnostic.ErrorReport(project, err))
			} else {
				fmt.Fprintf(os.Stderr, "recalldock doctor failed: %s\n", err)
			}
			exitFunc(1)
			return
		}
	}
	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	report, err := runDiagnostics(context.Background(), s, strings.TrimSpace(project), strings.TrimSpace(check))
	if err == nil && len(silence) > 0 {
		report, err = report.SilenceWarnings(silence)
	}
	if err != nil {
		report = diagnostic.ErrorReport(project, err)
	}
	if jsonOut {
		writeDoctorJSON(report)
	} else {
		renderDoctorText(report)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recalldock doctor failed: %s\n", err)
		}
	}
	if err != nil || report.Status == diagnostic.StatusBlocked || report.Status == diagnostic.StatusError || report.Status == "failed" {
		exitFunc(1)
	}
}

func printDoctorUsage() {
	fmt.Fprintln(os.Stdout, "usage: recalldock doctor [--json] [--project PROJECT] [--check CODE] [--silence-warning CODE]")
	fmt.Fprintln(os.Stdout, "       recalldock doctor repair --project PROJECT --check CODE (--plan|--dry-run|--apply)")
	fmt.Fprintln(os.Stdout, "checks: "+strings.Join(diagnostic.RegisteredCodes(), ", "))
}

func cmdDoctorRepair(cfg store.Config) {
	project := ""
	check := ""
	mode := diagnostic.RepairMode("")
	modeCount := 0
	for i := 3; i < len(os.Args); i++ {
		switch os.Args[i] {
		case "--project":
			if i+1 >= len(os.Args) {
				failDoctorRepair("--project requires a value")
				return
			}
			project = os.Args[i+1]
			i++
		case "--check":
			if i+1 >= len(os.Args) {
				failDoctorRepair("--check requires a value")
				return
			}
			check = os.Args[i+1]
			i++
		case "--plan":
			mode = diagnostic.RepairModePlan
			modeCount++
		case "--dry-run":
			mode = diagnostic.RepairModeDryRun
			modeCount++
		case "--apply":
			mode = diagnostic.RepairModeApply
			modeCount++
		case "--help", "-h", "help":
			printDoctorUsage()
			return
		default:
			failDoctorRepair(fmt.Sprintf("unknown doctor repair argument %q", os.Args[i]))
			return
		}
	}

	project, _ = store.NormalizeProject(project)
	project = strings.TrimSpace(project)
	check = strings.TrimSpace(check)
	if project == "" {
		failDoctorRepair("--project is required")
		return
	}
	if check == "" {
		failDoctorRepair("--check is required")
		return
	}
	if modeCount != 1 {
		failDoctorRepair("exactly one of --plan, --dry-run, or --apply is required")
		return
	}
	if !isSupportedDoctorRepairCheck(check) {
		failDoctorRepair("unsupported repair check " + check + " (repairable: " + diagnostic.CheckSessionProjectDirectoryMismatch + ", " + diagnostic.CheckManualSessionNameProjectMismatch + ")")
		return
	}

	s, err := storeNew(cfg)
	if err != nil {
		fatal(err)
		return
	}
	defer s.Close()

	ctx := context.Background()
	report, err := runDiagnostics(ctx, s, project, check)
	if err != nil {
		failDoctorRepair(err.Error())
		return
	}
	plan, err := diagnostic.BuildRepairPlan(ctx, diagnostic.Scope{Store: s, Project: project}, report, check, mode)
	if err != nil {
		failDoctorRepair(err.Error())
		return
	}
	actions := make([]store.SessionProjectReclassification, 0, len(plan.Actions))
	for _, action := range plan.Actions {
		actions = append(actions, store.SessionProjectReclassification{SessionID: action.SessionID, FromProject: action.FromProject, ToProject: action.ToProject})
	}
	if mode == diagnostic.RepairModeApply && len(actions) > 0 {
		counts, err := s.EstimateSessionProjectReclassification(actions)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		plan.Counts.SessionsPlanned = counts.Sessions
		plan.Counts.ObservationsPlanned = counts.Observations
		plan.Counts.PromptsPlanned = counts.Prompts
		result, err := s.ApplySessionProjectReclassification(actions)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		plan.Status = "applied"
		if result.Counts.Sessions+result.Counts.Observations+result.Counts.Prompts == 0 {
			// Another process applied the same plan first; the guarded UPDATEs
			// matched nothing. Report it honestly instead of claiming a change.
			plan.Status = "noop"
		}
		plan.BackupPath = result.BackupPath
		plan.Counts.SessionsApplied = result.Counts.Sessions
		plan.Counts.ObservationsApplied = result.Counts.Observations
		plan.Counts.PromptsApplied = result.Counts.Prompts
	} else {
		counts, err := s.EstimateSessionProjectReclassification(actions)
		if err != nil {
			failDoctorRepair(err.Error())
			return
		}
		plan.Counts.SessionsPlanned = counts.Sessions
		plan.Counts.ObservationsPlanned = counts.Observations
		plan.Counts.PromptsPlanned = counts.Prompts
	}
	writeDoctorRepairJSON(plan)
}

func isSupportedDoctorRepairCheck(check string) bool {
	switch check {
	case diagnostic.CheckSessionProjectDirectoryMismatch, diagnostic.CheckManualSessionNameProjectMismatch:
		return true
	default:
		return false
	}
}

func failDoctorRepair(message string) {
	fmt.Fprintln(os.Stderr, "recalldock doctor repair failed: "+message)
	printDoctorUsage()
	exitFunc(1)
}

func writeDoctorRepairJSON(plan diagnostic.RepairPlan) {
	out, err := jsonMarshalIndent(plan, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

func writeDoctorJSON(report diagnostic.Report) {
	out, err := jsonMarshalIndent(report, "", "  ")
	if err != nil {
		fatal(err)
		return
	}
	fmt.Println(string(out))
}

func renderDoctorText(report diagnostic.Report) {
	fmt.Printf("RecallDock Doctor: %s\n", report.Status)
	if report.Project != "" {
		fmt.Printf("Project: %s\n", report.Project)
	}
	fmt.Printf("Checks: %d ok=%d warnings=%d silenced_warnings=%d blocked=%d errors=%d\n\n", report.Summary.Total, report.Summary.OK, report.Summary.Warnings, report.Summary.SilencedWarnings, report.Summary.Blocked, report.Summary.Errors)
	for _, check := range report.Checks {
		if check.Silenced {
			fmt.Printf("[silenced] %s — warning deferred; stored data unchanged\n", check.CheckID)
			continue
		}
		fmt.Printf("[%s] %s — %s\n", check.Result, check.CheckID, check.Message)
		if check.Why != "" {
			fmt.Printf("  why: %s\n", check.Why)
		}
		if check.SafeNextStep != "" {
			fmt.Printf("  next: %s\n", check.SafeNextStep)
		}
		for _, finding := range check.Findings {
			fmt.Printf("  - %s: %s\n", finding.ReasonCode, finding.Message)
			if len(finding.Evidence) > 0 {
				fmt.Printf("    evidence: %s\n", string(finding.Evidence))
			}
		}
	}
}
