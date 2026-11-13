package config

import (
	"fmt"
	"os"
)

// Well-known Windows security identifiers.
const (
	sidSystem         = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"
)

// EnsureDataDir creates the data directory if needed and restricts access to
// it: it holds the secret key, the database password and the SQLite
// database. On Windows, where the Unix permission bits are ignored, the
// directory's inherited permissions (e.g. "Users: read" from ProgramData) are
// replaced by full control for SYSTEM and the Administrators group, plus the
// current user when it runs without administrator rights; the files in it
// inherit this. Elsewhere a new directory is created with mode 0700 and an
// existing one is left as it is.
func EnsureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create the data directory %s: %w", dir, err)
	}
	if err := restrictDir(dir); err != nil {
		return fmt.Errorf("could not restrict access to the data directory %s to SYSTEM, Administrators and the current user: %w", dir, err)
	}
	return nil
}

// icaclsArgs returns the icacls.exe arguments that give only SYSTEM,
// Administrators and userSID (if set) full control over dir and everything
// in it, removing inherited permissions. Without userSID it is the same
// command the Windows installer runs.
func icaclsArgs(dir, userSID string) []string {
	args := []string{dir, "/inheritance:r", "/grant:r", "*" + sidSystem + ":(OI)(CI)F", "*" + sidAdministrators + ":(OI)(CI)F"}
	if userSID != "" && userSID != sidSystem && userSID != sidAdministrators {
		args = append(args, "*"+userSID+":(OI)(CI)F")
	}
	return append(args, "/Q")
}
