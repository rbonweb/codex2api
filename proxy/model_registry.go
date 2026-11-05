package proxy

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/tidwall/gjson"
)

const (
	OfficialCodexModelsURL = "https://developers.openai.com/codex/models"

	ModelCategoryCodex = "codex"
	ModelCategoryImage = "image"

	ModelSourceBuiltin           = "builtin"
	ModelSourceOfficialCodexDocs = "official_codex_docs"
	ModelSourceReasoningEffort   = "reasoning_effort"
	ModelSourceUpstreamManifest  = "upstream_manifest"
)

// ModelInfo describes one model exposed by this proxy.
type ModelInfo struct {
	ID                  string     `json:"id"`
	Enabled             bool       `json:"enabled"`
	Category            string     `json:"category"`
	Source              string     `json:"source"`
	ProOnly             bool       `json:"pro_only"`
	APIKeyAuthAvailable bool       `json:"api_key_auth_available"`
	LastSeenAt          *time.Time `json:"last_seen_at,omitempty"`
	UpdatedAt           *time.Time `json:"updated_at,omitempty"`
}

// ModelCatalog is the admin-facing model list plus registry metadata.
type ModelCatalog struct {
	Models       []string    `json:"models"`
	Items        []ModelInfo `json:"items"`
	LastSyncedAt *time.Time  `json:"last_synced_at,omitempty"`
	SourceURL    string      `json:"source_url"`
	Warning      string      `json:"warning,omitempty"`
}

// ModelSyncResult is returned after a manual upstream sync.
type ModelSyncResult struct {
	Added     int      `json:"added"`
	Updated   int      `json:"updated"`
	Unchanged int      `json:"unchanged"`
	Skipped   []string `json:"skipped"`
	// Removed is the official_codex_docs source rows deleted by this sync (the official page no longer has them).
	Removed      []string    `json:"removed,omitempty"`
	Models       []string    `json:"models"`
	Items        []ModelInfo `json:"items"`
	LastSyncedAt time.Time   `json:"last_synced_at"`
	SourceURL    string      `json:"source_url"`
}

var builtinModelInfos = []ModelInfo{
	// gpt-6-astra: listed on both the official model page and the pricing page ($10/$50, long context $20/$75).
	// Official docs sync and manifest learning can both discover it, but the built-in row guarantees that cold-start and unsynced
	// deployments can also call it directly, without waiting for a sync or for a request that carries a manifest.
	modelInfoForID("gpt-6-astra", ModelSourceBuiltin),
	modelInfoForID("gpt-6-sol", ModelSourceBuiltin),
	modelInfoForID("gpt-6-luna", ModelSourceBuiltin),
	// gpt-5.6 series (Sol/Terra/Luna): new models already listed on the official site, built in first as a fallback;
	// once the official docs page sync (SyncOfficialCodexModels) is live, its synced results take precedence.
	modelInfoForID("gpt-5.6-sol", ModelSourceBuiltin),
	modelInfoForID("gpt-5.6-terra", ModelSourceBuiltin),
	modelInfoForID("gpt-5.6-luna", ModelSourceBuiltin),
	modelInfoForID("gpt-5.5", ModelSourceBuiltin),
	// gpt-5.4 and gpt-5.4-mini have been retired (as of 2026-09, the upstream ChatGPT account manifest no longer includes them,
	// and requests fail directly with 400 "model is not supported when using Codex with a ChatGPT account");
	// 5.3 keeps only the spark variant; gpt-5.3-codex and 5.2 and lower models are retired (including upstream sync filtering).
	modelInfoForID("gpt-5.3-codex-spark", ModelSourceBuiltin),
	// codex-auto-review — Codex internal auto-review model.
	// Upstream confirms: returns effective model "gpt-5.4" (tested 2026-05-20).
	// Available on Plus/Pro/Team/Business per official catalog; excludes free.
	// Pricing: gpt-5.4 standard ($2.50/$15.00), priority ($5.00/$30.00).
	// Ref: codex_client_models.json via CLIProxyAPI model registry.
	modelInfoForID("codex-auto-review", ModelSourceBuiltin),
	// gpt-reserve — non-versioned model ID; keep as builtin fallback only.
	// Note: the official docs sync only extracts gpt-<major>[.<minor>] IDs, so it
	// would never be discovered there; manifest learning does admit non-versioned
	// gpt-* slugs (issue #624), but a builtin row is still needed for cold start.
	modelInfoForID("gpt-reserve", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2.5-flare", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2.5-flare-2k", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2.5-flare-4k", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2.5-sunburst", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2.5-sunburst-2k", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2.5-sunburst-4k", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2-2k", ModelSourceBuiltin),
	modelInfoForID("gpt-image-2-4k", ModelSourceBuiltin),
}

// SupportedModels is the static built-in fallback list. Runtime handlers use
// SupportedModelIDs so synced registry entries can take effect immediately.
var SupportedModels = BuiltinModelIDs()

func BuiltinModelIDs() []string {
	ids := make([]string, 0, len(builtinModelInfos))
	for _, model := range builtinModelInfos {
		ids = append(ids, model.ID)
	}
	return ids
}

func modelInfoForID(id string, source string) ModelInfo {
	id = strings.TrimSpace(id)
	if source == "" {
		source = ModelSourceBuiltin
	}
	info := ModelInfo{
		ID:                  id,
		Enabled:             true,
		Category:            ModelCategoryCodex,
		Source:              source,
		APIKeyAuthAvailable: true,
	}
	switch strings.ToLower(id) {
	case "gpt-5.3-codex-spark":
		info.ProOnly = true
	case "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-6.1-sol":
		info.APIKeyAuthAvailable = false
	case "gpt-image-2":
		info.Category = ModelCategoryImage
	}
	if strings.Contains(strings.ToLower(id), "image") {
		info.Category = ModelCategoryImage
	}
	return info
}

func modelInfoFromRow(row database.ModelRegistryRow) ModelInfo {
	var lastSeenAt *time.Time
	if row.LastSeenAt.Valid {
		t := row.LastSeenAt.Time.UTC()
		lastSeenAt = &t
	}
	var updatedAt *time.Time
	if !row.UpdatedAt.IsZero() {
		t := row.UpdatedAt.UTC()
		updatedAt = &t
	}
	return ModelInfo{
		ID:                  row.ID,
		Enabled:             row.Enabled,
		Category:            valueOrDefault(row.Category, ModelCategoryCodex),
		Source:              valueOrDefault(row.Source, "manual"),
		ProOnly:             row.ProOnly,
		APIKeyAuthAvailable: row.APIKeyAuthAvailable,
		LastSeenAt:          lastSeenAt,
		UpdatedAt:           updatedAt,
	}
}

func modelInfoToRow(info ModelInfo, lastSeenAt time.Time) database.ModelRegistryRow {
	return database.ModelRegistryRow{
		ID:                  info.ID,
		Enabled:             info.Enabled,
		Category:            valueOrDefault(info.Category, ModelCategoryCodex),
		Source:              valueOrDefault(info.Source, "manual"),
		ProOnly:             info.ProOnly,
		APIKeyAuthAvailable: info.APIKeyAuthAvailable,
		LastSeenAt:          sql.NullTime{Time: lastSeenAt.UTC(), Valid: !lastSeenAt.IsZero()},
	}
}

func valueOrDefault(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

// ListModelCatalog returns enabled model IDs plus metadata. It falls back to
// built-ins if the registry cannot be read.
func ListModelCatalog(ctx context.Context, db *database.DB) (ModelCatalog, error) {
	catalog := builtinCatalog()
	if db == nil {
		return catalog, nil
	}

	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		catalog.Warning = err.Error()
		return catalog, err
	}

	merged := mergeModelInfos(rows)
	if settings, settingsErr := db.GetSystemSettings(ctx); settingsErr == nil && settings != nil {
		merged = appendReasoningEffortModelInfos(merged, settings.ReasoningEffortModels)
	} else if settingsErr != nil && catalog.Warning == "" {
		catalog.Warning = settingsErr.Error()
	}
	catalog.Items = merged
	catalog.Models = enabledModelIDs(merged, false)
	if len(catalog.Models) == 0 {
		catalog.Models = BuiltinModelIDs()
	}

	state, err := db.GetModelRegistrySyncState(ctx)
	if err != nil {
		catalog.Warning = err.Error()
		return catalog, err
	}
	if state != nil {
		catalog.SourceURL = valueOrDefault(state.SourceURL, OfficialCodexModelsURL)
		if state.LastSyncedAt.Valid {
			t := state.LastSyncedAt.Time.UTC()
			catalog.LastSyncedAt = &t
		}
	}
	return catalog, nil
}

func builtinCatalog() ModelCatalog {
	items := append([]ModelInfo(nil), builtinModelInfos...)
	return ModelCatalog{
		Models:    enabledModelIDs(items, false),
		Items:     items,
		SourceURL: OfficialCodexModelsURL,
	}
}

func mergeModelInfos(rows []database.ModelRegistryRow) []ModelInfo {
	byID := make(map[string]ModelInfo, len(builtinModelInfos)+len(rows))
	for _, info := range builtinModelInfos {
		byID[info.ID] = info
	}
	for _, row := range rows {
		info := modelInfoFromRow(row)
		if info.ID == "" {
			continue
		}
		// Retired models (5.3 non-spark, 5.2 and below, gpt-4*) and internal variants (-wm) are not exposed
		// even if residual rows remain in the registry, so old DB rows after an upgrade cannot bring them back.
		if isRetiredCodexModel(info.ID) || isInternalCodexModelVariant(info.ID) {
			continue
		}
		byID[info.ID] = info
	}

	builtins := make([]ModelInfo, 0, len(builtinModelInfos))
	for _, info := range builtinModelInfos {
		if merged, ok := byID[info.ID]; ok {
			builtins = append(builtins, merged)
			delete(byID, info.ID)
		}
	}
	// Synced and learned non-built-in models are placed first, ordered newest to oldest by version (gpt-6-x > gpt-5.6-x > ...),
	// same-version ties are ordered by ID lexicographically; codename families with no numeric version (daybreak etc.) come after numbered versions.
	// The built-in list is already a hand-maintained newest-to-oldest order and is appended after them; otherwise newly released models would be
	// pushed behind gpt-image-2-4k and buried alphabetically at the end of the list.
	extras := make([]ModelInfo, 0, len(byID))
	for _, info := range byID {
		extras = append(extras, info)
	}
	sort.Slice(extras, func(i, j int) bool {
		return compareModelIDsNewestFirst(extras[i].ID, extras[j].ID) < 0
	})
	return append(extras, builtins...)
}

// compareModelIDsNewestFirst compares two model IDs by "newest version to oldest": it first compares the numeric sequence in each ID
// (segment-by-segment numeric comparison, where the longer one is treated as the finer version). IDs with numbers sort before those without,
// and fully identical IDs are ordered by lexicographic ID. It returns <0 when a should sort before b.
func compareModelIDsNewestFirst(a, b string) int {
	if a == b {
		return 0
	}
	pa, pb := modelIDVersionParts(a), modelIDVersionParts(b)
	switch {
	case len(pa) == 0 && len(pb) > 0:
		return 1
	case len(pa) > 0 && len(pb) == 0:
		return -1
	}
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return -1
			}
			return 1
		}
	}
	if len(pa) != len(pb) {
		if len(pa) > len(pb) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

var modelIDVersionNumberPattern = regexp.MustCompile(`\d+`)

func modelIDVersionParts(id string) []int {
	matches := modelIDVersionNumberPattern.FindAllString(strings.ToLower(id), -1)
	if len(matches) == 0 {
		return nil
	}
	parts := make([]int, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m)
		if err != nil {
			continue
		}
		parts = append(parts, n)
	}
	return parts
}

// internalCodexModelVariantSuffixes are internal variant suffixes that appear in the upstream manifest but must not be exposed externally.
// slugs such as gpt-5.6-sol-wm appear only in some accounts' manifests and are absent from both the official model page and the pricing page,
// so learning them into the registry would only add an entry users do not recognize, and billing could only fall back to sol for it.
var internalCodexModelVariantSuffixes = []string{"-wm"}

// isInternalCodexModelVariant reports whether a slug is an internal variant. It is rejected on admission, and residual rows already in the registry
// are also no longer exposed (the same filter as retired models).
func isInternalCodexModelVariant(id string) bool {
	id = strings.TrimSpace(strings.ToLower(id))
	if !strings.HasPrefix(id, "gpt-") {
		return false
	}
	for _, suffix := range internalCodexModelVariantSuffixes {
		if strings.HasSuffix(id, suffix) {
			return true
		}
	}
	return false
}

// isRetiredCodexModel reports whether a model has been retired (no longer exposed or used for validation):
// the whole gpt-5.4 series, gpt-5.3 non-spark, gpt-5.2 and lower, and gpt-4* are retired; image,
// codex-auto-review, non-gpt- prefixes, and 5.5+ are kept. It is the exposure-side complement of isAllowedUpstreamCodexModel,
// but it returns false (kept) for image and non-gpt models.
func isRetiredCodexModel(id string) bool {
	id = strings.TrimSpace(strings.ToLower(id))
	if !strings.HasPrefix(id, "gpt-") || strings.Contains(id, "image") {
		return false
	}
	version := strings.TrimPrefix(id, "gpt-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	if major > 5 {
		return false
	}
	if major < 5 {
		return true
	}
	if minor >= 5 {
		return false
	}
	if minor == 3 {
		return !strings.Contains(id, "spark")
	}
	return true
}

func appendReasoningEffortModelInfos(items []ModelInfo, settingsJSON string) []ModelInfo {
	entries, _ := parseReasoningEffortModelEntries(settingsJSON, enabledModelIDs(items, false), false)
	if len(entries) == 0 {
		return items
	}

	result := append([]ModelInfo(nil), items...)
	byID := make(map[string]ModelInfo, len(result)+len(entries)*2)
	for _, item := range result {
		byID[strings.ToLower(strings.TrimSpace(item.ID))] = item
	}

	for _, entry := range entries {
		baseKey := strings.ToLower(entry.Model)
		baseInfo, baseExists := byID[baseKey]
		if !baseExists {
			baseInfo = modelInfoForID(entry.Model, ModelSourceReasoningEffort)
			result = append(result, baseInfo)
			byID[baseKey] = baseInfo
		}

		alias := ReasoningEffortModelAlias(entry.Model, entry.Effort)
		if alias == "" {
			continue
		}
		aliasKey := strings.ToLower(alias)
		if _, exists := byID[aliasKey]; exists {
			continue
		}
		aliasInfo := baseInfo
		aliasInfo.ID = alias
		aliasInfo.Source = ModelSourceReasoningEffort
		aliasInfo.Category = ModelCategoryCodex
		aliasInfo.LastSeenAt = nil
		aliasInfo.UpdatedAt = nil
		result = append(result, aliasInfo)
		byID[aliasKey] = aliasInfo
	}
	return result
}

// SupportedModelIDs returns enabled runtime model IDs.
func SupportedModelIDs(ctx context.Context, db *database.DB) []string {
	catalog, _ := ListModelCatalog(ctx, db)
	return catalog.Models
}

// TextTestModelIDs returns enabled non-image models for account connection tests.
func TextTestModelIDs(ctx context.Context, db *database.DB) []string {
	catalog, _ := ListModelCatalog(ctx, db)
	ids := enabledModelIDs(catalog.Items, true)
	filtered := ids[:0]
	for _, id := range ids {
		if strings.Contains(id, "(") || strings.Contains(id, ")") {
			continue
		}
		filtered = append(filtered, id)
	}
	return filtered
}

func IsTextTestModelID(ctx context.Context, db *database.DB, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, id := range TextTestModelIDs(ctx, db) {
		if model == id {
			return true
		}
	}
	return false
}

func enabledModelIDs(items []ModelInfo, textOnly bool) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		if textOnly && isImageModelInfo(item) {
			continue
		}
		ids = append(ids, item.ID)
	}
	return ids
}

func isImageModelInfo(info ModelInfo) bool {
	return strings.EqualFold(info.Category, ModelCategoryImage) || strings.Contains(strings.ToLower(info.ID), "image")
}

var codexModelIDPattern = regexp.MustCompile(`\bgpt-[0-9]+(?:\.[0-9]+)*(?:-[a-z][a-z0-9]*(?:-[a-z0-9]+)*)?\b`)

// ParseOfficialCodexModelIDs extracts allowed Codex model IDs from the official docs HTML.
func ParseOfficialCodexModelIDs(html string) (models []string, skipped []string) {
	seen := map[string]struct{}{}
	skippedSeen := map[string]struct{}{}
	lowered := strings.ToLower(html)
	for _, loc := range codexModelIDPattern.FindAllStringIndex(lowered, -1) {
		match := lowered[loc[0]:loc[1]]
		if !isOfficialCodexModelIDContext(lowered, loc[0], loc[1]) {
			continue
		}
		if isAllowedUpstreamCodexModel(match) {
			if _, ok := seen[match]; !ok {
				seen[match] = struct{}{}
				models = append(models, match)
			}
			continue
		}
		if _, ok := skippedSeen[match]; !ok {
			skippedSeen[match] = struct{}{}
			skipped = append(skipped, match)
		}
	}
	sort.SliceStable(models, func(i, j int) bool {
		return modelSortRank(models[i]) < modelSortRank(models[j])
	})
	sort.Strings(skipped)
	return models, skipped
}

// officialCodexModelCatalogContexts are the structured attributes where the model cards on the official model page carry the model IDs
// (lowercase; the quotes in astro-island props are the HTML entity &quot;, while server-rendered DOM attributes use bare quotes):
//   - data-model-slug="<id>"          card container attribute
//   - "slug":[0,"<id>"] / "name":[0,"<id>"]   card component props
var officialCodexModelCatalogContexts = []string{
	`data-model-slug="`,
	`"slug":[0,"`,
	`&quot;slug&quot;:[0,&quot;`,
	`"name":[0,"`,
	`&quot;name&quot;:[0,&quot;`,
}

// isOfficialCodexModelIDContext accepts an ID only where the official model page's **model catalog cards** carry it
// (see officialCodexModelCatalogContexts). Other spots on the page that merely look like a model ID never count:
// navigation copy ("Using GPT-6 Astra" -> gpt-6), anchors (#gpt-6-astra-in-enterprise),
// image filenames (gpt-6-astra-texture.webp), and **placeholder model names in usage example code**:
// `codex --model gpt-5.6` / `model = "gpt-5.6"` and similar examples use a family name, not a callable
// model ID. The earlier lenient rule that anything in quotes counts as an ID would learn a bare gpt-5.6 into the registry.
// If a page redesign leaves nothing parseable, the sync reports an error and keeps the local list instead of silently learning noise.
func isOfficialCodexModelIDContext(lowered string, start, end int) bool {
	before := lowered[:start]
	after := lowered[end:]
	if after == "" || isModelIDByte(after[0]) {
		return false
	}
	for _, prefix := range officialCodexModelCatalogContexts {
		if strings.HasSuffix(before, prefix) {
			return true
		}
	}
	return false
}

func isModelIDByte(b byte) bool {
	return b == '-' || b == '.' || b == '_' || b == '/' || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func modelSortRank(id string) int {
	for index, info := range builtinModelInfos {
		if info.ID == id {
			return index
		}
	}
	return len(builtinModelInfos) + 1000
}

// isAllowedUpstreamCodexModel reports whether a model discovered upstream may enter the local registry
// (shared by official docs sync and manifest learning). Policy:
//   - gpt-5.5 and later: allowed
//   - gpt-5.3: only spark variants (gpt-5.3-codex-spark) are allowed; other 5.3 models are retired
//   - the whole gpt-5.4 series, gpt-5.2 and lower, image, and non-gpt- prefixes: rejected
//   - gpt- codename families whose segment after gpt- is not a numeric version (stable aliases such as gpt-daybreak-blue-latest,
//     issue #624): allowed. The version-retirement rules cannot judge them, and their presence in the manifest reflects the account's real entitlement;
//     rejecting them would leave a model that probes can see but that returns 404 on calls permanently out of the registry
func isAllowedUpstreamCodexModel(id string) bool {
	id = strings.TrimSpace(strings.ToLower(id))
	if id == "" || strings.Contains(id, "image") {
		return false
	}
	if isInternalCodexModelVariant(id) {
		return false
	}
	if !strings.HasPrefix(id, "gpt-") {
		return false
	}
	version := strings.TrimPrefix(id, "gpt-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	// The version may be major-only (gpt-6-astra, gpt-6). Without a decimal point, minor is treated as 0,
	// so a new-generation model must not be rejected merely for lacking ".x".
	parts := strings.Split(version, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		// A first segment that starts with a letter is a codename family alias (daybreak etc.) with no version to compare; allow it per the upstream manifest,
		// consistent with isRetiredCodexModel, which always returns false (kept) for non-numeric IDs.
		// IDs that start with a digit but cannot be parsed (old-generation spellings such as gpt-4o) are still rejected as retired;
		// IDs that start with punctuation (gpt-.foo / gpt-_foo) match no known naming and are likewise rejected.
		return version != "" && version[0] >= 'a' && version[0] <= 'z'
	}
	minor := 0
	if len(parts) >= 2 {
		minor, err = strconv.Atoi(parts[1])
		if err != nil {
			return false
		}
	}
	if major > 5 {
		return true
	}
	if major < 5 {
		return false
	}
	// major == 5
	if minor >= 5 {
		return true
	}
	if minor == 3 {
		// 5.3 keeps only spark
		return strings.Contains(id, "spark")
	}
	return false

}

// SyncOfficialCodexModels fetches the fixed official docs page and merges discovered models.
// proxyURL is the global outbound proxy (may be an empty string for a direct connection). The official model page, like other upstream links, may
// be blocked by the network environment, so the sync request must follow the global ProxyURL (issue #371).
func SyncOfficialCodexModels(ctx context.Context, db *database.DB, proxyURL string) (*ModelSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("database unavailable, cannot sync model registry")
	}
	client := &http.Client{Transport: newCodexStandardTransport(proxyURL), Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OfficialCodexModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("official model page is temporarily unreachable, keeping the local model list: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("official model page returned %d, keeping the local model list", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	return ApplyOfficialCodexModelSync(ctx, db, string(body), time.Now().UTC())
}

// ApplyOfficialCodexModelSync merges a fetched official docs page into the registry.
func ApplyOfficialCodexModelSync(ctx context.Context, db *database.DB, html string, syncedAt time.Time) (*ModelSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("database unavailable, cannot sync model registry")
	}
	ids, skipped := ParseOfficialCodexModelIDs(html)
	if len(ids) == 0 {
		return nil, fmt.Errorf("no usable models parsed from the official model page, keeping the local model list")
	}

	existingRows, err := db.ListModelRegistry(ctx)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]database.ModelRegistryRow, len(existingRows))
	for _, row := range existingRows {
		existing[row.ID] = row
	}

	rows := make([]database.ModelRegistryRow, 0, len(ids))
	result := &ModelSyncResult{
		Skipped:   skipped,
		SourceURL: OfficialCodexModelsURL,
	}
	parsed := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		parsed[id] = struct{}{}
		info := modelInfoForID(id, ModelSourceOfficialCodexDocs)
		row := modelInfoToRow(info, syncedAt)
		if previous, ok := existing[id]; ok {
			row.Enabled = previous.Enabled
			if modelRegistryMetadataEqual(previous, row) {
				result.Unchanged++
			} else {
				result.Updated++
			}
		} else {
			result.Added++
		}
		rows = append(rows, row)
	}

	// The official page is the single source of truth for official_codex_docs source rows: rows that no longer appear on the page
	// (early parse noise such as a bare gpt-5.6, or models the official site has retired)
	// are deleted along with this sync. Otherwise the registry has no delete path and noise would stay in /v1/models forever. Only the
	// non-built-in rows of this source are touched; manifest learning, manual sources, and built-in models are unaffected.
	builtin := make(map[string]struct{}, len(builtinModelInfos))
	for _, info := range builtinModelInfos {
		builtin[strings.ToLower(info.ID)] = struct{}{}
	}
	stale := make([]string, 0)
	for _, row := range existingRows {
		if row.Source != ModelSourceOfficialCodexDocs {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(row.ID))
		if _, ok := parsed[key]; ok {
			continue
		}
		if _, ok := builtin[key]; ok {
			continue
		}
		stale = append(stale, row.ID)
	}
	sort.Strings(stale)

	if err := db.UpsertModelRegistryRows(ctx, rows); err != nil {
		return nil, err
	}
	if len(stale) > 0 {
		if err := db.DeleteModelRegistryRows(ctx, stale); err != nil {
			return nil, err
		}
		result.Removed = stale
	}
	if err := db.UpdateModelRegistrySyncState(ctx, OfficialCodexModelsURL, syncedAt); err != nil {
		return nil, err
	}

	catalog, err := ListModelCatalog(ctx, db)
	if err != nil {
		return nil, err
	}
	result.Models = catalog.Models
	result.Items = catalog.Items
	result.LastSyncedAt = syncedAt.UTC()
	return result, nil
}

func modelRegistryMetadataEqual(a database.ModelRegistryRow, b database.ModelRegistryRow) bool {
	return a.Enabled == b.Enabled &&
		valueOrDefault(a.Category, ModelCategoryCodex) == valueOrDefault(b.Category, ModelCategoryCodex) &&
		valueOrDefault(a.Source, "manual") == valueOrDefault(b.Source, "manual") &&
		a.ProOnly == b.ProOnly &&
		a.APIKeyAuthAvailable == b.APIKeyAuthAvailable
}

// ExtractManifestModelSlugs extracts models[].slug from the upstream model manifest.
// It depends only on the slug identity field, so other schema changes in the manifest do not affect extraction;
// invalid or overlong names are dropped directly. When no slug can be parsed, it returns an empty slice (callers treat it as a no-op).
func ExtractManifestModelSlugs(manifest []byte) []string {
	if len(manifest) == 0 {
		return nil
	}
	items := gjson.GetBytes(manifest, "models")
	if !items.IsArray() {
		return nil
	}
	seen := make(map[string]struct{})
	slugs := make([]string, 0, 8)
	items.ForEach(func(_, item gjson.Result) bool {
		slug := strings.TrimSpace(item.Get("slug").String())
		if slug == "" {
			return true
		}
		if err := security.ValidateModelName(slug); err != nil {
			return true
		}
		key := strings.ToLower(slug)
		if _, dup := seen[key]; dup {
			return true
		}
		seen[key] = struct{}{}
		slugs = append(slugs, slug)
		return true
	})
	return slugs
}

// LearnModelsFromManifest learns models the registry does not yet recognize from the upstream manifest into the registry.
//
// Strictly "add only, never modify or delete":
//   - it inserts only new slugs that appear in neither the built-in list nor any registry row (including disabled rows); an existing row
//     is not touched in any field, so a model an administrator disabled is never reversed;
//   - models absent from the manifest are not deleted: the manifest reflects the real permissions of the account used this time, and accounts on different plans
//     see different manifests, so absence does not mean global retirement; the registry converges to the union of pool-account permissions.
//
// It returns the model IDs newly inserted this time (empty when nothing was added).
func LearnModelsFromManifest(ctx context.Context, db *database.DB, manifest []byte, seenAt time.Time) ([]string, error) {
	if db == nil {
		return nil, nil
	}
	slugs := ExtractManifestModelSlugs(manifest)
	if len(slugs) == 0 {
		return nil, nil
	}

	known := make(map[string]struct{}, len(builtinModelInfos))
	for _, info := range builtinModelInfos {
		known[strings.ToLower(info.ID)] = struct{}{}
	}
	rows, err := db.ListModelRegistry(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		known[strings.ToLower(strings.TrimSpace(row.ID))] = struct{}{}
	}

	newRows := make([]database.ModelRegistryRow, 0, len(slugs))
	added := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		if _, exists := known[strings.ToLower(slug)]; exists {
			continue
		}
		// Upstream sync does not introduce models below 5.3 (5.3 is spark only); same policy as official docs sync.
		if !isAllowedUpstreamCodexModel(slug) {
			continue
		}
		info := modelInfoForID(slug, ModelSourceUpstreamManifest)
		newRows = append(newRows, modelInfoToRow(info, seenAt))
		added = append(added, slug)
	}
	if len(newRows) == 0 {
		return nil, nil
	}
	if err := db.UpsertModelRegistryRows(ctx, newRows); err != nil {
		return nil, err
	}
	return added, nil
}
