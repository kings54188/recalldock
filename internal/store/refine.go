package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"recalldock.local/recalldock/internal/memorypolicy"
)

// RefineScaffoldMarker starts the section a draft adds from absorbed sources.
// It must not survive into stored content: saving the scaffold unchanged would
// write the tool's working notes into long-term knowledge.
const RefineScaffoldMarker = "【待人工精炼】"

// RefineDraft is a reviewable proposal: the target's current text plus the
// details only the sources carry. It never writes knowledge; the operator edits
// it and submits ApplyRefinement.
type RefineDraft struct {
	Target  Observation
	Sources []Observation
	Skipped map[int64]string
	Content string
	Digest  string
	Notes   []string
}

// RefinementResult reports what an apply actually changed. Failed sources keep
// their content and stay active; the target may already hold the new revision.
type RefinementResult struct {
	Updated  bool
	Revision int
	Absorbed []int64
	Failed   map[int64]string
}

// BuildRefineDraft merges a target with the unique sentences of its sources.
// Sentences already covered by the target (or by an earlier source) are
// dropped, so the draft is a starting point rather than a concatenation.
func (s *Store) BuildRefineDraft(targetID int64, sourceIDs []int64) (*RefineDraft, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	target, err := s.getObservationTx(tx, targetID)
	if err != nil {
		return nil, err
	}
	if target.ArchivedAt != nil {
		return nil, errors.New("target is archived; reactivate or choose an active memory")
	}
	selected, err := selectionTx(tx, target)
	if err != nil {
		return nil, err
	}
	draft := &RefineDraft{
		Target:  *target,
		Skipped: map[int64]string{},
		Digest:  selected.SnapshotSHA256,
	}
	covered := similarityTokens(target.Title + " " + target.EffectiveAbstract() + " " + target.Content)
	seen := map[int64]bool{}
	var extras []string
	for _, sourceID := range sourceIDs {
		if sourceID == targetID || seen[sourceID] {
			continue
		}
		seen[sourceID] = true
		if len(seen) > 200 {
			return nil, errors.New("too many refinement sources")
		}
		source, err := s.getObservationTx(tx, sourceID)
		if err != nil {
			if !errors.Is(err, ErrObservationNotFound) {
				return nil, err
			}
			draft.Skipped[sourceID] = "找不到这条记忆"
			continue
		}
		if source.ArchivedAt != nil {
			draft.Skipped[sourceID] = "已归档（可能已经合并过）"
			continue
		}
		if source.Pinned {
			draft.Skipped[sourceID] = "已置顶；合并前需要先取消置顶"
			continue
		}
		if derefString(source.Project) != derefString(target.Project) ||
			normalizeScope(source.Scope) != normalizeScope(target.Scope) {
			draft.Skipped[sourceID] = "与目标不在同一项目/作用域"
			continue
		}
		draft.Sources = append(draft.Sources, *source)
		for _, sentence := range refineSentences(*source) {
			if len([]rune(sentence)) < 8 {
				continue
			}
			tokens := similarityTokens(sentence)
			if len(tokens) < 3 {
				continue
			}
			if memorySimilarity(tokens, covered) >= 0.82 {
				continue
			}
			extras = append(extras, fmt.Sprintf("- #%d %s", source.ID, sentence))
			for token := range tokens {
				covered[token] = struct{}{}
			}
		}
	}

	content := target.Content
	if len(extras) > 0 {
		header := RefineScaffoldMarker +
			"：下面是从被合并记忆里提取的独有细节。合并进正文后，删除这一段（连同标记）再保存。\n"
		content = strings.TrimRight(content, "\n") + "\n\n" + header + strings.Join(extras, "\n")
	} else {
		draft.Notes = append(draft.Notes, "没有发现来源独有的细节；可以直接保存正文并归档来源。")
	}
	// A truncated draft could silently discard reviewed source or target content.
	if limit := s.MaxObservationLength(); limit > 0 && len(content) > limit {
		return nil, errors.New("refinement draft exceeds the content limit; reduce the number of sources or refine them separately")
	}
	draft.Content = content
	draft.Digest, err = refinementDigest(tx, target, draft.Sources)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if len(draft.Skipped) > 0 {
		draft.Notes = append(draft.Notes, fmt.Sprintf("已跳过 %d 条来源（见下方说明）。", len(draft.Skipped)))
	}
	return draft, nil
}

// ApplyRefinement saves the edited text with the draft's snapshot digest, then
// archives the sources and records lineage. If the target text is unchanged it
// skips the revision but still verifies the digest before archiving anything.
func (s *Store) ApplyRefinement(targetID int64, sourceIDs []int64, content, expected, reason, directory string) (RefinementResult, error) {
	var result RefinementResult
	content = strings.TrimSpace(content)
	if content == "" {
		return result, errors.New("refined content required")
	}
	if !utf8.ValidString(content) {
		return result, errors.New("refined content must be valid UTF-8")
	}
	if len(content) > s.cfg.MaxObservationLength {
		return result, fmt.Errorf("refined content exceeds %d bytes", s.cfg.MaxObservationLength)
	}
	if strings.Contains(content, RefineScaffoldMarker) {
		return result, fmt.Errorf("refined content still contains %s; merge the details and delete the scaffold first", RefineScaffoldMarker)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 512 || !utf8.ValidString(reason) ||
		memorypolicy.ContainsSecret(reason) || memorypolicy.ContainsTranscript(reason) {
		return result, errors.New("bounded non-secret operator edit reason required")
	}
	target, err := s.GetObservation(targetID)
	if err != nil {
		return result, err
	}
	if err := s.validateRegisteredWrite(derefString(target.Project), directory, SessionOriginOperator); err != nil {
		return result, err
	}
	var failures map[int64]string
	err = s.withTx(func(tx *sql.Tx) error {
		// Acquire SQLite's writer lock before taking the snapshots used for CAS.
		locked, err := s.execHook(tx, "UPDATE observations SET id=id WHERE id=? AND deleted_at IS NULL", targetID)
		if err != nil {
			return err
		}
		count, err := locked.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrObservationNotFound
		}
		current, err := s.getObservationTx(tx, targetID)
		if err != nil {
			return err
		}
		if current.ArchivedAt != nil {
			return ErrObservationSnapshotChanged
		}
		if derefString(current.Project) != derefString(target.Project) {
			return ErrObservationSnapshotChanged
		}
		sources := make([]Observation, 0, len(sourceIDs))
		failures = make(map[int64]string)
		seen := make(map[int64]bool)
		for _, id := range sourceIDs {
			if id == targetID || seen[id] {
				continue
			}
			seen[id] = true
			if len(seen) > 200 {
				return errors.New("too many refinement sources")
			}
			source, err := s.getObservationTx(tx, id)
			if err != nil {
				if errors.Is(err, ErrObservationNotFound) {
					failures[id] = "not found"
					continue
				}
				return err
			}
			if source.ArchivedAt != nil {
				failures[id] = "already archived"
				continue
			}
			if source.Pinned {
				failures[id] = "pinned memory cannot be absorbed; unpin first"
				continue
			}
			if derefString(source.Project) != derefString(current.Project) || normalizeScope(source.Scope) != normalizeScope(current.Scope) {
				failures[id] = "source and target must share project and scope"
				continue
			}
			sources = append(sources, *source)
		}
		digest, err := refinementDigest(tx, current, sources)
		if err != nil {
			return err
		}
		if expected == "" || digest != expected {
			return ErrObservationSnapshotChanged
		}
		result = RefinementResult{}
		if content != current.Content {
			patch, changed, err := s.normalizeCASPatch(current, UpdateObservationParams{Content: &content})
			if err != nil {
				return err
			}
			if changed {
				selected, err := selectionTx(tx, current)
				if err != nil {
					return err
				}
				updated, err := s.updateObservationTx(tx, targetID, patch)
				if err != nil {
					return err
				}
				if _, err := s.execHook(tx, `INSERT INTO observation_edit_audits(observation_id,sequence,expected_snapshot_sha256,operator_reason) SELECT ?,max(sequence),?,? FROM observation_versions WHERE observation_id=?`, targetID, selected.SnapshotSHA256, reason, targetID); err != nil {
					return err
				}
				result.Updated = true
				result.Revision = updated.RevisionCount
			}
		}
		for i := range sources {
			source := &sources[i]
			flag := true
			patch, _, err := s.normalizeCASPatch(source, UpdateObservationParams{Archived: &flag})
			if err != nil {
				failures[source.ID] = err.Error()
				continue
			}
			selected, err := selectionTx(tx, source)
			if err != nil {
				return err
			}
			if _, err := s.updateObservationTx(tx, source.ID, patch); err != nil {
				return err
			}
			if _, err := s.execHook(tx, `INSERT INTO observation_edit_audits(observation_id,sequence,expected_snapshot_sha256,operator_reason) SELECT ?,max(sequence),?,? FROM observation_versions WHERE observation_id=?`, source.ID, selected.SnapshotSHA256, reason, source.ID); err != nil {
				return err
			}
			if _, err := s.execHook(tx, `INSERT OR IGNORE INTO memory_consolidations(consolidated_sync_id,source_sync_id,created_at) VALUES(?,?,?)`, current.SyncID, source.SyncID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
			result.Absorbed = append(result.Absorbed, source.ID)
		}
		return nil
	})
	if err != nil {
		return RefinementResult{}, err
	}
	if len(failures) != 0 {
		result.Failed = failures
		return result, &ConsolidationError{Failed: failures}
	}
	return result, nil
}

// refinementDigest binds the target and every eligible source to the reviewed draft.
func refinementDigest(tx *sql.Tx, target *Observation, sources []Observation) (string, error) {
	items := make([]string, 0, len(sources))
	selected, err := selectionTx(tx, target)
	if err != nil {
		return "", err
	}
	for i := range sources {
		source, err := selectionTx(tx, &sources[i])
		if err != nil {
			return "", err
		}
		items = append(items, fmt.Sprintf("%d:%s", sources[i].ID, source.SnapshotSHA256))
	}
	sort.Strings(items)
	encoded, err := json.Marshal(struct {
		Target  string
		Sources []string
	}{selected.SnapshotSHA256, items})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// refineSentences returns candidate details from a source. Titles are skipped
// on purpose: they are the most repeated text and add nothing to the target.
func refineSentences(source Observation) []string {
	text := source.Content
	if strings.TrimSpace(source.Abstract) != "" {
		text += "\n" + source.Abstract
	}
	var out []string
	for _, line := range strings.Split(text, "\n") {
		for _, sentence := range splitRefineSentence(line) {
			sentence = strings.TrimSpace(sentence)
			sentence = strings.Trim(sentence, "-*•· \t")
			if sentence != "" {
				out = append(out, sentence)
			}
		}
	}
	return out
}

func splitRefineSentence(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool {
		switch r {
		case '。', '！', '？', '；', ';', '!', '?':
			return true
		}
		return false
	})
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "\n…（草稿已截断）"
}

// SortedSkippedIDs makes the draft's skip list deterministic for callers that
// render it (map order is otherwise random).
func (d *RefineDraft) SortedSkippedIDs() []int64 {
	ids := make([]int64, 0, len(d.Skipped))
	for id := range d.Skipped {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
