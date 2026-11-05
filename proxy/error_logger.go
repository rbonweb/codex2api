package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/security"
)

// fileLogger is a single log file instance
type fileLogger struct {
	once   sync.Once
	logger *log.Logger
	file   *os.File
	path   string
}

var (
	badRequestLogger  = &fileLogger{path: "bad_request.log"}  // 400 errors
	serverErrorLogger = &fileLogger{path: "server_error.log"} // 5xx errors
)

const defaultLogDir = "logs"
const upstreamErrorLogBodyMaxBytes = 8 * 1024

func upstreamErrorLogBody(body []byte) string {
	if len(strings.TrimSpace(string(body))) > 0 && !json.Valid(body) {
		return fmt.Sprintf("[non-JSON upstream body omitted; %d bytes]", len(body))
	}
	truncated := false
	if len(body) > upstreamErrorLogBodyMaxBytes {
		body = body[:upstreamErrorLogBodyMaxBytes]
		truncated = true
	}
	bodyStr := security.MaskSensitiveData(string(body))
	bodyStr = security.SafeTruncate(bodyStr, 5000)
	if truncated {
		bodyStr += " ... [truncated]"
	}
	return bodyStr
}

func errorLogDir() string {
	if dir := strings.TrimSpace(os.Getenv("LOG_DIR")); dir != "" {
		return dir
	}
	return defaultLogDir
}

func (fl *fileLogger) init() *log.Logger {
	if security.FileLogsDisabled() {
		return nil
	}
	fl.once.Do(func() {
		dir := errorLogDir()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Printf("failed to create log directory: %v", err)
			return
		}
		f, err := os.OpenFile(filepath.Join(dir, fl.path), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Printf("failed to open log file %s: %v", fl.path, err)
			return
		}
		fl.file = f
		fl.logger = log.New(f, "", 0)
	})
	return fl.logger
}

func (fl *fileLogger) close() {
	if fl.file != nil {
		if err := fl.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close log file %s: %v\n", fl.path, err)
		}
	}
}

// writeEntry writes one error log entry (sensitive information is automatically redacted)
func (fl *fileLogger) writeEntry(endpoint string, statusCode int, model string, accountID int64, body []byte) {
	l := fl.init()
	if l == nil {
		return
	}

	// Redact the log content
	safeEndpoint := security.SanitizeLog(endpoint)
	safeModel := security.SanitizeLog(model)
	bodyStr := upstreamErrorLogBody(body)

	ts := time.Now().Format("2006/01/02 15:04:05")
	l.Printf("========== %s ==========\nEndpoint: %s\nStatus: %d\nModel: %s\nAccount: %d\nResponse:\n%s\n",
		ts, safeEndpoint, statusCode, safeModel, accountID, bodyStr)
}

// logUpstreamError routes to the matching log file based on the status code
func logUpstreamError(endpoint string, statusCode int, model string, accountID int64, body []byte) {
	switch {
	case statusCode == 400:
		badRequestLogger.writeEntry(endpoint, statusCode, model, accountID, body)
	case statusCode >= 500:
		serverErrorLogger.writeEntry(endpoint, statusCode, model, accountID, body)
	}
}

// CloseErrorLogger closes all error log files (called when the program exits)
func CloseErrorLogger() {
	badRequestLogger.close()
	serverErrorLogger.close()
}
