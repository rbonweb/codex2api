// proxy/output_guard_settings.go
package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	OutputGuardConfigPollInterval = 5 * time.Second
	OutputGuardConfigReadTimeout  = 3 * time.Second
)

// OutputGuardAppliedConfig is an immutable value snapshot of the runtime configuration of the model output guard.
// After the caller holds a copy, the background poller applying a new generation does not alter that request's view.
type OutputGuardAppliedConfig struct {
	Enabled               bool
	MaxToolArgumentBytes  int64
	MaxToolArgumentDeltas int
	MaxChannelBytes       int64
	MaxRequestBytes       int64
	Generation            int64
}

type OutputGuardConfigSyncStatus struct {
	LastSuccessfulSyncAt time.Time
	LastSyncError        string
}

type OutputGuardSettingsReader interface {
	GetOutputGuardSettings(context.Context) (database.OutputGuardSettings, error)
}

type outputGuardState struct {
	mu          sync.RWMutex
	config      OutputGuardAppliedConfig
	lastSyncAt  time.Time
	lastSyncErr string
	isReady     atomic.Bool
}

var globalOutputGuardState = &outputGuardState{
	config: OutputGuardAppliedConfig{
		Enabled:               false,
		MaxToolArgumentBytes:  262144,
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152,
		MaxRequestBytes:       4194304,
		Generation:            0,
	},
}

func IsOutputGuardReady() bool {
	return globalOutputGuardState.isReady.Load()
}

func CurrentOutputGuardConfig() OutputGuardAppliedConfig {
	globalOutputGuardState.mu.RLock()
	defer globalOutputGuardState.mu.RUnlock()
	return globalOutputGuardState.config
}

func GetOutputGuardConfigSyncStatus() OutputGuardConfigSyncStatus {
	globalOutputGuardState.mu.RLock()
	defer globalOutputGuardState.mu.RUnlock()
	return OutputGuardConfigSyncStatus{
		LastSuccessfulSyncAt: globalOutputGuardState.lastSyncAt,
		LastSyncError:        globalOutputGuardState.lastSyncErr,
	}
}

// ApplyOutputGuardSettings validates and applies one database snapshot.
// Invalid configs, and stale or duplicate generations, are rejected, and the last-known-good (LKG) config is kept.
func ApplyOutputGuardSettings(settings database.OutputGuardSettings) bool {
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		log.Printf("[output-guard] invalid config rejected, keeping LKG: %v", err)
		recordOutputGuardConfigSyncError(err)
		return false
	}

	globalOutputGuardState.mu.Lock()
	defer globalOutputGuardState.mu.Unlock()

	if settings.ConfigGeneration <= globalOutputGuardState.config.Generation {
		return false
	}

	enabledVal := false
	if settings.Enabled != nil {
		enabledVal = *settings.Enabled
	}

	globalOutputGuardState.config = OutputGuardAppliedConfig{
		Enabled:               enabledVal,
		MaxToolArgumentBytes:  settings.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: settings.MaxToolArgumentDeltas,
		MaxChannelBytes:       settings.MaxChannelBytes,
		MaxRequestBytes:       settings.MaxRequestBytes,
		Generation:            settings.ConfigGeneration,
	}
	globalOutputGuardState.isReady.Store(true)
	return true
}

// LoadOutputGuardSettings performs a synchronous cold-start read. On failure it stays not ready,
// and the background poller self-heals once the database recovers. It never blocks process startup.
func LoadOutputGuardSettings(ctx context.Context, reader OutputGuardSettingsReader) error {
	settings, err := readOutputGuardSettings(ctx, reader, OutputGuardConfigReadTimeout)
	if err != nil {
		// A cold-start failure must leave a sync error: outputGuardHTTPGateArmed decides
		// readiness from LastSyncError. If the record is skipped here because the parent ctx timed out or was cancelled,
		// then before the first background poll (about 5s) the HTTP entry lets requests through with the guard off,
		// so the cold-start 503 gate is ineffective.
		recordOutputGuardConfigSyncError(err)
		return err
	}
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		recordOutputGuardConfigSyncError(err)
		return err
	}
	ApplyOutputGuardSettings(settings)
	recordOutputGuardConfigSyncSuccess(time.Now())
	return nil
}

func StartOutputGuardConfigPoller(parent context.Context, db *database.DB) bool {
	if db == nil {
		return false
	}
	if parent == nil {
		parent = context.Background()
	}
	return db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, cancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(parent, cancel)
		defer func() {
			stopParent()
			cancel()
		}()
		runOutputGuardConfigPoller(
			taskCtx,
			db,
			OutputGuardConfigPollInterval,
			OutputGuardConfigReadTimeout,
		)
	})
}

func runOutputGuardConfigPoller(
	ctx context.Context,
	reader OutputGuardSettingsReader,
	interval time.Duration,
	readTimeout time.Duration,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = OutputGuardConfigPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = pollOutputGuardConfigOnce(ctx, reader, readTimeout)
		}
	}
}

func pollOutputGuardConfigOnce(
	ctx context.Context,
	reader OutputGuardSettingsReader,
	readTimeout time.Duration,
) error {
	settings, err := readOutputGuardSettings(ctx, reader, readTimeout)
	if err != nil {
		if ctx != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		recordOutputGuardConfigSyncError(err)
		return err
	}
	if err := database.ValidateOutputGuardSettings(settings); err != nil {
		recordOutputGuardConfigSyncError(err)
		return err
	}
	ApplyOutputGuardSettings(settings)
	recordOutputGuardConfigSyncSuccess(time.Now())
	return nil
}

func readOutputGuardSettings(
	parent context.Context,
	reader OutputGuardSettingsReader,
	timeout time.Duration,
) (database.OutputGuardSettings, error) {
	if reader == nil {
		return database.OutputGuardSettings{}, fmt.Errorf("output guard settings reader is nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return database.OutputGuardSettings{}, err
	}
	if timeout <= 0 || timeout > OutputGuardConfigReadTimeout {
		timeout = OutputGuardConfigReadTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return reader.GetOutputGuardSettings(ctx)
}

func recordOutputGuardConfigSyncSuccess(at time.Time) {
	globalOutputGuardState.mu.Lock()
	globalOutputGuardState.lastSyncAt = at
	globalOutputGuardState.lastSyncErr = ""
	globalOutputGuardState.mu.Unlock()
}

func recordOutputGuardConfigSyncError(err error) {
	if err == nil {
		return
	}
	globalOutputGuardState.mu.Lock()
	globalOutputGuardState.lastSyncErr = err.Error()
	globalOutputGuardState.mu.Unlock()
}

// RequireOutputGuardReadyHTTP hard-blocks requests with 503 when the cold-start config failed to load.
func RequireOutputGuardReadyHTTP(c *gin.Context) bool {
	if IsOutputGuardReady() {
		return true
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	api.SendErrorWithStatus(c, apiErr, http.StatusServiceUnavailable)
	return false
}

// RequireOutputGuardReadyWS, when not ready, first writes a protocol JSON error frame,
// then closes the connection with a 1013 CloseTryAgainLater control frame.
func RequireOutputGuardReadyWS(conn *websocket.Conn) bool {
	if IsOutputGuardReady() {
		return true
	}
	if conn == nil {
		return false
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	_ = writeResponsesWSErrorWithin(conn, apiErr, 2*time.Second)
	_ = conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "Service configuration unavailable"),
		time.Now().Add(time.Second),
	)
	_ = conn.Close()
	return false
}

// RequireOutputGuardReadyStream, when not ready, sends a 503 error frame to the associated stream.
func RequireOutputGuardReadyStream(conn *websocket.Conn, streamID string) bool {
	if IsOutputGuardReady() {
		return true
	}
	if conn == nil {
		return false
	}
	apiErr := api.NewAPIError(
		api.ErrCodeServiceUnavailable,
		"Service configuration unavailable; failed to load output guard settings",
		api.ErrorTypeServer,
	)
	_ = writeResponsesWSErrorForStream(conn, apiErr, streamID, 2*time.Second)
	return false
}
