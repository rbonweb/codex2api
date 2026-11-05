package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	// Windows and slim environments without zoneinfo have no IANA time zone database, so the embedded fallback lets TZ=Asia/Shanghai
	// names like this still resolve (the system zoneinfo is preferred when present). issue #498.
	_ "time/tzdata"

	"github.com/codex2api/admin"
	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/internal/version"
	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/wsrelay"
	"github.com/codex2api/security"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

//go:embed frontend/dist/*
var frontendFS embed.FS

func migrateOnlyEnabled() bool {
	value := strings.TrimSpace(os.Getenv("CODEX_MIGRATE_ONLY"))
	return value == "1" || strings.EqualFold(value, "true")
}

func configureCodexIdleUsageObserver(store *auth.Store) {
	wsrelay.SetIdleMetadataObserver(func(account *auth.Account, eventType string, payload []byte) {
		proxy.ApplyCodexIdleUsageFrame(store, account, eventType, payload)
	})
}

// main loads the config, initializes storage and routing, and starts the Codex2API HTTP service.
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("Codex2API v2 starting...")

	// 1. Load the config (.env)
	cfg, err := config.Load(".env")
	if err != nil {
		log.Fatalf("failed to load core environment config (check the .env file): %v", err)
	}
	proxy.ConfigureDownstreamKeepaliveFromEnv()
	log.Printf("physical layer config loaded successfully: port=%d, database=%s, cache=%s, tz=%s", cfg.Port, cfg.Database.Label(), cfg.Cache.Label(), time.Local)

	// 2. Initialize the database
	db, err := database.New(cfg.Database.Driver, cfg.Database.DSN(), cfg.Database.Schema)
	if err != nil {
		log.Fatalf("database initialization failed: %v", err)
	}
	defer db.Close()
	if migrateOnlyEnabled() {
		log.Println("database migration complete, CODEX_MIGRATE_ONLY is enabled, process exiting")
		return
	}
	switch cfg.Database.Driver {
	case "sqlite":
		log.Printf("%s connected successfully: %s", cfg.Database.Label(), cfg.Database.Path)
	default:
		if cfg.Database.Schema != "" {
			log.Printf("%s connected successfully: %s:%d/%s (schema=%s)", cfg.Database.Label(), cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName, cfg.Database.Schema)
		} else {
			log.Printf("%s connected successfully: %s:%d/%s", cfg.Database.Label(), cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName)
		}
	}

	// 3. Read the runtime system settings (must run before cache initialization, to get the connection pool size)
	sysCtx, sysCancel := context.WithTimeout(context.Background(), 5*time.Second)
	settings, err := db.GetSystemSettings(sysCtx)
	sysCancel()
	recommendedPromptFilter := promptfilter.RecommendedConfig()

	if err == nil && settings == nil {
		// First run: save the initial security settings to the database
		log.Printf("first run, initializing default system settings...")
		settings = &database.SystemSettings{
			SiteName:                          database.DefaultSiteName,
			MaxConcurrency:                    2,
			CodexTelemetryEnabled:             false, // experimental: simulated telemetry is not sent out by default, the deployer must enable it explicitly
			GlobalRPM:                         0,
			TestModel:                         auth.DefaultTestModel,
			TestContent:                       auth.DefaultTestContent,
			TestConcurrency:                   50,
			MaxRateLimitRetries:               1,
			BackgroundRefreshIntervalMinutes:  2,
			UsageProbeMaxAgeMinutes:           10,
			UsageProbeConcurrency:             16,
			RecoveryProbeIntervalMinutes:      30,
			LazyMode:                          false,
			ProxyURL:                          "",
			PgMaxConns:                        50,
			RedisPoolSize:                     30,
			AutoCleanUnauthorized:             false,
			AutoCleanRateLimited:              false,
			PromptFilterMode:                  recommendedPromptFilter.Mode,
			PromptFilterThreshold:             recommendedPromptFilter.Threshold,
			PromptFilterStrictThreshold:       recommendedPromptFilter.StrictThreshold,
			PromptFilterStrictTerminalEnabled: recommendedPromptFilter.StrictTerminalEnabled,
			PromptFilterAdvancedConfig:        promptfilter.MarshalAdvancedConfig(recommendedPromptFilter.Advanced),
			PromptFilterLogMatches:            true,
			PromptFilterMaxTextLength:         81920,
			PromptFilterCustomPatterns:        "[]",
			PromptFilterDisabledPatterns:      "[]",
			ClientCompatMode:                  proxy.ClientCompatModePreserve,
			CodexMinCLIVersion:                "0.160.0",
			UsageLogMode:                      database.UsageLogModeFull,
			UsageLogBatchSize:                 200,
			UsageLogFlushIntervalSeconds:      5,
			StreamFlushPolicy:                 proxy.StreamFlushPolicyImmediate,
			StreamFlushIntervalMS:             20,
			FirstTokenMode:                    proxy.FirstTokenModeLoose,
			FirstTokenTimeoutSeconds:          0,
			BillingTierPolicy:                 proxy.NormalizeBillingTierPolicy(os.Getenv("CODEX_BILLING_TIER_POLICY")),
			ImageStorageConfig:                "{}",
			PublicKeyUsagePageEnabled:         true,
			PublicImageStudioPageEnabled:      true,
			ShowUpstreamModelMismatch:         true,
			CodexWSHideUpstreamErrors:         true,
			CodexWSSilentRetryEnabled:         true,
			CodexWSSilentMaxRetries:           2,
			CodexWSStatelessSlots:             proxy.DefaultRuntimeSettings().CodexWSStatelessSlots,
			CodexWSDownstreamKeepaliveSlots:   database.DefaultCodexWSDownstreamKeepaliveSlots,
			CodexContinueMaxRounds:            8,
			AutoPause5hGuardBandPercent:       5,
			AutoPause5hGuardConcurrency:       1,
			SmartPacingMinConcurrency:         1,
			SmartPacingWindows:                "5h,7d",
			AutoResetCreditsBeforeExpiryMin:   60,
			UTLSShutdownTimeoutMinutes:        30,
		}
		_ = db.UpdateSystemSettings(context.Background(), settings)
	} else if err != nil {
		log.Printf("warning: failed to read system settings: %v, falling back to the safe fallback strategy", err)
		settings = &database.SystemSettings{
			SiteName:                          database.DefaultSiteName,
			MaxConcurrency:                    2,
			CodexTelemetryEnabled:             false, // experimental: simulated telemetry is not sent out by default, the deployer must enable it explicitly
			GlobalRPM:                         0,
			TestModel:                         auth.DefaultTestModel,
			TestContent:                       auth.DefaultTestContent,
			TestConcurrency:                   50,
			MaxRateLimitRetries:               1,
			BackgroundRefreshIntervalMinutes:  2,
			UsageProbeMaxAgeMinutes:           10,
			UsageProbeConcurrency:             16,
			RecoveryProbeIntervalMinutes:      30,
			LazyMode:                          false,
			PgMaxConns:                        50,
			RedisPoolSize:                     30,
			PromptFilterMode:                  recommendedPromptFilter.Mode,
			PromptFilterThreshold:             recommendedPromptFilter.Threshold,
			PromptFilterStrictThreshold:       recommendedPromptFilter.StrictThreshold,
			PromptFilterStrictTerminalEnabled: recommendedPromptFilter.StrictTerminalEnabled,
			PromptFilterAdvancedConfig:        promptfilter.MarshalAdvancedConfig(recommendedPromptFilter.Advanced),
			PromptFilterLogMatches:            true,
			PromptFilterMaxTextLength:         81920,
			PromptFilterCustomPatterns:        "[]",
			PromptFilterDisabledPatterns:      "[]",
			ClientCompatMode:                  proxy.ClientCompatModePreserve,
			CodexMinCLIVersion:                "0.160.0",
			UsageLogMode:                      database.UsageLogModeFull,
			UsageLogBatchSize:                 200,
			UsageLogFlushIntervalSeconds:      5,
			StreamFlushPolicy:                 proxy.StreamFlushPolicyImmediate,
			StreamFlushIntervalMS:             20,
			FirstTokenMode:                    proxy.FirstTokenModeLoose,
			FirstTokenTimeoutSeconds:          0,
			BillingTierPolicy:                 proxy.NormalizeBillingTierPolicy(os.Getenv("CODEX_BILLING_TIER_POLICY")),
			ImageStorageConfig:                "{}",
			PublicKeyUsagePageEnabled:         true,
			PublicImageStudioPageEnabled:      true,
			ShowUpstreamModelMismatch:         true,
			CodexWSHideUpstreamErrors:         true,
			CodexWSSilentRetryEnabled:         true,
			CodexWSSilentMaxRetries:           2,
			CodexWSStatelessSlots:             proxy.DefaultRuntimeSettings().CodexWSStatelessSlots,
			CodexWSDownstreamKeepaliveSlots:   database.DefaultCodexWSDownstreamKeepaliveSlots,
			CodexContinueMaxRounds:            8,
			AutoPause5hGuardBandPercent:       5,
			AutoPause5hGuardConcurrency:       1,
			SmartPacingMinConcurrency:         1,
			SmartPacingWindows:                "5h,7d",
			AutoResetCreditsBeforeExpiryMin:   60,
			UTLSShutdownTimeoutMinutes:        30,
		}
	} else {
		log.Printf("persisted business settings loaded: ProxyURL=%s, MaxConcurrency=%d, GlobalRPM=%d, PgMaxConns=%d, RedisPoolSize=%d",
			security.MaskURLCredentials(settings.ProxyURL), settings.MaxConcurrency, settings.GlobalRPM, settings.PgMaxConns, settings.RedisPoolSize)
	}
	modelCooldownCtx, modelCooldownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	modelCooldownSettings, modelCooldownErr := db.GetModelCooldownSettings(modelCooldownCtx)
	modelCooldownCancel()
	if modelCooldownErr != nil {
		log.Printf("warning: failed to read model cooldown settings, using safe defaults: %v", modelCooldownErr)
		modelCooldownSettings = database.DefaultModelCooldownSettings()
	}
	settings.RelayModelCooldownMode = modelCooldownSettings.RelayMode
	settings.RelayModelCooldownSeconds = modelCooldownSettings.RelaySeconds
	settings.RelayModelCooldownBackoffEnabled = modelCooldownSettings.RelayBackoffEnabled
	settings.OAuthModelCooldownMode = modelCooldownSettings.OAuthMode
	settings.OAuthModelCooldownSeconds = modelCooldownSettings.OAuthSeconds
	settings.OAuthModelCooldownBackoffEnabled = modelCooldownSettings.OAuthBackoffEnabled
	settings.OAuthUnsupportedModelCooldownDisabled = modelCooldownSettings.OAuthUnsupportedDisabled
	settings.OAuthUnsupportedModelCooldownSeconds = modelCooldownSettings.OAuthUnsupportedSeconds
	if envPolicy := strings.TrimSpace(os.Getenv("CODEX_BILLING_TIER_POLICY")); envPolicy != "" {
		settings.BillingTierPolicy = proxy.NormalizeBillingTierPolicy(envPolicy)
	}
	responseCacheCtx, responseCacheCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := proxy.LoadResponseCacheSettings(responseCacheCtx, db); err != nil {
		responseCacheCancel()
		log.Fatalf("failed to load response cache settings: %v", err)
	}
	responseCacheCancel()

	appliedResponseCache := proxy.GetResponseCacheAppliedConfig()
	log.Printf(
		"response cache settings loaded: generation=%d total=%d entry=%d reconstruct=%d",
		appliedResponseCache.Generation,
		appliedResponseCache.LocalMaxBytes,
		appliedResponseCache.LocalMaxEntryBytes,
		appliedResponseCache.ReconstructMaxBytes,
	)

	// Model output runaway guard settings: a cold-start read failure is not fatal, the background 5-second poller recovers it;
	// while not ready, the HTTP/WS/Stream gates uniformly return 503 service_unavailable.
	outputGuardCtx, outputGuardCancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := proxy.LoadOutputGuardSettings(outputGuardCtx, db); err != nil {
		outputGuardCancel()
		log.Printf("warning: failed to load model output runaway guard settings (the cold-start gate returns 503 until DB polling recovers): %v", err)
	} else {
		outputGuardCancel()
		appliedGuard := proxy.CurrentOutputGuardConfig()
		log.Printf(
			"model output runaway guard settings loaded: generation=%d enabled=%t tool_bytes=%d tool_deltas=%d channel_bytes=%d request_bytes=%d",
			appliedGuard.Generation, appliedGuard.Enabled, appliedGuard.MaxToolArgumentBytes,
			appliedGuard.MaxToolArgumentDeltas, appliedGuard.MaxChannelBytes, appliedGuard.MaxRequestBytes,
		)
	}
	proxy.StartOutputGuardConfigPoller(context.Background(), db)

	// 4. Initialize the cache (using the connection pool size saved in the database)
	redisPoolSize := 30
	if settings.RedisPoolSize > 0 {
		redisPoolSize = settings.RedisPoolSize
	}
	var tc cache.TokenCache
	switch cfg.Cache.Driver {
	case "memory":
		tc = cache.NewMemory(redisPoolSize)
	default:
		tc, err = cache.NewRedisWithOptions(cache.RedisOptions{
			Addr:               cfg.Cache.Redis.Addr,
			Username:           cfg.Cache.Redis.Username,
			Password:           cfg.Cache.Redis.Password,
			DB:                 cfg.Cache.Redis.DB,
			PoolSize:           redisPoolSize,
			TLS:                cfg.Cache.Redis.TLS,
			InsecureSkipVerify: cfg.Cache.Redis.InsecureSkipVerify,
		})
		if err != nil {
			log.Fatalf("cache initialization failed: %v", err)
		}
	}
	defer tc.Close()
	switch cfg.Cache.Driver {
	case "memory":
		log.Printf("%s cache enabled: pool_size=%d", cfg.Cache.Label(), redisPoolSize)
	default:
		log.Printf("%s connected successfully: %s, pool_size=%d", cfg.Cache.Label(), cache.RedactRedisAddr(cfg.Cache.Redis.Addr), redisPoolSize)
	}
	responseContextCache := tc
	var durableResponseContexts *cache.DurableResponseContextStore
	if storeCfg := cfg.Cache.ResponseContextStore; storeCfg.Path != "" {
		durableResponseContexts, err = cache.OpenDurableResponseContextStore(storeCfg.Path, cache.DurableResponseContextOptions{
			TTL:      storeCfg.TTL,
			MaxBytes: storeCfg.MaxBytes,
		})
		if err != nil {
			// The persistence layer is only a cache: when it cannot be opened, fall back to the original L1 behavior and do not block service startup.
			log.Printf("persisted response context store unavailable, continuing with the in-memory cache: %v", err)
			durableResponseContexts = nil
		} else {
			responseContextCache = cache.WithDurableResponseContexts(tc, durableResponseContexts)
			statsCtx, cancelStats := context.WithTimeout(context.Background(), 5*time.Second)
			stats, statsErr := durableResponseContexts.Stats(statsCtx)
			cancelStats()
			if statsErr != nil {
				log.Printf("persisted response context store enabled: path=%s ttl=%s max_mb=%d (stats failed: %v)", storeCfg.Path, storeCfg.TTL, storeCfg.MaxBytes>>20, statsErr)
			} else {
				log.Printf("persisted response context store enabled: path=%s ttl=%s max_mb=%d contexts=%d item_bytes=%d", storeCfg.Path, storeCfg.TTL, storeCfg.MaxBytes>>20, stats.Contexts, stats.ItemBytes)
			}
		}
	}
	proxy.SetResponseContextCache(responseContextCache)

	// 4b. Apply the database connection pool settings
	if settings.PgMaxConns > 0 {
		db.SetMaxOpenConns(settings.PgMaxConns)
		log.Printf("%s connection pool: max_conns=%d", cfg.Database.Label(), settings.PgMaxConns)
	}
	db.SetUsageLogConfig(settings.UsageLogMode, settings.UsageLogBatchSize, settings.UsageLogFlushIntervalSeconds)
	if overrides, perr := database.ParseModelPricingOverridesJSON(settings.ModelPricingOverrides); perr == nil {
		database.SetModelPricingOverrides(overrides)
	}
	runtimeSettings := proxy.ApplyRuntimeSettingsFromSystem(settings)
	log.Printf("runtime optimization config: client_compat=%s min_cli=%s usage_log=%s batch=%d flush=%ds stream_flush=%s/%dms first_token_mode=%s first_token_timeout=%ds billing_tier_policy=%s",
		runtimeSettings.ClientCompatMode,
		runtimeSettings.CodexMinCLIVersion,
		db.GetUsageLogMode(),
		db.GetUsageLogBatchSize(),
		db.GetUsageLogFlushIntervalSeconds(),
		runtimeSettings.StreamFlushPolicy,
		runtimeSettings.StreamFlushIntervalMS,
		runtimeSettings.FirstTokenMode,
		runtimeSettings.FirstTokenTimeoutSec,
		runtimeSettings.BillingTierPolicy,
	)

	// 4b'. Apply the image storage backend config
	imgLocalDir := strings.TrimSpace(os.Getenv("IMAGE_ASSET_DIR"))
	if imgLocalDir == "" {
		imgLocalDir = "/data/images"
	}
	if imgCfg, err := imagestore.ApplyFromJSON(settings.ImageStorageConfig, imgLocalDir); err != nil {
		log.Printf("failed to apply image storage config, fell back to local storage: %v", err)
	} else {
		log.Printf("image storage backend: %s", imgCfg.Backend)
	}

	// 4c. Initialize the Resin sticky proxy pool
	if settings.ResinURL != "" && settings.ResinPlatformName != "" {
		proxy.SetResinConfig(&proxy.ResinConfig{
			BaseURL:      settings.ResinURL,
			PlatformName: settings.ResinPlatformName,
		})
		// Inject the Resin URL decorator into the auth package (avoids an auth → proxy circular dependency)
		auth.ResinRequestDecorator = func(targetURL, accountID string) string {
			return proxy.BuildReverseProxyURL(targetURL)
		}
	}

	// 5. Initialize the account manager
	auth.CodexOAuthIdentityHeaders = proxy.CodexOAuthIdentityHeaders
	store := auth.NewStore(db, tc, settings)
	store.SetSchedulerWaitLimits(cfg.SchedulerMaxWaiters, cfg.SchedulerMaxWaitersPerKey)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := store.Init(ctx); err != nil {
		cancel()
		log.Fatalf("account initialization failed: %v", err)
	}
	cancel()

	// Global RPM rate limiter
	rateLimiter := proxy.NewRateLimiter(settings.GlobalRPM)
	adminHandler := admin.NewHandler(store, db, tc, rateLimiter, cfg.AdminSecret)
	// Initialize connection pool settings tracking for the admin handler
	adminHandler.SetPoolSizes(settings.PgMaxConns, settings.RedisPoolSize)
	store.SetUsageProbeFunc(adminHandler.ProbeUsageSnapshot)

	// Start background refresh
	store.StartBackgroundRefresh()
	store.TriggerUsageProbeAsync()
	store.TriggerRecoveryProbeAsync()
	store.TriggerAutoCleanupAsync()
	defer store.Stop()
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	adminHandler.StartQualityTests(backgroundCtx)
	defer cancelBackground()
	if !proxy.StartResponseCacheSettingsPoller(backgroundCtx, db) {
		log.Fatalf("failed to start response cache settings sync")
	}
	adminHandler.StartAutoResetCredits(backgroundCtx)
	adminHandler.StartAutoActivate5hWindow(backgroundCtx)
	adminHandler.StartScheduledConnectionTests(backgroundCtx)
	// Daily snapshots of official billing usage: upstream keeps only 7 days, so without persisting them the history is lost for good; this job is the only source of long-term history.
	adminHandler.StartWhamDailyUsageProbe(backgroundCtx)
	// Polling of official model prices is off by default; once enabled, it makes one short database write only after network resolution completes.
	adminHandler.StartOfficialPricingSync(backgroundCtx)
	// Prompt review log retention cleanup: keeps 7 days by default, deletes expired rows in batches every hour, and leaves CY-linked rows untouched.
	adminHandler.StartPromptLogRetention(backgroundCtx)

	// The version gate and the plan gate share the same upstream wording: after the version is raised, cooldowns on models learned as unsupported under the old version are void (issue #793).
	proxy.OnCodexClientVersionRaised(func() {
		if cleared := store.ClearModelCooldownsByReason(auth.ModelCooldownReasonUnsupported, 0); cleared > 0 {
			log.Printf("Codex client version raised, cleared %d unsupported-model cooldowns", cleared)
		}
	})
	// Background scheduled sync of the simulated Codex CLI version (fetched once at startup, then at the configured interval);
	// when the upstream raises its new-version threshold, this follows without a release. The switch and interval are adjustable on the settings page,
	// CODEX_DISABLE_CLI_VERSION_SYNC is a hard off switch.
	proxy.StartCodexCLIVersionSync(backgroundCtx, db, store.GetProxyURL)

	log.Printf("accounts ready: %d/%d available", store.AvailableCount(), store.AccountCount())

	// 6. Start the HTTP service
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	// By default, loopback and common private networks are trusted, so that a same-host or Docker WAF reverse proxy gets the real IP while direct public connections are protected against spoofing;
	// to tighten or extend the trusted proxy range, set CIDRs/IPs explicitly via CODEX_TRUSTED_PROXIES.
	if err := configureTrustedProxies(r, cfg.TrustedProxies); err != nil {
		log.Fatalf("failed to configure trusted proxies: %v", err)
	}
	r.Use(api.RecoveryMiddleware())
	r.Use(api.RequestContextMiddleware())
	r.Use(api.VersionMiddleware())
	security.MaxRequestBodySize = cfg.MaxRequestBodySize
	security.ConfigureRequestMemoryBudget(cfg.RequestMemoryBudgetBytes)
	// The account import endpoint (multipart file upload) has its own relaxed size limit, 200MB by default, which can be
	// overridden with CODEX_MAX_IMPORT_BODY_SIZE_MB. The frontend sends in size-based batches, each kept within this limit.
	if v := strings.TrimSpace(os.Getenv("CODEX_MAX_IMPORT_BODY_SIZE_MB")); v != "" {
		if mb, err := strconv.Atoi(v); err == nil && mb > 0 {
			security.MaxImportBodySize = int64(mb) * 1024 * 1024
		}
	}
	if security.MaxImportBodySize < int64(security.MaxRequestBodySize) {
		security.MaxImportBodySize = int64(security.MaxRequestBodySize)
	}
	configureRequestBodyMiddleware(r, int64(security.MaxRequestBodySize))
	r.Use(api.SecurityHeadersMiddleware())
	r.Use(loggerMiddleware())
	r.Use(security.SecurityHeadersMiddleware())

	// The handler no longer receives cfg.APIKeys
	// Read the Codex profile and Beta config from environment variables.
	deviceCfg := proxy.DeviceProfileConfigFromEnv(os.Getenv)
	handler := proxy.NewHandler(store, db, cfg, deviceCfg)
	handler.SetRuntimeCache(tc)
	defer handler.CloseAPIKeyAuthCache()
	adminHandler.SetAPIKeyAuthCacheHandler(handler)

	// Register the WebSocket execution function (avoids a proxy ↔ wsrelay circular dependency)
	proxy.WebsocketExecuteFunc = wsrelay.ExecuteRequestWebsocket
	// Connection-level usage frames (re-sent after the terminal state) are also handed to proxy to update the in-memory snapshot; the observer must not block.
	configureCodexIdleUsageObserver(store)
	// Throttles per-frame WS usage persistence; large account pools may raise it according to database load; 0 or invalid values keep the default of 30s.
	if raw := strings.TrimSpace(os.Getenv("CODEX_WS_USAGE_FRAME_PERSIST_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			proxy.SetCodexUsageFramePersistInterval(time.Duration(seconds) * time.Second)
		}
	}
	// Deployment-level escape hatch for frame ingestion; when false, falls back to the WHAM probe and HTTP response headers.
	if raw := strings.TrimSpace(os.Getenv("CODEX_WS_USAGE_FRAME_INGEST")); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			proxy.SetCodexUsageFrameIngestEnabled(enabled)
		}
	}

	// Register the Agent Identity task ensure function (proxy has no Store reference, so it is injected at startup)
	proxy.EnsureCodexAgentIdentityTaskFunc = store.EnsureCodexAgentIdentityTask

	// Upstream WS idle connection keepalive resident task (off by default: the goroutine stays resident but only sends Ping when the runtime switch is on)
	wsKeepalive := wsrelay.NewKeepaliveTask(
		wsrelay.GetManager(),
		store.CodexWSKeepaliveEnabled,
		store.CodexWSKeepaliveIntervalSec,
	)
	wsKeepalive.Start()

	r.Use(rateLimiter.Middleware())
	if settings.GlobalRPM > 0 {
		log.Printf("global rate limit in effect: %d RPM", settings.GlobalRPM)
	}
	log.Printf("per-account concurrency limit: %d", settings.MaxConcurrency)

	handler.RegisterRoutes(r)
	adminHandler.RegisterExternalImageRoutes(r, handler)
	imageWorkers, queueErr := admin.ImageJobWorkerCount()
	if queueErr != nil {
		log.Fatal(queueErr)
	}
	if err := adminHandler.StartImageJobQueue(backgroundCtx, imageWorkers); err != nil {
		log.Fatalf("Initialize image queue: %v", err)
	}
	if err := adminHandler.StartImageMaintenance(backgroundCtx); err != nil {
		log.Fatalf("Initialize image maintenance: %v", err)
	}
	adminHandler.StartPromptIntelligence(backgroundCtx)
	adminHandler.StartIntelligenceChecks(backgroundCtx, deviceCfg)
	adminHandler.RegisterRoutes(r)

	// Admin panel frontend static files
	subFS, err := fs.Sub(frontendFS, "frontend/dist")
	if err != nil {
		log.Printf("failed to load frontend static files (can be ignored in development mode): %v", err)
	} else {
		httpFS := http.FS(subFS)
		// Pre-read index.html (returned directly on SPA fallback, to avoid FileServer redirects)
		indexHTML, _ := fs.ReadFile(subFS, "index.html")

		serveFrontend := func(c *gin.Context) {
			fp := c.Param("filepath")
			// Try to open the requested file (excluding directories and the root path)
			if fp != "/" && len(fp) > 1 {
				trimmed := fp[1:] // strip the leading /
				if f, err := subFS.Open(trimmed); err == nil {
					fi, statErr := f.Stat()
					f.Close()
					if statErr == nil && !fi.IsDir() {
						if strings.HasPrefix(trimmed, "assets/") {
							c.Header("Cache-Control", "public, max-age=31536000, immutable")
						} else {
							c.Header("Cache-Control", "no-cache")
						}
						c.FileFromFS(fp, httpFS)
						return
					}
				}
				// A missing static asset with a hash in its name must return 404. If it fell back to index.html,
				// the browser would parse the HTML as JS/CSS and repeatedly trigger chunk load errors.
				if strings.HasPrefix(trimmed, "assets/") {
					c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
					c.Status(http.StatusNotFound)
					return
				}
			}
			// File does not exist or is a directory → return the index.html bytes directly (let React Router handle it)
			c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
			c.Header("Pragma", "no-cache")
			if c.Query("refresh-assets") == "1" {
				// Only clears the HTTP cache; login state, localStorage, and site configuration are unaffected.
				c.Header("Clear-Site-Data", `"cache"`)
			}
			c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
		}
		serveKeyUsageFrontend := func(c *gin.Context) {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			defer cancel()

			enabled, err := adminHandler.PublicAPIKeyUsagePageEnabled(ctx)
			if err != nil {
				log.Printf("failed to read the API key self-service usage page switch: %v", err)
				c.Status(http.StatusInternalServerError)
				return
			}
			if !enabled {
				c.Status(http.StatusNotFound)
				return
			}
			serveFrontend(c)
		}
		serveImageStudioFrontend := func(c *gin.Context) {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			defer cancel()

			enabled, err := adminHandler.PublicImageStudioPageEnabled(ctx)
			if err != nil {
				log.Printf("failed to read the image generation portal switch: %v", err)
				c.Status(http.StatusInternalServerError)
				return
			}
			if !enabled {
				c.Status(http.StatusNotFound)
				return
			}
			serveFrontend(c)
		}
		serveAccountPortalFrontend := func(c *gin.Context) {
			ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			defer cancel()

			enabled, err := adminHandler.PublicAccountPortalPageEnabled(ctx)
			if err != nil {
				log.Printf("failed to read the account self-service portal switch: %v", err)
				c.Status(http.StatusInternalServerError)
				return
			}
			if !enabled {
				c.Status(http.StatusNotFound)
				return
			}
			serveFrontend(c)
		}

		// Handle both /admin and /admin/* to avoid relying on automatic trailing-slash redirects.
		r.GET("/admin", serveFrontend)
		r.GET("/admin/*filepath", serveFrontend)
		r.HEAD("/admin", serveFrontend)
		r.HEAD("/admin/*filepath", serveFrontend)
		r.GET("/key-usage", serveKeyUsageFrontend)
		r.GET("/key-usage/*filepath", serveKeyUsageFrontend)
		r.HEAD("/key-usage", serveKeyUsageFrontend)
		r.HEAD("/key-usage/*filepath", serveKeyUsageFrontend)
		r.GET("/image-studio", serveImageStudioFrontend)
		r.GET("/image-studio/*filepath", serveImageStudioFrontend)
		r.HEAD("/image-studio", serveImageStudioFrontend)
		r.HEAD("/image-studio/*filepath", serveImageStudioFrontend)
		r.GET("/account-portal", serveAccountPortalFrontend)
		r.GET("/account-portal/*filepath", serveAccountPortalFrontend)
		r.HEAD("/account-portal", serveAccountPortalFrontend)
		r.HEAD("/account-portal/*filepath", serveAccountPortalFrontend)
	}

	// Redirect the root path to the admin panel (uses 302 to avoid permanent browser caching)
	r.GET("/", func(c *gin.Context) {
		c.Redirect(http.StatusFound, "/admin/")
	})

	// Health check: only does non-blocking best-effort stats, so lock contention on the account hot path cannot stall liveness.
	// But if the account pool read lock cannot be acquired for longer than the threshold in a row, treat it as a suspected deadlock and return 503 so
	// the healthcheck restarts the instance; otherwise a deadlocked instance would stay in service with 200 forever.
	healthProbe := &healthLockProbe{}
	r.GET("/health", func(c *gin.Context) {
		available, total, countsComplete := store.HealthCountsNonBlocking()
		blocked := healthProbe.observe(total >= 0, time.Now())
		if blocked >= healthStoreLockStallThreshold {
			c.JSON(503, gin.H{
				"status":          "unavailable",
				"reason":          "account store lock stalled",
				"blocked_seconds": int(blocked / time.Second),
				"available":       available,
				"total":           total,
				"counts_complete": countsComplete,
			})
			return
		}
		c.JSON(200, gin.H{
			"status":          "ok",
			"build_version":   version.Current(),
			"available":       available,
			"total":           total,
			"counts_complete": countsComplete,
		})
	})

	// 6.5 Start the security status self-check banner
	printSecurityBanner(db, cfg, settings)

	addr := fmt.Sprintf("%s:%d", cfg.BindAddress, cfg.Port)
	displayHost := cfg.BindAddress
	if displayHost == "0.0.0.0" || displayHost == "::" {
		displayHost = "localhost"
	}
	log.Println("==========================================")
	log.Printf("  Codex2API v2 started")
	log.Printf("  Listen: %s", addr)
	log.Printf("  HTTP:   http://%s:%d", displayHost, cfg.Port)
	log.Printf("  Admin panel: http://%s:%d/admin/", displayHost, cfg.Port)
	log.Printf("  Key usage: http://%s:%d/key-usage", displayHost, cfg.Port)
	log.Printf("  Image generation portal: http://%s:%d/image-studio", displayHost, cfg.Port)
	log.Printf("  Account self-service: http://%s:%d/account-portal", displayHost, cfg.Port)
	log.Printf("  API:    POST /v1/chat/completions")
	log.Printf("  API:    POST /v1/responses")
	log.Printf("  API:    POST /v1/images/generations")
	log.Printf("  API:    POST /v1/images/jobs")
	log.Printf("  API:    POST /v1/images/jobs/results")
	log.Printf("  API:    GET  /v1/images/jobs/:id")
	log.Printf("  API:    GET  /v1/images/jobs/:id/output")
	log.Printf("  API:    POST /v1/images/jobs/:id/ack")
	log.Printf("  API:    GET  /v1/models")
	log.Println("==========================================")

	// Graceful shutdown
	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// ReadHeaderTimeout guards against Slowloris slow attacks; ReadTimeout limits reading of the whole request body,
		// preventing a client that declares a large Content-Length but sends no body from holding connections and memory budget for long;
		// IdleTimeout reclaims idle keep-alive connections.
		// Note: WriteTimeout must stay 0. LLM streaming responses can last several minutes, and any fixed write timeout would cut long answers off midway.
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       10 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP service failed to start: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	// Stop background tasks that create new side effects first, then wait for in-flight HTTP requests to drain.
	cancelBackground()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP service graceful shutdown timed out: %v", err)
	}
	adminHandler.WaitAutoResetCredits()
	adminHandler.WaitAutoActivate5hWindow()
	adminHandler.WaitScheduledConnectionTests()
	adminHandler.WaitQualityTests()
	adminHandler.WaitIntelligenceChecks()
	wsKeepalive.Stop()
	wsrelay.ShutdownExecutor()
	if !proxy.DrainResponseCacheBackendWrites(2 * time.Second) {
		log.Printf("some response context background writes did not finish within the shutdown window")
	}
	if durableResponseContexts != nil {
		if err := durableResponseContexts.Close(); err != nil {
			log.Printf("failed to close persisted response context store: %v", err)
		}
	}
	store.Stop()
	// After all request entry points and background producers have stopped, drain the short tasks that may still access the Store, cache, or database.
	if !db.DrainBackgroundTasks(2 * time.Second) {
		log.Printf("some background tasks did not exit within the shutdown window")
	}
	proxy.CloseErrorLogger()
	log.Println("closed")
}

// configureTrustedProxies configures Gin's trusted proxy list.
func configureTrustedProxies(r *gin.Engine, proxies []string) error {
	if r == nil {
		return nil
	}
	return r.SetTrustedProxies(proxies)
}

// loggerMiddleware is a simple logging middleware (enhanced version, with sensitive information redaction)
func loggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		latency := time.Since(start)
		statusCode := c.Writer.Status()
		if override, ok := c.Get(proxy.AccessLogStatusContextKey); ok {
			if status, valid := override.(int); valid && status >= 100 && status <= 599 {
				statusCode = status
			}
		}
		if shouldSkipAccessLog(c.Request.Method, c.Request.URL.Path, statusCode) {
			return
		}

		email, _ := c.Get("x-account-email")
		proxyURL, _ := c.Get("x-account-proxy")
		modelVal, _ := c.Get("x-model")
		effortVal, _ := c.Get("x-reasoning-effort")
		tierVal, _ := c.Get("x-service-tier")

		emailStr := ""
		if e, ok := email.(string); ok && e != "" {
			// Redact email
			emailStr = security.MaskEmail(e)
		}
		proxyStr := "no proxy"
		if p, ok := proxyURL.(string); ok && p != "" {
			proxyStr = security.SanitizeLog(p)
		}

		// Build extended labels
		var tags []string
		if m, ok := modelVal.(string); ok && m != "" {
			tags = append(tags, security.SanitizeLog(m))
		}
		if e, ok := effortVal.(string); ok && e != "" {
			tags = append(tags, "effort="+security.SanitizeLog(e))
		}
		if t, ok := tierVal.(string); ok && t == "fast" {
			tags = append(tags, "fast")
		}
		tagStr := ""
		if len(tags) > 0 {
			tagStr = " " + strings.Join(tags, " ")
		}

		if emailStr != "" {
			log.Printf("%s %s %d %v%s [%s] [%s]", c.Request.Method, c.Request.URL.Path, statusCode, latency, tagStr, emailStr, proxyStr)
		} else {
			log.Printf("%s %s %d %v%s", c.Request.Method, c.Request.URL.Path, statusCode, latency, tagStr)
		}
	}
}

// printSecurityBanner prints the security status self-check banner at startup:
//   - No longer auto-generates ADMIN_SECRET. If both are empty, prompt the user to initialize by visiting the page for the first time.
//   - Checks the API key count, listen address, and anonymous switch, and shows a matching warning when a risk is hit.
func printSecurityBanner(db *database.DB, cfg *config.Config, settings *database.SystemSettings) {
	if db == nil || cfg == nil || settings == nil {
		return
	}

	envSecret := strings.TrimSpace(cfg.AdminSecret)
	dbSecret := strings.TrimSpace(settings.AdminSecret)
	needsBootstrap := envSecret == "" && dbSecret == ""

	apiKeyCount := 0
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if rows, err := db.ListAPIKeys(ctx); err == nil {
			apiKeyCount = len(rows)
		}
		cancel()
	}

	bind := strings.TrimSpace(cfg.BindAddress)
	publicBind := bind == "" || bind == "0.0.0.0" || bind == "::"
	const sep = "=========================================================="
	log.Println(sep)
	log.Println("[SECURITY] Codex2API security status self-check")
	log.Println(sep)

	switch {
	case needsBootstrap:
		log.Println("⚠ ADMIN_SECRET is not configured (environment variable and database are both empty).")
		log.Printf("  Open the admin panel at http://%s:%d/admin/ in a browser to complete first-time setup,", bannerDisplayHost(bind), cfg.Port)
		log.Println("  set a strong random admin secret; this secret also serves as the login key.")
		log.Println("  The setup page requires the one-time token below, to prove the operator can read this service's logs:")
		if _, err := admin.EnsureBootstrapToken(); err != nil {
			log.Printf("  failed to generate the setup token: %v", err)
		}
		log.Println("  Until setup is complete, all /api/admin/* endpoints (except the setup endpoint) return 503.")
	case envSecret != "":
		log.Println("✓ ADMIN_SECRET source: environment variable (.env)")
	default:
		log.Println("✓ ADMIN_SECRET source: database (to change it, go to the 'Settings' page)")
	}

	if apiKeyCount == 0 {
		if cfg.AllowAnonymousV1 {
			log.Println("⚠ /v1/* is currently in [anonymous access] mode (CODEX_ALLOW_ANONYMOUS=true).")
			log.Println("  Anyone who can reach the port can call /v1/* and consume your account pool's quota, so use this only in internal or test environments!")
		} else {
			log.Println("⚠ No external API key has been created yet. /v1/* endpoints return 503 until the first key is created.")
			log.Println("  Go to the admin panel's 'API Keys' page and create at least one key before serving external traffic.")
		}
	} else {
		log.Printf("✓ %d external API keys configured, /v1/* authentication is enforced.", apiKeyCount)
	}

	if publicBind {
		log.Printf("ℹ listen address = %s (all interfaces, compatible with Docker / reverse proxy / public network).", bind)
		log.Println("  In production, confirm that a reverse proxy with HTTPS is deployed, configure a firewall allowlist, and use a strong ADMIN_SECRET and API keys.")
		log.Println("  To make the service reachable only on loopback, set CODEX_BIND=127.0.0.1.")
	} else {
		log.Printf("✓ listen address = %s (restricted access).", bind)
	}

	log.Println(sep)
}

func bannerDisplayHost(bind string) string {
	if bind == "" || bind == "0.0.0.0" || bind == "::" {
		return "<your-host>"
	}
	return bind
}

func shouldSkipAccessLog(method string, path string, status int) bool {
	if status >= http.StatusBadRequest {
		return false
	}
	if method == http.MethodGet && path == "/api/admin/health" {
		return true
	}
	if method == http.MethodGet && (path == "/api/admin/images/jobs" || strings.HasPrefix(path, "/api/admin/images/jobs/")) {
		return true
	}
	return false
}
