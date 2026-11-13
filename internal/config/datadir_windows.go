//go:build windows

package config

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// restrictDir runs icacls to replace the inherited permissions of dir.
func restrictDir(dir string) error {
	// Without administrator rights (e.g. a portable run by a normal user)
	// the current user needs an entry of its own to keep using the folder.
	userSID := ""
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	if isAdmin, err := windows.Token(0).IsMember(admins); err != nil || !isAdmin {
		u, err := user.Current()
		if err != nil {
			return err
		}
		userSID = u.Uid
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	// The full path, so that no icacls.exe from the PATH or the current
	// directory is run.
	out, err := exec.Command(filepath.Join(root, "System32", "icacls.exe"), icaclsArgs(dir, userSID)...).CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("icacls: %v: %s", err, msg)
		}
		return fmt.Errorf("icacls: %w", err)
	}
	return nil
}
