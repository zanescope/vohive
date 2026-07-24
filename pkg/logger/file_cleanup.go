package logger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap/zapcore"
)

// managedFileSink serializes log writes with destructive maintenance such as
// clearing the active file. Logs written after ClearAllLogs returns are kept.
type managedFileSink struct {
	mu       sync.Mutex
	filename string
	writer   zapcore.WriteSyncer
}

func newManagedFileSink(filename string, writer zapcore.WriteSyncer) *managedFileSink {
	return &managedFileSink{filename: filepath.Clean(filename), writer: writer}
}

func (s *managedFileSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writer.Write(p)
}

func (s *managedFileSink) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writer.Sync()
}

func (s *managedFileSink) clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writer.Sync(); err != nil {
		return fmt.Errorf("sync log writer: %w", err)
	}
	return clearLogFiles(s.filename)
}

// LogFilename returns the path used by the active file logger.
func LogFilename() string {
	logMu.RLock()
	sink := activeFileSink
	logMu.RUnlock()
	if sink == nil {
		return "logs/app.log"
	}
	return sink.filename
}

// ClearAllLogs removes rotated log files and truncates the currently open log
// file without detaching the active writer from its inode.
func ClearAllLogs() error {
	logMu.RLock()
	sink := activeFileSink
	logMu.RUnlock()
	if sink == nil {
		return errors.New("file logger is not initialized")
	}
	if err := sink.clear(); err != nil {
		return err
	}
	GlobalBroadcaster.ClearBuffered()
	return nil
}

func clearLogFiles(filename string) error {
	filename = filepath.Clean(strings.TrimSpace(filename))
	if filename == "." || filepath.Base(filename) == "." {
		return errors.New("log filename is empty")
	}

	var errs []error
	activeInfo, statErr := os.Stat(filename)
	if statErr != nil && !os.IsNotExist(statErr) {
		errs = append(errs, fmt.Errorf("stat active log: %w", statErr))
	}
	if statErr == nil {
		if err := os.Truncate(filename, 0); err != nil {
			errs = append(errs, fmt.Errorf("truncate active log: %w", err))
		}
	} else if os.IsNotExist(statErr) {
		// Remove a broken link left by an interrupted external cleanup.
		if _, err := os.Lstat(filename); err == nil {
			if err := os.Remove(filename); err != nil {
				errs = append(errs, fmt.Errorf("remove broken active log link: %w", err))
			}
		}
	}

	dir := filepath.Dir(filename)
	name := filepath.Base(filename)
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("read log directory: %w", err))
		}
		return errors.Join(errs...)
	}
	prefix := base + "-"
	for _, entry := range entries {
		entryName := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(entryName, prefix) ||
			!(strings.HasSuffix(entryName, ext) || strings.HasSuffix(entryName, ext+".gz")) {
			continue
		}
		path := filepath.Join(dir, entryName)
		if activeInfo != nil {
			info, err := os.Stat(path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				errs = append(errs, fmt.Errorf("stat rotated log %s: %w", entryName, err))
				continue
			}
			if os.SameFile(activeInfo, info) {
				continue
			}
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove rotated log %s: %w", entryName, err))
		}
	}
	return errors.Join(errs...)
}
