package auth

import (
	"context"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security"
)

// EnhancedProxyPool is the enhanced proxy pool manager.
type EnhancedProxyPool struct {
	pool        *ProxyPool
	db          *database.DB
	enabled     atomic.Bool
	initialized atomic.Bool
}

// NewEnhancedProxyPool creates an enhanced proxy pool.
func NewEnhancedProxyPool(db *database.DB, config *ProxyPoolConfig) *EnhancedProxyPool {
	return &EnhancedProxyPool{
		pool: NewProxyPool(config),
		db:   db,
	}
}

// Init initializes the proxy pool.
func (e *EnhancedProxyPool) Init(ctx context.Context) error {
	if e.db == nil {
		return nil
	}

	// Load proxies from the database.
	proxies, err := e.db.ListEnabledProxies(ctx)
	if err != nil {
		return err
	}

	// Add to the proxy pool.
	for _, p := range proxies {
		// Use latency as the base of the weight (lower latency, higher weight).
		weight := int64(100)
		if p.TestLatencyMs > 0 {
			// Lower latency, higher weight.
			weight = int64(10000 / (p.TestLatencyMs + 1))
			if weight < 1 {
				weight = 1
			}
		}
		e.pool.AddProxy(p.URL, weight)
	}

	// Set callbacks.
	e.pool.SetOnIsolation(func(entry *ProxyEntry) {
		log.Printf("[ProxyPool] proxy isolated: %s (%d consecutive failures)", security.MaskURLCredentials(entry.URL), entry.ConsecutiveFailures)
	})

	e.pool.SetOnRecovery(func(entry *ProxyEntry) {
		log.Printf("[ProxyPool] proxy recovered: %s", security.MaskURLCredentials(entry.URL))
	})

	e.pool.SetOnHealthCheck(func(result *HealthCheckResult) {
		if !result.Healthy && result.Error != nil {
			log.Printf("[ProxyPool] proxy health check failed: %s, error: %v", security.MaskURLCredentials(result.URL), result.Error)
		}
	})

	// Start health checks.
	e.pool.StartHealthCheck()
	e.enabled.Store(true)
	e.initialized.Store(true)

	log.Printf("[ProxyPool] enhanced proxy pool initialized, %d proxies total", len(proxies))
	return nil
}

// Enabled returns whether the proxy pool is enabled.
func (e *EnhancedProxyPool) Enabled() bool {
	return e.enabled.Load()
}

// SetEnabled sets whether the proxy pool is enabled.
func (e *EnhancedProxyPool) SetEnabled(enabled bool) {
	e.enabled.Store(enabled)
}

// NextProxy returns the next proxy (kept for compatibility with the original interface).
func (e *EnhancedProxyPool) NextProxy() string {
	if !e.enabled.Load() || !e.initialized.Load() {
		return ""
	}

	entry := e.pool.Select()
	if entry == nil {
		return ""
	}
	return entry.URL
}

// SelectWithStrategy selects a proxy with the given strategy.
func (e *EnhancedProxyPool) SelectWithStrategy(strategy ProxySelectionStrategy) *ProxyEntry {
	if !e.enabled.Load() || !e.initialized.Load() {
		return nil
	}

	return e.pool.SelectWithStrategy(strategy)
}

// MarkProxySuccess marks the proxy as successful.
func (e *EnhancedProxyPool) MarkProxySuccess(url string) {
	if !e.initialized.Load() {
		return
	}
	e.pool.MarkSuccess(url)
}

// MarkProxyFailure marks the proxy as failed.
func (e *EnhancedProxyPool) MarkProxyFailure(url string) {
	if !e.initialized.Load() {
		return
	}
	e.pool.MarkFailure(url)
}

// AcquireConnection acquires a proxy connection.
func (e *EnhancedProxyPool) AcquireConnection(url string) bool {
	if !e.initialized.Load() {
		return false
	}
	return e.pool.AcquireConnection(url)
}

// ReleaseConnection releases a proxy connection.
func (e *EnhancedProxyPool) ReleaseConnection(url string) {
	if !e.initialized.Load() {
		return
	}
	e.pool.ReleaseConnection(url)
}

// ReloadFromDB reloads proxies from the database.
func (e *EnhancedProxyPool) ReloadFromDB(ctx context.Context) error {
	if e.db == nil {
		return nil
	}

	proxies, err := e.db.ListEnabledProxies(ctx)
	if err != nil {
		return err
	}

	// Get all current proxies.
	currentProxies := e.pool.GetAllProxies()
	currentMap := make(map[string]bool)
	for _, url := range currentProxies {
		currentMap[url] = true
	}

	// Add new proxies.
	for _, p := range proxies {
		if !currentMap[p.URL] {
			weight := int64(100)
			if p.TestLatencyMs > 0 {
				weight = int64(10000 / (p.TestLatencyMs + 1))
				if weight < 1 {
					weight = 1
				}
			}
			e.pool.AddProxy(p.URL, weight)
			log.Printf("[ProxyPool] proxy added: %s", security.MaskURLCredentials(p.URL))
		}
	}

	// Remove disabled proxies.
	newMap := make(map[string]bool)
	for _, p := range proxies {
		newMap[p.URL] = true
	}
	for _, url := range currentProxies {
		if !newMap[url] {
			e.pool.RemoveProxy(url)
			log.Printf("[ProxyPool] proxy removed: %s", security.MaskURLCredentials(url))
		}
	}

	return nil
}

// GetStats returns proxy statistics.
func (e *EnhancedProxyPool) GetStats() map[string]*ProxyStats {
	if !e.initialized.Load() {
		return make(map[string]*ProxyStats)
	}
	return e.pool.GetStats()
}

// GetPoolStatus returns the proxy pool status.
func (e *EnhancedProxyPool) GetPoolStatus() *ProxyPoolStatus {
	if !e.initialized.Load() {
		return &ProxyPoolStatus{
			Total:    0,
			Healthy:  0,
			Strategy: StrategyRoundRobin.String(),
		}
	}
	return e.pool.GetPoolStatus()
}

// SetStrategy sets the selection strategy.
func (e *EnhancedProxyPool) SetStrategy(strategy ProxySelectionStrategy) {
	if !e.initialized.Load() {
		return
	}
	e.pool.SetStrategy(strategy)
}

// ForceHealthCheck forces a health check.
func (e *EnhancedProxyPool) ForceHealthCheck(ctx context.Context) {
	if !e.initialized.Load() {
		return
	}
	e.pool.ForceHealthCheck(ctx)
}

// Stop stops the proxy pool.
func (e *EnhancedProxyPool) Stop() {
	if !e.initialized.Load() {
		return
	}
	e.pool.Stop()
	e.enabled.Store(false)
}

// GetHealthyProxies returns the list of healthy proxies.
func (e *EnhancedProxyPool) GetHealthyProxies() []string {
	if !e.initialized.Load() {
		return []string{}
	}
	return e.pool.GetHealthyProxies()
}

// HasHealthyProxy checks whether a healthy proxy exists.
func (e *EnhancedProxyPool) HasHealthyProxy() bool {
	if !e.initialized.Load() {
		return false
	}
	return e.pool.HasHealthyProxy()
}

// UpdateProxyStats writes proxy statistics to the database.
func (e *EnhancedProxyPool) UpdateProxyStats(ctx context.Context) error {
	if e.db == nil || !e.initialized.Load() {
		return nil
	}

	stats := e.pool.GetStats()
	for url, stat := range stats {
		// This could be extended with a database model to store more detailed statistics.
		_ = url
		_ = stat
		// TODO: implement the database update logic.
	}
	return nil
}

// SetProxyWeight sets the weight of a proxy.
func (e *EnhancedProxyPool) SetProxyWeight(url string, weight int64) {
	if !e.initialized.Load() {
		return
	}
	e.pool.UpdateProxyWeight(url, weight)
}

// StoreProxyPoolIntegration integrates Store with the proxy pool.
type StoreProxyPoolIntegration struct {
	store        *Store
	enhancedPool *EnhancedProxyPool
	useEnhanced  atomic.Bool
}

// NewStoreProxyPoolIntegration creates a Store-to-proxy-pool integration.
func NewStoreProxyPoolIntegration(store *Store, db *database.DB, settings *database.SystemSettings) *StoreProxyPoolIntegration {
	integration := &StoreProxyPoolIntegration{
		store: store,
	}

	// If the proxy pool is enabled, create the enhanced proxy pool.
	if settings != nil && settings.ProxyPoolEnabled {
		config := &ProxyPoolConfig{
			Strategy:           ParseStrategy(getEnv("PROXY_POOL_STRATEGY", "round_robin")),
			CheckInterval:      parseDuration(getEnv("PROXY_POOL_CHECK_INTERVAL", "30s"), 30*time.Second),
			Timeout:            parseDuration(getEnv("PROXY_POOL_TIMEOUT", "10s"), 10*time.Second),
			IsolationThreshold: parseInt(getEnv("PROXY_POOL_ISOLATION_THRESHOLD", "3"), 3),
			IsolationDuration:  parseDuration(getEnv("PROXY_POOL_ISOLATION_DURATION", "5m"), 5*time.Minute),
			HealthCheckURL:     getEnv("PROXY_POOL_HEALTH_CHECK_URL", "http://www.google.com/generate_204"),
		}

		integration.enhancedPool = NewEnhancedProxyPool(db, config)
		integration.useEnhanced.Store(true)
	}

	return integration
}

// Init initializes.
func (s *StoreProxyPoolIntegration) Init(ctx context.Context) error {
	if s.enhancedPool != nil {
		return s.enhancedPool.Init(ctx)
	}
	return nil
}

// NextProxy returns the next proxy.
func (s *StoreProxyPoolIntegration) NextProxy() string {
	if s.useEnhanced.Load() && s.enhancedPool != nil && s.enhancedPool.Enabled() {
		return s.enhancedPool.NextProxy()
	}
	// Fall back to the original implementation.
	if s.store != nil {
		return s.store.NextProxy()
	}
	return ""
}

// MarkProxySuccess marks the proxy as successful.
func (s *StoreProxyPoolIntegration) MarkProxySuccess(url string) {
	if s.useEnhanced.Load() && s.enhancedPool != nil {
		s.enhancedPool.MarkProxySuccess(url)
	}
}

// MarkProxyFailure marks the proxy as failed.
func (s *StoreProxyPoolIntegration) MarkProxyFailure(url string) {
	if s.useEnhanced.Load() && s.enhancedPool != nil {
		s.enhancedPool.MarkProxyFailure(url)
	}
}

// GetEnhancedPool returns the enhanced proxy pool.
func (s *StoreProxyPoolIntegration) GetEnhancedPool() *EnhancedProxyPool {
	return s.enhancedPool
}

// UseEnhanced reports whether the enhanced proxy pool is used.
func (s *StoreProxyPoolIntegration) UseEnhanced() bool {
	return s.useEnhanced.Load()
}

// SetUseEnhanced sets whether the enhanced proxy pool is used.
func (s *StoreProxyPoolIntegration) SetUseEnhanced(use bool) {
	s.useEnhanced.Store(use)
}

// Stop stops.
func (s *StoreProxyPoolIntegration) Stop() {
	if s.enhancedPool != nil {
		s.enhancedPool.Stop()
	}
}

// Helper functions.
func getEnv(key, defaultValue string) string {
	if value := getEnvFunc(key); value != "" {
		return value
	}
	return defaultValue
}

// For test injection.
var getEnvFunc = os.Getenv

func parseDuration(s string, defaultValue time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return defaultValue
	}
	return d
}

func parseInt(s string, defaultValue int) int {
	var result int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			result = result*10 + int(c-'0')
		} else {
			break
		}
	}
	if result == 0 {
		return defaultValue
	}
	return result
}
