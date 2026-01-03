package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DailyLogger manages logging with daily file rotation
type DailyLogger struct {
	mu          sync.Mutex
	baseLogPath string
	currentFile *os.File
	currentDate string
}

// NewDailyLogger creates a new logger that rotates log files daily
// logFile is the base path for log files (e.g., "/var/log/app.log")
// Daily files will be named like "/var/log/app-2024-01-15.log"
func NewDailyLogger(logFile string) (*DailyLogger, error) {
	dl := &DailyLogger{
		baseLogPath: logFile,
	}

	if err := dl.rotateIfNeeded(); err != nil {
		return nil, err
	}

	return dl, nil
}

// getLogFileName returns the log file name for a given date
func (dl *DailyLogger) getLogFileName(date string) string {
	ext := filepath.Ext(dl.baseLogPath)
	base := dl.baseLogPath[:len(dl.baseLogPath)-len(ext)]
	return fmt.Sprintf("%s-%s%s", base, date, ext)
}

// rotateIfNeeded checks if we need to rotate to a new file and does so if needed
func (dl *DailyLogger) rotateIfNeeded() error {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	return dl.rotateIfNeededLocked()
}

// rotateIfNeededLocked performs rotation while lock is already held
func (dl *DailyLogger) rotateIfNeededLocked() error {
	today := time.Now().Format("2006-01-02")

	// If we already have a file for today, no rotation needed
	if dl.currentDate == today && dl.currentFile != nil {
		return nil
	}

	// Close old file if exists
	if dl.currentFile != nil {
		dl.currentFile.Close()
	}

	// Open new file for today
	logFileName := dl.getLogFileName(today)
	f, err := os.OpenFile(logFileName, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", logFileName, err)
	}

	dl.currentFile = f
	dl.currentDate = today

	// Write rotation message directly to both outputs
	msg := fmt.Sprintf("Logging to console and file: %s\n", logFileName)
	os.Stdout.WriteString(msg)
	f.WriteString(msg)

	return nil
}

// Write implements io.Writer interface with automatic daily rotation
func (dl *DailyLogger) Write(p []byte) (n int, err error) {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	// Check if we need to rotate (date changed)
	today := time.Now().Format("2006-01-02")
	if dl.currentDate != today {
		if err := dl.rotateIfNeededLocked(); err != nil {
			// Log rotation failed, write to stdout only
			os.Stdout.Write(p)
			return len(p), nil
		}
	}

	// Write to both stdout and file
	os.Stdout.Write(p)
	if dl.currentFile != nil {
		dl.currentFile.Write(p)
	}
	return len(p), nil
}

// Close closes the current log file
func (dl *DailyLogger) Close() error {
	dl.mu.Lock()
	defer dl.mu.Unlock()

	if dl.currentFile != nil {
		return dl.currentFile.Close()
	}
	return nil
}

// SetupLogging configures logging to both console and file with daily rotation
// If logFile is empty, logs to console only
// Returns a closer function that should be called on shutdown
func SetupLogging(logFile string) (io.Closer, error) {
	// Set log format with timestamp
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	if logFile == "" {
		// Console only
		log.SetOutput(os.Stdout)
		return nil, nil
	}

	// Create daily logger
	dl, err := NewDailyLogger(logFile)
	if err != nil {
		return nil, err
	}

	// Set the daily logger as the output
	log.SetOutput(dl)

	return dl, nil
}
