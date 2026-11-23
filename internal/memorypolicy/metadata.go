package memorypolicy

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\b(?:[a-z0-9]+[_-])*(?:api[_-]?key|access[_-]?token|password|client[_-]?secret)\s*[:=]\s*["']?[^\s"';,]+`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`(?i)https?://[^/\s:@]+:[^@\s/]+@`),
	regexp.MustCompile(`(?:密码|密钥|令牌)\s*[:：=]\s*[^\s]+`),
}
var transcriptPattern = regexp.MustCompile(`(?mi)^\s*(?:user|assistant|system|tool|用户|助手|系统|工具)\s*[:：]`)
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,191}$`)
var errMetadata = errors.New("invalid lifecycle/proposal metadata: use bounded opaque session IDs and single-line project/path metadata; no secrets, raw transcripts or summaries")

// Lexical safety is a conservative guard, not semantic classification or a
// complete DLP guarantee. Do not echo rejected text into errors/audit records.
func ContainsSecret(value string) bool {
	if strings.Contains(strings.ToLower(value), "<private>") {
		return true
	}
	for _, p := range secretPatterns {
		if p.MatchString(value) {
			return true
		}
	}
	return false
}
func ContainsTranscript(value string) bool { return transcriptPattern.MatchString(value) }

func ValidateSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) || ContainsSecret(id) {
		return errMetadata
	}
	return nil
}

func safeMetadataText(value string, max int) bool {
	return len(value) <= max && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl) && !ContainsSecret(value) && !ContainsTranscript(value)
}

func ValidateLifecycleMetadata(fields map[string]any, end bool) error {
	for key, raw := range fields {
		value, ok := raw.(string)
		if !ok {
			return errMetadata
		}
		switch key {
		case "id":
			if ValidateSessionID(value) != nil {
				return errMetadata
			}
		case "project":
			if end || !safeMetadataText(value, 200) {
				return errMetadata
			}
		case "directory":
			if end || !safeMetadataText(value, 4096) {
				return errMetadata
			}
		case "summary":
			if strings.TrimSpace(value) != "" {
				return errMetadata
			}
		default:
			return errMetadata
		}
	}
	id, _ := fields["id"].(string)
	return ValidateSessionID(id)
}

func ValidateProposalMetadata(fields map[string]any) error {
	for key, raw := range fields {
		switch key {
		case "session_id":
			id, ok := raw.(string)
			if !ok || ValidateSessionID(id) != nil {
				return errMetadata
			}
		case "project":
			value, ok := raw.(string)
			if !ok || !safeMetadataText(value, 200) {
				return errMetadata
			}
		case "candidates": // Selected candidate parsing/admission has independent bounds.
		default:
			return errMetadata
		}
	}
	id, _ := fields["session_id"].(string)
	return ValidateSessionID(id)
}
