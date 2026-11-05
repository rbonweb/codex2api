package admin

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// modelVersionNumberRE extracts the numeric segments of a model name for version sorting (e.g. gpt-5.6-luna → 5,6).
var modelVersionNumberRE = regexp.MustCompile(`\d+`)

// preferredBillingModelOrder shows GPT-6 first in the pricing list, then GPT-5.6.
var preferredBillingModelOrder = []string{
	"gpt-6-astra",
	"gpt-6-sol",
	"gpt-6-luna",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
}

// modelPreferredRank returns the pinned priority (smaller comes first); models that are not pinned return -1.
func modelPreferredRank(model string) int {
	lower := strings.ToLower(strings.TrimSpace(model))
	for i, preferred := range preferredBillingModelOrder {
		if lower == preferred {
			return i
		}
		// Variant suffix / reasoning effort aliases: gpt-5.6-sol-high, gpt-5.6-sol(xhigh)
		if strings.HasPrefix(lower, preferred+"-") || strings.HasPrefix(lower, preferred+"(") {
			return i
		}
	}
	return -1
}

// modelVersionParts extracts the number sequence from a model ID, used for newest-to-oldest sorting.
func modelVersionParts(model string) []int {
	matches := modelVersionNumberRE.FindAllString(model, -1)
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

// compareModelKeysNewestFirst compares two model keys:
// 1) Pinned models (sol/terra/luna) always come first;
// 2) Other models sort by version number, newest first; within the same version, by name in lexicographic order.
// Returns -1 if a should sort before b, 1 if a sorts after b, and 0 if they are equal.
func compareModelKeysNewestFirst(a, b string) int {
	if a == b {
		return 0
	}
	ra, rb := modelPreferredRank(a), modelPreferredRank(b)
	if ra >= 0 || rb >= 0 {
		if ra < 0 {
			return 1
		}
		if rb < 0 {
			return -1
		}
		if ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		// Within the same pinned group, sort stably by name (e.g. sol-high follows sol).
		return strings.Compare(a, b)
	}

	va, vb := modelVersionParts(a), modelVersionParts(b)
	// Models without a version number sink to the bottom.
	if len(va) == 0 && len(vb) == 0 {
		return strings.Compare(a, b)
	}
	if len(va) == 0 {
		return 1
	}
	if len(vb) == 0 {
		return -1
	}
	n := len(va)
	if len(vb) < n {
		n = len(vb)
	}
	for i := 0; i < n; i++ {
		if va[i] != vb[i] {
			if va[i] > vb[i] {
				return -1
			}
			return 1
		}
	}
	// Same common prefix: more segments count as a higher version (5.6 > 5).
	if len(va) != len(vb) {
		if len(va) > len(vb) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// sortModelKeysNewestFirst sorts model keys by version number, newest first.
func sortModelKeysNewestFirst(keys []string) {
	sort.SliceStable(keys, func(i, j int) bool {
		return compareModelKeysNewestFirst(keys[i], keys[j]) < 0
	})
}

// modelPricingRow is one row per canonical model on the pricing admin page: the current effective price plus its source.
type modelPricingRow struct {
	Model          string                        `json:"model"`
	Channel        string                        `json:"channel"` // codex
	Source         string                        `json:"source"`  // custom / synced / default
	Pricing        database.ModelPricingOverride `json:"pricing"`
	CanonicalModel string                        `json:"canonical_model,omitempty"`
	IsAlias        bool                          `json:"is_alias,omitempty"`
	// A non-empty MediaUnit marks a media model (image = per image, second = per second), and its price comes from media_unit_cost
	// rather than the per-token price; a media_unit_cost of 0 means no price is set.
	MediaUnit string `json:"media_unit,omitempty"`
}

func modelPricingManagementKeys(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		key := database.PricingManagementModelKey(id)
		if key == "" || strings.Contains(key, "(") {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// ListModelPricing returns the current effective pricing and source of each canonical model, for the pricing table on the settings page.
func (h *Handler) ListModelPricing(c *gin.Context) {
	ctx := c.Request.Context()

	// Take the models currently exposed to clients and map them to canonical pricing keys, deduplicated (retired models drop out naturally).
	collect := func(ids []string) []string {
		return modelPricingManagementKeys(ids)
	}

	keys := collect(h.codexPricingModelIDs(ctx))
	// Within each channel, sort newer versions first.
	sortModelKeysNewestFirst(keys)

	rows := make([]modelPricingRow, 0, len(keys))
	for _, key := range keys {
		canonicalModel := database.PricingAliasTarget(key)
		rows = append(rows, modelPricingRow{
			Model:          key,
			Channel:        database.UpstreamChannelCodex,
			Source:         database.ModelPricingSourceFor(key),
			Pricing:        database.ModelPricingOverrideFromPricing(database.GetModelPricing(key), database.ModelPricingSourceFor(key)),
			CanonicalModel: canonicalModel,
			IsAlias:        canonicalModel != "",
			MediaUnit:      database.MediaBillingUnit(key),
		})
	}

	syncURL := ""
	if s, err := h.db.GetSystemSettings(ctx); err == nil && s != nil {
		syncURL = strings.TrimSpace(s.ModelPricingSyncURL)
	}
	officialCfg, _ := h.db.GetOfficialPricingSyncConfig(ctx)
	c.JSON(http.StatusOK, gin.H{
		"models":               rows,
		"sync_url":             syncURL,
		"default_sync_url":     proxy.DefaultModelPricingSyncURL,
		"models_dev_url":       proxy.ModelsDevPricingSyncURL,
		"litellm_url":          proxy.LiteLLMPricingSyncURL,
		"official_openai_url":  proxy.OfficialOpenAIPricingURL,
		"official_sync_config": officialPricingConfigResponse(officialCfg),
	})
}

// UpdateModelPricingRequest sets or clears a model's custom pricing override.
type UpdateModelPricingRequest struct {
	Model   string                         `json:"model"`
	Reset   bool                           `json:"reset"`   // when true, clears this model's override and falls back to the code default
	Pricing *database.ModelPricingOverride `json:"pricing"` // required when reset=false
}

// UpdateModelPricing writes or clears a model's custom pricing override.
func (h *Handler) UpdateModelPricing(c *gin.Context) {
	var req UpdateModelPricingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	key := database.PricingManagementModelKey(strings.TrimSpace(req.Model))
	if key == "" {
		writeError(c, http.StatusBadRequest, "model must not be empty")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if !req.Reset && req.Pricing != nil {
		if err := database.ValidateModelUserBilling(key, *req.Pricing); err != nil {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		normalized := database.NormalizeModelPricingOverride(key, *req.Pricing)
		req.Pricing = &normalized
	}
	if !req.Reset && (req.Pricing == nil || req.Pricing.IsEmpty()) {
		writeError(c, http.StatusBadRequest, "pricing must not be empty (or use reset to clear it)")
		return
	}
	_, err := h.db.MutateModelPricingSettings(ctx, nil, func(overrides map[string]database.ModelPricingOverride) error {
		if req.Reset {
			delete(overrides, key)
			return nil
		}
		ov := *req.Pricing
		ov.Source = database.ModelPricingSourceCustom
		overrides[key] = ov
		return nil
	})
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"model": key, "reset": req.Reset})
}

// SyncModelPricingRequest optionally carries a one-off sync URL (also saved as the default source).
type SyncModelPricingRequest struct {
	URL string `json:"url"`
}

// SyncModelPricing syncs pricing from a JSON URL (synced overrides; custom is left untouched).
func (h *Handler) SyncModelPricing(c *gin.Context) {
	var req SyncModelPricingRequest
	_ = c.ShouldBindJSON(&req)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 25*time.Second)
	defer cancel()

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	// The field is authoritative: pass the current value of the URL field on the settings page (empty means use the built-in default and clear the stored source).
	result, err := proxy.SyncModelPricingFromURL(ctx, h.db, req.URL, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}
