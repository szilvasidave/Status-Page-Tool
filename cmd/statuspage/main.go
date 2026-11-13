// Command statuspage is the status page server together with a few command
// line tools for installation and maintenance:
//
//	statuspage [run] [-data DIR] [-listen ADDR]
//	statuspage setup ...
//	statuspage service install|uninstall|start|stop|restart|status [-data DIR]
//	statuspage reset-password -username NAME [-data DIR]
//	statuspage local-login enable|disable|status [-data DIR]
//	statuspage version
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	_ "time/tzdata" // time zone database for Windows

	"github.com/kardianos/service"

	"github.com/szilvasidave/status-page-tool/internal/config"
)

// version is set at build time with -ldflags "-X main.version=1.2.3".
var version = "dev"

const usageText = `Status Page %s

Usage:
  statuspage [run] [-data DIR] [-listen ADDR]
        run the server (default command)
  statuspage setup [-data DIR] (-params-file FILE | -db sqlite|postgres ...) [-check-only] [-result-file FILE]
        configure the database connection and create the tables
  statuspage service install|uninstall|start|stop|restart|status [-data DIR]
        manage the Windows service / systemd unit
  statuspage reset-password -username NAME [-data DIR]
        set a new password for a local administrator account (read from standard input)
  statuspage local-login enable|disable|status [-data DIR]
        turn the sign-in with the local administrator account on or off
  statuspage version
        print the version

Run "statuspage <command> -h" to see the options of a command.
Default data directory: %s
`

// errUsage signals a command line error whose message was already printed.
var errUsage = errors.New("invalid command line")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	} else if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		cmd = "help"
	}

	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
		if err != nil && pauseOnError() {
			defer waitForEnter(stderr)
		}
	case "setup":
		err = cmdSetup(args, stdout, stderr)
	case "service":
		err = cmdService(args, stdout, stderr)
	case "reset-password":
		err = cmdResetPassword(args, stdout, stderr)
	case "local-login":
		err = cmdLocalLogin(args, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "statuspage %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "help":
		printUsage(stdout)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
		printUsage(stderr)
		return 2
	}

	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		return 2
	default:
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, usageText, version, config.DefaultDataDir())
}

// newFlagSet creates a flag set for a sub command with a usage line.
func newFlagSet(name, usage string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: %s\n\nOptions:\n", usage)
		fs.PrintDefaults()
	}
	return fs
}

// dataDirFlag registers the common -data flag.
func dataDirFlag(fs *flag.FlagSet) *string {
	return fs.String("data", config.DefaultDataDir(), "data directory (config.json, SQLite database, log files); env STATUSPAGE_DATA")
}

// parseFlags parses args and rejects positional arguments. It returns
// flag.ErrHelp for -h and errUsage for invalid flags (already printed).
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	return nil
}

// pauseOnError reports whether the program was most likely started by
// double-clicking the exe on Windows, where the console window would close
// before the error message can be read.
func pauseOnError() bool {
	return runtime.GOOS == "windows" && len(os.Args) == 1 && service.Interactive()
}

func waitForEnter(w io.Writer) {
	fmt.Fprint(w, "\nPress Enter to close this window.")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
