package database

import (
	"math"
	"testing"
)

func approxEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCalculateCostBreakdownWithCacheWrites_DefaultsToInputMultiples(t *testing.T) {
	// gpt-5.4 has no explicit cache-write prices: fall back to 1.25x / 2x of the input price.
	bd := CalculateCostBreakdownWithCacheWrites(3000, 0, 0, 1000, 1000, "gpt-5.4", "")
	if !approxEqual(bd.CacheWrite5mPricePerMToken, bd.InputPricePerMToken*1.25) || !approxEqual(bd.CacheWrite1hPricePerMToken, bd.InputPricePerMToken*2) {
		t.Fatalf("default write prices = %v / %v for input %v", bd.CacheWrite5mPricePerMToken, bd.CacheWrite1hPricePerMToken, bd.InputPricePerMToken)
	}
	if !approxEqual(bd.InputCost, 1000.0/1e6*bd.InputPricePerMToken) {
		t.Fatalf("InputCost must exclude cache-write tokens: %v", bd.InputCost)
	}
}

func TestCalculateCostBreakdown_UnchangedWithoutCacheWrites(t *testing.T) {
	legacy := CalculateCostBreakdown(1000, 500, 200, "gpt-5.4", "")
	next := CalculateCostBreakdownWithCacheWrites(1000, 500, 200, 0, 0, "gpt-5.4", "")
	if !approxEqual(legacy.TotalCost, next.TotalCost) || legacy.TotalCost != 0.00955 {
		t.Fatalf("legacy path changed: %v vs %v", legacy.TotalCost, next.TotalCost)
	}
}
