package memorypolicy

import (
	"strings"
	"testing"
)

func TestMetadataDoesNotBecomeAHiddenTranscriptOrSecretStore(t *testing.T) {
	for _, text := range []string{"DEEPSEEK_API_KEY=small", "password=short", "密码：不可保存", "https://name:password@example.invalid/repo", "用户：原始对话\n助手：完整回复", "系统: 原始指令"} {
		if !ContainsSecret(text) && !ContainsTranscript(text) {
			t.Fatalf("missed unsafe text %q", text)
		}
	}
	for _, fields := range []map[string]any{{"id": "valid", "project": "password=short"}, {"id": "transcript\nraw"}, {"id": strings.Repeat("x", 193)}, {"id": "safe", "directory": "/tmp/raw\n用户：原始聊天"}, {"id": "safe", "summary": "progress"}, {"id": "safe", "id_extra": "raw"}, {"id": 15}} {
		if ValidateLifecycleMetadata(fields, false) == nil {
			t.Fatalf("accepted unsafe metadata: keys=%d", len(fields))
		}
	}
	for _, fields := range []map[string]any{{"id": "session-abc:123", "project": "prj-a", "directory": "/tmp/local"}, {"id": "session", "summary": ""}} {
		if err := ValidateLifecycleMetadata(fields, false); err != nil {
			t.Fatalf("valid metadata: %v", err)
		}
	}
}
