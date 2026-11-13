//go:build windows

package main

import (
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// updateServiceCommand makes the installed Windows service run this
// executable with args, e.g. when the service was registered by a portable
// copy in another folder or with another data directory, so that an upgrade
// with the installer really runs the new program. It only changes the
// command line (logon account, start type etc. are kept) and reports whether
// it changed.
func updateServiceCommand(name string, args []string) (bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	// The same format as the command line written at installation.
	cmdline := syscall.EscapeArg(exe)
	for _, a := range args {
		cmdline += " " + syscall.EscapeArg(a)
	}
	m, err := mgr.Connect()
	if err != nil {
		return false, err
	}
	defer m.Disconnect()
	s, err := m.OpenService(name)
	if err != nil {
		return false, err
	}
	defer s.Close()
	c, err := s.Config()
	if err != nil {
		return false, err
	}
	if strings.EqualFold(c.BinaryPathName, cmdline) {
		return false, nil
	}
	p, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return false, err
	}
	err = windows.ChangeServiceConfig(s.Handle, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE,
		p, nil, nil, nil, nil, nil, nil)
	return err == nil, err
}
