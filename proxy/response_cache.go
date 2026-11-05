package proxy

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== Response context cache ====================
// Fixes the loss of tool calling context in the previous_response_id scenario.
// The proxy layer sets store=false and deletes previous_response_id, so upstream cannot recover the historical function_call.
// This module caches the accumulated conversation context of each response locally. When the next request carries previous_response_id,
// it automatically injects the historical items back into input, so upstream can match call_id without relying on server-side storage.
//
// Isolation: cache key = owner (downstream API Key ID) + response_id. Without an owner dimension, any request
// that carries (guessed or reused) someone else's response_id can inject their conversation history into its own input,
// causing cross-user context leakage. An owner mismatch is always treated as a cache miss.

const (
	responseCacheTTL        = 45 * time.Minute
	responseCacheMaxBytes   = 64 << 20
	responseCacheMaxEntry   = 8 << 20
	responseCacheMaxItems   = 2000 // maximum number of cache entries, to prevent memory bloat
	responseCacheMaxPerItem = 200  // maximum number of raw items per cache entry
	responseCacheMaxMarkers = 2000
	responseCleanupInterval = 2 * time.Minute

	// Under the on_demand write policy, the sliding window in which an owner that has sent previous_response_id keeps its write eligibility.
	// Chained clients continue the chain turn by turn (seconds apart), so 1 hour is enough to cover session gaps;
	// after the window expires the gate closes again, and the owner's next chained request misses once first (consistent with
	// the behaviour after TTL expiry), then writes resume.
	responseCacheChainOwnerTTL  = time.Hour
	responseCacheMaxChainOwners = 4096
)

// responseCacheOwner generates the owner namespace for the response context cache.
// Mainly the downstream API Key ID; when there is no auth context (0), it falls back to a shared anonymous namespace (tests and embedded scenarios only).
func responseCacheOwner(apiKeyID int64) string {
	if apiKeyID > 0 {
		return fmt.Sprintf("key:%d", apiKeyID)
	}
	return "anon"
}

// responseCacheStoreKey combines owner and response_id into the cache key (shared by the local map and Redis).
func responseCacheStoreKey(owner, responseID string) string {
	if owner == "" {
		owner = "anon"
	}
	return owner + "|" + responseID
}

type responseCacheEntry struct {
	key              string
	serial           uint64
	items            []json.RawMessage
	blobs            []*sharedResponseContextItem
	bytes            int64
	expiresAt        time.Time
	element          *list.Element
	nativeProvenance *nativeWSReplayProvenance
}

type responseCacheConfig struct {
	maxBytes            int64
	maxEntryBytes       int64
	reconstructMaxBytes int64
	maxEntries          int
	ttl                 time.Duration
	maxItems            int
	writePolicy         string
}

func defaultResponseCacheConfig() responseCacheConfig {
	return responseCacheConfig{
		maxBytes:            responseCacheMaxBytes,
		maxEntryBytes:       responseCacheMaxEntry,
		reconstructMaxBytes: responseCacheMaxBytes,
		maxEntries:          responseCacheMaxItems,
		ttl:                 responseCacheTTL,
		maxItems:            responseCacheMaxPerItem,
		writePolicy:         database.DefaultResponseCacheWritePolicy,
	}
}

// ResponseCacheStats is a point-in-time snapshot of the bounded local cache.
// Bytes are logical retained JSON payload bytes; they are not an RSS estimate.
// Hits/Misses are end-to-end counts: a query that finally gets the context (locally or from the shared backend) counts as a Hit,
// otherwise it counts as a Miss (including negative markers, backend errors/corruption, and rebuilds over the limit). Local*/Remote* are tiered counts.
// Invariants: Hits = LocalHits + RemoteHits; Hits + Misses = LocalHits + LocalMisses.
type ResponseCacheStats struct {
	// Unique retained payload bytes, excluding slice/map overhead and active
	// readers. Bytes below remains the conservative logical eviction budget.
	SharedPayloadBytes     int64
	Entries                int
	Bytes                  int64
	HighWaterBytes         int64
	LargestSeenEntryBytes  int64
	Hits                   uint64
	Misses                 uint64
	LocalHits              uint64
	LocalMisses            uint64
	RemoteHits             uint64
	RemoteMisses           uint64
	Expirations            uint64
	CountEvictions         uint64
	ByteEvictions          uint64
	OversizeSkips          uint64
	OversizeBypasses       uint64
	OversizeRejections     uint64
	KnownUnavailableErrors uint64
	// SkippedWrites is the number of writes skipped under the on_demand write policy because the owner did not chain
	// (both local and Redis writes are skipped); ChainOwners is the number of owners currently inside the write-eligibility window.
	SkippedWrites uint64
	ChainOwners   int
	// BackendWriteFailures includes explicit admission failures and backend
	// errors. A failed shared write never makes an existing L1 hit unavailable.
	BackendWriteFailures uint64
}

type responseCacheState struct {
	mu           sync.RWMutex
	store        map[string]*responseCacheEntry
	sharedItems  map[[sha256.Size]byte]*sharedResponseContextItem
	lru          *list.List
	markers      map[string]*responseCacheMarker
	markerLRU    *list.List
	chainOwners  map[string]*responseCacheChainOwner
	chainLRU     *list.List
	config       responseCacheConfig
	generation   int64
	stats        ResponseCacheStats
	entrySerial  uint64
	runtimeCache cache.TokenCache
	lastSyncAt   time.Time
	lastSyncErr  string
}

// responseCacheChainOwner records the expiry time of an owner's most recent previous_response_id chained request,
// which is the admission basis for the on_demand write policy.
type responseCacheChainOwner struct {
	owner     string
	expiresAt time.Time
	element   *list.Element
}

type responseCacheLookupKind uint8

const (
	responseCacheLookupMiss responseCacheLookupKind = iota
	responseCacheLookupHit
	responseCacheLookupExpired
	responseCacheLookupKnownEvicted
	responseCacheLookupKnownOversize
	responseCacheLookupReconstructionTooLarge
	responseCacheLookupBackendCorrupt
	responseCacheLookupBackendError
	responseCacheLookupBackendPending
)

type responseCacheLookupSource uint8

const (
	responseCacheSourceNone responseCacheLookupSource = iota
	responseCacheSourceLocal
	responseCacheSourceBackend
)

type responseCacheLookupResult struct {
	Items            []json.RawMessage
	Kind             responseCacheLookupKind
	Source           responseCacheLookupSource
	Promoted         bool
	Err              error
	nativeProvenance *nativeWSReplayProvenance
	// Bookkeeping helper: filled in by lookupResponseCacheResult; getResponseCacheResult
	// uses it to update all counters inside a single critical section, keeping the snapshot invariant true at every instant.
	remoteMiss     bool
	oversizeBypass bool
}

type responseCacheMarker struct {
	key       string
	serial    uint64
	kind      responseCacheLookupKind
	expiresAt time.Time
	element   *list.Element
}

var respCache responseCacheState

func init() {
	respCache.store = make(map[string]*responseCacheEntry)
	respCache.sharedItems = make(map[[sha256.Size]byte]*sharedResponseContextItem)
	respCache.lru = list.New()
	respCache.markers = make(map[string]*responseCacheMarker)
	respCache.markerLRU = list.New()
	respCache.chainOwners = make(map[string]*responseCacheChainOwner)
	respCache.chainLRU = list.New()
	respCache.config = defaultResponseCacheConfig()
	go respCacheCleanupLoop()
}

// SetResponseContextCache installs the L2 backend behind the local L1. A
// shared (Redis) cache or a durable local store qualifies; a plain in-process
// cache would only duplicate L1 and is ignored.
func SetResponseContextCache(tc cache.TokenCache) {
	respCache.mu.Lock()
	respCache.runtimeCache = nil
	if tc != nil && (tc.SharedAcrossInstances() || cache.IsDurableResponseContextCache(tc)) {
		respCache.runtimeCache = tc
	}
	respCache.mu.Unlock()
}

// backendPersistsKeyLocked reports whether the L2 backend also holds key.
// Native WebSocket snapshots carry account provenance and are persisted only
// to a shared backend, so with a durable local store they keep the L1-only
// eviction and oversize markers.
func (c *responseCacheState) backendPersistsKeyLocked(key string) bool {
	if c.runtimeCache == nil {
		return false
	}
	return c.runtimeCache.SharedAcrossInstances() || !strings.HasPrefix(key, nativeWSCachePrefix)
}

// GetResponseCacheStats returns a thread-safe local-cache snapshot.
func GetResponseCacheStats() ResponseCacheStats {
	respCache.mu.RLock()
	stats := respCache.stats
	respCache.mu.RUnlock()
	return stats
}

// resetResponseCacheStateForTest replaces all local state with a deterministic
// test configuration. It deliberately remains package-private.
func resetResponseCacheStateForTest(config responseCacheConfig) {
	// Background writes from the previous test must land first, otherwise they would write into the reset backend.
	drainResponseCacheBackendWrites()
	responseCacheBackendWriter.mu.Lock()
	responseCacheBackendWriter.draining = false
	responseCacheBackendWriter.maxActive = responseCacheBackendWriteSlots
	responseCacheBackendWriter.maxBytes = responseCacheBackendWriteBytes
	responseCacheBackendWriter.maxWaiters = responseCacheBackendMaxWaiters
	responseCacheBackendWriter.highWaterActive = 0
	responseCacheBackendWriter.highWaterBytes = 0
	responseCacheBackendWriter.queueRejections = 0
	responseCacheBackendWriter.waitTimeouts = 0
	responseCacheBackendWriter.mu.Unlock()
	respCache.mu.Lock()
	respCache.store = make(map[string]*responseCacheEntry)
	respCache.sharedItems = make(map[[sha256.Size]byte]*sharedResponseContextItem)
	respCache.lru = list.New()
	respCache.markers = make(map[string]*responseCacheMarker)
	respCache.markerLRU = list.New()
	respCache.chainOwners = make(map[string]*responseCacheChainOwner)
	respCache.chainLRU = list.New()
	respCache.config = config
	respCache.generation = 0
	respCache.stats = ResponseCacheStats{}
	respCache.entrySerial = 0
	respCache.runtimeCache = nil
	respCache.lastSyncAt = time.Time{}
	respCache.lastSyncErr = ""
	respCache.mu.Unlock()
}

// configureResponseCacheForTest exercises the same immediate runtime-shrink
// path future configuration wiring will use, without exposing configuration.
func configureResponseCacheForTest(config responseCacheConfig) {
	respCache.mu.Lock()
	respCache.config = config
	respCache.enforceConfigLocked()
	respCache.mu.Unlock()
}

// markResponseCacheChainOwnerLocked marks the owner as a "recent chainer" and renews its sliding window.
// When at capacity, the owner that chained least recently is evicted by LRU.
func markResponseCacheChainOwnerLocked(owner string) {
	if owner == "" {
		owner = "anon"
	}
	expiresAt := time.Now().Add(responseCacheChainOwnerTTL)
	if existing := respCache.chainOwners[owner]; existing != nil {
		existing.expiresAt = expiresAt
		respCache.chainLRU.MoveToFront(existing.element)
		return
	}
	record := &responseCacheChainOwner{owner: owner, expiresAt: expiresAt}
	record.element = respCache.chainLRU.PushFront(record)
	respCache.chainOwners[owner] = record
	for len(respCache.chainOwners) > responseCacheMaxChainOwners {
		element := respCache.chainLRU.Back()
		if element == nil {
			break
		}
		oldest, _ := element.Value.(*responseCacheChainOwner)
		removeResponseCacheChainOwnerLocked(oldest.owner)
	}
	respCache.stats.ChainOwners = len(respCache.chainOwners)
}

func removeResponseCacheChainOwnerLocked(owner string) {
	record := respCache.chainOwners[owner]
	if record == nil {
		return
	}
	delete(respCache.chainOwners, owner)
	if record.element != nil {
		respCache.chainLRU.Remove(record.element)
	}
	respCache.stats.ChainOwners = len(respCache.chainOwners)
}

// markResponseCacheChainOwnerIfOnDemand marks the owner as a
// recent chainer under the on_demand write policy; under the always policy writes are already allowed and do not take an LRU slot.
func markResponseCacheChainOwnerIfOnDemand(owner string) {
	respCache.mu.Lock()
	defer respCache.mu.Unlock()
	if respCache.config.writePolicy != database.ResponseCacheWritePolicyOnDemand {
		return
	}
	markResponseCacheChainOwnerLocked(owner)
}

// responseCacheWriteAllowed reports whether the owner is eligible to write under the current write policy.
// Under on_demand, expired records are treated as ineligible (lazy deletion is left to the cleanup loop).
func responseCacheWriteAllowed(owner string) bool {
	if owner == "" {
		owner = "anon"
	}
	respCache.mu.RLock()
	defer respCache.mu.RUnlock()
	if respCache.config.writePolicy != database.ResponseCacheWritePolicyOnDemand {
		return true
	}
	record := respCache.chainOwners[owner]
	return record != nil && time.Now().Before(record.expiresAt)
}

// setResponseCache stores the response context (isolated by owner namespace).
// Under the on_demand write policy, owners that have never chained are skipped directly (both local and Redis).
func setResponseCache(owner, responseID string, items []json.RawMessage) {
	if skipResponseCacheWrite(owner) {
		return
	}
	storeKey := responseCacheStoreKey(owner, responseID)
	runtimeItems, _, _, serial := admitResponseCacheWithTicket(storeKey, items)

	respCache.mu.RLock()
	runtimeCache := respCache.runtimeCache
	if !respCache.backendPersistsKeyLocked(storeKey) {
		runtimeCache = nil
	}
	respCache.mu.RUnlock()

	if runtimeCache != nil && len(runtimeItems) > 0 {
		_ = writeResponseContextBackendWithOwnership(runtimeCache, responseID, storeKey, runtimeItems, serial)
	}
}

func skipResponseCacheWrite(owner string) bool {
	if responseCacheWriteAllowed(owner) {
		return false
	}
	respCache.mu.Lock()
	respCache.stats.SkippedWrites++
	respCache.mu.Unlock()
	return true
}

func admitResponseCache(storeKey string, items []json.RawMessage) ([]json.RawMessage, bool, bool) {
	items, admitted, oversize, _ := admitResponseCacheWithTicket(storeKey, items)
	return items, admitted, oversize
}

// A ticket lets a backend writer borrow this exact admission after acquiring
// capacity. Waiting writers must not pin bodies already evicted from L1.
func admitResponseCacheWithTicket(storeKey string, items []json.RawMessage) ([]json.RawMessage, bool, bool, uint64) {
	respCache.mu.Lock()
	defer respCache.mu.Unlock()
	respCache.entrySerial++
	serial := respCache.entrySerial

	native := strings.HasPrefix(storeKey, nativeWSCachePrefix)
	if native && (len(items) > respCache.config.maxItems || responseContextLogicalBytes(items) > respCache.config.reconstructMaxBytes) {
		if existing := respCache.store[storeKey]; existing != nil {
			respCache.removeEntryLocked(existing, responseCacheRemovalReplace)
		}
		respCache.setMarkerLocked(storeKey, responseCacheLookupKnownOversize, time.Now().Add(respCache.config.ttl))
		respCache.stats.OversizeSkips++
		respCache.stats.OversizeRejections++
		return nil, false, true, serial
	}
	items = trimResponseContextTail(items, respCache.config.maxItems)
	items, hashes, normalized := respCache.normalizeResponseContextItemsLocked(items)
	var entryBytes int64
	for _, item := range items {
		entryBytes += int64(len(item))
	}
	if entryBytes > respCache.stats.LargestSeenEntryBytes {
		respCache.stats.LargestSeenEntryBytes = entryBytes
	}
	if existing := respCache.store[storeKey]; existing != nil {
		respCache.removeEntryLocked(existing, responseCacheRemovalReplace)
	}
	overL1ByteBudget := entryBytes > respCache.config.maxEntryBytes ||
		entryBytes > respCache.config.maxBytes
	if respCache.config.maxEntries <= 0 || overL1ByteBudget {
		respCache.stats.OversizeSkips++
		if !respCache.backendPersistsKeyLocked(storeKey) && overL1ByteBudget {
			respCache.stats.OversizeRejections++
		}
		if !respCache.backendPersistsKeyLocked(storeKey) {
			respCache.setMarkerLocked(storeKey, responseCacheLookupKnownOversize, time.Now().Add(respCache.config.ttl))
		} else {
			respCache.setWriteMarkerLocked(storeKey, responseCacheLookupBackendPending, time.Now().Add(respCache.config.ttl), serial)
		}
		return items, false, overL1ByteBudget, serial
	}

	for len(respCache.store)+1 > respCache.config.maxEntries ||
		respCache.stats.Bytes+entryBytes > respCache.config.maxBytes {
		oldest := respCache.oldestLocked()
		if oldest == nil {
			overL1ByteBudget = respCache.stats.Bytes+entryBytes > respCache.config.maxBytes
			respCache.stats.OversizeSkips++
			if !respCache.backendPersistsKeyLocked(storeKey) && overL1ByteBudget {
				respCache.stats.OversizeRejections++
			}
			if !respCache.backendPersistsKeyLocked(storeKey) {
				respCache.setMarkerLocked(storeKey, responseCacheLookupKnownOversize, time.Now().Add(respCache.config.ttl))
			} else {
				respCache.setWriteMarkerLocked(storeKey, responseCacheLookupBackendPending, time.Now().Add(respCache.config.ttl), serial)
			}
			return items, false, overL1ByteBudget, serial
		}
		reason := responseCacheRemovalByteEviction
		if len(respCache.store)+1 > respCache.config.maxEntries {
			reason = responseCacheRemovalCountEviction
		}
		respCache.removeEntryLocked(oldest, reason)
	}

	retainedItems, blobs := respCache.retainResponseContextItemsLocked(items, hashes, normalized)
	entry := &responseCacheEntry{
		key:       storeKey,
		serial:    serial,
		items:     retainedItems,
		blobs:     blobs,
		bytes:     entryBytes,
		expiresAt: time.Now().Add(respCache.config.ttl),
	}
	entry.element = respCache.lru.PushFront(entry)
	respCache.store[storeKey] = entry
	respCache.removeMarkerLocked(storeKey)
	respCache.stats.Entries = len(respCache.store)
	respCache.stats.Bytes += entryBytes
	if respCache.stats.Bytes > respCache.stats.HighWaterBytes {
		respCache.stats.HighWaterBytes = respCache.stats.Bytes
	}
	return items, true, false, entry.serial
}

func cloneResponseContextItems(items []json.RawMessage) []json.RawMessage {
	itemsCopy := make([]json.RawMessage, len(items))
	for i, item := range items {
		itemsCopy[i] = append(json.RawMessage(nil), item...)
	}
	return itemsCopy
}

// Cached bodies are immutable. Identical items across response IDs share an
// owned allocation; reference counts follow cache entries only. Eviction drops
// ownership without modifying bytes, so an active replay remains valid.
type sharedResponseContextItem struct {
	key        [sha256.Size]byte
	body       json.RawMessage
	refs       int
	normalized bool
}

// Previously interned canonical bytes have already passed JSON validation and
// escape normalization. Rechecking every historical item on every turn would
// consume the allocation savings in repeated parsing. Only new bytes need it.
func (c *responseCacheState) normalizeResponseContextItemsLocked(items []json.RawMessage) ([]json.RawMessage, [][sha256.Size]byte, bool) {
	hashes := make([][sha256.Size]byte, len(items))
	var normalized []json.RawMessage
	for i, item := range items {
		key := sha256.Sum256(item)
		if blob := c.sharedItems[key]; blob != nil && blob.normalized && bytes.Equal(blob.body, item) {
			hashes[i] = key
			continue
		}
		next, err := cache.NormalizeResponseContextItems([]json.RawMessage{item})
		if err != nil {
			return items, nil, false
		} // Preserve legacy all-or-nothing normalization.
		if !bytes.Equal(item, next[0]) {
			if normalized == nil {
				normalized = append([]json.RawMessage(nil), items...)
			}
			normalized[i] = next[0]
			key = sha256.Sum256(next[0])
		}
		hashes[i] = key
	}
	if normalized != nil {
		items = normalized
	}
	return items, hashes, true
}

func (c *responseCacheState) retainResponseContextItemsLocked(items []json.RawMessage, hashes [][sha256.Size]byte, normalized bool) ([]json.RawMessage, []*sharedResponseContextItem) {
	if c.sharedItems == nil {
		c.sharedItems = make(map[[sha256.Size]byte]*sharedResponseContextItem)
	}
	retained := make([]json.RawMessage, len(items))
	blobs := make([]*sharedResponseContextItem, len(items))
	for i, item := range items {
		var key [sha256.Size]byte
		if hashes != nil {
			key = hashes[i]
		} else {
			key = sha256.Sum256(item)
		}
		blob := c.sharedItems[key]
		if blob == nil || !bytes.Equal(blob.body, item) {
			blob = &sharedResponseContextItem{key: key, body: append(json.RawMessage(nil), item...)}
			// A hash collision must never alias unrelated content or replace a
			// live intern entry. The colliding item stays privately owned.
			if c.sharedItems[key] == nil {
				c.sharedItems[key] = blob
			}
			c.stats.SharedPayloadBytes += int64(len(item))
		}
		blob.refs++
		blob.normalized = blob.normalized || normalized
		retained[i], blobs[i] = blob.body, blob
	}
	return retained, blobs
}

type responseCacheRemovalReason uint8

const (
	responseCacheRemovalReplace responseCacheRemovalReason = iota
	responseCacheRemovalExpiration
	responseCacheRemovalCountEviction
	responseCacheRemovalByteEviction
)

func (c *responseCacheState) oldestLocked() *responseCacheEntry {
	if c.lru == nil {
		return nil
	}
	element := c.lru.Back()
	if element == nil {
		return nil
	}
	entry, _ := element.Value.(*responseCacheEntry)
	return entry
}

func (c *responseCacheState) removeEntryLocked(entry *responseCacheEntry, reason responseCacheRemovalReason) {
	if entry == nil || c.store[entry.key] != entry {
		return
	}
	delete(c.store, entry.key)
	// Preserve a bounded identity fence after Redis-backed eviction so an old
	// asynchronous result cannot overwrite a newer admission's failure state.
	if c.backendPersistsKeyLocked(entry.key) && reason != responseCacheRemovalReplace && c.markers[entry.key] == nil {
		c.setWriteMarkerLocked(entry.key, responseCacheLookupBackendPending, entry.expiresAt, entry.serial)
	}
	for _, blob := range entry.blobs {
		blob.refs--
		if blob.refs == 0 {
			if c.sharedItems[blob.key] == blob {
				delete(c.sharedItems, blob.key)
				if len(c.sharedItems) == 0 {
					c.sharedItems = nil
				}
			}
			c.stats.SharedPayloadBytes -= int64(len(blob.body))
		}
	}
	if entry.element != nil {
		c.lru.Remove(entry.element)
	}
	c.stats.Entries = len(c.store)
	c.stats.Bytes -= entry.bytes
	switch reason {
	case responseCacheRemovalExpiration:
		c.stats.Expirations++
	case responseCacheRemovalCountEviction:
		c.stats.CountEvictions++
		if !c.backendPersistsKeyLocked(entry.key) {
			c.setMarkerLocked(entry.key, responseCacheLookupKnownEvicted, entry.expiresAt)
		}
	case responseCacheRemovalByteEviction:
		c.stats.ByteEvictions++
		if !c.backendPersistsKeyLocked(entry.key) {
			c.setMarkerLocked(entry.key, responseCacheLookupKnownEvicted, entry.expiresAt)
		}
	}
}

func (c *responseCacheState) setMarkerLocked(key string, kind responseCacheLookupKind, expiresAt time.Time) {
	if key == "" || (kind != responseCacheLookupKnownEvicted && kind != responseCacheLookupKnownOversize && kind != responseCacheLookupBackendError && kind != responseCacheLookupBackendPending) {
		return
	}
	maxExpiry := time.Now().Add(c.config.ttl)
	if expiresAt.IsZero() || expiresAt.After(maxExpiry) {
		expiresAt = maxExpiry
	}
	if existing := c.markers[key]; existing != nil {
		existing.serial = 0
		existing.kind = kind
		existing.expiresAt = expiresAt
		c.markerLRU.MoveToFront(existing.element)
		return
	}
	marker := &responseCacheMarker{key: key, kind: kind, expiresAt: expiresAt}
	marker.element = c.markerLRU.PushFront(marker)
	c.markers[key] = marker
	for len(c.markers) > responseCacheMaxMarkers {
		element := c.markerLRU.Back()
		if element == nil {
			break
		}
		oldest, _ := element.Value.(*responseCacheMarker)
		c.removeMarkerLocked(oldest.key)
	}
}

func (c *responseCacheState) setWriteMarkerLocked(key string, kind responseCacheLookupKind, expiresAt time.Time, serial uint64) {
	c.setMarkerLocked(key, kind, expiresAt)
	if marker := c.markers[key]; marker != nil {
		marker.serial = serial
	}
}

func (c *responseCacheState) removeMarkerLocked(key string) {
	marker := c.markers[key]
	if marker == nil {
		return
	}
	delete(c.markers, key)
	if marker.element != nil {
		c.markerLRU.Remove(marker.element)
	}
}

func (c *responseCacheState) enforceConfigLocked() {
	for element := c.lru.Back(); element != nil; {
		previous := element.Prev()
		entry, _ := element.Value.(*responseCacheEntry)
		if entry != nil && entry.bytes > c.config.maxEntryBytes {
			c.removeEntryLocked(entry, responseCacheRemovalByteEviction)
		}
		element = previous
	}
	for len(c.store) > c.config.maxEntries || c.stats.Bytes > c.config.maxBytes {
		oldest := c.oldestLocked()
		if oldest == nil {
			break
		}
		reason := responseCacheRemovalByteEviction
		if len(c.store) > c.config.maxEntries {
			reason = responseCacheRemovalCountEviction
		}
		c.removeEntryLocked(oldest, reason)
	}
}

// trimResponseContextTail keeps one contiguous suffix no longer than maxItems.
// If the initial boundary splits a known one-to-one call/output match, that
// interval is excluded. Moving the boundary can expose another crossing
// interval, so the scan repeats until every retained matched pair is complete.
func trimResponseContextTail(items []json.RawMessage, maxItems int) []json.RawMessage {
	if maxItems <= 0 {
		return nil
	}
	if len(items) <= maxItems {
		return items
	}

	type callKey struct {
		callID   string
		callType string
	}
	type matchedInterval struct {
		callIndex   int
		outputIndex int
	}
	pendingCalls := make(map[callKey][]int)
	var intervals []matchedInterval
	for index, item := range items {
		typ := gjson.GetBytes(item, "type").String()
		callType, isCall, isOutput := responseContextPairType(typ)
		if callType == "" {
			continue
		}
		callID := gjson.GetBytes(item, "call_id").String()
		if callID == "" {
			continue
		}
		key := callKey{callID: callID, callType: callType}
		if isCall {
			pendingCalls[key] = append(pendingCalls[key], index)
			continue
		}
		if !isOutput || len(pendingCalls[key]) == 0 {
			continue
		}
		callIndex := pendingCalls[key][0]
		pendingCalls[key] = pendingCalls[key][1:]
		intervals = append(intervals, matchedInterval{
			callIndex:   callIndex,
			outputIndex: index,
		})
	}

	start := len(items) - maxItems
	for {
		nextStart := start
		for _, interval := range intervals {
			if interval.callIndex < start && interval.outputIndex >= start && interval.outputIndex+1 > nextStart {
				nextStart = interval.outputIndex + 1
			}
		}
		if nextStart == start {
			return items[start:]
		}
		start = nextStart
	}
}

func responseContextPairType(typ string) (callType string, isCall, isOutput bool) {
	switch typ {
	case "function_call", "tool_call", "local_shell_call", "shell_call",
		"apply_patch_call", "tool_search_call", "custom_tool_call", "mcp_tool_call":
		return typ, true, false
	case "function_call_output":
		return "function_call", false, true
	case "tool_call_output":
		return "tool_call", false, true
	case "local_shell_call_output":
		return "local_shell_call", false, true
	case "shell_call_output":
		return "shell_call", false, true
	case "apply_patch_call_output":
		return "apply_patch_call", false, true
	case "tool_search_call_output":
		return "tool_search_call", false, true
	case "custom_tool_call_output":
		return "custom_tool_call", false, true
	case "mcp_tool_call_output":
		return "mcp_tool_call", false, true
	default:
		return "", false, false
	}
}

// getResponseCache looks up the cached response context; an owner mismatch is equivalent to a cache miss,
// preventing cross-API-Key use of a response_id to fetch someone else's conversation history.
func getResponseCache(owner, responseID string) []json.RawMessage {
	result := getResponseCacheResult(owner, responseID)
	if result.Kind != responseCacheLookupHit {
		return nil
	}
	return result.Items
}

// getResponseCacheForReplay is reserved for request preparation. Callers must
// treat item bodies as immutable and replace, never mutate, an item to edit it.
func getResponseCacheForReplay(owner, responseID string) responseCacheLookupResult {
	return getResponseCacheResultWithOwnership(owner, responseID, true)
}

// getResponseCacheResult preserves enough lookup state for the HTTP handlers
// to distinguish a reconstructable hit from a final local/backend failure.
// Hit/miss counts are recorded only once at this exit, inside a single critical section: the aggregate Hits/Misses are
// end-to-end counts, Local*/Remote* are tiered counts, and the two groups stay consistent at any snapshot instant.
func getResponseCacheResult(owner, responseID string) responseCacheLookupResult {
	return getResponseCacheResultWithOwnership(owner, responseID, false)
}

func getResponseCacheResultWithOwnership(owner, responseID string, borrow bool) responseCacheLookupResult {
	result := lookupResponseCacheResultWithOwnership(owner, responseID, borrow)
	recordResponseCacheLookup(owner, result)
	return result
}

func recordResponseCacheLookup(owner string, result responseCacheLookupResult) {
	respCache.mu.Lock()
	// Any chained query (hit or not) grants the owner write eligibility: this is the admission signal
	// for the on_demand write policy, and the hit rate is unrelated to it.
	markResponseCacheChainOwnerLocked(owner)
	if result.Kind == responseCacheLookupHit {
		respCache.stats.Hits++
		if result.Source == responseCacheSourceBackend {
			respCache.stats.LocalMisses++
			respCache.stats.RemoteHits++
			if result.oversizeBypass {
				respCache.stats.OversizeBypasses++
			}
		} else {
			respCache.stats.LocalHits++
		}
	} else {
		respCache.stats.Misses++
		respCache.stats.LocalMisses++
		if result.remoteMiss {
			respCache.stats.RemoteMisses++
		}
	}
	respCache.mu.Unlock()
}

func lookupResponseCacheResult(owner, responseID string) responseCacheLookupResult {
	return lookupResponseCacheResultWithOwnership(owner, responseID, false)
}

func lookupResponseCacheResultWithOwnership(owner, responseID string, borrow bool) responseCacheLookupResult {
	storeKey := responseCacheStoreKey(owner, responseID)
	respCache.mu.Lock()
	entry, ok := respCache.store[storeKey]
	runtimeCache := respCache.runtimeCache
	if !respCache.backendPersistsKeyLocked(storeKey) {
		runtimeCache = nil
	}
	config := respCache.config
	writeFailed := false
	expired := false
	if ok {
		if !time.Now().Before(entry.expiresAt) {
			respCache.removeEntryLocked(entry, responseCacheRemovalExpiration)
			expired = true
		} else {
			respCache.lru.MoveToFront(entry.element)
			// entry.items is immutable after insertion (a private clone is stored at admit time),
			// so take the reference and clone outside the lock, to avoid a megabyte-scale copy under the lock on large-entry hits.
			items := entry.items
			respCache.mu.Unlock()
			if borrow {
				// Each reader owns its sequence header. Bodies stay immutable;
				// append/replacing an item cannot alter another reader's list.
				return responseCacheLookupResult{Items: append([]json.RawMessage(nil), items...), Kind: responseCacheLookupHit, Source: responseCacheSourceLocal}
			}
			return responseCacheLookupResult{
				Items:  cloneResponseContextItems(items),
				Kind:   responseCacheLookupHit,
				Source: responseCacheSourceLocal,
			}
		}
	}
	if runtimeCache == nil {
		if marker := respCache.markers[storeKey]; marker != nil {
			if time.Now().Before(marker.expiresAt) {
				respCache.markerLRU.MoveToFront(marker.element)
				kind := marker.kind
				if kind == responseCacheLookupBackendPending {
					kind = responseCacheLookupMiss
				}
				respCache.mu.Unlock()
				return responseCacheLookupResult{Kind: kind}
			}
			respCache.removeMarkerLocked(storeKey)
		}
	} else if marker := respCache.markers[storeKey]; marker != nil && marker.kind == responseCacheLookupBackendError {
		writeFailed = time.Now().Before(marker.expiresAt)
		if !writeFailed {
			respCache.removeMarkerLocked(storeKey)
		}
	}
	respCache.mu.Unlock()

	if runtimeCache == nil {
		if expired {
			return responseCacheLookupResult{Kind: responseCacheLookupExpired}
		}
		return responseCacheLookupResult{Kind: responseCacheLookupMiss}
	}
	readTimeout := 500 * time.Millisecond
	if cache.IsDurableResponseContextCache(runtimeCache) && !runtimeCache.SharedAcrossInstances() {
		readTimeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	backendResult, err := readResponseContextBackend(ctx, runtimeCache, storeKey, config)
	if err != nil {
		log.Printf("failed to read response context from backend: response_id=%s err=%v", responseID, err)
		return responseCacheLookupResult{Kind: responseCacheLookupBackendError, Err: err}
	}
	switch backendResult.Status {
	case cache.ResponseContextReadMiss:
		if writeFailed {
			return responseCacheLookupResult{Kind: responseCacheLookupBackendError, Err: errResponseCacheBackendWriteFailed, remoteMiss: true}
		}
		if expired {
			return responseCacheLookupResult{Kind: responseCacheLookupExpired, remoteMiss: true}
		}
		return responseCacheLookupResult{Kind: responseCacheLookupMiss, remoteMiss: true}
	case cache.ResponseContextReadTooLarge:
		return responseCacheLookupResult{Kind: responseCacheLookupReconstructionTooLarge}
	case cache.ResponseContextReadCorrupt:
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	case cache.ResponseContextReadFound:
	default:
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	}
	items, err := cache.NormalizeResponseContextItems(backendResult.Items)
	if err != nil {
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	}
	if responseContextLogicalBytes(items) > config.reconstructMaxBytes {
		return responseCacheLookupResult{Kind: responseCacheLookupReconstructionTooLarge}
	}
	items = trimResponseContextTail(items, config.maxItems)
	runtimeItems, promoted, overL1ByteBudget := admitResponseCache(storeKey, items)
	return responseCacheLookupResult{
		Items:          runtimeItems,
		Kind:           responseCacheLookupHit,
		Source:         responseCacheSourceBackend,
		Promoted:       promoted,
		oversizeBypass: !promoted && overL1ByteBudget,
	}
}

func setResponseCacheLocal(storeKey string, items []json.RawMessage) bool {
	_, admitted, _ := admitResponseCache(storeKey, items)
	return admitted
}

func recordResponseCacheKnownUnavailableError() {
	respCache.mu.Lock()
	respCache.stats.KnownUnavailableErrors++
	respCache.mu.Unlock()
}

func readResponseContextBackend(ctx context.Context, runtimeCache cache.TokenCache, storeKey string, config responseCacheConfig) (cache.ResponseContextReadResult, error) {
	if bounded, ok := runtimeCache.(cache.BoundedResponseContextReader); ok {
		return bounded.GetResponseContextBounded(ctx, storeKey, responseContextWireLimit(config))
	}
	// Third-party shared caches retain compatibility through the original
	// TokenCache method. Only the official Redis implementation guarantees the
	// pre-deserialization bound provided by BoundedResponseContextReader.
	items, err := runtimeCache.GetResponseContext(ctx, storeKey)
	if err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
			return cache.ResponseContextReadResult{Status: cache.ResponseContextReadCorrupt}, nil
		}
		return cache.ResponseContextReadResult{}, err
	}
	if len(items) == 0 {
		return cache.ResponseContextReadResult{Status: cache.ResponseContextReadMiss}, nil
	}
	return cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: items}, nil
}

func responseContextWireLimit(config responseCacheConfig) int64 {
	return cache.ResponseContextWireLimit(config.reconstructMaxBytes, config.maxItems)
}

func responseContextLogicalBytes(items []json.RawMessage) int64 {
	var total int64
	for _, item := range items {
		total += int64(len(item))
	}
	return total
}

// respCacheCleanupLoop removes expired entries in the background
func respCacheCleanupLoop() {
	ticker := time.NewTicker(responseCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		cleanupResponseCacheExpired(time.Now())
	}
}

func cleanupResponseCacheExpired(now time.Time) {
	respCache.mu.Lock()
	for _, entry := range respCache.store {
		if !now.Before(entry.expiresAt) {
			respCache.removeEntryLocked(entry, responseCacheRemovalExpiration)
		}
	}
	for key, marker := range respCache.markers {
		if !now.Before(marker.expiresAt) {
			respCache.removeMarkerLocked(key)
		}
	}
	for owner, record := range respCache.chainOwners {
		if !now.Before(record.expiresAt) {
			removeResponseCacheChainOwnerLocked(owner)
		}
	}
	respCache.mu.Unlock()
}

// expandPreviousResponse checks whether the request has previous_response_id,
// and if so, and the cache hits (and belongs to the same owner), injects the historical conversation items at the head of input.
// Returns the processed body and the extracted previous_response_id (for the subsequent cache chain).
func expandPreviousResponse(codexBody []byte, owner string) ([]byte, string) {
	prevID := gjson.GetBytes(codexBody, "previous_response_id").String()
	if prevID == "" {
		return codexBody, ""
	}

	currentInput := gjson.GetBytes(codexBody, "input")

	// When the client already carries function_call and other chaining items, skip injection.
	// The cache only stores function_call-type items (see cacheCompletedResponse + isCodexToolCallContextType),
	// so injecting again would make the same call_id appear twice, and upstream rejects it with a 400 such as "duplicate call_id".
	// Still return prevID so cacheCompletedResponse can link this turn's response into the cache.
	if currentInput.IsArray() && inputHasToolCallContext(currentInput) {
		log.Printf("input already carries tool continuation items, skipping history injection for previous_response_id=%s", prevID)
		return codexBody, prevID
	}

	cached := getResponseCache(owner, prevID)
	if cached == nil {
		// Cache miss (first request / expired / another instance); cannot expand, continue as-is.
		// If input contains only function_call_output and no matching function_call can be found,
		// upstream usually returns a 400 "No tool call found for function call output",
		// so log here for diagnosis (not blocking; let the upstream error pass through to the client).
		if currentInput.IsArray() && inputHasFunctionCallOutput(currentInput) {
			log.Printf("cache miss and input contains function_call_output, previous_response_id=%s, upstream may return 400", prevID)
		}
		return codexBody, prevID
	}

	// Build new input: cached history items + current input items
	var merged []json.RawMessage
	merged = append(merged, cached...)
	if currentInput.IsArray() {
		currentInput.ForEach(func(_, v gjson.Result) bool {
			merged = append(merged, json.RawMessage(v.Raw))
			return true
		})
	}

	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		log.Printf("failed to expand previous_response_id: %v", err)
		return codexBody, prevID
	}

	codexBody, _ = sjson.SetRawBytes(codexBody, "input", mergedJSON)
	log.Printf("expanded previous_response_id=%s, injected %d history items", prevID, len(cached))
	return codexBody, prevID
}

// inputHasToolCallContext reports whether the input array already contains function_call-type continuation items,
// which would cause a call_id conflict if they also appeared in the cache.
func inputHasToolCallContext(input gjson.Result) bool {
	found := false
	input.ForEach(func(_, v gjson.Result) bool {
		if isCodexToolCallContextType(v.Get("type").String()) {
			found = true
			return false
		}
		return true
	})
	return found
}

// inputHasFunctionCallOutput reports whether the input array contains *_output items (upstream returns 400 when the paired function_call is missing).
func inputHasFunctionCallOutput(input gjson.Result) bool {
	found := false
	input.ForEach(func(_, v gjson.Result) bool {
		if isCodexToolCallOutputType(v.Get("type").String()) {
			found = true
			return false
		}
		return true
	})
	return found
}

// isCodexToolCallOutputType reports whether an item type is a tool call output item (*_call_output),
// in one-to-one correspondence with the call item set of isCodexToolCallContextType.
func isCodexToolCallOutputType(typ string) bool {
	switch typ {
	case "function_call_output",
		"tool_call_output",
		"local_shell_call_output",
		"shell_call_output",
		"apply_patch_call_output",
		"tool_search_call_output",
		"custom_tool_call_output",
		"mcp_tool_call_output":
		return true
	default:
		return false
	}
}

// cacheCompletedResponse extracts response.id and response.output from the response.completed event,
// and merges them with the current request's expanded input before storing in the owner-namespaced cache.
// Caches only when the response contains Codex tool calls that need call_id continuation, to avoid wasting memory on ordinary conversation.
func cacheCompletedResponse(owner string, expandedInputRaw []byte, completedData []byte) {
	cacheCompletedResponseWithOutputItems(owner, expandedInputRaw, completedData, nil)
}

// cacheCompletedResponseWithOutputItems, when response.completed.output carries no tool calls,
// uses the outputItems already collected from streaming response.output_item.done as a fallback.
func cacheCompletedResponseWithOutputItems(owner string, expandedInputRaw []byte, completedData []byte, outputItems []json.RawMessage) {
	respID := gjson.GetBytes(completedData, "response.id").String()
	if respID == "" {
		return
	}

	// Cache only when the response contains Codex tool calls that can be safely replayed (ordinary conversation does not need
	// previous_response_id expansion). image_generation_call / web_search_call
	// also end in *_call, but they do not belong to the call_id tool continuation system.
	output := gjson.GetBytes(completedData, "response.output")
	if !output.IsArray() && len(outputItems) == 0 {
		return
	}
	var replayableOutput []json.RawMessage
	output.ForEach(func(_, item gjson.Result) bool {
		if replayable, ok := replayableCachedOutputItem(item); ok {
			replayableOutput = append(replayableOutput, replayable)
		}
		return true
	})
	if len(replayableOutput) == 0 {
		for _, raw := range outputItems {
			if replayable, ok := replayableCachedOutputItem(gjson.ParseBytes(raw)); ok {
				replayableOutput = append(replayableOutput, replayable)
			}
		}
	}
	if len(replayableOutput) == 0 {
		return
	}
	// This HTTP tool-output path previously built a full snapshot before the
	// on_demand gate in setResponseCache. Unqualified owners need none of that
	// work. Native WS store:false has its own ingress fence.
	if skipResponseCacheWrite(owner) {
		return
	}

	var items []json.RawMessage

	// Add the expanded request input items. First remove, in pairs, the ordinary function_call/function_call_output whose arguments were truncated,
	// to avoid the cache prolonging poisoned history into the next turn.
	inputItems := gjson.ParseBytes(sanitizeResponseCacheInput(expandedInputRaw))
	if inputItems.IsArray() {
		inputItems.ForEach(func(_, v gjson.Result) bool {
			if item, ok := replayableCachedInputItem(v); ok {
				items = append(items, item)
			}
			return true
		})
	}

	// Add the tool context from the response output that truly needs continuation and has been verified; reasoning/message
	// and other server output items carry rs_/msg_ ids, and replaying them with store=false triggers item not found.
	items = append(items, replayableOutput...)

	if len(items) > 0 {
		setResponseCache(owner, respID, items)
	}
}

func sanitizeResponseCacheInput(raw []byte) []byte {
	var input []any
	if err := json.Unmarshal(raw, &input); err != nil {
		return raw
	}
	body := map[string]any{"input": input}
	normalizeResponsesToolCallArgumentTypes(body)
	if !sanitizeMalformedResponsesFunctionCalls(body) {
		return raw
	}
	sanitized, err := json.Marshal(body["input"])
	if err != nil {
		return raw
	}
	return sanitized
}

func replayableCachedInputItem(item gjson.Result) (json.RawMessage, bool) {
	if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").Exists() {
		return nil, false
	}
	raw, ok := normalizeReplayableCachedFunctionCall(json.RawMessage(item.Raw))
	if !ok {
		return nil, false
	}
	return stripResponseItemID(raw)
}

func replayableCachedOutputItem(item gjson.Result) (json.RawMessage, bool) {
	if !isCodexToolCallContextType(item.Get("type").String()) {
		return nil, false
	}
	raw, ok := normalizeReplayableCachedFunctionCall(json.RawMessage(item.Raw))
	if !ok {
		return nil, false
	}
	return stripResponseItemID(raw)
}

func normalizeReplayableCachedFunctionCall(raw json.RawMessage) (json.RawMessage, bool) {
	if gjson.GetBytes(raw, "type").String() != "function_call" {
		return raw, true
	}
	argumentValue := gjson.GetBytes(raw, "arguments")
	arguments := ""
	if argumentValue.Exists() && argumentValue.Type != gjson.Null {
		if argumentValue.Type == gjson.String {
			arguments = argumentValue.String()
		} else {
			arguments = argumentValue.Raw
		}
	}
	normalized, valid := normalizeOrdinaryFunctionCallArguments(arguments)
	if !valid {
		return nil, false
	}
	if argumentValue.Type == gjson.String && normalized == arguments {
		return raw, true
	}
	updated, err := sjson.SetBytes(raw, "arguments", normalized)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(updated), true
}

func stripResponseItemID(raw json.RawMessage) (json.RawMessage, bool) {
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return raw, true
	}
	if _, exists := item["id"]; !exists {
		return raw, true
	}
	delete(item, "id")
	stripped, err := json.Marshal(item)
	if err != nil {
		return nil, false
	}
	return stripped, true
}

func isCodexToolCallContextType(typ string) bool {
	switch typ {
	case "function_call",
		"tool_call",
		"local_shell_call",
		"shell_call",
		"apply_patch_call",
		"tool_search_call",
		"custom_tool_call",
		"mcp_tool_call":
		return true
	default:
		return false
	}
}
