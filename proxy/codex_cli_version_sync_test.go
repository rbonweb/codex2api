package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestExtractCodexCLIVersion(t *testing.T) {
	cases := map[string]string{
		"0.144.1":           "0.144.1",
		"v0.144.1":          "0.144.1",
		"rust-v0.144.1":     "0.144.1",
		"0.144.0-alpha.10":  "", // A prerelease with a hyphen is cut off by LastIndexByte, so this checks the invalid fallback
		"":                  "",
		"not-a-version":     "",
		"codex-cli-0.145.0": "0.145.0",
	}
	for in, want := range cases {
		if got := extractCodexCLIVersion(in); got != want {
			t.Errorf("extractCodexCLIVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchLatestCodexCLIVersion_PrefersNameThenTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"0.144.1","tag_name":"rust-v0.144.1","prerelease":false}`))
	}))
	defer server.Close()
	old := codexReleasesLatestURLForTest
	codexReleasesLatestURLForTest = server.URL
	defer func() { codexReleasesLatestURLForTest = old }()

	got, err := FetchLatestCodexCLIVersion(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchLatestCodexCLIVersion error: %v", err)
	}
	if got != "0.144.1" {
		t.Fatalf("version = %q, want 0.144.1", got)
	}
}

func TestFetchLatestCodexCLIVersion_TagFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"Some Release Title","tag_name":"rust-v0.150.0"}`))
	}))
	defer server.Close()
	old := codexReleasesLatestURLForTest
	codexReleasesLatestURLForTest = server.URL
	defer func() { codexReleasesLatestURLForTest = old }()

	got, err := FetchLatestCodexCLIVersion(context.Background(), "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got != "0.150.0" {
		t.Fatalf("version = %q, want 0.150.0", got)
	}
}

// effectiveLatestCodexCLIVersion is never lower than the built-in constant: when the remote returns an old version, the built-in constant is kept.
func TestEffectiveLatestCodexCLIVersion_NeverDowngrades(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	s := prev
	s.CodexSyncedCLIVersion = "0.100.0" // Well below the built-in version
	ApplyRuntimeSettings(s)
	if got := effectiveLatestCodexCLIVersion(); got != latestCodexCLIVersion {
		t.Fatalf("effective = %q, want builtin %q (no downgrade)", got, latestCodexCLIVersion)
	}

	s.CodexSyncedCLIVersion = "9.9.9" // Above the built-in version
	ApplyRuntimeSettings(s)
	if got := effectiveLatestCodexCLIVersion(); got != "9.9.9" {
		t.Fatalf("effective = %q, want 9.9.9 (upgrade honored)", got)
	}
}

// After syncing to a version above the built-in one, the generated outbound UA / version number should rise to that version.
func TestGeneratedCodexClientHeaders_UsesSyncedVersion(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	s := prev
	s.ClientCompatMode = ClientCompatModeForce
	s.CodexUserAgentConfig = "{}"
	s.CodexSyncedCLIVersion = "0.200.0"
	ApplyRuntimeSettings(s)

	ua, version := generatedCodexClientHeaders(nil, CurrentRuntimeSettings())
	if version != "0.200.0" {
		t.Fatalf("generated version = %q, want 0.200.0", version)
	}
	if !strings.Contains(ua, "0.200.0") {
		t.Fatalf("generated UA = %q, want to contain 0.200.0", ua)
	}
}

func TestUpdateRuntimeSettingsSerializesConcurrentFieldPatches(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	initial := DefaultRuntimeSettings()
	initial.ClientCompatMode = ClientCompatModePreserve
	initial.CodexSyncedCLIVersion = ""
	ApplyRuntimeSettings(initial)

	syncPatchEntered := make(chan struct{})
	releaseSyncPatch := make(chan struct{})
	syncPatchDone := make(chan struct{})
	go func() {
		UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings {
			close(syncPatchEntered)
			<-releaseSyncPatch
			settings.CodexSyncedCLIVersion = "9.9.9"
			return settings
		})
		close(syncPatchDone)
	}()
	<-syncPatchEntered

	adminPatchStarted := make(chan struct{})
	adminPatchDone := make(chan struct{})
	go func() {
		close(adminPatchStarted)
		UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings {
			settings.ClientCompatMode = ClientCompatModeForce
			return settings
		})
		close(adminPatchDone)
	}()
	<-adminPatchStarted

	adminPatchSerialized := true
	select {
	case <-adminPatchDone:
		adminPatchSerialized = false
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSyncPatch)
	<-syncPatchDone
	<-adminPatchDone

	if !adminPatchSerialized {
		t.Fatal("concurrent runtime patch completed while another patch held the update lock")
	}
	got := CurrentRuntimeSettings()
	if got.ClientCompatMode != ClientCompatModeForce || got.CodexSyncedCLIVersion != "9.9.9" {
		t.Fatalf("runtime settings = compat:%q synced:%q, want force / 9.9.9", got.ClientCompatMode, got.CodexSyncedCLIVersion)
	}
}

// generatedCodexClientHeaders is a test helper: it ignores version-unavailable errors and only asserts the generated result.
func generatedCodexClientHeaders(account *auth.Account, settings RuntimeSettings) (string, string) {
	ua, version, _ := generatedCodexClientHeadersChecked(account, settings)
	return ua, version
}
