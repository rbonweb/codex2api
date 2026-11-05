// proxy/acceptance_config_test.go
package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// Scenario 22: 5-second background revision polling, in-flight snapshot isolation, hard blocking of cold start with 503/1013, LKG retention
func TestAcceptanceScenario22_RuntimePollerLKGAndColdStart503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetOutputGuardStateForTest()

	// 1. Cold-start not-ready block: HTTP returns 503 service_unavailable
	t.Run("ColdStart503Block", func(t *testing.T) {
		SetOutputGuardReadyForTest(false)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

		if RequireOutputGuardReadyHTTP(c) {
			t.Fatalf("expected cold start HTTP request to be blocked, but allowed")
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected HTTP 503, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), string(api.ErrCodeServiceUnavailable)) {
			t.Fatalf("expected error body to contain %q, got %s", api.ErrCodeServiceUnavailable, w.Body.String())
		}
	})

	// 2. Cold-start WebSocket not ready: first send a protocol error frame, then send a 1013 Close frame
	t.Run("ColdStartWebSocket1013Block", func(t *testing.T) {
		SetOutputGuardReadyForTest(false)
		upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = RequireOutputGuardReadyWS(conn)
		}))
		defer s.Close()

		wsURL := "ws" + strings.TrimPrefix(s.URL, "http")
		clientConn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("websocket dial failed: %v", err)
		}
		defer clientConn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		// Must read the JSON error frame first
		msgType, msg, err := clientConn.ReadMessage()
		if err != nil {
			t.Fatalf("expected error frame before close, got err: %v", err)
		}
		if msgType != websocket.TextMessage || !strings.Contains(string(msg), string(api.ErrCodeServiceUnavailable)) {
			t.Fatalf("expected service_unavailable error frame, got: %s", string(msg))
		}

		// Then must receive the 1013 CloseTryAgainLater control frame
		_, _, err = clientConn.ReadMessage()
		if err == nil {
			t.Fatalf("expected close error 1013, got nil")
		}
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater {
			t.Fatalf("expected CloseTryAgainLater (1013), got %v", err)
		}
	})

	// 3. In-flight old snapshot isolation; two Applys verify the old value copy, and an invalid config is rejected while the old snapshot / Ready is kept
	t.Run("InFlightSnapshotIsolationAndValueCopy", func(t *testing.T) {
		ResetOutputGuardStateForTest()
		enabledVal := true
		initial := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      1,
		}
		if !ApplyOutputGuardSettings(initial) {
			t.Fatalf("initial ApplyOutputGuardSettings failed")
		}

		// The in-flight request takes its snapshot when the turn starts
		inFlightSnapshot := CurrentOutputGuardConfig()
		if inFlightSnapshot.Generation != 1 || inFlightSnapshot.MaxToolArgumentBytes != 262144 {
			t.Fatalf("unexpected in-flight snapshot: %+v", inFlightSnapshot)
		}

		// The system applies the new config in the background (Generation 2)
		gen2Bytes := int64(524288)
		gen2 := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  gen2Bytes,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      2,
		}
		if !ApplyOutputGuardSettings(gen2) {
			t.Fatalf("second ApplyOutputGuardSettings failed")
		}

		// Assert that the old snapshot held by the in-flight request is an immutable value copy, never altered by the newly applied config
		if inFlightSnapshot.Generation != 1 {
			t.Fatalf("in-flight snapshot generation was mutated! want 1, got %d", inFlightSnapshot.Generation)
		}
		if inFlightSnapshot.MaxToolArgumentBytes != 262144 {
			t.Fatalf("in-flight snapshot bytes was mutated! want 262144, got %d", inFlightSnapshot.MaxToolArgumentBytes)
		}

		// A new request gets the latest snapshot
		newTurnSnapshot := CurrentOutputGuardConfig()
		if newTurnSnapshot.Generation != 2 || newTurnSnapshot.MaxToolArgumentBytes != 524288 {
			t.Fatalf("new turn snapshot want gen 2 / 524288 bytes, got %+v", newTurnSnapshot)
		}

		// Scenario 21/22 reinforcement: the runtime rejects an invalid value (for example tool > channel, or out of the allowed range), and strictly keeps the old snapshot and Ready state
		illegalSettings := database.OutputGuardSettings{
			Enabled:               &enabledVal,
			MaxToolArgumentBytes:  50000000, // over the 4MiB limit and greater than the channel
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
			ConfigGeneration:      3,
		}
		if ApplyOutputGuardSettings(illegalSettings) {
			t.Fatalf("expected ApplyOutputGuardSettings to reject illegal settings")
		}
		afterIllegalSnapshot := CurrentOutputGuardConfig()
		if afterIllegalSnapshot.Generation != 2 || afterIllegalSnapshot.MaxToolArgumentBytes != 524288 {
			t.Fatalf("LKG snapshot violated after applying illegal settings: got %+v, want gen 2 / 524288 bytes", afterIllegalSnapshot)
		}
		if !IsOutputGuardReady() {
			t.Fatalf("expected IsOutputGuardReady to remain true after illegal settings rejected")
		}
	})

	// 4. The 5-second background poller, synchronous refresh and DB failure keep the LKG
	t.Run("PollerLKGAndFailurePreservation", func(t *testing.T) {
		ResetOutputGuardStateForTest()
		db, err := database.New("sqlite", filepath.Join(t.TempDir(), "poller_test.db"))
		if err != nil {
			t.Fatal(err)
		}
		// database.DB.Close is not idempotent (a second internal close(db.logStop) call panics),
		// so a flag ensures cleanup closes it only once as a fallback, when fault injection has not already closed it.
		dbClosed := false
		t.Cleanup(func() {
			if !dbClosed {
				_ = db.Close()
			}
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		started := StartOutputGuardConfigPoller(ctx, db)
		if !started {
			t.Fatalf("failed to start poller")
		}

		// Trigger the first poll to make sure it is ready
		_ = PollOutputGuardConfigOnceForTest(ctx, db)
		if !IsOutputGuardReady() {
			t.Fatalf("expected output guard to be ready after poller initial load")
		}

		// Record the current LKG config
		lkgBefore := CurrentOutputGuardConfig()

		// Update the DB config
		newBytes := int64(524288)
		_, err = db.UpdateOutputGuardSettings(ctx, database.OutputGuardSettingsUpdate{
			MaxToolArgumentBytes: &newBytes,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Provide a single injected tick poll interface for fast sync, while also supporting generation polling sync with a timeout, to rule out sleeps without assertions
		_ = PollOutputGuardConfigOnceForTest(ctx, db)

		timeout := time.Now().Add(2 * time.Second)
		updated := false
		for time.Now().Before(timeout) {
			cfg := CurrentOutputGuardConfig()
			if cfg.MaxToolArgumentBytes == newBytes && cfg.Generation > lkgBefore.Generation {
				updated = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !updated {
			t.Fatalf("timed out waiting for poller generation update; current=%+v", CurrentOutputGuardConfig())
		}

		lkgCurrent := CurrentOutputGuardConfig()

		// Simulate a severe DB failure (such as closing the connection directly)
		if err := db.Close(); err != nil {
			t.Fatalf("close db for fault injection: %v", err)
		}
		dbClosed = true

		// Run a single poll to trigger a read failure
		err = PollOutputGuardConfigOnceForTest(ctx, db)
		if err == nil {
			t.Fatalf("expected poll on closed DB to fail")
		}

		// Assert that after the DB failure the last-known-good config is kept, without crashing, clearing, or falling back to zero values
		preserved := CurrentOutputGuardConfig()
		if preserved.Generation != lkgCurrent.Generation || preserved.MaxToolArgumentBytes != lkgCurrent.MaxToolArgumentBytes {
			t.Fatalf("LKG violated after DB failure: want %+v, got %+v", lkgCurrent, preserved)
		}
		if !IsOutputGuardReady() {
			t.Fatalf("output guard must remain ready on LKG after DB fault")
		}
	})
}

// Scenario 23: when the Guard is explicitly disabled, transport framing still applies hard limits to oversized lines and multi-line events, and keeps the existing WS 48MB capacity
func TestAcceptanceScenario23_FramingGuardOffStillBounded(t *testing.T) {
	// Explicitly turn off the semantic Guard switch: Enabled=false in the config, but the transport-layer framing hard limits must still apply independently.
	ResetOutputGuardStateForTest()
	t.Cleanup(ResetOutputGuardStateForTest)
	disabledVal := false
	if !ApplyOutputGuardSettings(database.OutputGuardSettings{
		Enabled:               &disabledVal,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		ConfigGeneration:      1,
	}) {
		t.Fatalf("failed to apply explicitly disabled output guard settings")
	}
	if CurrentOutputGuardConfig().Enabled {
		t.Fatalf("expected output guard Enabled=false for framing independence check")
	}

	// 1. With the semantic Guard switch off, an oversized SSE single line with no newline (8 MiB) is still blocked
	t.Run("SSELineCapRemainsActiveWithGuardOff", func(t *testing.T) {
		oversizedLine := strings.Repeat("a", MaxSSELineBytes+1024)
		err := ReadSSEStreamWithEvent(strings.NewReader(oversizedLine), func(event string, data []byte) bool {
			return true
		})
		if err == nil || !errors.Is(err, ErrSSELineTooLong) {
			t.Fatalf("expected ErrSSELineTooLong regardless of guard switch, got: %v", err)
		}
	})

	// 2. A multi-line event that accumulates over 8 MiB is still blocked
	t.Run("SSEEventCapRemainsActiveWithGuardOff", func(t *testing.T) {
		chunk := "data: " + strings.Repeat("b", 1<<20) + "\n"
		oversizedEvent := strings.Repeat(chunk, 9) + "\n"
		err := ReadSSEStreamWithEvent(strings.NewReader(oversizedEvent), func(event string, data []byte) bool {
			return true
		})
		if err == nil || !errors.Is(err, ErrSSEEventTooLarge) {
			t.Fatalf("expected ErrSSEEventTooLarge regardless of guard switch, got: %v", err)
		}
	})

	// 3. Bounded blocking of an oversized non-streaming Response Body (16 MiB) on the path that previously lacked it
	t.Run("ResponseBodyCapActive", func(t *testing.T) {
		oversizedBody := strings.Repeat("c", 1024)
		_, err := ReadBoundedResponseBody(strings.NewReader(oversizedBody), 512)
		if err == nil || !errors.Is(err, ErrResponseBodyTooLarge) {
			t.Fatalf("expected ErrResponseBodyTooLarge, got: %v", err)
		}
	})

	// 4. Verify the WebSocket capacity is still 48MB (security.MaxRequestBodySize), not wrongly tightened to 8MB
	t.Run("WSWireBudgetRemains48MB", func(t *testing.T) {
		if security.MaxRequestBodySize != 48*1024*1024 {
			t.Fatalf("WebSocket wire limit must remain 48MB (%d), got %d", 48*1024*1024, security.MaxRequestBodySize)
		}
	})
}
