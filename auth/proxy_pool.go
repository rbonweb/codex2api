package auth

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/security"
)

// ProxyHealthStatus is the health status of a proxy
type ProxyHealthStatus int

const (
	ProxyStatusHealthy   ProxyHealthStatus = iota // healthy
	ProxyStatusUnhealthy                          // unhealthy
	ProxyStatusIsolated                           // isolated
)

// ProxySelectionStrategy is the proxy selection strategy
type ProxySelectionStrategy int

const (
	StrategyRoundRobin       ProxySelectionStrategy = iota // round robin
	StrategyWeighted                                       // weighted selection
	StrategyLeastConnections                               // fewest connections
)

// ProxyEntry is a proxy entry
type ProxyEntry struct {
	URL                 string
	Healthy             bool
	LastCheck           time.Time
	Latency             time.Duration
	SuccessRate         float64
	Weight              int64
	ActiveConns         atomic.Int64
	TotalRequests       atomic.Int64
	FailedRequests      atomic.Int64
	Status              ProxyHealthStatus
	IsolatedAt          time.Time
	ConsecutiveFailures int

	mu sync.RWMutex
}

// ProxyStats is the proxy statistics
type ProxyStats struct {
	URL                 string        `json:"url"`
	Healthy             bool          `json:"healthy"`
	LastCheck           time.Time     `json:"last_check"`
	Latency             time.Duration `json:"latency"`
	LatencyMs           float64       `json:"latency_ms"`
	SuccessRate         float64       `json:"success_rate"`
	Weight              int64         `json:"weight"`
	ActiveConns         int64         `json:"active_conns"`
	TotalRequests       int64         `json:"total_requests"`
	FailedRequests      int64         `json:"failed_requests"`
	Status              string        `json:"status"`
	ConsecutiveFailures int           `json:"consecutive_failures"`
}

// HealthCheckResult is the result of a health check
type HealthCheckResult struct {
	URL       string
	Healthy   bool
	Latency   time.Duration
	Error     error
	Timestamp time.Time
}

// ProxyPool is the proxy pool manager
type ProxyPool struct {
	proxies []*ProxyEntry
	stats   map[string]*ProxyStats
	healthy []*ProxyEntry

	mu                 sync.RWMutex
	strategy           ProxySelectionStrategy
	roundRobinIdx      atomic.Uint64
	checkInterval      time.Duration
	timeout            time.Duration
	isolationThreshold int
	isolationDuration  time.Duration
	healthCheckURL     string

	// Runtime state
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// Callback functions
	onHealthCheck func(result *HealthCheckResult)
	onIsolation   func(entry *ProxyEntry)
	onRecovery    func(entry *ProxyEntry)
}

// ProxyPoolConfig is the proxy pool configuration
type ProxyPoolConfig struct {
	Strategy           ProxySelectionStrategy
	CheckInterval      time.Duration
	Timeout            time.Duration
	IsolationThreshold int           // consecutive failure count threshold
	IsolationDuration  time.Duration // isolation duration
	HealthCheckURL     string
}

// DefaultProxyPoolConfig returns the default configuration
func DefaultProxyPoolConfig() *ProxyPoolConfig {
	return &ProxyPoolConfig{
		Strategy:           StrategyRoundRobin,
		CheckInterval:      30 * time.Second,
		Timeout:            10 * time.Second,
		IsolationThreshold: 3,
		IsolationDuration:  5 * time.Minute,
		HealthCheckURL:     "http://www.google.com/generate_204",
	}
}

// NewProxyPool creates a new proxy pool
func NewProxyPool(config *ProxyPoolConfig) *ProxyPool {
	if config == nil {
		config = DefaultProxyPoolConfig()
	}

	return &ProxyPool{
		proxies:            make([]*ProxyEntry, 0),
		stats:              make(map[string]*ProxyStats),
		healthy:            make([]*ProxyEntry, 0),
		strategy:           config.Strategy,
		checkInterval:      config.CheckInterval,
		timeout:            config.Timeout,
		isolationThreshold: config.IsolationThreshold,
		isolationDuration:  config.IsolationDuration,
		healthCheckURL:     config.HealthCheckURL,
		stopCh:             make(chan struct{}),
	}
}

// AddProxy adds a proxy to the pool
func (p *ProxyPool) AddProxy(url string, weight int64) {
	if url == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Check whether it already exists
	for _, entry := range p.proxies {
		if entry.URL == url {
			return
		}
	}

	entry := &ProxyEntry{
		URL:         url,
		Healthy:     true,
		LastCheck:   time.Time{},
		Latency:     0,
		SuccessRate: 1.0,
		Weight:      weight,
		Status:      ProxyStatusHealthy,
	}

	p.proxies = append(p.proxies, entry)
	p.stats[url] = &ProxyStats{
		URL:     url,
		Healthy: true,
		Weight:  weight,
		Status:  "healthy",
	}
	p.rebuildHealthyList()
}

// RemoveProxy removes a proxy from the pool
func (p *ProxyPool) RemoveProxy(url string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, entry := range p.proxies {
		if entry.URL == url {
			p.proxies = append(p.proxies[:i], p.proxies[i+1:]...)
			delete(p.stats, url)
			p.rebuildHealthyList()
			return
		}
	}
}

// UpdateProxyWeight updates the proxy weight
func (p *ProxyPool) UpdateProxyWeight(url string, weight int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, entry := range p.proxies {
		if entry.URL == url {
			entry.Weight = weight
			if stats, ok := p.stats[url]; ok {
				stats.Weight = weight
			}
			return
		}
	}
}

// rebuildHealthyList rebuilds the healthy proxy list
func (p *ProxyPool) rebuildHealthyList() {
	healthy := make([]*ProxyEntry, 0, len(p.proxies))
	for _, entry := range p.proxies {
		if entry.Healthy && entry.Status == ProxyStatusHealthy {
			healthy = append(healthy, entry)
		}
	}
	p.healthy = healthy
}

// Select picks a proxy
func (p *ProxyPool) Select() *ProxyEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.healthy) == 0 {
		return nil
	}

	return p.selectByStrategy(p.strategy)
}

// SelectWithStrategy picks a proxy using the given strategy (does not change the global strategy)
func (p *ProxyPool) SelectWithStrategy(strategy ProxySelectionStrategy) *ProxyEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if len(p.healthy) == 0 {
		return nil
	}

	return p.selectByStrategy(strategy)
}

// selectByStrategy picks a proxy according to the strategy
func (p *ProxyPool) selectByStrategy(strategy ProxySelectionStrategy) *ProxyEntry {
	switch strategy {
	case StrategyWeighted:
		return p.selectWeighted()
	case StrategyLeastConnections:
		return p.selectLeastConnections()
	default: // StrategyRoundRobin
		return p.selectRoundRobin()
	}
}

// selectRoundRobin picks round robin
func (p *ProxyPool) selectRoundRobin() *ProxyEntry {
	if len(p.healthy) == 0 {
		return nil
	}
	idx := p.roundRobinIdx.Add(1) % uint64(len(p.healthy))
	return p.healthy[idx]
}

// selectWeighted picks by weighted random selection
func (p *ProxyPool) selectWeighted() *ProxyEntry {
	if len(p.healthy) == 0 {
		return nil
	}

	// Compute the total weight
	var totalWeight int64
	for _, entry := range p.healthy {
		if entry.Weight <= 0 {
			totalWeight += 1
		} else {
			totalWeight += entry.Weight
		}
	}

	if totalWeight <= 0 {
		return p.healthy[0]
	}

	// Weighted random selection
	r := rand.Int63n(totalWeight)
	var currentWeight int64
	for _, entry := range p.healthy {
		w := entry.Weight
		if w <= 0 {
			w = 1
		}
		currentWeight += w
		if r < currentWeight {
			return entry
		}
	}

	return p.healthy[len(p.healthy)-1]
}

// selectLeastConnections picks the proxy with the fewest connections
func (p *ProxyPool) selectLeastConnections() *ProxyEntry {
	if len(p.healthy) == 0 {
		return nil
	}

	var best *ProxyEntry
	var minConns int64 = math.MaxInt64

	for _, entry := range p.healthy {
		conns := entry.ActiveConns.Load()
		// Take the success rate into account as an adjustment factor, which must be read outside the lock
		entry.mu.RLock()
		successRate := entry.SuccessRate
		entry.mu.RUnlock()
		if successRate <= 0 {
			successRate = 0.1
		}
		// Adjusted connection count = actual connection count / success rate
		adjustedConns := int64(float64(conns) / successRate)

		if adjustedConns < minConns {
			minConns = adjustedConns
			best = entry
		}
	}

	return best
}

// MarkSuccess marks a proxy as successful
func (p *ProxyPool) MarkSuccess(url string) {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return
	}

	entry.mu.Lock()
	entry.TotalRequests.Add(1)
	entry.ConsecutiveFailures = 0

	// Update the success rate (exponential moving average)
	if entry.TotalRequests.Load() == 1 {
		entry.SuccessRate = 1.0
	} else {
		entry.SuccessRate = entry.SuccessRate*0.9 + 0.1
	}

	// If it was isolated before, try to recover it
	if entry.Status == ProxyStatusIsolated {
		entry.Status = ProxyStatusHealthy
		entry.IsolatedAt = time.Time{}
		if p.onRecovery != nil {
			p.onRecovery(entry)
		}
	}

	// Update stats under the lock
	if stats, ok := p.stats[url]; ok {
		stats.TotalRequests = entry.TotalRequests.Load()
		stats.FailedRequests = entry.FailedRequests.Load()
		stats.SuccessRate = entry.SuccessRate
		stats.Status = p.statusString(entry.Status)
		stats.ConsecutiveFailures = entry.ConsecutiveFailures
	}

	entry.mu.Unlock()

	p.rebuildHealthyListIfNeeded()
}

// MarkFailure marks a proxy as failed
func (p *ProxyPool) MarkFailure(url string) {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return
	}

	entry.mu.Lock()
	entry.TotalRequests.Add(1)
	entry.FailedRequests.Add(1)
	entry.ConsecutiveFailures++

	// Update the success rate
	if entry.TotalRequests.Load() == 1 {
		entry.SuccessRate = 0.0
	} else {
		entry.SuccessRate = entry.SuccessRate * 0.9
	}

	// Check whether isolation is needed
	if entry.Status != ProxyStatusIsolated && entry.ConsecutiveFailures >= p.isolationThreshold {
		entry.Status = ProxyStatusIsolated
		entry.IsolatedAt = time.Now()
		entry.Healthy = false
		if p.onIsolation != nil {
			p.onIsolation(entry)
		}
	}

	// Update stats under the lock
	if stats, ok := p.stats[url]; ok {
		stats.TotalRequests = entry.TotalRequests.Load()
		stats.FailedRequests = entry.FailedRequests.Load()
		stats.SuccessRate = entry.SuccessRate
		stats.Status = p.statusString(entry.Status)
		stats.ConsecutiveFailures = entry.ConsecutiveFailures
	}

	entry.mu.Unlock()

	p.rebuildHealthyListIfNeeded()
}

// findEntry looks up a proxy entry
func (p *ProxyPool) findEntry(url string) (*ProxyEntry, bool) {
	for _, entry := range p.proxies {
		if entry.URL == url {
			return entry, true
		}
	}
	return nil, false
}

// rebuildHealthyListIfNeeded rebuilds the healthy list if needed
func (p *ProxyPool) rebuildHealthyListIfNeeded() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rebuildHealthyList()
}

// AcquireConnection acquires a connection (increments the active connection count)
func (p *ProxyPool) AcquireConnection(url string) bool {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return false
	}

	if entry.Status != ProxyStatusHealthy {
		return false
	}

	entry.ActiveConns.Add(1)
	return true
}

// ReleaseConnection releases a connection (decrements the active connection count)
func (p *ProxyPool) ReleaseConnection(url string) {
	p.mu.RLock()
	entry, exists := p.findEntry(url)
	p.mu.RUnlock()

	if !exists || entry == nil {
		return
	}

	// Prevent the counter from going negative
	if entry.ActiveConns.Load() > 0 {
		entry.ActiveConns.Add(-1)
	}
}

// HealthCheck runs a health check
func (p *ProxyPool) HealthCheck(ctx context.Context) {
	p.mu.RLock()
	proxies := make([]*ProxyEntry, len(p.proxies))
	copy(proxies, p.proxies)
	p.mu.RUnlock()

	var wg sync.WaitGroup
	results := make(chan *HealthCheckResult, len(proxies))

	for _, entry := range proxies {
		wg.Add(1)
		go func(e *ProxyEntry) {
			defer wg.Done()
			result := p.checkProxy(ctx, e)
			results <- result
		}(entry)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	// Handle the result
	for result := range results {
		p.processHealthCheckResult(result)
	}
}

// checkProxy checks a single proxy
func (p *ProxyPool) checkProxy(ctx context.Context, entry *ProxyEntry) *HealthCheckResult {
	start := time.Now()

	// Create a context with a timeout
	checkCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// Build the request
	req, err := http.NewRequestWithContext(checkCtx, "HEAD", p.healthCheckURL, nil)
	if err != nil {
		return &HealthCheckResult{
			URL:       entry.URL,
			Healthy:   false,
			Latency:   0,
			Error:     err,
			Timestamp: time.Now(),
		}
	}

	// Create a transport with the proxy
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		DisableKeepAlives: true, // disable keep-alive to avoid connection leaks
		MaxIdleConns:      0,
		IdleConnTimeout:   0,
	}

	// Configure the proxy
	if err := ConfigureTransportProxy(transport, entry.URL, &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}); err != nil {
		return &HealthCheckResult{
			URL:       entry.URL,
			Healthy:   false,
			Latency:   0,
			Error:     err,
			Timestamp: time.Now(),
		}
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   p.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	latency := time.Since(start)

	result := &HealthCheckResult{
		URL:       entry.URL,
		Latency:   latency,
		Timestamp: time.Now(),
	}

	if err != nil {
		result.Healthy = false
		result.Error = err
	} else {
		resp.Body.Close()
		// Only 2xx and 3xx status codes are considered healthy
		result.Healthy = resp.StatusCode >= 200 && resp.StatusCode < 400
		if !result.Healthy {
			result.Error = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}

	return result
}

// processHealthCheckResult handles the health check result
func (p *ProxyPool) processHealthCheckResult(result *HealthCheckResult) {
	p.mu.Lock()
	entry, exists := p.findEntry(result.URL)
	p.mu.Unlock()

	if !exists || entry == nil {
		return
	}

	entry.mu.Lock()
	entry.LastCheck = result.Timestamp
	entry.Latency = result.Latency

	if result.Healthy {
		entry.Healthy = true
		entry.ConsecutiveFailures = 0

		// If isolated and the isolation time has passed, recover
		if entry.Status == ProxyStatusIsolated {
			if time.Since(entry.IsolatedAt) >= p.isolationDuration {
				entry.Status = ProxyStatusHealthy
				entry.IsolatedAt = time.Time{}
				if p.onRecovery != nil {
					p.onRecovery(entry)
				}
			}
		}
	} else {
		entry.Healthy = false
		entry.Status = ProxyStatusUnhealthy
		entry.ConsecutiveFailures++

		// Check whether isolation is needed
		if entry.Status != ProxyStatusIsolated && entry.ConsecutiveFailures >= p.isolationThreshold {
			entry.Status = ProxyStatusIsolated
			entry.IsolatedAt = time.Now()
			if p.onIsolation != nil {
				p.onIsolation(entry)
			}
		}
	}
	entry.mu.Unlock()

	// Update statistics
	if stats, ok := p.stats[result.URL]; ok {
		stats.LastCheck = result.Timestamp
		stats.Latency = result.Latency
		stats.LatencyMs = float64(result.Latency.Milliseconds())
		stats.Healthy = result.Healthy
		stats.Status = p.statusString(entry.Status)
		stats.ConsecutiveFailures = entry.ConsecutiveFailures
	}

	p.rebuildHealthyListIfNeeded()

	// Callback
	if p.onHealthCheck != nil {
		p.onHealthCheck(result)
	}
}

// statusString returns the status string
func (p *ProxyPool) statusString(status ProxyHealthStatus) string {
	switch status {
	case ProxyStatusHealthy:
		return "healthy"
	case ProxyStatusUnhealthy:
		return "unhealthy"
	case ProxyStatusIsolated:
		return "isolated"
	default:
		return "unknown"
	}
}

// StartHealthCheck starts periodic health checks
func (p *ProxyPool) StartHealthCheck() {
	// Validate checkInterval to avoid a panic from a non-positive value
	checkInterval := p.checkInterval
	if checkInterval <= 0 {
		checkInterval = 30 * time.Second
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()

		// Run one check immediately
		p.HealthCheck(context.Background())

		for {
			select {
			case <-ticker.C:
				p.HealthCheck(context.Background())
			case <-p.stopCh:
				return
			}
		}
	}()
}

// Stop stops the proxy pool
func (p *ProxyPool) Stop() {
	p.stopOnce.Do(func() {
		close(p.stopCh)
		p.wg.Wait()
	})
}

// GetStats returns the proxy statistics
func (p *ProxyPool) GetStats() map[string]*ProxyStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make(map[string]*ProxyStats)
	for url, stats := range p.stats {
		// Copy the statistics
		entry, _ := p.findEntry(url)
		if entry != nil {
			result[url] = &ProxyStats{
				URL:                 stats.URL,
				Healthy:             entry.Healthy,
				LastCheck:           entry.LastCheck,
				Latency:             entry.Latency,
				LatencyMs:           float64(entry.Latency.Milliseconds()),
				SuccessRate:         entry.SuccessRate,
				Weight:              entry.Weight,
				ActiveConns:         entry.ActiveConns.Load(),
				TotalRequests:       entry.TotalRequests.Load(),
				FailedRequests:      entry.FailedRequests.Load(),
				Status:              p.statusString(entry.Status),
				ConsecutiveFailures: entry.ConsecutiveFailures,
			}
		} else {
			result[url] = stats
		}
	}
	return result
}

// GetPoolStatus returns the proxy pool status
func (p *ProxyPool) GetPoolStatus() *ProxyPoolStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()

	total := len(p.proxies)
	healthy := len(p.healthy)
	isolated := 0
	unhealthy := 0

	for _, entry := range p.proxies {
		switch entry.Status {
		case ProxyStatusIsolated:
			isolated++
		case ProxyStatusUnhealthy:
			unhealthy++
		}
	}

	return &ProxyPoolStatus{
		Total:         total,
		Healthy:       healthy,
		Unhealthy:     total - healthy - isolated,
		Isolated:      isolated,
		Strategy:      p.strategy.String(),
		CheckInterval: p.checkInterval,
		LastUpdated:   time.Now(),
	}
}

// ProxyPoolStatus is the proxy pool status
type ProxyPoolStatus struct {
	Total         int           `json:"total"`
	Healthy       int           `json:"healthy"`
	Unhealthy     int           `json:"unhealthy"`
	Isolated      int           `json:"isolated"`
	Strategy      string        `json:"strategy"`
	CheckInterval time.Duration `json:"check_interval"`
	LastUpdated   time.Time     `json:"last_updated"`
}

// SetStrategy sets the selection strategy
func (p *ProxyPool) SetStrategy(strategy ProxySelectionStrategy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strategy = strategy
}

// GetStrategy returns the current selection strategy
func (p *ProxyPool) GetStrategy() ProxySelectionStrategy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategy
}

// SetOnHealthCheck sets the health check callback
func (p *ProxyPool) SetOnHealthCheck(fn func(result *HealthCheckResult)) {
	p.onHealthCheck = fn
}

// SetOnIsolation sets the isolation callback
func (p *ProxyPool) SetOnIsolation(fn func(entry *ProxyEntry)) {
	p.onIsolation = fn
}

// SetOnRecovery sets the recovery callback
func (p *ProxyPool) SetOnRecovery(fn func(entry *ProxyEntry)) {
	p.onRecovery = fn
}

// String returns the string form of the strategy
func (s ProxySelectionStrategy) String() string {
	switch s {
	case StrategyRoundRobin:
		return "round_robin"
	case StrategyWeighted:
		return "weighted"
	case StrategyLeastConnections:
		return "least_connections"
	default:
		return "unknown"
	}
}

// ParseStrategy parses a strategy string
func ParseStrategy(s string) ProxySelectionStrategy {
	switch s {
	case "weighted":
		return StrategyWeighted
	case "least_connections":
		return StrategyLeastConnections
	default:
		return StrategyRoundRobin
	}
}

// LoadProxiesFromURLs loads proxies from a list of URLs
func (p *ProxyPool) LoadProxiesFromURLs(urls []string, defaultWeight int64) {
	for _, url := range urls {
		if url != "" {
			p.AddProxy(url, defaultWeight)
		}
	}
}

// GetHealthyProxies returns the list of healthy proxies
func (p *ProxyPool) GetHealthyProxies() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]string, 0, len(p.healthy))
	for _, entry := range p.healthy {
		result = append(result, entry.URL)
	}
	return result
}

// GetAllProxies returns all proxy URLs
func (p *ProxyPool) GetAllProxies() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]string, 0, len(p.proxies))
	for _, entry := range p.proxies {
		result = append(result, entry.URL)
	}
	return result
}

// IsHealthy checks whether the proxy is healthy
func (p *ProxyPool) IsHealthy(url string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, entry := range p.healthy {
		if entry.URL == url {
			return true
		}
	}
	return false
}

// Size returns the size of the proxy pool
func (p *ProxyPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

// HealthySize returns the number of healthy proxies
func (p *ProxyPool) HealthySize() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.healthy)
}

// IsEmpty checks whether the proxy pool is empty
func (p *ProxyPool) IsEmpty() bool {
	return p.Size() == 0
}

// HasHealthyProxy checks whether there is a healthy proxy
func (p *ProxyPool) HasHealthyProxy() bool {
	return p.HealthySize() > 0
}

// ForceHealthCheck forces a health check to run immediately
func (p *ProxyPool) ForceHealthCheck(ctx context.Context) {
	p.HealthCheck(ctx)
}

// RecoverIsolatedProxies tries to recover proxies whose isolation period has passed
func (p *ProxyPool) RecoverIsolatedProxies() {
	p.mu.Lock()

	now := time.Now()
	// Collect the proxies that need to be recovered
	var recoveredEntries []*ProxyEntry
	for _, entry := range p.proxies {
		if entry.Status == ProxyStatusIsolated && !entry.IsolatedAt.IsZero() {
			if now.Sub(entry.IsolatedAt) >= p.isolationDuration {
				entry.Status = ProxyStatusHealthy
				entry.IsolatedAt = time.Time{}
				entry.Healthy = true
				entry.ConsecutiveFailures = 0
				recoveredEntries = append(recoveredEntries, entry)
			}
		}
	}
	p.rebuildHealthyList()
	p.mu.Unlock()

	// Run the callbacks outside the lock to avoid deadlock
	for _, entry := range recoveredEntries {
		log.Printf("[ProxyPool] proxy recovered: %s", security.MaskURLCredentials(entry.URL))
		if p.onRecovery != nil {
			p.onRecovery(entry)
		}
	}
}
