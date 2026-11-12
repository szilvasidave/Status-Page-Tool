package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const (
	logFileName = "statuspage.log"
	// maxLogSize: a larger log file is renamed to statuspage.log.1 at start.
	maxLogSize = 10 << 20
)

// newLogger returns a text logger writing to stdout (if toStdout) and to
// <dataDir>/statuspage.log, and makes it the default logger. The level is
// read from STATUSPAGE_LOG_LEVEL (debug, info, warn, error; default info).
// The returned function closes the log file.
func newLogger(dataDir string, toStdout bool) (*slog.Logger, func()) {
	level, levelErr := logLevel(os.Getenv("STATUSPAGE_LOG_LEVEL"))

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

// logLevel parses a log level name; empty means info.
func logLevel(s string) (slog.Level, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return slog.LevelInfo, nil
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo, fmt.Errorf("invalid STATUSPAGE_LOG_LEVEL %q (use debug, info, warn or error), using info", s)
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
