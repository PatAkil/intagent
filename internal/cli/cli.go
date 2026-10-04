// Package cli implements the intagent command.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// App runs intagent subcommands against injectable streams.
type App struct {
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Version string
	// Dir overrides the working directory, for tests.
	Dir string
	// HookBudget, when set, replaces the time one hook run may take (see
	// hookBudget).
	HookBudget time.Duration
	// TeamFilePoll is how often serve looks for changes to the team file.
	// Zero means 2 seconds.
	TeamFilePoll time.Duration
	// Listening, if set, is told the address serve and demo listen on, so a
	// test can give them port 0.
	Listening func(addr string)
	// Now, if set, replaces the clock that paces hooks' footprint scans.
	Now func() time.Time
}

// now reads the App's clock.
func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

const usage = `intagent: intent for agents. Every coding agent on your team sees what the
others are changing, before it writes the same file.

Try it in ten seconds:
  intagent demo                            a dashboard with four simulated agents

Run the team server (one per team):
  intagent serve --config team.json        start the server and dashboard
  intagent token add <name>                add a member and print their token

Set up, once per person and once per repository:
  intagent login --url <server>            save your token for a server
  intagent init [--url <server>]           enrol this repository for Claude Code, Codex, Cursor,
                                           Copilot CLI and Gemini CLI
  intagent doctor                          check that everything is connected

See and steer the work:
  intagent board                           who is working on what in this repository
  intagent check <path>...                 who else is working on these paths
  intagent declare [-x] -m <why> <glob>... reserve paths for this worktree (-x: exclusive)
  intagent release [<glob>...]             release this worktree's intents
  intagent note <member|claim|path> <text> leave a note for another member's agents

Called by agents and git:
  intagent hook [<agent>]                  handle one hook event from stdin
  intagent mcp                             serve intagent's tools over MCP on stdio
  intagent watch                           report this worktree for agents without hooks
  intagent guard                           pre-commit check against exclusive intents

Run 'intagent <command> -h' for a command's options.
`

// errUsage marks errors already explained to the user.
var errUsage = errors.New("usage")

// intagent never exits with 2: agents' hook runners read exit code 2 as "block
// this tool call", so a usage mistake in a hook configuration must not stop an
// agent. Usage errors exit 1 like any other error.
const exitUsage = 1

// Run executes one command line and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Err, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	run, ok := a.commands()[cmd]
	if !ok {
		switch cmd {
		case "-h", "--help", "help":
			fmt.Fprint(a.Out, usage)
			return 0
		case "-v", "--version", "version":
			fmt.Fprintln(a.Out, "intagent", a.Version)
			return 0
		}
		fmt.Fprintf(a.Err, "intagent: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	if err := run(ctx, rest); err != nil {
		var exit exitError
		switch {
		case errors.As(err, &exit):
			if exit.msg != "" {
				fmt.Fprintln(a.Err, exit.msg)
			}
			return exit.code
		case errors.Is(err, flag.ErrHelp):
			return 0
		case errors.Is(err, errUsage):
			return exitUsage
		}
		fmt.Fprintf(a.Err, "intagent %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func (a *App) commands() map[string]func(context.Context, []string) error {
	return map[string]func(context.Context, []string) error{
		"serve":   a.serve,
		"token":   a.token,
		"login":   a.login,
		"init":    a.initRepo,
		"doctor":  a.doctor,
		"board":   a.board,
		"check":   a.check,
		"declare": a.declare,
		"release": a.release,
		"note":    a.note,
		"hook":    a.hook,
		"mcp":     a.mcp,
		"watch":   a.watch,
		"guard":   a.guard,
		"demo":    a.demo,
	}
}

// exitError ends a command with a specific code and message.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

// parse parses a command's flags wherever they stand among its arguments
// ("declare 'web/**' -x" is "declare -x 'web/**'"). Only the command's own
// flags move: anything else that starts with a dash ("->" in a note) stays
// an argument, and so does everything after "--". Before the first argument,
// an unknown flag or -h is reported as usual. -h and --help come back as
// flag.ErrHelp, anything else wrong as errUsage, the usage having been printed.
func parse(fs *flag.FlagSet, args []string) error {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		f := fs.Lookup(name)
		if len(arg) < 2 || arg[0] != '-' || (f == nil && len(pos) > 0) {
			pos = append(pos, arg)
			continue
		}
		flags = append(flags, arg)
		if f != nil && !strings.Contains(arg, "=") && !isBoolFlag(f) && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	err := fs.Parse(append(append(flags, "--"), pos...))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, flag.ErrHelp):
		return flag.ErrHelp
	}
	return errUsage
}

// flagSet reports whether a flag was given on the command line.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func (a *App) flags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Err)
	fs.Usage = func() {
		fmt.Fprintf(a.Err, "usage: intagent %s\n", synopsis)
		var opts strings.Builder
		fs.SetOutput(&opts)
		fs.PrintDefaults()
		fs.SetOutput(a.Err)
		if opts.Len() > 0 {
			fmt.Fprintf(a.Err, "\noptions:\n%s", opts.String())
		}
	}
	return fs
}

func (a *App) workdir() (string, error) {
	if a.Dir != "" {
		return a.Dir, nil
	}
	return os.Getwd()
}
