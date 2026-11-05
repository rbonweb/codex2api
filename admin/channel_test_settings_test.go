package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestConnectionTestPayloadUsesGlobalTestContent(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetTestContent("全局测活 {{date}}")
	handler := &Handler{store: store}
	account := &auth.Account{DBID: 9, AccessToken: "codex-token"}

	payload := handler.buildAccountConnectionTestPayload(context.Background(), account, "gpt-5.5")
	if !strings.Contains(string(payload), "全局测活") || strings.Contains(string(payload), "{{date}}") {
		t.Fatalf("payload %s must carry the rendered global test content", payload)
	}
}
