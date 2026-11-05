package admin

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/internal/version"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const (
	systemUpdateRepo             = "rbonweb/codex2api"
	systemUpdateUserAgent        = "Codex2API-Updater"
	systemUpdateMaxDownloadBytes = 200 * 1024 * 1024
	systemUpdateRestartDelay     = 900 * time.Millisecond
	systemUpdateReleaseCacheTTL  = 2 * time.Minute
)

var (
	errSystemUpdateBusy        = errors.New("an update is already in progress")
	errSystemUpdateLatest      = errors.New("already on the latest version")
	errSystemUpdateUnsupported = errors.New("online update is not supported in this environment")
)

type systemUpdater struct {
	currentVersion     string
	client             systemReleaseClient
	goos               string
	goarch             string
	executablePath     func() (string, error)
	restartProcess     func(string) error
	restartDelay       time.Duration
	runningInContainer func() bool

	mu                    sync.Mutex
	releaseCacheMu        sync.Mutex
	releaseCache          *systemGitHubRelease
	releaseCacheExpiresAt time.Time
}

type systemReleaseClient interface {
	FetchLatestRelease(ctx context.Context) (*systemGitHubRelease, error)
	DownloadFile(ctx context.Context, rawURL, dest string, maxSize int64) error
	FetchText(ctx context.Context, rawURL string, maxSize int64) ([]byte, error)
}

type systemUpdateInfo struct {
	CurrentVersion    string `json:"current_version"`
	LatestVersion     string `json:"latest_version"`
	HasUpdate         bool   `json:"has_update"`
	Supported         bool   `json:"supported"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`
	RuntimeOS         string `json:"runtime_os"`
	RuntimeArch       string `json:"runtime_arch"`
	Mode              string `json:"mode"`
	ReleaseURL        string `json:"release_url,omitempty"`
	AssetName         string `json:"asset_name,omitempty"`
	PublishedAt       string `json:"published_at,omitempty"`
	Warning           string `json:"warning,omitempty"`
}

type systemUpdateResult struct {
	Message        string `json:"message"`
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	NeedRestart    bool   `json:"need_restart"`
	Restarting     bool   `json:"restarting"`
	Mode           string `json:"mode"`
	BackupPath     string `json:"backup_path,omitempty"`
}

type systemUpdateInspection struct {
	info          *systemUpdateInfo
	asset         *systemGitHubAsset
	checksumAsset *systemGitHubAsset
}

type systemGitHubRelease struct {
	TagName     string              `json:"tag_name"`
	Name        string              `json:"name"`
	Body        string              `json:"body"`
	PublishedAt string              `json:"published_at"`
	HTMLURL     string              `json:"html_url"`
	Assets      []systemGitHubAsset `json:"assets"`
}

type systemGitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	Digest             string `json:"digest"`
}

type defaultSystemReleaseClient struct {
	apiClient      *http.Client
	downloadClient *http.Client
}

func newSystemUpdater() *systemUpdater {
	client := newDefaultSystemReleaseClient()
	return &systemUpdater{
		currentVersion:     version.Current(),
		client:             client,
		goos:               runtime.GOOS,
		goarch:             runtime.GOARCH,
		executablePath:     os.Executable,
		restartProcess:     defaultRestartProcess,
		restartDelay:       systemUpdateRestartDelay,
		runningInContainer: detectRunningInContainer,
	}
}

// detectRunningInContainer makes a best-effort check of whether the current process runs inside a container:
// a binary updated inside a container is overwritten by the image version when the container is recreated, so the user is told to upgrade through the image instead.
func detectRunningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil { // podman
		return true
	}
	data, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	content := string(data)
	return strings.Contains(content, "docker") || strings.Contains(content, "kubepods") || strings.Contains(content, "containerd")
}

func newDefaultSystemReleaseClient() *defaultSystemReleaseClient {
	redirectPolicy := func(req *http.Request, _ []*http.Request) error {
		return validateSystemUpdateURL(req.URL.String())
	}
	// Dedicated GitHub proxy (issue #522): used first when configured, otherwise the previous behavior (environment proxy) applies.
	// Resolved per request, so settings changes take effect immediately.
	proxyFunc := func(req *http.Request) (*url.URL, error) {
		if req != nil && req.URL != nil {
			if dedicated := proxy.GithubProxyOrDefault(req.URL.String(), ""); dedicated != "" {
				return url.Parse(dedicated)
			}
		}
		return http.ProxyFromEnvironment(req)
	}
	return &defaultSystemReleaseClient{
		apiClient: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: redirectPolicy,
			Transport:     &http.Transport{Proxy: proxyFunc},
		},
		downloadClient: &http.Client{
			Timeout:       10 * time.Minute,
			CheckRedirect: redirectPolicy,
			Transport:     &http.Transport{Proxy: proxyFunc},
		},
	}
}

func (h *Handler) GetSystemUpdate(c *gin.Context) {
	updater := h.systemUpdater()
	info, err := updater.Check(c.Request.Context())
	if err != nil {
		log.Printf("system update check failed, silently degraded: %v", err)
		c.JSON(http.StatusOK, updater.unavailableInfo())
		return
	}
	c.JSON(http.StatusOK, info)
}

func (u *systemUpdater) unavailableInfo() *systemUpdateInfo {
	current := normalizeSystemVersion(u.currentVersion)
	info := &systemUpdateInfo{
		CurrentVersion: current,
		LatestVersion:  current,
		RuntimeOS:      u.goos,
		RuntimeArch:    u.goarch,
		Mode:           "binary",
		Supported:      true,
		Warning:        "the update source is temporarily unavailable, skipping this automatic check",
	}
	if current == "" || current == "dev" {
		info.Supported = false
		info.UnsupportedReason = "development build has no injected version, so the upgrade target cannot be determined safely"
	} else if _, ok := parseSystemVersion(current); !ok {
		info.Supported = false
		info.UnsupportedReason = "the current build version is not a semantic version, so the upgrade target cannot be determined safely"
	}
	if u.goos == "windows" {
		info.Supported = false
		info.UnsupportedReason = "online replacement of the running executable is not supported on Windows"
	}
	return info
}

func (h *Handler) PerformSystemUpdate(c *gin.Context) {
	result, err := h.systemUpdater().PerformUpdate(c.Request.Context())
	if err == nil {
		c.JSON(http.StatusOK, result)
		return
	}
	switch {
	case errors.Is(err, errSystemUpdateBusy):
		writeError(c, http.StatusConflict, err.Error())
	case errors.Is(err, errSystemUpdateLatest):
		writeError(c, http.StatusConflict, err.Error())
	case errors.Is(err, errSystemUpdateUnsupported):
		writeError(c, http.StatusBadRequest, err.Error())
	default:
		writeError(c, http.StatusInternalServerError, "online update failed: "+err.Error())
	}
}

func (h *Handler) systemUpdater() *systemUpdater {
	h.systemUpdateOnce.Do(func() {
		if h.systemUpdate == nil {
			h.systemUpdate = newSystemUpdater()
		}
	})
	return h.systemUpdate
}

func (u *systemUpdater) Check(ctx context.Context) (*systemUpdateInfo, error) {
	inspection, err := u.inspect(ctx)
	if err != nil {
		return nil, err
	}
	return inspection.info, nil
}

func (u *systemUpdater) PerformUpdate(ctx context.Context) (*systemUpdateResult, error) {
	if !u.mu.TryLock() {
		return nil, errSystemUpdateBusy
	}
	defer u.mu.Unlock()

	inspection, err := u.inspect(ctx)
	if err != nil {
		return nil, err
	}
	if !inspection.info.Supported {
		if inspection.info.UnsupportedReason != "" {
			return nil, fmt.Errorf("%w: %s", errSystemUpdateUnsupported, inspection.info.UnsupportedReason)
		}
		return nil, errSystemUpdateUnsupported
	}
	if !inspection.info.HasUpdate {
		return nil, errSystemUpdateLatest
	}
	if inspection.asset == nil {
		return nil, fmt.Errorf("%w: no release asset found for %s/%s", errSystemUpdateUnsupported, u.goos, u.goarch)
	}

	exePath, backupPath, err := u.applyBinaryUpdate(ctx, inspection)
	if err != nil {
		return nil, err
	}

	u.scheduleRestart(exePath)
	return &systemUpdateResult{
		Message:        "update applied, the service is restarting",
		CurrentVersion: inspection.info.CurrentVersion,
		LatestVersion:  inspection.info.LatestVersion,
		NeedRestart:    true,
		Restarting:     true,
		Mode:           inspection.info.Mode,
		BackupPath:     backupPath,
	}, nil
}

func (u *systemUpdater) inspect(ctx context.Context) (*systemUpdateInspection, error) {
	current := normalizeSystemVersion(u.currentVersion)
	info := &systemUpdateInfo{
		CurrentVersion: current,
		LatestVersion:  current,
		RuntimeOS:      u.goos,
		RuntimeArch:    u.goarch,
		Mode:           "binary",
		Supported:      true,
	}

	if current == "" || current == "dev" {
		info.Supported = false
		info.UnsupportedReason = "development build has no injected version, so the upgrade target cannot be determined safely"
	} else if _, ok := parseSystemVersion(current); !ok {
		info.Supported = false
		info.UnsupportedReason = "the current build version is not a semantic version, so the upgrade target cannot be determined safely"
	}
	if u.goos == "windows" {
		info.Supported = false
		info.UnsupportedReason = "online replacement of the running executable is not supported on Windows"
	}
	if u.runningInContainer != nil && u.runningInContainer() {
		info.Warning = "container environment detected: online update only replaces the binary inside this container, and the image version is restored when the container is recreated; upgrade by pulling a new image instead"
	}

	release, err := u.fetchLatestRelease(ctx)
	if err != nil {
		return nil, err
	}
	latest := normalizeSystemVersion(release.TagName)
	if latest == "" {
		return nil, fmt.Errorf("the latest release has no valid version")
	}
	info.LatestVersion = latest
	info.ReleaseURL = release.HTMLURL
	info.PublishedAt = release.PublishedAt
	info.HasUpdate = compareSystemVersions(current, latest) < 0

	asset := findSystemUpdateAsset(release, latest, u.goos, u.goarch)
	checksum := findSystemChecksumAsset(release)
	if asset != nil {
		info.AssetName = asset.Name
	} else if info.Supported {
		info.Supported = false
		info.UnsupportedReason = fmt.Sprintf("no release asset found for %s/%s", u.goos, u.goarch)
	}

	return &systemUpdateInspection{
		info:          info,
		asset:         asset,
		checksumAsset: checksum,
	}, nil
}

func (u *systemUpdater) fetchLatestRelease(ctx context.Context) (*systemGitHubRelease, error) {
	now := time.Now()
	u.releaseCacheMu.Lock()
	defer u.releaseCacheMu.Unlock()

	if u.releaseCache != nil && now.Before(u.releaseCacheExpiresAt) {
		return cloneSystemGitHubRelease(u.releaseCache), nil
	}

	release, err := u.client.FetchLatestRelease(ctx)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, fmt.Errorf("GitHub release response is empty")
	}

	u.releaseCache = cloneSystemGitHubRelease(release)
	u.releaseCacheExpiresAt = now.Add(systemUpdateReleaseCacheTTL)

	return cloneSystemGitHubRelease(release), nil
}

func cloneSystemGitHubRelease(release *systemGitHubRelease) *systemGitHubRelease {
	if release == nil {
		return nil
	}
	cloned := *release
	cloned.Assets = append([]systemGitHubAsset(nil), release.Assets...)
	return &cloned
}

func (u *systemUpdater) applyBinaryUpdate(ctx context.Context, inspection *systemUpdateInspection) (string, string, error) {
	asset := inspection.asset
	if asset == nil {
		return "", "", fmt.Errorf("update asset is empty")
	}
	if err := validateSystemUpdateURL(asset.BrowserDownloadURL); err != nil {
		return "", "", fmt.Errorf("release asset URL is not trusted: %w", err)
	}
	if inspection.checksumAsset != nil {
		if err := validateSystemUpdateURL(inspection.checksumAsset.BrowserDownloadURL); err != nil {
			return "", "", fmt.Errorf("checksum URL is not trusted: %w", err)
		}
	}

	exePath, err := u.executablePath()
	if err != nil {
		return "", "", fmt.Errorf("failed to get the current executable path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	exeDir := filepath.Dir(exePath)

	tempDir, err := os.MkdirTemp(exeDir, ".codex2api-update-*")
	if err != nil {
		return "", "", fmt.Errorf("failed to create the update temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	archivePath := filepath.Join(tempDir, filepath.Base(asset.Name))
	if err := u.client.DownloadFile(ctx, asset.BrowserDownloadURL, archivePath, systemUpdateMaxDownloadBytes); err != nil {
		return "", "", fmt.Errorf("failed to download the update package: %w", err)
	}
	if err := verifySystemUpdateChecksum(ctx, u.client, archivePath, asset, inspection.checksumAsset); err != nil {
		return "", "", err
	}

	newBinaryPath := filepath.Join(tempDir, systemBinaryName(u.goos))
	if err := extractSystemUpdateBinary(archivePath, newBinaryPath); err != nil {
		return "", "", fmt.Errorf("failed to extract the update package: %w", err)
	}
	if err := os.Chmod(newBinaryPath, 0755); err != nil {
		return "", "", fmt.Errorf("failed to set execute permission on the new binary: %w", err)
	}

	backupPath := exePath + ".backup"
	if err := replaceExecutable(exePath, newBinaryPath, backupPath); err != nil {
		return "", "", err
	}
	return exePath, backupPath, nil
}

func (u *systemUpdater) scheduleRestart(exePath string) {
	restart := u.restartProcess
	if restart == nil {
		return
	}
	delay := u.restartDelay
	if delay < 0 {
		delay = 0
	}
	go func() {
		time.Sleep(delay)
		if err := restart(exePath); err != nil {
			log.Printf("restart after online update failed: %v", err)
		}
	}()
}

func (c *defaultSystemReleaseClient) FetchLatestRelease(ctx context.Context) (*systemGitHubRelease, error) {
	apiURL := "https://api.github.com/repos/" + systemUpdateRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", systemUpdateUserAgent)
	proxy.ApplyGithubAuth(req)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned HTTP %d", resp.StatusCode)
	}

	var release systemGitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}
	return &release, nil
}

func (c *defaultSystemReleaseClient) DownloadFile(ctx context.Context, rawURL, dest string, maxSize int64) error {
	if err := validateSystemUpdateURL(rawURL); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", systemUpdateUserAgent)

	resp, err := c.downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxSize {
		return fmt.Errorf("download is too large: %d bytes", resp.ContentLength)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	limited := io.LimitReader(resp.Body, maxSize+1)
	written, copyErr := io.Copy(out, limited)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dest)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return closeErr
	}
	if written > maxSize {
		_ = os.Remove(dest)
		return fmt.Errorf("download exceeds the size limit: %d bytes", maxSize)
	}
	return nil
}

func (c *defaultSystemReleaseClient) FetchText(ctx context.Context, rawURL string, maxSize int64) ([]byte, error) {
	if err := validateSystemUpdateURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", systemUpdateUserAgent)

	resp, err := c.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checksum download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("checksum file exceeds the size limit: %d bytes", maxSize)
	}
	return data, nil
}

func validateSystemUpdateURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("only HTTPS is allowed")
	}
	host := strings.ToLower(parsed.Hostname())
	switch host {
	case "github.com", "api.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com":
		return nil
	default:
		if strings.HasSuffix(host, ".githubusercontent.com") {
			return nil
		}
		return fmt.Errorf("download host not allowed: %s", host)
	}
}

func findSystemUpdateAsset(release *systemGitHubRelease, latestVersion, goos, goarch string) *systemGitHubAsset {
	if release == nil {
		return nil
	}
	prefix := fmt.Sprintf("codex2api_%s_%s_%s", strings.TrimPrefix(latestVersion, "v"), goos, goarch)
	for i := range release.Assets {
		asset := &release.Assets[i]
		name := strings.ToLower(asset.Name)
		if !strings.HasPrefix(name, strings.ToLower(prefix)) {
			continue
		}
		if goos == "windows" {
			if strings.HasSuffix(name, ".zip") {
				return asset
			}
			continue
		}
		if strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") {
			return asset
		}
	}
	return nil
}

func findSystemChecksumAsset(release *systemGitHubRelease) *systemGitHubAsset {
	if release == nil {
		return nil
	}
	for i := range release.Assets {
		asset := &release.Assets[i]
		if strings.EqualFold(asset.Name, "SHA256SUMS.txt") || strings.EqualFold(asset.Name, "sha256sums.txt") {
			return asset
		}
	}
	return nil
}

func verifySystemUpdateChecksum(ctx context.Context, client systemReleaseClient, filePath string, asset *systemGitHubAsset, checksumAsset *systemGitHubAsset) error {
	actual, err := sha256File(filePath)
	if err != nil {
		return fmt.Errorf("failed to compute the update package checksum: %w", err)
	}
	if asset != nil && strings.HasPrefix(strings.ToLower(asset.Digest), "sha256:") {
		expected := strings.TrimPrefix(strings.ToLower(asset.Digest), "sha256:")
		if actual != expected {
			return fmt.Errorf("update package checksum mismatch: expected %s, got %s", expected, actual)
		}
		return nil
	}
	if checksumAsset == nil {
		return fmt.Errorf("release provides no SHA256 checksum")
	}
	data, err := client.FetchText(ctx, checksumAsset.BrowserDownloadURL, 2*1024*1024)
	if err != nil {
		return fmt.Errorf("failed to download SHA256SUMS.txt: %w", err)
	}
	expected, ok := checksumForFile(data, filepath.Base(filePath))
	if !ok {
		return fmt.Errorf("%s not found in SHA256SUMS.txt", filepath.Base(filePath))
	}
	if actual != expected {
		return fmt.Errorf("update package checksum mismatch: expected %s, got %s", expected, actual)
	}
	return nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func checksumForFile(data []byte, name string) (string, bool) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if fields[1] == name {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

func extractSystemUpdateBinary(archivePath, destPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if strings.Contains(hdr.Name, "..") || filepath.IsAbs(hdr.Name) {
			return fmt.Errorf("update package contains an unsafe path: %s", hdr.Name)
		}
		if filepath.Base(hdr.Name) != "codex2api" {
			continue
		}
		if hdr.Size > systemUpdateMaxDownloadBytes {
			return fmt.Errorf("binary in the update package is too large: %d bytes", hdr.Size)
		}
		out, err := os.Create(destPath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(tr, systemUpdateMaxDownloadBytes+1))
		closeErr := out.Close()
		if copyErr != nil {
			_ = os.Remove(destPath)
			return copyErr
		}
		if closeErr != nil {
			_ = os.Remove(destPath)
			return closeErr
		}
		return nil
	}
	return fmt.Errorf("codex2api binary not found in the update package")
}

func replaceExecutable(currentPath, newPath, backupPath string) error {
	_ = os.Remove(backupPath)
	if err := os.Rename(currentPath, backupPath); err != nil {
		return fmt.Errorf("failed to back up the current binary: %w", err)
	}
	if err := os.Rename(newPath, currentPath); err != nil {
		if restoreErr := os.Rename(backupPath, currentPath); restoreErr != nil {
			return fmt.Errorf("failed to replace the binary and failed to restore the backup: %w (restore: %v)", err, restoreErr)
		}
		return fmt.Errorf("failed to replace the binary, the old version was restored: %w", err)
	}
	return nil
}

func systemBinaryName(goos string) string {
	if goos == "windows" {
		return "codex2api.exe"
	}
	return "codex2api"
}

func normalizeSystemVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "refs/tags/")
	v = strings.TrimPrefix(v, "V")
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return ""
	}
	return v
}

func compareSystemVersions(a, b string) int {
	av, okA := parseSystemVersion(a)
	bv, okB := parseSystemVersion(b)
	if !okA && !okB {
		return strings.Compare(a, b)
	}
	if !okA {
		return -1
	}
	if !okB {
		return 1
	}
	for i := 0; i < 3; i++ {
		if av[i] < bv[i] {
			return -1
		}
		if av[i] > bv[i] {
			return 1
		}
	}
	return 0
}

func parseSystemVersion(v string) ([3]int, bool) {
	var result [3]int
	v = normalizeSystemVersion(v)
	if idx := strings.IndexAny(v, "+-"); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return result, false
	}
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" {
			return result, false
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return result, false
		}
		result[i] = n
	}
	return result, true
}
