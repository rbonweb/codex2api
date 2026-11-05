package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func onDemandResponseCacheConfig() responseCacheConfig {
	config := defaultResponseCacheConfig()
	config.writePolicy = database.ResponseCacheWritePolicyOnDemand
	return config
}

func writePolicyTestItems() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"do","arguments":"{}"}`),
	}
}

func TestOnDemandWritePolicySkipsWriteForUnchainedOwner(t *testing.T) {
	resetResponseCacheStateForTest(onDemandResponseCacheConfig())

	setResponseCache("key:1", "resp_1", writePolicyTestItems())

	if cached := getResponseCache("key:1", "resp_1"); cached != nil {
		t.Fatalf("expected write to be skipped for unchained owner, got %d items", len(cached))
	}
	stats := GetResponseCacheStats()
	if stats.SkippedWrites != 1 {
		t.Fatalf("SkippedWrites = %d, want 1", stats.SkippedWrites)
	}
	if stats.Entries != 0 {
		t.Fatalf("Entries = %d, want 0", stats.Entries)
	}
}

func TestOnDemandWritePolicyAllowsWriteAfterOwnerChains(t *testing.T) {
	resetResponseCacheStateForTest(onDemandResponseCacheConfig())

	// Any single chained query (here it necessarily misses) grants that owner write eligibility.
	if cached := getResponseCache("key:1", "resp_unknown"); cached != nil {
		t.Fatalf("expected miss, got %d items", len(cached))
	}

	setResponseCache("key:1", "resp_1", writePolicyTestItems())

	if cached := getResponseCache("key:1", "resp_1"); len(cached) != 1 {
		t.Fatalf("expected 1 cached item after owner chained, got %d", len(cached))
	}
	if stats := GetResponseCacheStats(); stats.SkippedWrites != 0 {
		t.Fatalf("SkippedWrites = %d, want 0", stats.SkippedWrites)
	}
}

func TestOnDemandWritePolicyGatePerOwner(t *testing.T) {
	resetResponseCacheStateForTest(onDemandResponseCacheConfig())

	getResponseCache("key:1", "resp_unknown")

	setResponseCache("key:1", "resp_a", writePolicyTestItems())
	setResponseCache("key:2", "resp_b", writePolicyTestItems())

	if cached := getResponseCache("key:1", "resp_a"); len(cached) != 1 {
		t.Fatalf("chained owner write missing: got %d items", len(cached))
	}
	if cached := getResponseCache("key:2", "resp_b"); cached != nil {
		t.Fatalf("unchained owner should be skipped, got %d items", len(cached))
	}
}

func TestAlwaysWritePolicyKeepsLegacyBehavior(t *testing.T) {
	resetResponseCacheStateForTest(defaultResponseCacheConfig())

	setResponseCache("key:1", "resp_1", writePolicyTestItems())

	if cached := getResponseCache("key:1", "resp_1"); len(cached) != 1 {
		t.Fatalf("expected 1 cached item under always policy, got %d", len(cached))
	}
	if stats := GetResponseCacheStats(); stats.SkippedWrites != 0 {
		t.Fatalf("SkippedWrites = %d, want 0", stats.SkippedWrites)
	}
}

func TestChainOwnerExpiresAfterWindow(t *testing.T) {
	resetResponseCacheStateForTest(onDemandResponseCacheConfig())

	getResponseCache("key:1", "resp_unknown")

	// Manually push the eligibility window into the past, simulating one hour without chaining.
	respCache.mu.Lock()
	for _, record := range respCache.chainOwners {
		record.expiresAt = time.Now().Add(-time.Minute)
	}
	respCache.mu.Unlock()

	setResponseCache("key:1", "resp_1", writePolicyTestItems())
	if cached := getResponseCache("key:1", "resp_1"); cached != nil {
		t.Fatalf("expired chain owner should be gated, got %d items", len(cached))
	}

	// The cleanup loop should remove expired records.
	cleanupResponseCacheExpired(time.Now())
	if stats := GetResponseCacheStats(); stats.ChainOwners != 1 {
		// The miss query above via getResponseCache re-marked the owner, so this is 1.
		t.Fatalf("ChainOwners = %d, want 1 (re-marked by the gated lookup)", stats.ChainOwners)
	}
}

func TestApplyResponseCacheSettingsPropagatesWritePolicy(t *testing.T) {
	resetResponseCacheStateForTest(defaultResponseCacheConfig())

	settings := database.DefaultResponseCacheSettings()
	settings.WritePolicy = database.ResponseCacheWritePolicyOnDemand
	settings.Generation = 2
	if !ApplyResponseCacheSettings(settings) {
		t.Fatal("ApplyResponseCacheSettings returned false")
	}
	if got := GetResponseCacheAppliedConfig().WritePolicy; got != database.ResponseCacheWritePolicyOnDemand {
		t.Fatalf("applied write policy = %q, want on_demand", got)
	}

	setResponseCache("key:9", "resp_1", writePolicyTestItems())
	if cached := getResponseCache("key:9", "resp_1"); cached != nil {
		t.Fatalf("write should be gated after applying on_demand policy, got %d items", len(cached))
	}
}
