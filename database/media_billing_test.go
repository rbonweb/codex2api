package database

import "testing"

func withPricingOverrides(t *testing.T, overrides map[string]ModelPricingOverride) {
	t.Helper()
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(overrides)
}

func TestGrokMediaUpstreamReportedCostPrecedence(t *testing.T) {
	withPricingOverrides(t, nil)
	log := &UsageLogInput{Model: "grok-imagine-video", StatusCode: 200, VideoSeconds: 8, UpstreamCostUSD: 0.5}
	if got := UsageLogBilledCost(log); !approxEqual(got, 0.5) {
		t.Fatalf("upstream-reported cost should win over the built-in rate: %v", got)
	}
	withPricingOverrides(t, map[string]ModelPricingOverride{
		"grok-imagine-video": {Source: ModelPricingSourceCustom, MediaUnitCost: 0.1},
	})
	if got := UsageLogBilledCost(log); !approxEqual(got, 0.8) {
		t.Fatalf("custom unit cost should win over upstream-reported cost: %v", got)
	}
	withPricingOverrides(t, map[string]ModelPricingOverride{
		"grok-imagine-video": {Source: ModelPricingSourceSynced, MediaUnitCost: 0.1},
	})
	if got := UsageLogBilledCost(log); !approxEqual(got, 0.5) {
		t.Fatalf("synced unit cost must not override upstream-reported cost: %v", got)
	}
}

func TestValidateMediaUserBilling(t *testing.T) {
	cases := []struct {
		model string
		o     ModelPricingOverride
		ok    bool
	}{
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: UserBillingModePerSecond, ImageUnitPrice: 0.1}, true},
		{"grok-imagine-video-1.5-preview", ModelPricingOverride{UserBillingMode: UserBillingModePerVideo, ImageUnitPrice: 0.5}, true},
		{"grok-imagine-image", ModelPricingOverride{UserBillingMode: UserBillingModePerImage, ImageUnitPrice: 0.03, MediaUnitCost: 0.02}, true},
		{"grok-imagine-image", ModelPricingOverride{UserBillingMode: UserBillingModePerSecond, ImageUnitPrice: 0.1}, false},
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: UserBillingModePerImage, ImageUnitPrice: 0.1}, false},
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: UserBillingModePerVideo}, false},
		{"grok-4.7", ModelPricingOverride{MediaUnitCost: 0.1}, false},
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: "per_minute", ImageUnitPrice: 1}, false},
	}
	for _, tc := range cases {
		err := ValidateModelUserBilling(tc.model, tc.o)
		if (err == nil) != tc.ok {
			t.Fatalf("%s %+v: err=%v, want ok=%v", tc.model, tc.o, err, tc.ok)
		}
	}
}

func TestUsageLogDisplayForMediaRows(t *testing.T) {
	l := &UsageLog{Model: "grok-imagine-video", AccountBilled: 0.4, UserBilled: 0.4, VideoSeconds: 8}
	l.populateBillingBreakdown()
	if !approxEqual(l.TotalCost, 0.4) || l.InputCost != 0 || l.OutputCost != 0 {
		t.Fatalf("media display cost = %+v", l)
	}
}
