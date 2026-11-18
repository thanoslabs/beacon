// Package cli implements the beacon command line: it parses arguments and
// dispatches to subcommands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
)

// Exit codes returned by Run.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// env carries what a command needs from the process, so that tests can run
// commands without touching global state.
type env struct {
	getenv func(string) string
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

// command is one subcommand of beacon.
type command struct {
	summary string
	run     func(ctx context.Context, e env, args []string) error
}

// commands lists every subcommand by name.
func commands() map[string]command {
	return map[string]command{
		"hash-password": {summary: "Hash a password for the bootstrap admin", run: runHashPassword},
		"migrate":       {summary: "Apply database schema migrations", run: runMigrate},
		"serve":         {summary: "Run the Beacon server", run: runServe},
		"validate":      {summary: "Check the configuration without applying it", run: runValidate},
		"version":       {summary: "Print the version and exit", run: runVersion},
	}
}

// errUsage marks an error that was caused by wrong arguments. The command has
// already printed its usage.
var errUsage = errors.New("usage error")

// Run executes the beacon command line and returns the process exit code.
// args excludes the program name. getenv looks up environment variables.
func Run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := env{getenv: getenv, stdin: stdin, stdout: stdout, stderr: stderr}
	cmds := commands()

	if len(args) == 0 {
		usage(stderr, cmds)
		return exitUsage
	}

	switch args[0] {
	case "help", "-h", "-help", "--help":
		usage(stdout, cmds)
		return exitOK
	case "--version":
		args[0] = "version"
	}

	cmd, ok := cmds[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "beacon: unknown command %q\n\n", args[0])
		usage(stderr, cmds)
		return exitUsage
	}

	err := cmd.run(ctx, e, args[1:])
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, errUsage):
		return exitUsage
	default:
		fmt.Fprintf(stderr, "beacon %s: %v\n", args[0], err)
		return exitError
	}
}

// usage prints the list of subcommands.
func usage(w io.Writer, cmds map[string]command) {
	names := make([]string, 0, len(cmds))
	for name := range cmds {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprint(w, "Beacon is an alerting and on-call platform.\n\nUsage:\n\n  beacon <command> [flags]\n\nCommands:\n\n")
	for _, name := range names {
		fmt.Fprintf(w, "  %-14s %s\n", name, cmds[name].summary)
	}
	fmt.Fprint(w, "\nRun \"beacon <command> -h\" for the flags of a command.\n")
}

// newFlagSet returns a flag set for a subcommand that reports errors instead
// of exiting, and writes its usage to stderr.
func newFlagSet(e env, name, summary string) *flag.FlagSet {
	fs := flag.NewFlagSet("beacon "+name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "%s.\n\nUsage:\n\n  beacon %s [flags]\n", summary, name)
		hasFlags := false
		fs.VisitAll(func(*flag.Flag) { hasFlags = true })
		if hasFlags {
			fmt.Fprint(e.stderr, "\nFlags:\n\n")
			fs.PrintDefaults()
		}
	}
	return fs
}

// parseFlags parses args and rejects positional arguments, which no beacon
// command takes. envErrs are problems found in environment variables while the
// flags were registered; they are reported unless the user only asked for help.
func parseFlags(fs *flag.FlagSet, args []string, envErrs ...error) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if len(envErrs) > 0 {
		return errors.Join(envErrs...)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return errUsage
	}
	return nil
}
