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
)

// App runs intagent subcommands against injectable streams.
type App struct {
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	Version string
	// Dir overrides the working directory, for tests.
	Dir string
}

const usage = `intagent: intent for agents. Every coding agent on your team sees what the
others are changing, before it writes the same file.

Run the team server (one per team):
  intagent serve --config team.json        start the server and dashboard
  intagent token add <name>                add a member and print their token

Set up, once per person and once per repository:
  intagent login --url <server>            save your token for a server
  intagent init [--url <server>]           enrol this repository for Claude Code, Codex and Cursor
  intagent doctor                          check that everything is connected

See and steer the work:
  intagent board                           who is working on what in this repository
  intagent check <path>...                 who else is working on these paths
  intagent declare [-x] -m <why> <glob>... reserve paths for this worktree (-x: exclusive)
  intagent release [<glob>...]             release this worktree's intents
  intagent note <member|claim|path> <text> leave a note for another member's agents

Called by agents and git:
  intagent hook <claude-code|codex|cursor> handle one hook event from stdin
  intagent mcp                             serve intagent's tools over MCP on stdio
  intagent watch                           report this worktree for agents without hooks
  intagent guard                           pre-commit check against exclusive intents

Run 'intagent <command> -h' for a command's options.
`

// errUsage marks errors already explained to the user.
var errUsage = errors.New("usage")

// Run executes one command line and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Err, usage)
		return 2
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
		return 2
	}
	if err := run(ctx, rest); err != nil {
		var exit exitError
		switch {
		case errors.As(err, &exit):
			if exit.msg != "" {
				fmt.Fprintln(a.Err, exit.msg)
			}
			return exit.code
		case errors.Is(err, errUsage), errors.Is(err, flag.ErrHelp):
			return 2
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
	}
}

// exitError ends a command with a specific code and message.
type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

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
