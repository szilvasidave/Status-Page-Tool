package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

const (
	logFileName = "statuspage.log"
	// maxLogSize: a larger log file is renamed to statuspage.log.1 at start.
	maxLogSize = 10 << 20
)

// newLogger returns a text logger writing to stdout (if toStdout) and to
// <dataDir>/statuspage.log, and makes it the default logger. The level
// (debug, info, warn, error; default info) is read from the environment
// variable STATUSPAGE_LOG_LEVEL or else from "log_level" in config.json; the
// latter also works for the Windows service, which does not see environment
// variables set after the last reboot. The returned function closes the log
// file.
func newLogger(dataDir string, toStdout bool) (*slog.Logger, func()) {
	name, source := os.Getenv("STATUSPAGE_LOG_LEVEL"), "STATUSPAGE_LOG_LEVEL"
	if strings.TrimSpace(name) == "" {
		// Errors are reported by the server when it loads the file.
		if cfg, err := config.Load(dataDir); err == nil {
			name, source = cfg.LogLevel, "log_level in "+config.FileName
		}
	}
	level, levelErr := logLevel(name, source)

	var out teeWriter
	if toStdout {
		out = append(out, os.Stdout)
	}
	f, fileErr := openLogFile(filepath.Join(dataDir, logFileName))
	if fileErr == nil {
		out = append(out, f)
	}

	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	if fileErr != nil {
		log.Warn("could not open the log file, logging to the console only", "err", fileErr)
	}
	if levelErr != nil {
		log.Warn(levelErr.Error())
	}
	return log, func() {
		if f != nil {
			f.Close()
		}
	}
}

// logLevel parses a log level name from source; empty means info.
func logLevel(s, source string) (slog.Level, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return slog.LevelInfo, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo, fmt.Errorf("invalid log level %q in %s (use debug, info, warn or error), using info", s, source)
	}
	return l, nil
}

// openLogFile opens the log file for appending. A file larger than
// maxLogSize is renamed to <name>.1 first (replacing an older one).
func openLogFile(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1") // on failure keep appending
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// teeWriter writes to all writers, ignoring individual errors so that an
// unusable console does not stop logging to the file.
type teeWriter []io.Writer

func (t teeWriter) Write(p []byte) (int, error) {
	for _, w := range t {
		_, _ = w.Write(p)
	}
	return len(p), nil
}
