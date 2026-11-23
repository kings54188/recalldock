package worklog

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

func DecodeLedger(body []byte, recordedRoot string) (Checkpoint, int64, error) {
	var cp Checkpoint
	if !filepath.IsAbs(recordedRoot) || len(body) > FileLimit {
		return cp, 0, errors.New("absolute recorded root and bounded ledger required")
	}
	generation, err := checkpointGeneration(body, recordedRoot)
	if err != nil {
		return cp, 0, err
	}
	_, current, ok := strings.Cut(string(body), "\n# Current task\n\n")
	if !ok {
		return cp, 0, errors.New("missing task section")
	}
	cp.Goal, current, ok = strings.Cut(current, "\n\n")
	if !ok {
		return cp, 0, errors.New("missing task summary")
	}
	cp.Summary, current, ok = strings.Cut(current, "\n\n## Next\n")
	if !ok {
		return cp, 0, errors.New("missing next section")
	}
	next, current, ok := strings.Cut(current, detailMarker)
	if !ok {
		return cp, 0, errors.New("missing detail boundary")
	}
	for _, line := range strings.Split(strings.TrimSpace(next), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "- ") {
			return cp, 0, errors.New("invalid next step")
		}
		cp.Next = append(cp.Next, strings.TrimPrefix(line, "- "))
	}
	operations, details, ok := strings.Cut(current, "\n## Selected details\n")
	if !ok {
		return cp, 0, errors.New("missing selected details")
	}
	cp.Details = strings.TrimSuffix(details, "\n")
	for _, line := range strings.Split(operations, "\n") {
		if !strings.HasPrefix(line, "- id=") {
			continue
		}
		var op Operation
		if _, err := fmt.Sscanf(line, "- id=%s status=%s input=%q evidence=%q", &op.ID, &op.Status, &op.InputFingerprint, &op.Evidence); err != nil {
			return cp, 0, errors.New("invalid operation metadata")
		}
		cp.Operations = append(cp.Operations, op)
	}
	if err := ValidateCheckpoint(recordedRoot, cp); err != nil {
		return cp, 0, err
	}
	return cp, generation, nil
}

func RenderLedger(root string, generation int64, cp Checkpoint) ([]byte, error) {
	if !filepath.IsAbs(root) || generation < 1 {
		return nil, errors.New("absolute root and positive generation required")
	}
	return render(root, generation, cp)
}
