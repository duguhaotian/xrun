package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger provides unified logging for xrun.
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
	level  LogLevel
}

// LogLevel represents logging level.
type LogLevel int

const (
	DEBUG LogLevel = iota
	INFO
	WARN
	ERROR
)

var (
	defaultLogger *Logger
	once          sync.Once
)

// Init initializes the default logger.
func Init(dataDir string) error {
	var err error
	once.Do(func() {
		logFile := filepath.Join(dataDir, "xrun.log")

		// Ensure directory exists
		if err = os.MkdirAll(dataDir, 0755); err != nil {
			return
		}

		// Open log file
		f, openErr := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if openErr != nil {
			err = openErr
			return
		}

		defaultLogger = &Logger{
			writer: f,
			level:  INFO,
		}
	})
	return err
}

// GetLogger returns the default logger.
func GetLogger() *Logger {
	if defaultLogger == nil {
		defaultLogger = &Logger{
			writer: os.Stderr,
			level:  INFO,
		}
	}
	return defaultLogger
}

// log writes a log message.
func (l *Logger) log(level LogLevel, format string, args ...interface{}) {
	if level < l.level {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	levelStr := []string{"DEBUG", "INFO", "WARN", "ERROR"}[level]

	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.writer, "[%s] [%s] %s\n", timestamp, levelStr, msg)
}

// Debug logs debug message.
func (l *Logger) Debug(format string, args ...interface{}) {
	l.log(DEBUG, format, args...)
}

// Info logs info message.
func (l *Logger) Info(format string, args ...interface{}) {
	l.log(INFO, format, args...)
}

// Warn logs warning message.
func (l *Logger) Warn(format string, args ...interface{}) {
	l.log(WARN, format, args...)
}

// Error logs error message.
func (l *Logger) Error(format string, args ...interface{}) {
	l.log(ERROR, format, args...)
}

// Convenience functions for default logger.
func Debug(format string, args ...interface{}) { GetLogger().Debug(format, args...) }
func Info(format string, args ...interface{})  { GetLogger().Info(format, args...) }
func Warn(format string, args ...interface{})  { GetLogger().Warn(format, args...) }
func Error(format string, args ...interface{}) { GetLogger().Error(format, args...) }
