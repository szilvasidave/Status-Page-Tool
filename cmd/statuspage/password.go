package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/szilvasidave/status-page-tool/internal/auth"
	"github.com/szilvasidave/status-page-tool/internal/store"
)

const resetPasswordUsage = `statuspage reset-password -username NAME [-data DIR]

Sets a new password for a local administrator account and signs out all its
sessions. The password is read from standard input (asked twice without echo
when run in a terminal). If the sign-in with the local administrator account
is turned off, turn it on with "statuspage local-login enable".`

func cmdResetPassword(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("reset-password", resetPasswordUsage, stderr)
	dataDir := dataDirFlag(fs)
	username := fs.String("username", "", "user name of the local administrator account")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	name := strings.TrimSpace(*username)
	if name == "" {
		fmt.Fprintln(stderr, "-username is required")
		fs.Usage()
		return errUsage
	}
	dir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	st, closeDB, err := openStore(ctx, dir)
	if err != nil {
		return err
	}
	defer closeDB()

	user, err := st.GetUserByUsername(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		// The name can be changed on the account page, and when single
		// sign-on is down nobody can look it up there.
		if names, err := st.LocalAdminUsernames(ctx); err == nil && len(names) > 0 {
			return fmt.Errorf("there is no local account named %q; local administrator account: %s", name, strings.Join(names, ", "))
		}
		return fmt.Errorf("there is no local account named %q", name)
	}
	if err != nil {
		return err
	}

	pw, err := readNewPassword(os.Stdin, stderr)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(pw) < auth.MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters long", auth.MinPasswordLength)
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := st.SetPasswordHash(ctx, user.ID, hash); err != nil {
		return fmt.Errorf("could not save the new password: %w", err)
	}
	// Like a password change in the web UI: whoever knew the old password
	// must not stay signed in.
	if err := st.DeleteUserSessions(ctx, user.ID); err != nil {
		return fmt.Errorf("the password was changed, but the existing sessions could not be signed out: %w", err)
	}
	fmt.Fprintf(stdout, "The password of %q was changed. All its sessions were signed out.\n", user.Username)
	if settings, err := st.GetSettings(ctx); err == nil && !settings.LocalLoginEnabled {
		fmt.Fprintf(stdout, "Note: sign-in with the local administrator account is turned off. "+
			"Turn it on with: statuspage local-login enable -data \"%s\"\n", dir)
	}
	return nil
}

// readNewPassword reads the new password: twice without echo from a
// terminal, otherwise the first line of in.
func readNewPassword(in *os.File, prompt io.Writer) (string, error) {
	fd := int(in.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(prompt, "New password: ")
		first, err := term.ReadPassword(fd)
		fmt.Fprintln(prompt)
		if err != nil {
			return "", fmt.Errorf("could not read the password: %w", err)
		}
		fmt.Fprint(prompt, "Repeat the new password: ")
		second, err := term.ReadPassword(fd)
		fmt.Fprintln(prompt)
		if err != nil {
			return "", fmt.Errorf("could not read the password: %w", err)
		}
		if string(first) != string(second) {
			return "", errors.New("the two passwords do not match")
		}
		return string(first), nil
	}
	return readPasswordLine(in)
}

// readPasswordLine returns the first line of r without the line ending.
func readPasswordLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", errors.New("could not read the new password from standard input")
	}
	return strings.TrimRight(line, "\r\n"), nil
}
