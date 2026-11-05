package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newTestAdminHandlerFixture(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	handler, db, _ := newTestAdminHandlerFixtureWithPath(t)
	return handler, db
}

func newTestAdminHandlerFixtureWithPath(t *testing.T) (*Handler, *database.DB, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "admin-output-guard-test.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("new test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	return handler, db, dbPath
}

// execAdminOutputGuardTestSQLite runs one raw SQL statement on the test database, to simulate protection column
// read/write failures (for example a missing schema) without affecting the other system_settings columns.
func execAdminOutputGuardTestSQLite(t *testing.T, dbPath, statement string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite db: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(statement); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func queryAdminOutputGuardTestSQLiteBool(t *testing.T, dbPath, query string) bool {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite db: %v", err)
	}
	defer func() { _ = raw.Close() }()
	var value int
	if err := raw.QueryRow(query).Scan(&value); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return value != 0
}

// lockedLogBuffer is a concurrency-safe log capture: the handler and background goroutines may both write
// to the standard logger, so test assertions must take the lock when reading.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func invokeAdminSettingsEndpoint(t *testing.T, handler *Handler, method string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var bodyReader *strings.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload failed: %v", err)
		}
		bodyReader = strings.NewReader(string(body))
	} else {
		bodyReader = strings.NewReader("")
	}
	c.Request = httptest.NewRequest(method, "/api/admin/settings", bodyReader)
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		handler.GetSettings(c)
	} else {
		handler.UpdateSettings(c)
	}
	return w
}

func TestAdminOutputGuardSettings_GetSymmetricFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newTestAdminHandlerFixture(t)

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodGet, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	fields := []string{
		"output_guard_enabled", "output_guard_max_tool_argument_bytes",
		"output_guard_max_tool_argument_deltas", "output_guard_max_channel_bytes",
		"output_guard_max_request_bytes", "output_guard_config_generation",
	}
	for _, f := range fields {
		if _, ok := resp[f]; !ok {
			t.Fatalf("expected field %q in GET /api/admin/settings response", f)
		}
	}
	if resp["output_guard_enabled"] != false {
		t.Fatalf("expected default enabled = false, got %v", resp["output_guard_enabled"])
	}
	if int64(resp["output_guard_max_tool_argument_bytes"].(float64)) != 262144 {
		t.Fatalf("expected default tool bytes 262144, got %v", resp["output_guard_max_tool_argument_bytes"])
	}
}

func TestAdminOutputGuardSettings_PutValidationAndReadonlyRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)

	// 1. Submitting a read-only generation must return 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_config_generation": 999})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "read-only") {
			t.Fatalf("expected 400 with readonly rejection, got code=%d body=%s", w.Code, w.Body.String())
		}
	}
	// 2. A value outside the valid range of a single field must return 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_bytes": 1024})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on out of range tool bytes, got %d", w.Code)
		}
	}
	// 3. Violating the tool <= channel <= request level validation must return 400 and must not rewrite the DB
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
			"output_guard_max_tool_argument_bytes": 3145728,
			"output_guard_max_channel_bytes":       2097152,
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on hierarchy violation, got %d", w.Code)
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.MaxToolArgumentBytes == 3145728 {
			t.Fatalf("database should not be updated on illegal hierarchy validation, err=%v", err)
		}
	}
	// 4. An explicit true is persisted accurately and hot-applied
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_enabled": true})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on explicit true update, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["output_guard_enabled"] != true {
			t.Fatalf("expected output_guard_enabled = true in response, got %v", resp["output_guard_enabled"])
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.Enabled == nil || *current.Enabled != true {
			t.Fatalf("database must store explicit true")
		}
	}
	// 5. Partial field update isolation: updating only deltas leaves other fields unchanged, and they are hot-applied immediately
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_deltas": 8192})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on partial update, got %d", w.Code)
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if int(resp["output_guard_max_tool_argument_deltas"].(float64)) != 8192 || resp["output_guard_enabled"] != true {
			t.Fatalf("expected deltas = 8192 and preserved enabled=true, got %v", resp)
		}
		if runtimeCfg := proxy.CurrentOutputGuardConfig(); runtimeCfg.MaxToolArgumentDeltas != 8192 {
			t.Fatalf("runtime snapshot must be immediately updated, got %d", runtimeCfg.MaxToolArgumentDeltas)
		}
	}
}

// TestAdminOutputGuardSettings_NotPersistedOrAppliedWhenLaterValidationFails covers
// the partial-commit regression where protection settings are persisted and hot-applied before later validation: when a later field validation in the same request
// fails, the protection settings must be neither written to the DB nor hot-applied.
func TestAdminOutputGuardSettings_NotPersistedOrAppliedWhenLaterValidationFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()

	beforeDB, err := db.GetOutputGuardSettings(ctx)
	if err != nil {
		t.Fatalf("read guard settings before request: %v", err)
	}
	beforeRuntime := proxy.CurrentOutputGuardConfig()

	// The validation of models_list_read_max_bytes happens after the protection settings; when it fails, the whole save
	// request returns 400, and the protection settings must not leave any trace of being committed or applied.
	w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"output_guard_enabled":       true,
		"models_list_read_max_bytes": 1,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on invalid models_list_read_max_bytes, got %d, body: %s", w.Code, w.Body.String())
	}

	afterDB, err := db.GetOutputGuardSettings(ctx)
	if err != nil {
		t.Fatalf("read guard settings after request: %v", err)
	}
	if afterDB.ConfigGeneration != beforeDB.ConfigGeneration ||
		afterDB.MaxToolArgumentBytes != beforeDB.MaxToolArgumentBytes ||
		afterDB.MaxChannelBytes != beforeDB.MaxChannelBytes ||
		afterDB.MaxRequestBytes != beforeDB.MaxRequestBytes ||
		afterDB.MaxToolArgumentDeltas != beforeDB.MaxToolArgumentDeltas ||
		afterDB.Enabled == nil || beforeDB.Enabled == nil || *afterDB.Enabled != *beforeDB.Enabled {
		t.Fatalf("guard settings must not be committed when later validation fails: before=%+v after=%+v", beforeDB, afterDB)
	}

	afterRuntime := proxy.CurrentOutputGuardConfig()
	if afterRuntime.Generation != beforeRuntime.Generation ||
		afterRuntime.Enabled != beforeRuntime.Enabled ||
		afterRuntime.MaxToolArgumentBytes != beforeRuntime.MaxToolArgumentBytes {
		t.Fatalf("guard runtime config must not be applied when later validation fails: before=%+v after=%+v", beforeRuntime, afterRuntime)
	}
}

// TestAdminOutputGuardSettings_GetSurfacesReadFailure covers F1: a failure to read protection settings
// must not silently fall back to DefaultOutputGuardSettings (that would make the UI show enabled=false,
// while the proxy may still be using the enabled old config); it must return an explicit error and log it.
func TestAdminOutputGuardSettings_GetSurfacesReadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, dbPath := newTestAdminHandlerFixtureWithPath(t)
	execAdminOutputGuardTestSQLite(t, dbPath,
		"ALTER TABLE system_settings DROP COLUMN output_guard_config_generation")

	var logs lockedLogBuffer
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodGet, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when guard settings read fails, got %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Failed to read guard settings") {
		t.Fatalf("expected explicit guard read failure in response, got: %s", body)
	}
	if strings.Contains(body, "output_guard_enabled") {
		t.Fatalf("must not silently return default guard settings on read failure, got: %s", body)
	}
	if logText := logs.String(); !strings.Contains(logText, "failed to read output guard settings") {
		t.Fatalf("expected guard read failure to be logged, got logs: %s", logText)
	}
}

// TestAdminOutputGuardSettings_UpdateDBErrorMapsTo500 covers F2: a failure to persist protection settings
// (not a validation error) must map to 500. Here one protection column is dropped, so the read-merge-validate step inside the transaction
// fails at its read step; it also verifies that the whole transaction rolls back and enabled is not written.
func TestAdminOutputGuardSettings_UpdateDBErrorMapsTo500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, dbPath := newTestAdminHandlerFixtureWithPath(t)
	execAdminOutputGuardTestSQLite(t, dbPath,
		"ALTER TABLE system_settings DROP COLUMN output_guard_max_channel_bytes")

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_enabled": true})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on database error during guard update, got %d, body: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Failed to save guard settings") {
		t.Fatalf("expected guard persistence failure in response, got: %s", w.Body.String())
	}
	if queryAdminOutputGuardTestSQLiteBool(t, dbPath, "SELECT output_guard_enabled FROM system_settings WHERE id = 1") {
		t.Fatalf("guard update must be rolled back when the transaction fails")
	}
}

// TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenGuardSaveFails covers review feedback:
// when UpdateSettings carries max_concurrency / max_retries / max_rate_limit_retries / global_rpm and the guard save fails (an invalid parameter returns 400, or a DB failure returns 500),
// the runtime config must keep its old values and must not be polluted; the database also keeps its old values; on the success path the values are hot-applied and persisted as usual.
func TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenGuardSaveFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()

	// Prepare the initial state: max_concurrency starts at 5, max_retries at 3, max_rate_limit_retries at 1, global_rpm at 100
	initMaxConcurrency := 5
	initMaxRetries := 3
	initMaxRateLimitRetries := 1
	initGlobalRPM := 100
	wInit := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":        initMaxConcurrency,
		"max_retries":            initMaxRetries,
		"max_rate_limit_retries": initMaxRateLimitRetries,
		"global_rpm":             initGlobalRPM,
	})
	if wInit.Code != http.StatusOK {
		t.Fatalf("init settings failed: code=%d body=%s", wInit.Code, wInit.Body.String())
	}
	if got := handler.store.GetMaxConcurrency(); got != initMaxConcurrency {
		t.Fatalf("expected store max_concurrency=%d, got %d", initMaxConcurrency, got)
	}
	if got := handler.store.GetMaxRetries(); got != initMaxRetries {
		t.Fatalf("expected store max_retries=%d, got %d", initMaxRetries, got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != initMaxRateLimitRetries {
		t.Fatalf("expected store max_rate_limit_retries=%d, got %d", initMaxRateLimitRetries, got)
	}
	if got := handler.rateLimiter.GetRPM(); got != initGlobalRPM {
		t.Fatalf("expected rate limiter rpm=%d, got %d", initGlobalRPM, got)
	}

	// 1. PUT max_concurrency: 99, max_retries: 7, max_rate_limit_retries: 8, global_rpm: 999 + an invalid output_guard_max_channel_bytes
	// Expect 400; the runtime keeps its old values, and so does the database
	wErr := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":                99,
		"max_retries":                    7,
		"max_rate_limit_retries":         8,
		"global_rpm":                     999,
		"output_guard_max_channel_bytes": 10, // below MinOutputGuardMaxChannelBytes (128KiB)
	})
	if wErr.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 on invalid guard settings, got %d, body: %s", wErr.Code, wErr.Body.String())
	}
	if got := handler.store.GetMaxConcurrency(); got != initMaxConcurrency {
		t.Fatalf("store max_concurrency must remain %d after guard validation failure, got %d", initMaxConcurrency, got)
	}
	if got := handler.store.GetMaxRetries(); got != initMaxRetries {
		t.Fatalf("store max_retries must remain %d after guard validation failure, got %d", initMaxRetries, got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != initMaxRateLimitRetries {
		t.Fatalf("store max_rate_limit_retries must remain %d after guard validation failure, got %d", initMaxRateLimitRetries, got)
	}
	if got := handler.rateLimiter.GetRPM(); got != initGlobalRPM {
		t.Fatalf("rate limiter rpm must remain %d after guard validation failure, got %d", initGlobalRPM, got)
	}
	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("get persisted settings: %v", err)
	}
	if persisted.MaxConcurrency != initMaxConcurrency {
		t.Fatalf("persisted max_concurrency must remain %d, got %d", initMaxConcurrency, persisted.MaxConcurrency)
	}
	if persisted.MaxRetries != initMaxRetries {
		t.Fatalf("persisted max_retries must remain %d, got %d", initMaxRetries, persisted.MaxRetries)
	}
	if persisted.MaxRateLimitRetries != initMaxRateLimitRetries {
		t.Fatalf("persisted max_rate_limit_retries must remain %d, got %d", initMaxRateLimitRetries, persisted.MaxRateLimitRetries)
	}
	if persisted.GlobalRPM != initGlobalRPM {
		t.Fatalf("persisted global_rpm must remain %d, got %d", initGlobalRPM, persisted.GlobalRPM)
	}

	// 2. Success path: PUT max_concurrency: 12, max_retries: 6, max_rate_limit_retries: 4, global_rpm: 250 + a valid output_guard_max_channel_bytes
	// Expect 200; the runtime updates immediately, and the database update is persisted
	targetChannelBytes := database.MinOutputGuardMaxChannelBytes * 2
	wSuccess := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":                12,
		"max_retries":                    6,
		"max_rate_limit_retries":         4,
		"global_rpm":                     250,
		"output_guard_max_channel_bytes": targetChannelBytes,
	})
	if wSuccess.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 on valid update, got %d, body: %s", wSuccess.Code, wSuccess.Body.String())
	}
	if got := handler.store.GetMaxConcurrency(); got != 12 {
		t.Fatalf("store max_concurrency must be updated to 12 on success, got %d", got)
	}
	if got := handler.store.GetMaxRetries(); got != 6 {
		t.Fatalf("store max_retries must be updated to 6 on success, got %d", got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != 4 {
		t.Fatalf("store max_rate_limit_retries must be updated to 4 on success, got %d", got)
	}
	if got := handler.rateLimiter.GetRPM(); got != 250 {
		t.Fatalf("rate limiter rpm must be updated to 250 on success, got %d", got)
	}
	persistedSuccess, err := db.GetSystemSettings(ctx)
	if err != nil || persistedSuccess == nil {
		t.Fatalf("get persisted settings on success: %v", err)
	}
	if persistedSuccess.MaxConcurrency != 12 {
		t.Fatalf("persisted max_concurrency must be 12 on success, got %d", persistedSuccess.MaxConcurrency)
	}
	if persistedSuccess.MaxRetries != 6 {
		t.Fatalf("persisted max_retries must be 6 on success, got %d", persistedSuccess.MaxRetries)
	}
	if persistedSuccess.MaxRateLimitRetries != 4 {
		t.Fatalf("persisted max_rate_limit_retries must be 4 on success, got %d", persistedSuccess.MaxRateLimitRetries)
	}
	if persistedSuccess.GlobalRPM != 250 {
		t.Fatalf("persisted global_rpm must be 250 on success, got %d", persistedSuccess.GlobalRPM)
	}
	// 3. 500 case: when a database error makes the guard save return 500, the runtime and the DB likewise keep their old values
	{
		handler500, db500, dbPath := newTestAdminHandlerFixtureWithPath(t)
		wInit500 := invokeAdminSettingsEndpoint(t, handler500, http.MethodPut, map[string]any{
			"max_concurrency":        7,
			"max_retries":            2,
			"max_rate_limit_retries": 1,
			"global_rpm":             50,
		})
		if wInit500.Code != http.StatusOK {
			t.Fatalf("init settings for 500 test failed: %d", wInit500.Code)
		}
		// Break the output_guard_max_channel_bytes column so that the guard update in the transaction reports a DB error
		execAdminOutputGuardTestSQLite(t, dbPath,
			"ALTER TABLE system_settings DROP COLUMN output_guard_max_channel_bytes")

		wErr500 := invokeAdminSettingsEndpoint(t, handler500, http.MethodPut, map[string]any{
			"max_concurrency":        88,
			"max_retries":            9,
			"max_rate_limit_retries": 5,
			"global_rpm":             888,
			"output_guard_enabled":   true,
		})
		if wErr500.Code != http.StatusInternalServerError {
			t.Fatalf("expected HTTP 500 on db error during guard update, got %d, body: %s", wErr500.Code, wErr500.Body.String())
		}
		if got := handler500.store.GetMaxConcurrency(); got != 7 {
			t.Fatalf("store max_concurrency must remain 7 after guard db failure, got %d", got)
		}
		if got := handler500.store.GetMaxRetries(); got != 2 {
			t.Fatalf("store max_retries must remain 2 after guard db failure, got %d", got)
		}
		if got := handler500.store.GetMaxRateLimitRetries(); got != 1 {
			t.Fatalf("store max_rate_limit_retries must remain 1 after guard db failure, got %d", got)
		}
		if got := handler500.rateLimiter.GetRPM(); got != 50 {
			t.Fatalf("rate limiter rpm must remain 50 after guard db failure, got %d", got)
		}
		persisted500, err := db500.GetSystemSettings(ctx)
		if err != nil || persisted500 == nil {
			t.Fatalf("get persisted settings after 500: %v", err)
		}
		if persisted500.MaxConcurrency != 7 {
			t.Fatalf("persisted max_concurrency must remain 7 after 500, got %d", persisted500.MaxConcurrency)
		}
		if persisted500.MaxRetries != 2 {
			t.Fatalf("persisted max_retries must remain 2 after 500, got %d", persisted500.MaxRetries)
		}
		if persisted500.MaxRateLimitRetries != 1 {
			t.Fatalf("persisted max_rate_limit_retries must remain 1 after 500, got %d", persisted500.MaxRateLimitRetries)
		}
		if persisted500.GlobalRPM != 50 {
			t.Fatalf("persisted global_rpm must remain 50 after 500, got %d", persisted500.GlobalRPM)
		}
	}
}

// TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenModelsListWriteFails covers review feedback:
// when the settings transaction has committed successfully, but the later independent write of models-list-read-max-bytes fails (returns 500),
// the system settings committed in the transaction, such as max_concurrency / max_retries, are already hot-applied once the transaction succeeds,
// so the runtime and the database stay consistent and must not keep the old values.
func TestAdminOutputGuardSettings_MaxConcurrencyNotChangedWhenModelsListWriteFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db, dbPath := newTestAdminHandlerFixtureWithPath(t)
	ctx := context.Background()

	// Initial state: max_concurrency is 5, max_retries is 2
	wInit := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency": 5,
		"max_retries":     2,
	})
	if wInit.Code != http.StatusOK {
		t.Fatalf("init settings failed: %d", wInit.Code)
	}

	// Install a trigger on SQLite that forbids modifying models_list_read_max_bytes, simulating UpdateModelsListReadMaxBytes throwing an error and returning 500
	execAdminOutputGuardTestSQLite(t, dbPath, `
		CREATE TRIGGER fail_models_list_update BEFORE UPDATE OF models_list_read_max_bytes ON system_settings
		BEGIN
			SELECT RAISE(ABORT, 'simulated models-list-read-max-bytes write failure');
		END;
	`)

	// PUT updates max_concurrency: 15, max_retries: 4, models_list_read_max_bytes: 16MiB (a valid value, but rejected by the trigger)
	wFail := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":            15,
		"max_retries":                4,
		"models_list_read_max_bytes": int64(16 << 20),
	})
	if wFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when models_list write fails, got %d, body: %s", wFail.Code, wFail.Body.String())
	}

	// Once the transaction commits successfully, it is hot-applied: max_concurrency must be hot-applied as 15 and max_retries as 4, matching the DB
	if got := handler.store.GetMaxConcurrency(); got != 15 {
		t.Fatalf("store max_concurrency should be applied on settings transaction success, expected 15, got %d", got)
	}
	if got := handler.store.GetMaxRetries(); got != 4 {
		t.Fatalf("store max_retries should be applied on settings transaction success, expected 4, got %d", got)
	}

	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("get persisted settings: %v", err)
	}
	if persisted.MaxConcurrency != 15 {
		t.Fatalf("persisted max_concurrency must be 15, got %d", persisted.MaxConcurrency)
	}
	if persisted.MaxRetries != 4 {
		t.Fatalf("persisted max_retries must be 4, got %d", persisted.MaxRetries)
	}
}

// TestAdminSettings_RuntimeLimitsAppliedWhenContinuousRetryFails covers T1 review feedback:
// the runtime retry limits are hot-applied before UpdateContinuousRetryPolicy: if that write fails and returns 500,
// the DB has already committed the new max_rate_limit_retries / max_retries / max_concurrency / global_rpm,
// so this instance's memory must already have been updated and applied right after the settings transaction committed successfully.
func TestAdminSettings_RuntimeLimitsAppliedWhenContinuousRetryFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db, dbPath := newTestAdminHandlerFixtureWithPath(t)
	ctx := context.Background()

	// Initial state: max_concurrency: 3, max_retries: 2, max_rate_limit_retries: 1, global_rpm: 60
	wInit := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":        3,
		"max_retries":            2,
		"max_rate_limit_retries": 1,
		"global_rpm":             60,
	})
	if wInit.Code != http.StatusOK {
		t.Fatalf("init settings failed: %d, body: %s", wInit.Code, wInit.Body.String())
	}

	// Install a trigger on SQLite that forbids modifying continuous_retry_policy, simulating UpdateContinuousRetryPolicy throwing an error and returning 500
	execAdminOutputGuardTestSQLite(t, dbPath, `
		CREATE TRIGGER fail_continuous_retry_update BEFORE UPDATE OF continuous_retry_policy ON system_settings
		BEGIN
			SELECT RAISE(ABORT, 'simulated continuous_retry_policy write failure');
		END;
	`)

	// PUT updates max_concurrency: 18, max_retries: 6, max_rate_limit_retries: 5, global_rpm: 300, continuous_retry_enabled: true
	wFail := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":          18,
		"max_retries":              6,
		"max_rate_limit_retries":   5,
		"global_rpm":               300,
		"continuous_retry_enabled": true,
	})
	if wFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when continuous_retry write fails, got %d, body: %s", wFail.Code, wFail.Body.String())
	}
	if !strings.Contains(wFail.Body.String(), "Failed to save continuous retry policy") {
		t.Fatalf("expected error message to contain 'Failed to save continuous retry policy', got: %s", wFail.Body.String())
	}

	// The settings transaction committed successfully: the runtime config must already be hot-applied, matching the DB
	if got := handler.store.GetMaxConcurrency(); got != 18 {
		t.Fatalf("store max_concurrency must be applied on settings transaction success, expected 18, got %d", got)
	}
	if got := handler.store.GetMaxRetries(); got != 6 {
		t.Fatalf("store max_retries must be applied on settings transaction success, expected 6, got %d", got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != 5 {
		t.Fatalf("store max_rate_limit_retries must be applied on settings transaction success, expected 5, got %d", got)
	}
	if got := handler.rateLimiter.GetRPM(); got != 300 {
		t.Fatalf("rate limiter rpm must be applied on settings transaction success, expected 300, got %d", got)
	}

	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("get persisted settings: %v", err)
	}
	if persisted.MaxConcurrency != 18 {
		t.Fatalf("persisted max_concurrency must be 18, got %d", persisted.MaxConcurrency)
	}
	if persisted.MaxRetries != 6 {
		t.Fatalf("persisted max_retries must be 6, got %d", persisted.MaxRetries)
	}
	if persisted.MaxRateLimitRetries != 5 {
		t.Fatalf("persisted max_rate_limit_retries must be 5, got %d", persisted.MaxRateLimitRetries)
	}
	if persisted.GlobalRPM != 300 {
		t.Fatalf("persisted global_rpm must be 300, got %d", persisted.GlobalRPM)
	}
}

// TestAdminSettings_UpdateSystemSettingsDBErrorReturns500 covers review feedback:
// when only max_concurrency / max_retries / max_rate_limit_retries / global_rpm are changed and UpdateSystemSettings fails,
// it must return 500, and neither the runtime nor the database may apply the change (it must not fall through to the success path returning 200).
func TestAdminSettings_UpdateSystemSettingsDBErrorReturns500(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db, dbPath := newTestAdminHandlerFixtureWithPath(t)
	ctx := context.Background()

	// Initial state: max_concurrency 3, max_retries 2, max_rate_limit_retries 1, global_rpm 50
	wInit := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":        3,
		"max_retries":            2,
		"max_rate_limit_retries": 1,
		"global_rpm":             50,
	})
	if wInit.Code != http.StatusOK {
		t.Fatalf("init settings failed: %d, body: %s", wInit.Code, wInit.Body.String())
	}

	// Install a trigger on SQLite that forbids modifying max_concurrency, simulating UpdateSystemSettings throwing an error and returning 500
	execAdminOutputGuardTestSQLite(t, dbPath, `
		CREATE TRIGGER fail_system_settings_update BEFORE UPDATE OF max_concurrency ON system_settings
		BEGIN
			SELECT RAISE(ABORT, 'simulated system_settings write failure');
		END;
	`)

	// Change only max_concurrency, max_retries, max_rate_limit_retries, global_rpm
	wFail := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
		"max_concurrency":        9,
		"max_retries":            5,
		"max_rate_limit_retries": 4,
		"global_rpm":             500,
	})
	if wFail.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 when UpdateSystemSettings fails, got %d, body: %s", wFail.Code, wFail.Body.String())
	}
	if !strings.Contains(wFail.Body.String(), "Failed to save system settings") {
		t.Fatalf("expected error message to contain 'Failed to save system settings', got: %s", wFail.Body.String())
	}

	// Assert the runtime config keeps its old values and is not hot-applied
	if got := handler.store.GetMaxConcurrency(); got != 3 {
		t.Fatalf("store max_concurrency must remain 3 after DB failure, got %d", got)
	}
	if got := handler.store.GetMaxRetries(); got != 2 {
		t.Fatalf("store max_retries must remain 2 after DB failure, got %d", got)
	}
	if got := handler.store.GetMaxRateLimitRetries(); got != 1 {
		t.Fatalf("store max_rate_limit_retries must remain 1 after DB failure, got %d", got)
	}
	if got := handler.rateLimiter.GetRPM(); got != 50 {
		t.Fatalf("rate limiter rpm must remain 50 after DB failure, got %d", got)
	}

	// Assert the database keeps its old values
	persisted, err := db.GetSystemSettings(ctx)
	if err != nil || persisted == nil {
		t.Fatalf("get persisted settings: %v", err)
	}
	if persisted.MaxConcurrency != 3 {
		t.Fatalf("persisted max_concurrency must remain 3, got %d", persisted.MaxConcurrency)
	}
	if persisted.MaxRetries != 2 {
		t.Fatalf("persisted max_retries must remain 2, got %d", persisted.MaxRetries)
	}
	if persisted.MaxRateLimitRetries != 1 {
		t.Fatalf("persisted max_rate_limit_retries must remain 1, got %d", persisted.MaxRateLimitRetries)
	}
	if persisted.GlobalRPM != 50 {
		t.Fatalf("persisted global_rpm must remain 50, got %d", persisted.GlobalRPM)
	}
}
