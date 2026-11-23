// Package worklog stores bounded task recovery notes, never long-term knowledge.
package worklog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"recalldock.local/recalldock/internal/memorypolicy"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const SummaryLimit = 2048
const FileLimit = 12288
const marker = "<!-- recalldock-worklog:v1 -->\n"
const detailMarker = "<!-- details -->\n"

var ErrGeneration = errors.New("checkpoint generation mismatch; reread before retry")
var ErrBudget = errors.New("checkpoint byte budget exceeded; condense or reference evidence")
var lanePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)

type Operation struct {
	ID               string `json:"id"`
	InputFingerprint string `json:"input_fingerprint"`
	Status           string `json:"status"`
	Evidence         string `json:"evidence,omitempty"`
}

type Checkpoint struct {
	Goal       string      `json:"goal"`
	Summary    string      `json:"summary"`
	Next       []string    `json:"next"`
	Operations []Operation `json:"operations,omitempty"`
	Details    string      `json:"details,omitempty"`
}

func paths(root, lane string, create bool) (string, string, error) {
	if !filepath.IsAbs(root) || !lanePattern.MatchString(lane) {
		return "", "", errors.New("absolute project root and safe exact lane required")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", errors.New("project root must be a directory")
	}
	dir := root
	for _, part := range []string{".recalldock", "work", lane} {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) && create {
			if err = os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
				return "", "", err
			}
			info, err = os.Lstat(dir)
		}
		if err != nil {
			return "", "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("unsafe checkpoint directory")
		}
	}
	return root, filepath.Join(dir, "current.md"), nil
}

func openCheckpoint(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > FileLimit {
		return nil, errors.New("unsafe/oversize checkpoint file")
	}
	return os.Open(path)
}

func checkpointGeneration(body []byte, canonical string) (int64, error) {
	if !strings.HasPrefix(string(body), marker) {
		return 0, errors.New("manual/unsupported ledger: preserve and inspect it explicitly")
	}
	lines := strings.SplitN(string(body), "\n", 4)
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "generation: ") {
		return 0, errors.New("malformed checkpoint generation")
	}
	if !strings.HasPrefix(lines[2], "root: ") {
		return 0, errors.New("missing checkpoint root identity")
	}
	recorded, err := strconv.Unquote(strings.TrimPrefix(lines[2], "root: "))
	if err != nil || recorded != canonical {
		return 0, errors.New("checkpoint canonical root differs; preserve the ledger and reconcile before using a new lane")
	}
	generation, err := strconv.ParseInt(strings.TrimPrefix(lines[1], "generation: "), 10, 64)
	if err != nil || generation < 1 {
		return 0, errors.New("invalid checkpoint generation")
	}
	return generation, nil
}

// Read defaults to physically reading at most SummaryLimit+1 bytes. Full detail
// requires an explicit opt-in. Old manual ledgers are not silently adopted.
func Read(root, lane string, full bool) (string, error) {
	canonical, path, err := paths(root, lane, false)
	if err != nil {
		return "", err
	}
	f, err := openCheckpoint(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	limit := SummaryLimit
	if full {
		limit = FileLimit
	}
	body, err := io.ReadAll(io.LimitReader(f, int64(limit+1)))
	if err != nil {
		return "", err
	}
	if _, err := checkpointGeneration(body, canonical); err != nil {
		return "", err
	}
	if !full {
		idx := strings.Index(string(body), detailMarker)
		if idx < 0 || idx > SummaryLimit {
			return "", ErrBudget
		}
		return string(body[:idx]), nil
	}
	if len(body) > FileLimit {
		return "", ErrBudget
	}
	return string(body), nil
}

func ValidateCheckpoint(root string, cp Checkpoint) error {
	_, err := render(root, 1, cp)
	return err
}

func render(root string, generation int64, cp Checkpoint) ([]byte, error) {
	fields := []string{cp.Goal, cp.Summary, cp.Details}
	fields = append(fields, cp.Next...)
	for _, op := range cp.Operations {
		fields = append(fields, op.InputFingerprint, op.Evidence)
	}
	for _, field := range fields {
		unsafeControl := strings.ContainsFunc(field, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' })
		if unsafeControl || memorypolicy.ContainsSecret(field) || memorypolicy.ContainsTranscript(field) {
			return nil, errors.New("checkpoint rejects secrets, raw transcripts and unsafe control characters; condense and reference evidence")
		}
	}

	if strings.TrimSpace(cp.Goal) == "" || strings.ContainsAny(cp.Goal, "\r\n") || len(cp.Goal) > 300 || len(cp.Next) > 3 || len(cp.Operations) > 12 {
		return nil, errors.New("goal must be one line <=300 bytes; next <=3; operations <=12")
	}
	for _, next := range cp.Next {
		if len(next) > 300 || strings.ContainsAny(next, "\r\n") {
			return nil, errors.New("each next step must be one line <=300 bytes")
		}
	}
	var summary strings.Builder
	fmt.Fprintf(&summary, "%sgeneration: %d\nroot: %q\n\n# Current task\n\n%s\n\n%s\n\n## Next\n", marker, generation, root, cp.Goal, cp.Summary)
	for _, next := range cp.Next {
		fmt.Fprintf(&summary, "- %s\n", next)
	}
	if strings.Contains(summary.String()[len(marker):], marker) || strings.Contains(summary.String(), detailMarker) {
		return nil, errors.New("reserved checkpoint delimiter in input")
	}
	if summary.Len()+len(detailMarker) > SummaryLimit {
		return nil, ErrBudget
	}
	summary.WriteString(detailMarker)
	summary.WriteString("\n## Operations (reported state; reconcile with actual artifacts)\n")
	ids := map[string]bool{}
	for _, op := range cp.Operations {
		if !lanePattern.MatchString(op.ID) || ids[op.ID] || op.InputFingerprint == "" || len(op.InputFingerprint) > 200 || len(op.Evidence) > 400 || strings.ContainsAny(op.InputFingerprint+op.Evidence, "\r\n") {
			return nil, errors.New("operation IDs must be unique/safe and metadata bounded single lines")
		}
		ids[op.ID] = true
		switch op.Status {
		case "reported", "started", "applied", "failed", "needs_check":
		case "verified":
			if op.Evidence == "" {
				return nil, errors.New("reported verified operation requires an evidence reference")
			}
		default:
			return nil, errors.New("invalid operation status; allowed: reported, started, applied, failed, needs_check, verified (verified requires evidence)")
		}
		fmt.Fprintf(&summary, "- id=%s status=%s input=%q evidence=%q\n", op.ID, op.Status, op.InputFingerprint, op.Evidence)
	}
	summary.WriteString("\n## Selected details\n")
	summary.WriteString(cp.Details)
	summary.WriteByte('\n')
	if summary.Len() > FileLimit {
		return nil, ErrBudget
	}
	if !utf8.ValidString(summary.String()) {
		return nil, errors.New("checkpoint must be valid UTF-8")
	}
	return []byte(summary.String()), nil
}

// Save is generation-checked and cross-process serialized. Stale locks are not
// stolen by timeout: inspect active processes before manual recovery. Individual
// file replacement is atomic; an error after rename requires rereading state.
func Save(root, lane string, expected int64, cp Checkpoint) (int64, error) {
	if expected < 0 || expected == int64(^uint64(0)>>1) {
		return 0, ErrGeneration
	}
	// Validate budgets before creating any ledger directories.
	if _, err := render(root, expected+1, cp); err != nil {
		return 0, err
	}
	canonical, path, err := paths(root, lane, true)
	if err != nil {
		return 0, err
	}
	lockPath := filepath.Join(filepath.Dir(path), ".checkpoint.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, fmt.Errorf("checkpoint lock unavailable; inspect before recovery: %w", err)
	}
	defer func() { lock.Close(); os.Remove(lockPath) }()
	generation := int64(0)
	f, err := openCheckpoint(path)
	if err == nil {
		body, readErr := io.ReadAll(io.LimitReader(f, SummaryLimit+1))
		f.Close()
		if readErr != nil {
			return 0, readErr
		}
		generation, err = checkpointGeneration(body, canonical)
		if err != nil {
			return 0, err
		}
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	if generation != expected {
		return 0, ErrGeneration
	}
	body, err := render(canonical, generation+1, cp)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return 0, err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return 0, err
		}
		defer dir.Close()
		if err := dir.Sync(); err != nil {
			return 0, err
		}
	}
	return generation + 1, nil
}
