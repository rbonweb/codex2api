package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// CodexReleasesLatestURL is the GitHub API endpoint for the latest stable release of openai/codex.
// GitHub's "latest" naturally excludes prerelease/draft (such as 0.145.0-alpha.1) and returns only stable releases.
const CodexReleasesLatestURL = "https://api.github.com/repos/openai/codex/releases/latest"

// codexReleasesLatestURLForTest lets tests replace the default URL. Production code must not assign it.
var codexReleasesLatestURLForTest = ""

// CodexCLIVersionSyncDisabled reports whether client version auto-sync is turned off through the compatibility environment variable.
// When CODEX_DISABLE_CLI_VERSION_SYNC=1 (or true), the background timed and startup sync for all clients is turned off;
// the admin panel's "Sync now" button is not affected.
func CodexCLIVersionSyncDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_DISABLE_CLI_VERSION_SYNC"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// CodexCLIVersionSyncResult is the result projection of one version sync.
type CodexCLIVersionSyncResult struct {
	// FetchedVersion is the latest version parsed from upstream this time (may be empty, meaning parsing failed).
	FetchedVersion string `json:"fetched_version"`
	// EffectiveVersion is the simulated version in effect after sync (the larger of the built-in constant and the synced value).
	EffectiveVersion string `json:"effective_version"`
	// BuiltinVersion is the version constant built into the binary at compile time.
	BuiltinVersion string `json:"builtin_version"`
	// Updated reports whether this run actually raised the effective version and persisted it.
	Updated bool `json:"updated"`
}

// FetchLatestCodexCLIVersion fetches the latest stable version number (such as "0.144.1") from openai/codex releases.
// It prefers the release name (upstream fills in a clean version number) and falls back to parsing tag_name (rust-v0.144.1).
func FetchLatestCodexCLIVersion(ctx context.Context, proxyURL string) (string, error) {
	endpoint := CodexReleasesLatestURL
	if codexReleasesLatestURLForTest != "" {
		endpoint = codexReleasesLatestURLForTest
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build codex releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codex2api")
	// GitHub access settings (issue #522): the token raises the API rate limit quota, and the dedicated proxy is decoupled from the global proxy.
	ApplyGithubAuth(req)

	client, closeClient := newVersionSyncClient(endpoint, GithubProxyOrDefault(endpoint, proxyURL), 20*time.Second)
	defer closeClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("codex releases request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("codex releases upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Name    string `json:"name"`
		TagName string `json:"tag_name"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read codex releases response: %w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse codex releases response: %w", err)
	}

	version := extractCodexCLIVersion(payload.Name)
	if version == "" {
		version = extractCodexCLIVersion(payload.TagName)
	}
	if version == "" {
		return "", fmt.Errorf("no valid version in codex release (name=%q tag=%q)", payload.Name, payload.TagName)
	}
	return version, nil
}

// extractCodexCLIVersion extracts a clean semantic version from the release name / tag_name.
// It accepts forms such as "0.144.1", "v0.144.1" and "rust-v0.144.1"; it returns a value only if it validates as a legal Codex version.
func extractCodexCLIVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.LastIndexByte(raw, '-'); idx >= 0 {
		raw = raw[idx+1:]
	}
	raw = normalizeCodexClientVersionText(raw)
	if raw == "" || !validCodexClientVersionString(raw) {
		return ""
	}
	return raw
}

// SyncCodexCLIVersion fetches the latest upstream version and, when it is higher than the current effective version, persists it to system settings,
// then refreshes RuntimeSettings. It never writes a value below the built-in constant (a remote anomaly does not cause a downgrade).
func SyncCodexCLIVersion(ctx context.Context, db *database.DB, proxyURL string) (*CodexCLIVersionSyncResult, error) {
	result := &CodexCLIVersionSyncResult{
		BuiltinVersion:   latestCodexCLIVersion,
		EffectiveVersion: effectiveLatestCodexCLIVersion(),
	}
	if db == nil {
		return result, fmt.Errorf("database unavailable, cannot sync Codex CLI version")
	}

	fetched, err := FetchLatestCodexCLIVersion(ctx, proxyURL)
	if err != nil {
		return result, err
	}
	result, err = applyCodexCLIVersion(ctx, db, result, fetched)
	if err == nil && result.Updated {
		notifyCodexClientVersionRaised()
	}
	return result, err
}

// applyCodexCLIVersion is the persistence step after SyncCodexCLIVersion fetches. It is reused to save results in sequence after concurrent fetches.
func applyCodexCLIVersion(ctx context.Context, db *database.DB, result *CodexCLIVersionSyncResult, fetched string) (*CodexCLIVersionSyncResult, error) {
	result.FetchedVersion = fetched

	// It only matters when the fetched value is above the built-in constant (otherwise the runtime falls back to the built-in constant automatically).
	if cmp, ok := compareCodexClientVersions(fetched, latestCodexCLIVersion); !ok || cmp <= 0 {
		result.EffectiveVersion = effectiveLatestCodexCLIVersion()
		return result, nil
	}

	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		return result, err
	}
	if settings == nil {
		settings = &database.SystemSettings{}
	}
	if cmp, ok := compareCodexClientVersions(fetched, settings.CodexSyncedCLIVersion); ok && cmp <= 0 {
		UpdateRuntimeSettings(func(runtime RuntimeSettings) RuntimeSettings {
			runtime.CodexSyncedCLIVersion = settings.CodexSyncedCLIVersion
			return runtime
		})
		result.EffectiveVersion = effectiveLatestCodexCLIVersion()
		return result, nil
	}
	if strings.TrimSpace(settings.CodexSyncedCLIVersion) == fetched {
		result.EffectiveVersion = effectiveLatestCodexCLIVersion()
		return result, nil
	}

	if err := db.UpdateCodexSyncedCLIVersion(ctx, fetched); err != nil {
		return result, err
	}
	UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings {
		settings.CodexSyncedCLIVersion = fetched
		return settings
	})

	result.Updated = true
	result.EffectiveVersion = effectiveLatestCodexCLIVersion()
	return result, nil
}

// StartCodexCLIVersionSync keeps the old entry point and syncs the CLI, Desktop and VSCode versions in the background.
// The switch (CodexCLIVersionSyncEnabled) and the interval (CodexCLIVersionSyncIntervalHours) are read each cycle;
// a new interval takes effect from the next timing round, without a restart. The environment variable CODEX_DISABLE_CLI_VERSION_SYNC is a hard switch with the highest priority.
// proxyResolver lets the caller inject an outbound proxy (may be nil).
func StartCodexCLIVersionSync(ctx context.Context, db *database.DB, proxyResolver func() string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if db == nil {
		return
	}
	if err := LoadCodexClientVersionCache(ctx, db); err != nil {
		fmt.Printf("[codex-client-version-sync] cache load failed: %v\n", err)
	}
	if CodexCLIVersionSyncDisabled() {
		return
	}
	resolveProxy := func() string {
		if proxyResolver == nil {
			return ""
		}
		return proxyResolver()
	}

	runOnce := func(runCtx context.Context) {
		syncCtx, cancel := context.WithTimeout(runCtx, 15*time.Minute)
		defer cancel()
		if res, err := SyncCodexClientVersions(syncCtx, db, resolveProxy()); err != nil {
			fmt.Printf("[codex-client-version-sync] sync failed: %v\n", err)
		} else {
			for kind, item := range map[string]CodexAppBuildSyncResult{
				"cli": res.CLI, "desktop-mac": res.DesktopMac,
				"desktop-windows": res.DesktopWindows, "vscode": res.VSCode,
			} {
				if item.Error != "" {
					fmt.Printf("[codex-client-version-sync] %s sync failed: %s\n", kind, item.Error)
				} else if item.Updated {
					fmt.Printf("[codex-client-version-sync] %s updated to %s\n", kind, item.EffectiveVersion)
				}
			}
		}
	}

	// The currently configured sync interval (hours to a duration), clamped to [1h, 720h].
	currentInterval := func() time.Duration {
		hours := CurrentRuntimeSettings().CodexCLIVersionSyncIntervalHours
		if hours <= 0 {
			hours = 12
		}
		if hours > 720 {
			hours = 720
		}
		return time.Duration(hours) * time.Hour
	}

	db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, taskCancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(ctx, taskCancel)
		defer func() {
			stopParent()
			taskCancel()
		}()
		if CurrentRuntimeSettings().CodexCLIVersionSyncEnabled {
			runOnce(taskCtx)
		}
		for {
			select {
			case <-taskCtx.Done():
				return
			case <-time.After(currentInterval()):
				if CurrentRuntimeSettings().CodexCLIVersionSyncEnabled {
					runOnce(taskCtx)
				}
			}
		}
	})
}
