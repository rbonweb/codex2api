package admin

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

const automaticModelDiscoveryTimeout = 30 * time.Second

// The automatic price poll discovers models first, so the price sync can include the models added in this round.
// A discovery failure does not block price updates for existing models; the specific failure is also written to the sync status.
func (h *Handler) runAutomaticModelPricingSync(ctx context.Context, cfg *database.OfficialPricingSyncConfig) (*proxy.OfficialPricingSyncResult, error) {
	warnings := h.discoverModelsForPricing(ctx, cfg)
	return h.runOfficialPricingSyncWithWarnings(ctx, proxy.OfficialPricingSyncOptions{
		IncludeOpenAI: cfg.IncludeOpenAI,
	}, warnings)
}

func (h *Handler) discoverModelsForPricing(ctx context.Context, cfg *database.OfficialPricingSyncConfig) []string {
	discoveryCtx, cancel := context.WithTimeout(ctx, automaticModelDiscoveryTimeout)
	defer cancel()
	selected := map[string]bool{
		database.UpstreamChannelCodex: cfg.IncludeOpenAI,
	}
	funcs := h.modelRefreshFuncs
	if funcs == nil {
		funcs = h.defaultModelRefreshFuncs()
	}
	results := make(chan []string, len(selected))
	pending := 0
	for channel, enabled := range selected {
		if !enabled {
			continue
		}
		pending++
		go func(channel string) {
			results <- discoverChannelModelsForPricing(discoveryCtx, channel, funcs[channel])
		}(channel)
	}
	var warnings []string
	for range pending {
		warnings = append(warnings, (<-results)...)
	}
	sort.Strings(warnings)
	return warnings
}

func discoverChannelModelsForPricing(ctx context.Context, channel string, refresh channelModelRefreshFunc) []string {
	if refresh == nil {
		return []string{fmt.Sprintf("%s model auto-refresh is unavailable", channel)}
	}
	var warnings []string
	emit := func(event modelRefreshEvent) {
		if event.Error != "" {
			// Plan probes call back in parallel; report via logs, and use the final results in the summary to avoid a race on the shared slice.
			log.Printf("model auto-discovery failed: channel=%s plan=%s error=%s", channel, event.Plan, event.Error)
		}
	}
	result := runChannelModelRefresh(ctx, channel, refresh, emit)
	if result.Error != "" {
		warnings = append(warnings, fmt.Sprintf("%s model auto-refresh failed: %s", channel, result.Error))
	}
	if result.Failed > 0 {
		warnings = append(warnings, fmt.Sprintf("some %s model sources failed auto-refresh; data that was not updated successfully is kept, see the service log for details", channel))
	}
	log.Printf("model auto-discovery finished: channel=%s added=%v refreshed=%d failed=%d", channel, result.Added, result.Refreshed, result.Failed)
	return warnings
}
