//go:build !windows

package config

// restrictDir does nothing: os.MkdirAll creates a new data directory with
// mode 0700, so that only its owner can open it.
func restrictDir(string) error { return nil }
