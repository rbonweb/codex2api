package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
)

func newTestModelRegistryDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestParseOfficialCodexModelIDs(t *testing.T) {
	html := `
		<astro-island props="{&quot;name&quot;:[0,&quot;gpt-5.5&quot;]}"></astro-island>
		<div data-model-slug="gpt-5.4"></div>
		<astro-island props="{&quot;slug&quot;:[0,&quot;gpt-5.3-codex-spark&quot;]}"></astro-island>
		<div data-model-slug="gpt-5.2"></div>
		<div data-model-slug="gpt-5.2-codex"></div>
		<div data-model-slug="gpt-4.1"></div>
	`
	models, skipped := ParseOfficialCodexModelIDs(html)
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark"} {
		if !slices.Contains(models, model) {
			t.Fatalf("parsed models missing %q in %v", model, models)
		}
	}
	// the whole gpt-5.4 series is retired; 5.3 keeps only spark; gpt-5.2 and below, gpt-5.2-codex, and gpt-4.1 are all filtered out.
	for _, model := range []string{"gpt-5.4", "gpt-5.2", "gpt-5.2-codex", "gpt-4.1"} {
		if !slices.Contains(skipped, model) {
			t.Fatalf("skipped models missing %q in %v", model, skipped)
		}
	}
	if slices.Contains(models, "gpt-5.2") {
		t.Fatalf("gpt-5.2 should be filtered out, got %v", models)
	}
}

func TestApplyOfficialCodexModelSyncMergesWithBuiltinImageModel(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	html := ``
	for _, id := range []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-5.2-codex", "gpt-4.1"} {
		html += `<div data-model-slug="` + id + `"></div>`
	}

	result, err := ApplyOfficialCodexModelSync(ctx, db, html, time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ApplyOfficialCodexModelSync error: %v", err)
	}
	for _, model := range []string{"gpt-image-2", "gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(result.Models, model) {
			t.Fatalf("sync should keep builtin image model %q, got %v", model, result.Models)
		}
	}
	if !slices.Contains(result.Skipped, "gpt-5.2-codex") {
		t.Fatalf("sync should skip gpt-5.2-codex, got %v", result.Skipped)
	}

	var spark *ModelInfo
	for i := range result.Items {
		if result.Items[i].ID == "gpt-5.3-codex-spark" {
			spark = &result.Items[i]
			break
		}
	}
	if spark == nil || !spark.ProOnly {
		t.Fatalf("spark model should be marked pro_only, got %#v", spark)
	}
}

func TestDynamicModelRegistryAffectsValidationImmediately(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	err := db.UpsertModelRegistryRows(ctx, []database.ModelRegistryRow{
		{
			ID:                  "gpt-6.0",
			Enabled:             true,
			Category:            ModelCategoryCodex,
			Source:              ModelSourceOfficialCodexDocs,
			APIKeyAuthAvailable: true,
		},
	})
	if err != nil {
		t.Fatalf("UpsertModelRegistryRows error: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	models := handler.supportedModelIDs(ctx)
	if !slices.Contains(models, "gpt-6.0") {
		t.Fatalf("runtime supported models missing synced model: %v", models)
	}

	result := api.ValidateResponsesAPIRequest([]byte(`{"model":"gpt-6.0","input":"hello"}`), models)
	if !result.Valid {
		t.Fatalf("synced model should pass validation: %#v", result.Errors)
	}
}

func TestReasoningEffortModelsAreIncludedInCatalog(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings error: %v", err)
	}
	if settings == nil {
		settings = &database.SystemSettings{
			SiteName:                         "CodexProxy",
			MaxConcurrency:                   2,
			TestModel:                        "gpt-5.4",
			TestConcurrency:                  50,
			BackgroundRefreshIntervalMinutes: 2,
			UsageProbeMaxAgeMinutes:          10,
			UsageProbeConcurrency:            16,
			RecoveryProbeIntervalMinutes:     30,
			PgMaxConns:                       50,
			RedisPoolSize:                    30,
			MaxRetries:                       2,
			MaxRateLimitRetries:              1,
			ModelMapping:                     "{}",
			CodexModelMapping:                "{}",
			PromptFilterMode:                 "monitor",
			PromptFilterThreshold:            50,
			PromptFilterStrictThreshold:      90,
			PromptFilterLogMatches:           true,
			PromptFilterMaxTextLength:        81920,
			PromptFilterCustomPatterns:       "[]",
			PromptFilterDisabledPatterns:     "[]",
			ClientCompatMode:                 "preserve",
			CodexMinCLIVersion:               "0.118.0",
			UsageLogMode:                     "full",
			UsageLogBatchSize:                200,
			UsageLogFlushIntervalSeconds:     5,
			StreamFlushPolicy:                "immediate",
			StreamFlushIntervalMS:            20,
			BillingTierPolicy:                "actual",
			ImageStorageConfig:               "{}",
			SchedulerMode:                    "round_robin",
			AffinityMode:                     "bounded",
			BackgroundConfig:                 "{}",
		}
	}
	settings.ReasoningEffortModels = `[{"model":"gpt-5.5","effort":"xhigh"}]`
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings error: %v", err)
	}

	catalog, err := ListModelCatalog(ctx, db)
	if err != nil {
		t.Fatalf("ListModelCatalog error: %v", err)
	}
	if !slices.Contains(catalog.Models, "gpt-5.5(xhigh)") {
		t.Fatalf("catalog models missing reasoning alias: %v", catalog.Models)
	}

	var aliasInfo *ModelInfo
	for i := range catalog.Items {
		if catalog.Items[i].ID == "gpt-5.5(xhigh)" {
			aliasInfo = &catalog.Items[i]
			break
		}
	}
	if aliasInfo == nil {
		t.Fatalf("catalog items missing reasoning alias: %#v", catalog.Items)
	}
	if aliasInfo.Source != ModelSourceReasoningEffort {
		t.Fatalf("alias source = %q, want %q", aliasInfo.Source, ModelSourceReasoningEffort)
	}
	if aliasInfo.Category != ModelCategoryCodex {
		t.Fatalf("alias category = %q, want %q", aliasInfo.Category, ModelCategoryCodex)
	}
	if slices.Contains(TextTestModelIDs(ctx, db), "gpt-5.5(xhigh)") {
		t.Fatalf("reasoning alias should not be used for direct connection tests")
	}
}

func TestExtractManifestModelSlugs(t *testing.T) {
	manifest := []byte(`{"models":[
		{"slug":"gpt-5.5","prefer_websockets":true},
		{"slug":"gpt-5.5"},
		{"slug":"  "},
		{"slug":"bad slug with spaces"},
		{"slug":"gpt-9-new"}
	]}`)
	got := ExtractManifestModelSlugs(manifest)
	want := []string{"gpt-5.5", "gpt-9-new"}
	if !slices.Equal(got, want) {
		t.Fatalf("ExtractManifestModelSlugs = %v, want %v", got, want)
	}

	if got := ExtractManifestModelSlugs([]byte(`{"data":[{"id":"x"}]}`)); got != nil {
		t.Fatalf("non-manifest shape should yield nil, got %v", got)
	}
	if got := ExtractManifestModelSlugs(nil); got != nil {
		t.Fatalf("empty body should yield nil, got %v", got)
	}
}

func TestLearnModelsFromManifest_AddsOnlyUnknownAndNeverTouchesExisting(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()

	// Pre-seed an administrator-disabled row: learning must never reverse it.
	disabled := database.ModelRegistryRow{
		ID: "gpt-old", Enabled: false, Category: ModelCategoryCodex, Source: "manual",
		APIKeyAuthAvailable: true,
	}
	if err := db.UpsertModelRegistryRows(ctx, []database.ModelRegistryRow{disabled}); err != nil {
		t.Fatalf("seed disabled row: %v", err)
	}

	manifest := []byte(`{"models":[
		{"slug":"gpt-5.5"},
		{"slug":"gpt-old"},
		{"slug":"gpt-9.9-new"}
	]}`)
	added, err := LearnModelsFromManifest(ctx, db, manifest, time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("LearnModelsFromManifest error: %v", err)
	}
	if !slices.Equal(added, []string{"gpt-9.9-new"}) {
		t.Fatalf("added = %v, want [gpt-9.9-new] (builtin/existing must be skipped)", added)
	}

	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		t.Fatalf("ListModelRegistry: %v", err)
	}
	var sawNew, sawOld bool
	for _, row := range rows {
		switch row.ID {
		case "gpt-9.9-new":
			sawNew = true
			if !row.Enabled || row.Source != ModelSourceUpstreamManifest {
				t.Fatalf("learned row = %+v, want enabled with source %s", row, ModelSourceUpstreamManifest)
			}
		case "gpt-old":
			sawOld = true
			if row.Enabled {
				t.Fatal("disabled row must never be re-enabled by manifest learning")
			}
		}
	}
	if !sawNew || !sawOld {
		t.Fatalf("registry rows missing expected entries: new=%v old=%v", sawNew, sawOld)
	}

	// Learned models enter the request-side supported list immediately.
	if !slices.Contains(SupportedModelIDs(ctx, db), "gpt-9.9-new") {
		t.Fatal("learned model should appear in SupportedModelIDs immediately")
	}
	if slices.Contains(SupportedModelIDs(ctx, db), "gpt-old") {
		t.Fatal("disabled model must stay out of SupportedModelIDs")
	}
}

func TestLearnModelsFromManifest_AllKnownIsNoOp(t *testing.T) {
	db := newTestModelRegistryDB(t)
	added, err := LearnModelsFromManifest(context.Background(), db,
		[]byte(`{"models":[{"slug":"gpt-5.5"},{"slug":"gpt-5.4"}]}`), time.Now().UTC())
	if err != nil {
		t.Fatalf("LearnModelsFromManifest error: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("added = %v, want empty for builtin-only manifest", added)
	}
	rows, err := db.ListModelRegistry(context.Background())
	if err != nil {
		t.Fatalf("ListModelRegistry: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("no rows should be written for all-known manifest, got %d", len(rows))
	}
}

// Admission policy for upstream-synced and learned models: 5.5+ allowed, 5.3 spark only, the whole 5.4 series and 5.2 and below retired.
func TestIsAllowedUpstreamCodexModel_Policy(t *testing.T) {
	cases := map[string]bool{
		"gpt-6-astra":         true,
		"gpt-5.6-sol":         true,
		"gpt-5.5":             true,
		"gpt-5.4":             false,
		"gpt-5.4-mini":        false,
		"gpt-6.0":             true,
		"gpt-5.3-codex-spark": true,
		"gpt-5.3-codex":       false,
		"gpt-5.3":             false,
		"gpt-5.2":             false,
		"gpt-5.2-codex":       false,
		"gpt-5.1-codex":       false,
		"gpt-4.1":             false,
		"gpt-4o":              false,
		"gpt-image-2":         false,
		"":                    false,
		// Trusted Access for Cyber (issue #624): stable aliases have no numeric version,
		// but their presence in the manifest reflects real account entitlement, so they must be allowed; cyber variants with a version number follow the regular rules.
		"gpt-daybreak-blue-latest": true,
		"gpt-daybreak-red-latest":  true,
		"gpt-5.6-cyber":            true,
		"gpt-5.5-cyber-preview":    true,
		"gpt-":                     false,
		"gpt-4o-mini":              false,
		"gpt-5o":                   false,
		"gpt-.foo":                 false,
		"gpt-_foo":                 false,
		"gpt-+foo":                 false,
		"gpt-daybreak-image":       false,
		"daybreak-blue":            false,
		// Internal variant suffixes (gpt-5.6-sol-wm in some accounts' manifests) are not learned into the registry.
		"gpt-5.6-sol-wm": false,
		"gpt-6-astra-wm": false,
	}
	for id, want := range cases {
		if got := isAllowedUpstreamCodexModel(id); got != want {
			t.Errorf("isAllowedUpstreamCodexModel(%q) = %v, want %v", id, got, want)
		}
	}
}

// Outbound requests from /models/sync must follow the global ProxyURL (issue #371): the official model page
// (https) accessed through an HTTP proxy shows up as a CONNECT. Here a fake proxy records the CONNECT
// target to prove the request actually went through the proxy rather than connecting directly.
func TestSyncOfficialCodexModelsUsesGlobalProxy(t *testing.T) {
	db := newTestModelRegistryDB(t)

	var mu sync.Mutex
	var connectHosts []string
	fakeProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodConnect {
			connectHosts = append(connectHosts, r.URL.Host)
		}
		mu.Unlock()
		// Refuse the tunnel so the sync ends with a network error; this test only cares whether the traffic went through the proxy.
		http.Error(w, "tunnel rejected by test proxy", http.StatusBadGateway)
	}))
	defer fakeProxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := SyncOfficialCodexModels(ctx, db, fakeProxy.URL)
	if err == nil {
		t.Fatal("expected sync to fail through rejecting test proxy")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(connectHosts) == 0 {
		t.Fatal("sync request bypassed the configured global proxy (no CONNECT observed)")
	}
	wantHost := "developers.openai.com:443"
	if connectHosts[0] != wantHost {
		t.Fatalf("CONNECT host = %q, want %q", connectHosts[0], wantHost)
	}
}

// An empty proxyURL keeps direct-connection semantics: the request should reach the target site itself (an overridable URL is not feasible,
// because the official URL is a constant, so fall back to checking that an empty proxy does not fail on proxy configuration and reaches the network stage).
func TestSyncOfficialCodexModelsEmptyProxyStillAttempts(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// Extremely short timeout: it returns quickly whatever the network conditions; assert the error is network-class, not argument-class.
	_, err := SyncOfficialCodexModels(ctx, db, "")
	if err == nil {
		return // if the network is fast enough to finish the sync within 50ms, that also counts as a pass
	}
	if !strings.Contains(err.Error(), "official model page") {
		t.Fatalf("unexpected error kind: %v", err)
	}
}

func TestIsAllowedUpstreamCodexModelAcceptsMajorOnlyVersions(t *testing.T) {
	// New-generation models with no decimal point, such as gpt-6-astra, must be allowed into the registry.
	for _, id := range []string{"gpt-6-astra", "gpt-6", "gpt-7-nova"} {
		if !isAllowedUpstreamCodexModel(id) {
			t.Fatalf("%s should be allowed", id)
		}
	}
	for _, id := range []string{"gpt-4", "gpt-4-turbo", "gpt-5", "gpt-5-codex"} {
		if isAllowedUpstreamCodexModel(id) {
			t.Fatalf("%s should be rejected", id)
		}
	}
	models, _ := ParseOfficialCodexModelIDs(`<div data-model-slug="gpt-6-astra"></div> &quot;slug&quot;:[0,&quot;gpt-5.5&quot;]`)
	if !slices.Contains(models, "gpt-6-astra") || !slices.Contains(models, "gpt-5.5") {
		t.Fatalf("parsed models missing gpt-6-astra / gpt-5.5: %v", models)
	}
}

func TestParseOfficialCodexModelIDsIgnoresNonModelContexts(t *testing.T) {
	// Real noise on the official page that looks like a model ID: navigation copy, anchors, image filenames, and usage-example
	// placeholder family names in code (codex --model gpt-5.6 / model = "gpt-5.6"). The official site has no
	// model named gpt-5.6; only the card's data-model-slug / slug / name attributes count as catalog entries.
	html := `
		<a href="#gpt-6-astra-in-enterprise">Using GPT-6 Astra</a>
		<img src="/images/api/models/gpt-6-astra-texture.webp" alt="Astra">
		<img src="/images/api/models/gpt-5.6-sol.webp">
		<astro-island props="{&quot;name&quot;:[0,&quot;gpt-6-astra&quot;],&quot;wallpaperUrl&quot;:[0,&quot;/images/api/models/gpt-6-astra-texture.webp&quot;]}"></astro-island>
		<div class="not-prose" data-model-slug="gpt-5.6-sol"><code>codex -m gpt-5.6-sol</code></div>
		<astro-island props="{&quot;slug&quot;:[0,&quot;gpt-5.6-luna&quot;]}"></astro-island>
		<astro-island props="{&quot;code&quot;:[0,&quot;codex --model gpt-5.6&quot;]}"></astro-island>
		<astro-island props="{&quot;code&quot;:[0,&quot;codex exec -m gpt-5.6 \&quot;review\&quot;&quot;]}"></astro-island>
		<span class="shiki-token">"gpt-5.6"</span>
		<code>codex -m gpt-5.5</code>
		<script>{"model":"gpt-5.4"}</script>
	`
	models, skipped := ParseOfficialCodexModelIDs(html)
	want := []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-luna"}
	if len(models) != len(want) {
		t.Fatalf("models = %v, want exactly %v (skipped=%v)", models, want, skipped)
	}
	for _, model := range want {
		if !slices.Contains(models, model) {
			t.Fatalf("parsed models missing %q in %v", model, models)
		}
	}
	for _, junk := range []string{"gpt-6", "gpt-6-astra-in-enterprise", "gpt-6-astra-texture", "gpt-5.6", "gpt-5.5", "gpt-5.4"} {
		if slices.Contains(models, junk) || slices.Contains(skipped, junk) {
			t.Fatalf("%q should not be extracted at all (models=%v skipped=%v)", junk, models, skipped)
		}
	}
}

// issue #624: the manifest of a Trusted Access for Cyber account includes gpt-daybreak-blue-latest,
// so after learning it must immediately enter the request-side supported list, otherwise /v1/models does not list it and /responses rejects it.
func TestLearnModelsFromManifest_AdmitsNonVersionedCyberAlias(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	manifest := []byte(`{"models":[
		{"slug":"gpt-5.5"},
		{"slug":"gpt-daybreak-blue-latest"},
		{"slug":"gpt-5.2-codex"}
	]}`)
	added, err := LearnModelsFromManifest(ctx, db, manifest, time.Now().UTC())
	if err != nil {
		t.Fatalf("LearnModelsFromManifest error: %v", err)
	}
	if !slices.Equal(added, []string{"gpt-daybreak-blue-latest"}) {
		t.Fatalf("added = %v, want [gpt-daybreak-blue-latest] (retired 5.2 must still be rejected)", added)
	}
	catalog, err := ListModelCatalog(ctx, db)
	if err != nil {
		t.Fatalf("ListModelCatalog: %v", err)
	}
	if !slices.Contains(catalog.Models, "gpt-daybreak-blue-latest") {
		t.Fatalf("learned alias missing from catalog: %v", catalog.Models)
	}
	for _, item := range catalog.Items {
		if item.ID == "gpt-daybreak-blue-latest" {
			if item.Source != ModelSourceUpstreamManifest || item.Category != ModelCategoryCodex || !item.Enabled {
				t.Fatalf("learned item = %+v", item)
			}
		}
	}
	if !slices.Contains(SupportedModelIDs(ctx, db), "gpt-daybreak-blue-latest") {
		t.Fatal("learned alias must be accepted by the request-side model gate immediately")
	}
	if isRetiredCodexModel("gpt-daybreak-blue-latest") {
		t.Fatal("non-versioned alias must never be treated as retired")
	}
}

// The official page is the single source of truth for official_codex_docs source rows: such a source row that no longer appears on the page
// (early parse noise such as a bare gpt-5.6) is removed by the sync; manifest learning, manual sources, and built-in rows are left alone.
func TestApplyOfficialCodexModelSyncPrunesStaleOfficialRows(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	seed := []database.ModelRegistryRow{
		{ID: "gpt-5.6", Enabled: true, Category: ModelCategoryCodex, Source: ModelSourceOfficialCodexDocs, APIKeyAuthAvailable: true},
		{ID: "gpt-daybreak-blue-latest", Enabled: true, Category: ModelCategoryCodex, Source: ModelSourceUpstreamManifest, APIKeyAuthAvailable: true},
		{ID: "gpt-7-manual", Enabled: true, Category: ModelCategoryCodex, Source: "manual", APIKeyAuthAvailable: true},
		// A built-in model with an official source that is not currently listed on the page must not be deleted either.
		{ID: "gpt-5.5", Enabled: false, Category: ModelCategoryCodex, Source: ModelSourceOfficialCodexDocs, APIKeyAuthAvailable: true},
	}
	if err := db.UpsertModelRegistryRows(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	html := `<div data-model-slug="gpt-6-astra"></div><div data-model-slug="gpt-5.6-sol"></div>`
	result, err := ApplyOfficialCodexModelSync(ctx, db, html, time.Now())
	if err != nil {
		t.Fatalf("ApplyOfficialCodexModelSync: %v", err)
	}
	if !slices.Equal(result.Removed, []string{"gpt-5.6"}) {
		t.Fatalf("Removed = %v, want [gpt-5.6]", result.Removed)
	}
	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		t.Fatalf("ListModelRegistry: %v", err)
	}
	got := map[string]database.ModelRegistryRow{}
	for _, row := range rows {
		got[row.ID] = row
	}
	if _, ok := got["gpt-5.6"]; ok {
		t.Fatal("stale official row gpt-5.6 should be deleted")
	}
	for _, keep := range []string{"gpt-daybreak-blue-latest", "gpt-7-manual", "gpt-5.5", "gpt-6-astra", "gpt-5.6-sol"} {
		if _, ok := got[keep]; !ok {
			t.Fatalf("row %s should survive the sync", keep)
		}
	}
	if got["gpt-5.5"].Enabled {
		t.Fatal("admin-disabled builtin row must keep enabled=false")
	}
	if slices.Contains(result.Models, "gpt-5.6") {
		t.Fatalf("catalog still exposes gpt-5.6: %v", result.Models)
	}
}

// Synced and learned non-built-in models are placed first in the list, ordered newest to oldest by version; the built-in list follows.
func TestMergeModelInfosPutsSyncedModelsFirstNewestFirst(t *testing.T) {
	rows := []database.ModelRegistryRow{
		// Residual internal variant rows learned earlier: no longer exposed.
		{ID: "gpt-5.6-sol-wm", Enabled: true, Source: ModelSourceUpstreamManifest},
		{ID: "gpt-daybreak-blue-latest", Enabled: true, Source: ModelSourceUpstreamManifest},
		{ID: "gpt-6-nova", Enabled: true, Source: ModelSourceUpstreamManifest},
		{ID: "gpt-5.7-terra", Enabled: true, Source: ModelSourceOfficialCodexDocs},
		{ID: "gpt-5.7-sol", Enabled: true, Source: ModelSourceOfficialCodexDocs},
		{ID: "gpt-7", Enabled: true, Source: ModelSourceUpstreamManifest},
		// Built-in rows keep their built-in order and are not moved to the front.
		{ID: "gpt-5.5", Enabled: true, Source: ModelSourceOfficialCodexDocs},
	}
	merged := mergeModelInfos(rows)
	ids := make([]string, 0, len(merged))
	for _, info := range merged {
		ids = append(ids, info.ID)
	}
	wantHead := []string{"gpt-7", "gpt-6-nova", "gpt-5.7-sol", "gpt-5.7-terra", "gpt-daybreak-blue-latest"}
	if !slices.Equal(ids[:len(wantHead)], wantHead) {
		t.Fatalf("head = %v, want %v", ids[:len(wantHead)], wantHead)
	}
	if !slices.Equal(ids[len(wantHead):], BuiltinModelIDs()) {
		t.Fatalf("tail = %v, want builtin order %v", ids[len(wantHead):], BuiltinModelIDs())
	}
	if slices.Contains(ids, "gpt-5.6-sol-wm") {
		t.Fatalf("internal variant row must stay hidden: %v", ids)
	}
}
