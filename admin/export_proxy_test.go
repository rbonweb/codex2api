package admin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func exportRequestContext(t *testing.T, target string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	return c
}

// When include_proxy is omitted, the channel default applies; when passed explicitly, it overrides the default. Codex does not export by default
// proxies (proxy URLs often contain plaintext passwords).
func TestExportIncludeProxyParsing(t *testing.T) {
	cases := []struct {
		target       string
		defaultValue bool
		want         bool
	}{
		{"/api/admin/accounts/export", false, false},
		{"/api/admin/accounts/export", true, true},
		{"/api/admin/accounts/export?include_proxy=1", false, true},
		{"/api/admin/accounts/export?include_proxy=true", false, true},
		{"/api/admin/accounts/export?include_proxy=0", true, false},
		{"/api/admin/accounts/export?include_proxy=false", true, false},
		// An empty value counts as present and false, so the frontend can use include_proxy= to turn off a channel that is on by default.
		{"/api/admin/accounts/export?include_proxy=", true, false},
	}
	for _, tc := range cases {
		if got := exportIncludeProxy(exportRequestContext(t, tc.target), tc.defaultValue); got != tc.want {
			t.Fatalf("exportIncludeProxy(%q, default=%v) = %v, want %v", tc.target, tc.defaultValue, got, tc.want)
		}
	}
}

// When off, exported entries carry no proxy fields at all, and even a ProxyURL already on the account row is not written out.
func TestExportProxyResolverDisabledDropsEverything(t *testing.T) {
	resolver := exportProxyResolver{}
	url, label, enabled := resolver.resolve("http://user:pass@127.0.0.1:8080")
	if url != "" || label != "" || enabled != nil {
		t.Fatalf("resolve while disabled = (%q, %q, %v), want all empty", url, label, enabled)
	}
}

// When on: proxies that exist in the proxy table get label + enabled added; a custom proxy the account entered itself carries only the URL.
func TestExportProxyResolverEnabledResolvesTableMetadata(t *testing.T) {
	resolver := exportProxyResolver{include: true, byURL: map[string]*database.ProxyRow{
		"http://127.0.0.1:8080": {URL: "http://127.0.0.1:8080", Label: "hk-1", Enabled: false},
	}}

	url, label, enabled := resolver.resolve(" http://127.0.0.1:8080 ")
	if url != "http://127.0.0.1:8080" || label != "hk-1" {
		t.Fatalf("resolve(managed) = (%q, %q), want (http://127.0.0.1:8080, hk-1)", url, label)
	}
	if enabled == nil || *enabled {
		t.Fatalf("resolve(managed) enabled = %v, want explicit false", enabled)
	}

	url, label, enabled = resolver.resolve("socks5://127.0.0.1:1080")
	if url != "socks5://127.0.0.1:1080" || label != "" || enabled != nil {
		t.Fatalf("resolve(custom) = (%q, %q, %v), want URL only", url, label, enabled)
	}

	if url, label, enabled = resolver.resolve("   "); url != "" || label != "" || enabled != nil {
		t.Fatalf("resolve(blank) = (%q, %q, %v), want all empty", url, label, enabled)
	}
}

func codexExportRow() *database.AccountRow {
	return &database.AccountRow{
		ID:       11,
		Name:     "codex user",
		Platform: "openai",
		Enabled:  true,
		ProxyURL: "http://127.0.0.1:8080",
		Credentials: map[string]interface{}{
			"email":         "codex@example.com",
			"refresh_token": "rt-codex",
			"plan_type":     "plus",
		},
	}
}

// Exported entries must never leak a proxy URL while the switch is off.
func TestAccountRowToExportEntryOmitsProxyByDefault(t *testing.T) {
	codex, ok := accountRowToCPAExportEntry(codexExportRow(), exportProxyResolver{})
	if !ok {
		t.Fatal("codex export entry not produced")
	}
	if codex.ProxyURL != "" || codex.ProxyLabel != "" || codex.ProxyEnabled != nil {
		t.Fatalf("codex entry carried proxy while disabled: %+v", codex)
	}

}

// When on, Codex entries get the URL + label + enabled added.
func TestAccountRowToExportEntryCarriesProxyWhenEnabled(t *testing.T) {
	resolver := exportProxyResolver{include: true, byURL: map[string]*database.ProxyRow{
		"http://127.0.0.1:8080": {URL: "http://127.0.0.1:8080", Label: "hk-1", Enabled: true},
	}}

	// Go through the public dispatcher, and confirm the resolver is really passed through to the Codex branch rather than dropped midway.
	dispatched, ok := accountRowToExportEntry(codexExportRow(), resolver)
	if !ok {
		t.Fatal("codex export entry not produced")
	}
	codex, isCPA := dispatched.(cpaExportEntry)
	if !isCPA {
		t.Fatalf("dispatched entry type = %T, want cpaExportEntry", dispatched)
	}
	if codex.ProxyURL != "http://127.0.0.1:8080" || codex.ProxyLabel != "hk-1" {
		t.Fatalf("codex proxy = (%q, %q), want (http://127.0.0.1:8080, hk-1)", codex.ProxyURL, codex.ProxyLabel)
	}
	if codex.ProxyEnabled == nil || !*codex.ProxyEnabled {
		t.Fatalf("codex proxy_enabled = %v, want explicit true", codex.ProxyEnabled)
	}

}

// A failed read of the proxy table must not fail the whole export: degrade to carrying only the URL.
func TestNewExportProxyResolverDegradesWhenProxyTableUnavailable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "export-proxy-degrade.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	if _, err := rawDB.Exec(`DROP TABLE proxies`); err != nil {
		t.Fatalf("drop proxies: %v", err)
	}
	handler := &Handler{db: db}

	resolver := handler.newExportProxyResolver(context.Background(), true)
	if !resolver.include {
		t.Fatal("resolver.include = false, want export to continue with URL only")
	}
	url, label, enabled := resolver.resolve("http://127.0.0.1:8080")
	if url != "http://127.0.0.1:8080" || label != "" || enabled != nil {
		t.Fatalf("degraded resolve = (%q, %q, %v), want URL only", url, label, enabled)
	}
}

func TestNewExportProxyResolverDisabledSkipsQuery(t *testing.T) {
	// It must still not panic when db is nil: the off state should never touch the proxy table at all.
	handler := &Handler{}
	if resolver := handler.newExportProxyResolver(context.Background(), false); resolver.include {
		t.Fatal("resolver.include = true while include_proxy is off")
	}
}
