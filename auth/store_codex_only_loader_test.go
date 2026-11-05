package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
)

func TestStoreInitLoadsOnlyCodexAccounts(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex-only-loader.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}

	fixtures := []struct {
		name        string
		credentials map[string]any
		wantLoaded  bool
	}{
		{name: "grok", credentials: map[string]any{"upstream_type": "grok", "api_key": "xai-key"}},
		{name: "xai", credentials: map[string]any{"upstream_type": "XAI", "api_key": "xai-key"}},
		{name: "claude", credentials: map[string]any{"upstream_type": "claude", "access_token": "claude-at", "refresh_token": "claude-rt"}},
		{name: "antigravity", credentials: map[string]any{"upstream_type": "antigravity", "access_token": "ag-at", "project_id": "p"}},
		{name: "relay", credentials: map[string]any{"upstream_type": "openai_responses", "base_url": "https://relay.invalid/v1", "api_key": "sk-relay"}},
		{name: "codex", wantLoaded: true, credentials: map[string]any{"refresh_token": "codex-rt", "plan_type": "plus"}},
		{name: "codex-explicit-type", wantLoaded: true, credentials: map[string]any{"upstream_type": "codex", "access_token": "codex-at"}},
		{name: "agent-identity", wantLoaded: true, credentials: map[string]any{
			"auth_mode":         CodexAuthModeAgentIdentity,
			"agent_runtime_id":  "runtime-id",
			"agent_private_key": "private-key",
			"task_id":           "task-id",
		}},
	}
	wantIDs := map[int64]string{}
	for _, fixture := range fixtures {
		id, err := db.InsertAccountWithCredentials(ctx, fixture.name, fixture.credentials, "")
		if err != nil {
			t.Fatalf("insert %s: %v", fixture.name, err)
		}
		if fixture.wantLoaded {
			wantIDs[id] = fixture.name
		}
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, FastSchedulerEnabled: true})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init must not fail on removed-provider rows: %v", err)
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(store.accounts) != len(wantIDs) {
		t.Fatalf("loaded %d accounts, want %d", len(store.accounts), len(wantIDs))
	}
	for _, account := range store.accounts {
		if _, ok := wantIDs[account.DBID]; !ok {
			t.Fatalf("removed-provider account %d (upstream %q) was loaded", account.DBID, account.UpstreamType)
		}
		delete(wantIDs, account.DBID)
	}
	if len(wantIDs) != 0 {
		t.Fatalf("Codex accounts were not loaded: %v", wantIDs)
	}
}

func TestIsRemovedProviderUpstreamType(t *testing.T) {
	for _, value := range []string{"claude", "Grok", " xai ", "ANTIGRAVITY", "openai_responses"} {
		if !isRemovedProviderUpstreamType(value) {
			t.Errorf("%q should be a removed provider", value)
		}
	}
	for _, value := range []string{"", "codex", "openai", "something-new"} {
		if isRemovedProviderUpstreamType(value) {
			t.Errorf("%q must stay a Codex account", value)
		}
	}
}
