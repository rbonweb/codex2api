package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AuditLogger is the security audit log recorder.
type AuditLogger struct {
	mu         sync.Mutex
	file       *os.File
	logDir     string
	logFile    string
	maxSize    int64
	maxBackups int
}

var (
	defaultAuditLogger *AuditLogger
	auditOnce          sync.Once
)

const defaultAuditLogDir = "logs/security"

// GetAuditLogger returns the default audit log recorder.
func GetAuditLogger() *AuditLogger {
	auditOnce.Do(func() {
		if FileLogsDisabled() {
			defaultAuditLogger = &AuditLogger{}
			return
		}
		defaultAuditLogger = NewAuditLogger(securityLogDir(), "audit.log", 100*1024*1024, 10)
	})
	return defaultAuditLogger
}

func securityLogDir() string {
	if dir := strings.TrimSpace(os.Getenv("SECURITY_LOG_DIR")); dir != "" {
		return dir
	}
	if dir := strings.TrimSpace(os.Getenv("LOG_DIR")); dir != "" {
		return filepath.Join(dir, "security")
	}
	return defaultAuditLogDir
}

// NewAuditLogger creates a new audit log recorder.
func NewAuditLogger(logDir, logFile string, maxSize int64, maxBackups int) *AuditLogger {
	if maxSize <= 0 {
		maxSize = 100 * 1024 * 1024 // 100MB
	}
	if maxBackups <= 0 {
		maxBackups = 10
	}

	al := &AuditLogger{
		logDir:     logDir,
		logFile:    logFile,
		maxSize:    maxSize,
		maxBackups: maxBackups,
	}

	// Initialize the log file.
	if err := al.init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize audit log: %v\n", err)
	}

	return al
}

// init initializes the log file.
func (al *AuditLogger) init() error {
	if err := os.MkdirAll(al.logDir, 0750); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath := filepath.Join(al.logDir, al.logFile)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	al.file = f
	return nil
}

// Write writes an audit log entry.
func (al *AuditLogger) Write(level, action, details string) {
	al.mu.Lock()
	defer al.mu.Unlock()

	if al.file == nil {
		return
	}

	timestamp := time.Now().Format(time.RFC3339)
	line := fmt.Sprintf("[%s] [%s] %s: %s\n", timestamp, level, action, details)

	// Check whether rotation is needed.
	if al.shouldRotate() {
		al.rotate()
	}

	al.file.WriteString(line)
}

// shouldRotate checks whether the log needs rotation.
func (al *AuditLogger) shouldRotate() bool {
	if al.file == nil {
		return false
	}

	info, err := al.file.Stat()
	if err != nil {
		return false
	}

	return info.Size() >= al.maxSize
}

// rotate performs log rotation.
func (al *AuditLogger) rotate() {
	if al.file != nil {
		if err := al.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close log file: %v\n", err)
		}
	}

	// Rotate the old logs.
	logPath := filepath.Join(al.logDir, al.logFile)
	for i := al.maxBackups - 1; i > 0; i-- {
		oldPath := fmt.Sprintf("%s.%d", logPath, i)
		newPath := fmt.Sprintf("%s.%d", logPath, i+1)
		if err := os.Rename(oldPath, newPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "failed to rename log file %s -> %s: %v\n", oldPath, newPath, err)
		}
	}

	// Rename the current log.
	if err := os.Rename(logPath, logPath+".1"); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "failed to rename current log file: %v\n", err)
	}

	// Create a new log file.
	if err := al.init(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize new log file: %v\n", err)
	}
}

// Close closes the audit log.
func (al *AuditLogger) Close() error {
	al.mu.Lock()
	defer al.mu.Unlock()

	if al.file != nil {
		return al.file.Close()
	}
	return nil
}

// SecurityAuditLog records a security audit log entry (convenience function).
func SecurityAuditLog(action, details string) {
	logger := GetAuditLogger()
	sanitizedDetails := SanitizeLog(details)
	logger.Write("SECURITY", action, sanitizedDetails)
}

// Info records an info-level audit log entry.
func Info(action, details string) {
	GetAuditLogger().Write("INFO", action, SanitizeLog(details))
}

// Warning records a warning-level audit log entry.
func Warning(action, details string) {
	GetAuditLogger().Write("WARNING", action, SanitizeLog(details))
}

// Error records an error-level audit log entry.
func Error(action, details string) {
	GetAuditLogger().Write("ERROR", action, SanitizeLog(details))
}

// Critical records a critical-level audit log entry.
func Critical(action, details string) {
	GetAuditLogger().Write("CRITICAL", action, SanitizeLog(details))
}
