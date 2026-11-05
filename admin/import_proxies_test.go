package admin

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newImportProxyTestHandler builds a Handler with a real sqlite database and an enabled proxy pool,
// because the core contract of registerImportedProxies (write the table, then sync the pool) must be verified end to end.
func newImportProxyTestHandler(t *testing.T) (*Handler, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "import-proxies.sqlite")
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

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, ProxyPoolEnabled: true})
	t.Cleanup(store.Stop)
	store.SetProxyPoolEnabled(true)
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatalf("ReloadProxyPool: %v", err)
	}
	return &Handler{db: db, store: store}, rawDB
}

func proxyURLsInTable(t *testing.T, rawDB *sql.DB) []string {
	t.Helper()
	rows, err := rawDB.Query(`SELECT url FROM proxies ORDER BY id`)
	if err != nil {
		t.Fatalf("query proxies: %v", err)
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			t.Fatalf("scan proxy: %v", err)
		}
		urls = append(urls, url)
	}
	return urls
}

// Deduplication + normalisation + skipping invalid entries. An account with an invalid proxy must have its proxyURL cleared, so that it can fall back
// to the form proxy, rather than stay bound to a URL that never entered the pool and become unschedulable.
func TestRegisterImportedProxiesDedupsAndSkipsInvalid(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)

	tokens := []importToken{
		{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"},
		{refreshToken: "rt-2", proxyURL: "  http://127.0.0.1:8080  "},
		{refreshToken: "rt-3", proxyURL: "socks5://127.0.0.1:1080"},
		{refreshToken: "rt-4", proxyURL: "not a proxy url"},
		{refreshToken: "rt-5"},
	}

	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 2 {
		t.Fatalf("inserted = %d, want 2 unique proxies", outcome.inserted)
	}
	if outcome.skipped != 1 {
		t.Fatalf("skipped = %d, want 1 invalid proxy", outcome.skipped)
	}
	if !strings.Contains(outcome.warning(), "invalid format") {
		t.Fatalf("warning = %q, want it to mention the invalid proxy", outcome.warning())
	}

	if got := proxyURLsInTable(t, rawDB); !reflect.DeepEqual(got, []string{"http://127.0.0.1:8080", "socks5://127.0.0.1:1080"}) {
		t.Fatalf("proxies table = %v, want the two unique URLs", got)
	}

	wantTokenProxies := []string{"http://127.0.0.1:8080", "http://127.0.0.1:8080", "socks5://127.0.0.1:1080", "", ""}
	for i, want := range wantTokenProxies {
		if tokens[i].proxyURL != want {
			t.Fatalf("tokens[%d].proxyURL = %q, want %q", i, tokens[i].proxyURL, want)
		}
	}
}

// Importing the same file again should not insert again and again.
func TestRegisterImportedProxiesIsIdempotent(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)
	newTokens := func() []importToken {
		return []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	}

	first, err := handler.registerImportedProxies(context.Background(), newTokens())
	if err != nil {
		t.Fatalf("first registerImportedProxies: %v", err)
	}
	second, err := handler.registerImportedProxies(context.Background(), newTokens())
	if err != nil {
		t.Fatalf("second registerImportedProxies: %v", err)
	}
	if first.inserted != 1 || second.inserted != 0 {
		t.Fatalf("inserted = %d then %d, want 1 then 0", first.inserted, second.inserted)
	}
	if got := proxyURLsInTable(t, rawDB); len(got) != 1 {
		t.Fatalf("proxies table = %v, want a single row", got)
	}
}

// This is the core race guard of the whole design: once the proxies are written to the table they must be synced into the in-memory proxy pool at once. Otherwise an account bound to
// a URL that is "in managedProxySet but not in proxyPoolSet" would be judged to have no usable egress and
// the whole batch would be unschedulable.
func TestRegisterImportedProxiesReloadsPoolSoAccountsAreSchedulable(t *testing.T) {
	handler, _ := newImportProxyTestHandler(t)
	store := handler.store

	// Precondition: before the import this URL is completely unknown to the proxy pool, so it is not mistaken for "managed but disabled".
	if store.ManagedProxyUnavailable("http://127.0.0.1:8080") {
		t.Fatal("proxy is unavailable before import, test premise is wrong")
	}

	tokens := []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.unusable != 0 {
		t.Fatalf("unusable = %d, want 0 right after registration", outcome.unusable)
	}
	// Key assertion: as soon as registerImportedProxies returns, the proxy is already in the enabled set, so writing the account
	// does not open a fail-closed window.
	if store.ManagedProxyUnavailable("http://127.0.0.1:8080") {
		t.Fatal("imported proxy is still unusable after registration: proxy pool was not reloaded")
	}
	if unusable := store.UnusableManagedProxies([]string{"http://127.0.0.1:8080"}); len(unusable) != 0 {
		t.Fatalf("UnusableManagedProxies = %v, want empty", unusable)
	}
}

// When the target already has the same URL disabled, ON CONFLICT DO NOTHING does not revive it, so an account bound to it
// is never schedulable. An explicit alert is required rather than silently importing a batch of dead accounts.
func TestRegisterImportedProxiesWarnsWhenTargetProxyIsDisabled(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)

	if _, err := handler.db.InsertProxies(context.Background(), []string{"http://127.0.0.1:8080"}, "pre-existing"); err != nil {
		t.Fatalf("seed proxy: %v", err)
	}
	if _, err := rawDB.Exec(`UPDATE proxies SET enabled = 0 WHERE url = ?`, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("disable seeded proxy: %v", err)
	}

	tokens := []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 0 {
		t.Fatalf("inserted = %d, want 0 (URL already present)", outcome.inserted)
	}
	if outcome.unusable != 1 {
		t.Fatalf("unusable = %d, want 1", outcome.unusable)
	}
	if !strings.Contains(outcome.warning(), "disabled or failed") {
		t.Fatalf("warning = %q, want it to flag the disabled target proxy", outcome.warning())
	}
	// The label stays as it was: an import must not quietly rewrite the metadata of a proxy that already exists on the target.
	var label string
	if err := rawDB.QueryRow(`SELECT label FROM proxies WHERE url = ?`, "http://127.0.0.1:8080").Scan(&label); err != nil {
		t.Fatalf("read label: %v", err)
	}
	if label != "pre-existing" {
		t.Fatalf("label = %q, want the pre-existing label untouched", label)
	}
}

// Proxies disabled at the source are always imported as enabled (copying the disabled state would make the accounts fail closed at once), but an alert is raised.
func TestRegisterImportedProxiesWarnsWhenSourceMarkedDisabled(t *testing.T) {
	handler, _ := newImportProxyTestHandler(t)
	disabled := false
	enabled := true

	tokens := []importToken{
		{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080", proxyEnabled: &disabled},
		{refreshToken: "rt-2", proxyURL: "http://127.0.0.1:8081", proxyEnabled: &enabled},
		// An old file has no proxy_enabled; it is treated as enabled and must not count toward alerts.
		{refreshToken: "rt-3", proxyURL: "http://127.0.0.1:8082"},
	}
	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 3 {
		t.Fatalf("inserted = %d, want 3", outcome.inserted)
	}
	if !strings.Contains(outcome.warning(), "disabled at the source") {
		t.Fatalf("warning = %q, want it to flag the source-disabled proxy", outcome.warning())
	}
	// Imported as enabled, so it is immediately usable for the proxy pool.
	if outcome.unusable != 0 {
		t.Fatalf("unusable = %d, want 0", outcome.unusable)
	}
}

// When the limit is exceeded, none are registered, and every token's proxy is cleared so that it falls back to the form value; a half-applied state is the hardest to explain.
func TestRegisterImportedProxiesRejectsWholeBatchOverCap(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)

	tokens := make([]importToken, 0, maxImportedProxies+1)
	for i := 0; i <= maxImportedProxies; i++ {
		tokens = append(tokens, importToken{
			refreshToken: fmt.Sprintf("rt-%d", i),
			proxyURL:     fmt.Sprintf("http://127.0.0.1:%d", 10000+i),
		})
	}

	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 0 {
		t.Fatalf("inserted = %d, want 0 when over the cap", outcome.inserted)
	}
	if outcome.skipped != maxImportedProxies+1 {
		t.Fatalf("skipped = %d, want %d", outcome.skipped, maxImportedProxies+1)
	}
	if !strings.Contains(outcome.warning(), "per-import limit") {
		t.Fatalf("warning = %q, want it to mention the cap", outcome.warning())
	}
	if got := proxyURLsInTable(t, rawDB); len(got) != 0 {
		t.Fatalf("proxies table has %d rows, want none", len(got))
	}
	for i := range tokens {
		if tokens[i].proxyURL != "" {
			t.Fatalf("tokens[%d].proxyURL = %q, want cleared", i, tokens[i].proxyURL)
		}
	}
}

// A proxy pool sync failure must abort the whole import: carrying on to write accounts would only produce a batch of unschedulable accounts.
func TestRegisterImportedProxiesFailsWhenPoolReloadFails(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)
	// Use a store with no loader configured, so ReloadProxyPool is bound to fail; the proxy table itself is still writable,
	// so that the path "table written but pool not synced" can be verified as really being blocked.
	brokenStore := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, ProxyPoolEnabled: true})
	t.Cleanup(brokenStore.Stop)
	brokenStore.SetProxyPoolEnabled(true)
	handler.store = brokenStore

	tokens := []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	if _, err := handler.registerImportedProxies(context.Background(), tokens); err == nil {
		t.Fatal("registerImportedProxies returned nil error, want the import to abort")
	}
	// The proxies are expected to have been written to the table already (the next import will skip them via ON CONFLICT); the key point is that the caller gets the error
	// and does not go on to write accounts.
	if got := proxyURLsInTable(t, rawDB); len(got) != 1 {
		t.Fatalf("proxies table = %v, want the row that was written before the reload failed", got)
	}
}

func TestCountDisabledAtSource(t *testing.T) {
	disabled := false
	enabled := true
	bindings := []importProxyBinding{
		{url: "http://a:8080", enabled: &disabled},
		// A proxy referenced by several accounts counts as one.
		{url: "http://a:8080", enabled: &disabled},
		{url: "http://b:8080", enabled: &enabled},
		{url: "http://c:8080"},
		{enabled: &disabled},
	}
	if got := countDisabledAtSource(bindings); got != 1 {
		t.Fatalf("countDisabledAtSource = %d, want 1", got)
	}
}

// A proxy in the file takes precedence over the form proxy; when the switch is off, proxies in the file have no effect at all.
func TestImportSettingsProxyForToken(t *testing.T) {
	fileToken := importToken{proxyURL: "http://from-file:8080"}
	bareToken := importToken{}

	cases := []struct {
		name     string
		settings importSettings
		token    importToken
		want     string
	}{
		{"file wins over form", importSettings{defaultProxyURL: "http://from-form:8080", importProxies: true}, fileToken, "http://from-file:8080"},
		{"form fills the gap", importSettings{defaultProxyURL: "http://from-form:8080", importProxies: true}, bareToken, "http://from-form:8080"},
		{"switch off ignores file", importSettings{defaultProxyURL: "http://from-form:8080"}, fileToken, "http://from-form:8080"},
		{"nothing configured", importSettings{}, fileToken, ""},
		{"form value is trimmed", importSettings{defaultProxyURL: "  http://from-form:8080  "}, bareToken, "http://from-form:8080"},
	}
	for _, tc := range cases {
		if got := tc.settings.proxyForToken(tc.token); got != tc.want {
			t.Fatalf("%s: proxyForToken = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Proxies brought in by the file are passive data and do not overwrite bindings that already exist on the target; a form proxy is the operator's explicit rebinding
// intent, so the existing overwrite semantics are kept.
func TestImportSettingsProxyOverwritePolicy(t *testing.T) {
	fileToken := importToken{proxyURL: "http://from-file:8080"}

	if got := (importSettings{importProxies: true}).proxyOverwritePolicyForToken(fileToken); got != preserveAccountProxy {
		t.Fatal("file-carried proxy should preserve the existing account binding")
	}
	if got := (importSettings{importProxies: true, defaultProxyURL: "http://from-form:8080"}).proxyOverwritePolicyForToken(importToken{}); got != overwriteAccountProxy {
		t.Fatal("form-entered proxy should keep overwriting the existing binding")
	}
	if got := (importSettings{defaultProxyURL: "http://from-form:8080"}).proxyOverwritePolicyForToken(fileToken); got != overwriteAccountProxy {
		t.Fatal("file proxy must not change the policy while the switch is off")
	}
}

// Regression lock: proxy fields must never go into the deduplication fingerprint. The same credentials with different proxies are still the same account,
// and including them in the fingerprint would make it be imported twice as two separate records.
func TestImportDedupIsUnaffectedByProxyFields(t *testing.T) {
	enabled := true
	base := importToken{
		refreshToken:     "rt-shared",
		accessToken:      "at-shared",
		email:            "user@example.com",
		chatgptAccountID: "workspace-1",
		planType:         "plus",
		// Fix the expiry time: the default is "now +1h", and two calls would differ by a few hundred nanoseconds, which would drown out the real
		// proxy field drift.
		expiresAt: "2026-08-31T12:00:00Z",
	}
	withProxy := base
	withProxy.proxyURL = "http://127.0.0.1:8080"
	withProxy.proxyLabel = "hk-1"
	withProxy.proxyEnabled = &enabled

	conflicts := conflictingImportChatGPTIDs([]importToken{base, withProxy})
	if len(conflicts) != 0 {
		t.Fatalf("conflictingImportChatGPTIDs = %v, want no conflict from proxy-only differences", conflicts)
	}
	if got, want := importTokenCredentialFingerprint(withProxy, conflicts), importTokenCredentialFingerprint(base, conflicts); got != want {
		t.Fatalf("credential fingerprint drifted with proxy: %q vs %q", got, want)
	}
	if got, want := importTokenOAuthIdentityKey(withProxy, conflicts), importTokenOAuthIdentityKey(base, conflicts); got != want {
		t.Fatalf("oauth identity key drifted with proxy: %q vs %q", got, want)
	}
	if got, want := importTokenSeed(withProxy, conflicts), importTokenSeed(base, conflicts); !reflect.DeepEqual(got, want) {
		t.Fatalf("credential seed drifted with proxy:\n got %+v\nwant %+v", got, want)
	}
}

// All three JSON shapes must carry the three proxy fields into importToken.
func TestParseImportJSONTokensStreamCarriesProxyFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"top-level array", `[{"refresh_token":"rt-1","email":"a@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}]`},
		{"single flat object", `{"refresh_token":"rt-1","email":"a@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}`},
		// A proxy is an account attribute. Some export implementations put it on the entry root and others stuff it into credentials, so both places must be read.
		{"sub2api wrapper, proxy on entry", `{"accounts":[{"credentials":{"refresh_token":"rt-1","email":"a@example.com"},"proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}]}`},
		{"sub2api wrapper, proxy in credentials", `{"accounts":[{"credentials":{"refresh_token":"rt-1","email":"a@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}}]}`},
	}
	for _, tc := range cases {
		tokens, err := parseImportJSONTokensStream(strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		if len(tokens) != 1 {
			t.Fatalf("%s: parsed %d tokens, want 1", tc.name, len(tokens))
		}
		tok := tokens[0]
		if tok.proxyURL != "http://127.0.0.1:8080" || tok.proxyLabel != "hk-1" {
			t.Fatalf("%s: proxy = (%q, %q), want (http://127.0.0.1:8080, hk-1)", tc.name, tok.proxyURL, tok.proxyLabel)
		}
		// An explicit false must be distinguishable from "field missing", or the disabled state at the source is silently lost.
		if tok.proxyEnabled == nil || *tok.proxyEnabled {
			t.Fatalf("%s: proxy_enabled = %v, want explicit false", tc.name, tok.proxyEnabled)
		}
	}
}

// When both the entry root and credentials carry a proxy, the entry root wins, and label/enabled follow the URL,
// so that metadata from the two places is never mixed into a wrongly paired set.
func TestSub2apiEntryProxyFieldsPreferEntryLevel(t *testing.T) {
	entryEnabled := true
	credentialEnabled := false
	entry := sub2apiAccountEntry{
		ProxyURL:     " http://entry:8080 ",
		ProxyLabel:   "entry-label",
		ProxyEnabled: &entryEnabled,
		Credentials: sub2apiAccountCredentials{
			ProxyURL:     "http://credentials:8080",
			ProxyLabel:   "credentials-label",
			ProxyEnabled: &credentialEnabled,
		},
	}
	url, label, enabled := entry.proxyFields()
	if url != "http://entry:8080" || label != "entry-label" || enabled == nil || !*enabled {
		t.Fatalf("proxyFields = (%q, %q, %v), want the entry-level trio", url, label, enabled)
	}

	// When there is no URL on the root, the whole group falls back to credentials.
	entry.ProxyURL = ""
	url, label, enabled = entry.proxyFields()
	if url != "http://credentials:8080" || label != "credentials-label" || enabled == nil || *enabled {
		t.Fatalf("proxyFields = (%q, %q, %v), want the credentials-level trio", url, label, enabled)
	}
}

// When an old file has no proxy fields, proxyEnabled stays nil (treated as enabled) and must not be filled with the zero value false.
func TestParseImportJSONTokensStreamLeavesProxyUnsetForLegacyFiles(t *testing.T) {
	tokens, err := parseImportJSONTokensStream(strings.NewReader(`[{"refresh_token":"rt-1","email":"a@example.com"}]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("parsed %d tokens, want 1", len(tokens))
	}
	if tokens[0].proxyURL != "" || tokens[0].proxyLabel != "" || tokens[0].proxyEnabled != nil {
		t.Fatalf("legacy entry produced proxy fields: %+v", tokens[0])
	}
}

// Agent Identity entries take a different branch, and they must carry the proxy too.
func TestParseImportJSONTokensStreamCarriesProxyForAgentIdentity(t *testing.T) {
	body := `[{"auth_mode":"agentIdentity","agent_runtime_id":"runtime-1","agent_private_key":"key-1",` +
		`"email":"agent@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":true}]`
	tokens, err := parseImportJSONTokensStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("parsed %d tokens, want 1", len(tokens))
	}
	if !tokens[0].isAgentIdentity() {
		t.Fatalf("token is not recognised as agent identity: %+v", tokens[0])
	}
	if tokens[0].proxyURL != "http://127.0.0.1:8080" || tokens[0].proxyLabel != "hk-1" {
		t.Fatalf("agent identity proxy = (%q, %q), want it carried through", tokens[0].proxyURL, tokens[0].proxyLabel)
	}
}

func TestImportedProxyLabelIsBatchScoped(t *testing.T) {
	label := importedProxyLabel()
	if !strings.HasPrefix(label, "imported-") || len(label) != len("imported-20060102-1504") {
		t.Fatalf("importedProxyLabel = %q, want an imported-<timestamp> batch label", label)
	}
}

// newImportEndToEndHandler builds a Handler with a "real sqlite + Store with the proxy pool enabled" setup,
// to verify end to end that after import the account really is bound to a proxy and is schedulable at once.
func newImportEndToEndHandler(t *testing.T) (*Handler, *auth.Store, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4", ProxyPoolEnabled: true,
	})
	t.Cleanup(store.Stop)
	store.SetProxyPoolEnabled(true)
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatalf("ReloadProxyPool: %v", err)
	}
	handler := &Handler{
		db:         db,
		store:      store,
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	return handler, store, db
}

func runImportAccountsCommon(t *testing.T, handler *Handler, tokens []importToken, settings importSettings) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)
	handler.importAccountsCommon(ctx, tokens, settings)
	return recorder
}

func accountRowByRefreshToken(t *testing.T, db *database.DB, refreshToken string) *database.AccountRow {
	t.Helper()
	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, row := range rows {
		if row.GetCredential("refresh_token") == refreshToken {
			return row
		}
	}
	t.Fatalf("no active account with refresh_token %q", refreshToken)
	return nil
}

// Full path: the file carries a proxy → the proxy is registered in the table and the in-memory pool → the account is bound to it → it is schedulable at once.
// This path is the whole purpose of the feature; missing any one step degrades it to "import succeeded but all accounts are dead".
func TestImportAccountsCommonBindsAndRegistersFileProxy(t *testing.T) {
	handler, store, db := newImportEndToEndHandler(t)

	recorder := runImportAccountsCommon(t, handler, []importToken{{
		name: "imported", refreshToken: "rt-file-proxy", accessToken: "at-file-proxy",
		email: "file@example.com", proxyURL: "http://127.0.0.1:8080",
	}}, importSettings{importProxies: true})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"proxies_imported":1`) {
		t.Fatalf("import stream did not report the registered proxy: %s", recorder.Body.String())
	}

	row := accountRowByRefreshToken(t, db, "rt-file-proxy")
	if row.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("account proxy_url = %q, want the file-carried proxy", row.ProxyURL)
	}

	// Key point: the proxy pool is synced and the account has a usable egress; it is not a dead account bound to a URL that never entered the pool.
	account := &auth.Account{DBID: row.ID, ProxyURL: row.ProxyURL}
	if !store.AccountHasUsableEgress(account) {
		t.Fatal("imported account has no usable egress: the proxy pool was not in sync when the account was written")
	}
	if got := store.ResolveProxyForAccount(account); got != "http://127.0.0.1:8080" {
		t.Fatalf("ResolveProxyForAccount = %q, want the imported proxy", got)
	}
}

// When the switch is off, proxies in the file have no effect at all: nothing is registered or bound, and the form proxy still applies as usual.
func TestImportAccountsCommonIgnoresFileProxyWhenSwitchOff(t *testing.T) {
	handler, _, db := newImportEndToEndHandler(t)

	recorder := runImportAccountsCommon(t, handler, []importToken{{
		name: "imported", refreshToken: "rt-switch-off", accessToken: "at-switch-off",
		email: "off@example.com", proxyURL: "http://from-file:8080",
	}}, importSettings{defaultProxyURL: "http://from-form:8080"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "proxies_imported") {
		t.Fatalf("import stream reported proxy registration while the switch is off: %s", recorder.Body.String())
	}

	if row := accountRowByRefreshToken(t, db, "rt-switch-off"); row.ProxyURL != "http://from-form:8080" {
		t.Fatalf("account proxy_url = %q, want the form-entered proxy", row.ProxyURL)
	}
	proxies, err := db.ListProxies(context.Background())
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(proxies) != 0 {
		t.Fatalf("proxies table has %d rows, want none while the switch is off", len(proxies))
	}
}

// Proxies brought in by the file do not overwrite bindings that already exist on the target: those may already have been finely allocated (for example by auto-balancing),
// and silently overwriting them counts as data corruption. A form proxy is the operator's explicit rebinding intent, so the overwrite semantics are kept.
func TestImportAccountsCommonProxyOverwriteSemantics(t *testing.T) {
	cases := []struct {
		name     string
		settings importSettings
		token    importToken
		want     string
	}{
		{
			name:     "file proxy preserves the existing binding",
			settings: importSettings{importProxies: true, allowDuplicate: true},
			token:    importToken{proxyURL: "http://from-file:8080"},
			want:     "http://existing:8080",
		},
		{
			name:     "form proxy still overwrites",
			settings: importSettings{defaultProxyURL: "http://from-form:8080", allowDuplicate: true},
			want:     "http://from-form:8080",
		},
		{
			name:     "empty proxy never clears the existing binding",
			settings: importSettings{importProxies: true, allowDuplicate: true},
			want:     "http://existing:8080",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, _, db := newImportEndToEndHandler(t)
			idToken := makeOAuthTestIDToken("bound@example.com", "acc-bound", "team")
			if _, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
				"refresh_token": "rt-old",
				"email":         "bound@example.com",
				"account_id":    "acc-bound",
				"workspace_id":  "acc-bound",
			}, "http://existing:8080"); err != nil {
				t.Fatalf("InsertAccountWithCredentials: %v", err)
			}

			token := tc.token
			token.refreshToken = "rt-new"
			token.accessToken = "at-new"
			token.idToken = idToken
			token.email = "bound@example.com"
			token.accountID = "acc-bound"
			token.planType = "team"

			recorder := runImportAccountsCommon(t, handler, []importToken{token}, tc.settings)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}

			rows, err := db.ListActive(context.Background())
			if err != nil {
				t.Fatalf("ListActive: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("active rows = %d, want the single updated account", len(rows))
			}
			if rows[0].ProxyURL != tc.want {
				t.Fatalf("account proxy_url = %q, want %q", rows[0].ProxyURL, tc.want)
			}
		})
	}
}
