package outputguard

import (
	"errors"
	"math"
	"sync"
)

const (
	MaxRequestEvents   = 131072
	MaxRequestChannels = 128
	TriggeredCode      = "model_output_guard_triggered"
)

type Config struct {
	Enabled               bool
	MaxToolArgumentBytes  int64
	MaxToolArgumentDeltas int
	MaxChannelBytes       int64
	MaxRequestBytes       int64
}

type RequestGuard struct {
	mu            sync.RWMutex
	cfg           Config
	requestEvents int
	requestBytes  int64
	channels      map[string]*ChannelGuard
	aliasMap      map[string]string
	cachedChanID  string
	cachedAliasID string
	cachedTarget  *ChannelGuard
	tripped       bool
	trippedReason string
}

func NewRequestGuard(cfg Config) *RequestGuard {
	return &RequestGuard{
		cfg:      cfg,
		channels: make(map[string]*ChannelGuard),
		aliasMap: make(map[string]string),
	}
}

func (g *RequestGuard) IsTripped() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.tripped
}

func (g *RequestGuard) TrippedReason() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.trippedReason
}

func (g *RequestGuard) ActiveChannelCount() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.channels)
}

func (g *RequestGuard) tripLocked(reason string) error {
	if !g.tripped {
		g.tripped = true
		g.trippedReason = reason
	}
	return errors.New(g.trippedReason)
}

func (g *RequestGuard) resolveChannelLocked(channelID, aliasID string, kind ChannelKind) (*ChannelGuard, error) {
	if g.cachedTarget != nil && g.cachedChanID == channelID && g.cachedAliasID == aliasID {
		return g.cachedTarget, nil
	}

	// Late-ID merging and two-way mapping mechanism (Spec §4.3 & review feedback M2 fix):
	// Early real Responses streaming deltas usually contain only item_id (such as item_call_1); call_id is provided only at output_item.done.
	// Creating them separately would give the same tool call two channels.
	var primaryKey string
	if channelID != "" && g.aliasMap[channelID] != "" {
		primaryKey = g.aliasMap[channelID]
	} else if aliasID != "" && g.aliasMap[aliasID] != "" {
		primaryKey = g.aliasMap[aliasID]
	} else if channelID != "" && g.channels[channelID] != nil {
		primaryKey = channelID
	} else if aliasID != "" && g.channels[aliasID] != nil {
		primaryKey = aliasID
	} else if channelID != "" {
		primaryKey = channelID
	} else if aliasID != "" {
		primaryKey = aliasID
	} else {
		primaryKey = "default"
	}

	// Establish a two-way alias mapping so that later lookups by either channelID or aliasID point to the same primary ChannelGuard instance
	if channelID != "" {
		g.aliasMap[channelID] = primaryKey
	}
	if aliasID != "" {
		g.aliasMap[aliasID] = primaryKey
	}

	cg, exists := g.channels[primaryKey]
	if !exists {
		// Step 2: channel count check (Spec §5.3)
		if len(g.channels)+1 > MaxRequestChannels {
			return nil, g.tripLocked("channel_count")
		}
		maxBytes := g.cfg.MaxChannelBytes
		if kind == KindToolArguments {
			maxBytes = g.cfg.MaxToolArgumentBytes
		}
		cg = NewChannelGuard(primaryKey, kind, maxBytes, g.cfg.MaxToolArgumentDeltas)
		g.channels[primaryKey] = cg
	}

	g.cachedChanID = channelID
	g.cachedAliasID = aliasID
	g.cachedTarget = cg

	return cg, nil
}

// RecordEvent counts exactly once per upstream business event (step 1: request_events check)
func (g *RequestGuard) RecordEvent() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}
	return nil
}

// ObserveAppend strictly runs the Spec §5.3 priority pipeline (event count at step 1 first, then the step 2-7 decisions; the first reason is frozen)
func (g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	// Step 1: event budget check
	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}

	return g.observeAppendLocked(channelID, aliasID, kind, delta)
}

// ObserveAppendNoEvent runs only the step 2-7 decisions and does not add to requestEvents again (called by ObserveEvent batch processing)
func (g *RequestGuard) ObserveAppendNoEvent(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	return g.observeAppendLocked(channelID, aliasID, kind, delta)
}

func (g *RequestGuard) observeAppendLocked(channelID string, aliasID string, kind ChannelKind, delta []byte) error {
	// Step 2: channel check
	cg, err := g.resolveChannelLocked(channelID, aliasID, kind)
	if err != nil {
		return err
	}

	// Steps 3 & 4: delta count and semantic byte high-water check
	growth, cost, err := cg.CheckAppend(delta)
	if err != nil {
		return g.tripLocked(err.Error())
	}

	// Step 4: request-level semantic byte check (must come before the step 6 scan budget decision, and includes int64 overflow protection)
	if growth > 0 && g.requestBytes > math.MaxInt64-growth {
		return g.tripLocked("request_bytes")
	}
	if g.requestBytes+growth > g.cfg.MaxRequestBytes {
		return g.tripLocked("request_bytes")
	}

	// Step 6: scan budget deduction and check (Spec §5.3 step 6, runs after step 4 request_bytes)
	if cg.remainingScanBudget < cost {
		if cg.kind == KindToolArguments {
			return g.tripLocked("tool_argument_bytes")
		}
		return g.tripLocked("channel_bytes")
	}

	// Steps 5, 6, 7: commit the high-water mark and run the periodicity scan
	g.requestBytes += growth
	if err := cg.CommitAppend(delta, growth, cost); err != nil {
		return g.tripLocked(err.Error())
	}

	return nil
}

// ObserveSnapshot strictly runs the Spec §5.3 snapshot decision pipeline (event count at step 1 first, then the step 2-7 decisions)
func (g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	// Step 1: event budget check
	g.requestEvents++
	if g.requestEvents > MaxRequestEvents {
		return g.tripLocked("request_events")
	}

	return g.observeSnapshotLocked(channelID, aliasID, kind, snapshot, terminalStage)
}

// ObserveSnapshotNoEvent runs only the step 2-7 decisions and does not add to requestEvents again (called by ObserveEvent batch processing)
func (g *RequestGuard) ObserveSnapshotNoEvent(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if !g.cfg.Enabled {
		return nil
	}
	if g.tripped {
		return errors.New(g.trippedReason)
	}

	return g.observeSnapshotLocked(channelID, aliasID, kind, snapshot, terminalStage)
}

func (g *RequestGuard) observeSnapshotLocked(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error {
	// Step 2: channel check
	cg, err := g.resolveChannelLocked(channelID, aliasID, kind)
	if err != nil {
		return err
	}

	// Step 4: semantic byte budget check (first check high-water advancement and the request_bytes limit)
	snapLen := int64(len(snapshot))
	var growth int64
	if snapLen > cg.highwater {
		growth = snapLen - cg.highwater
	}
	if snapLen > cg.maxBytes {
		if cg.kind == KindToolArguments {
			return g.tripLocked("tool_argument_bytes")
		}
		return g.tripLocked("channel_bytes")
	}
	if growth > 0 && g.requestBytes > math.MaxInt64-growth {
		return g.tripLocked("request_bytes")
	}
	if g.requestBytes+growth > g.cfg.MaxRequestBytes {
		return g.tripLocked("request_bytes")
	}

	// Steps 5 & 6: eligibility/equivalence check and scan budget deduction
	growth, cost, _, err := cg.CheckSnapshot(snapshot, terminalStage)
	if err != nil {
		if errors.Is(err, ErrScanBudgetExhausted) {
			if cg.kind == KindToolArguments {
				return g.tripLocked("tool_argument_bytes")
			}
			return g.tripLocked("channel_bytes")
		}
		return g.tripLocked(err.Error())
	}

	// Step 7: commit the high-water mark and the scan
	g.requestBytes += growth
	if err := cg.CommitSnapshot(snapshot, growth, cost, terminalStage); err != nil {
		return g.tripLocked(err.Error())
	}

	return nil
}

// EndChannel runs the tail check before closing the channel and releases the window memory; the cumulative channel mapping is not returned
func (g *RequestGuard) EndChannel(channelID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	targetKey := channelID
	if alias := g.aliasMap[channelID]; alias != "" {
		targetKey = alias
	}

	if cg, exists := g.channels[targetKey]; exists {
		// Run the tail check first (Spec §5.2 rule 6)
		if err := cg.CheckTail(); err != nil {
			return g.tripLocked(err.Error())
		}
		// After the tail check passes, safely release the sliding window memory (<=64 KiB)
		cg.ReleaseWindow()
		if g.cachedTarget == cg {
			g.cachedChanID = ""
			g.cachedAliasID = ""
			g.cachedTarget = nil
		}
	}
	return nil
}
