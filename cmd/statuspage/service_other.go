//go:build !windows

package main

// updateServiceCommand is only needed on Windows, where the installer
// upgrades an existing service; a systemd unit or launchd job keeps its
// command line (run "statuspage service uninstall" first to change it).
func updateServiceCommand(string, []string) (bool, error) { return false, nil }
