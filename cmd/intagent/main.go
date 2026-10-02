// Command intagent gives every coding agent on a team a live view of what the
// other agents are changing. See https://github.com/patakil/intagent.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/patakil/intagent/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	app := &cli.App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Version: buildVersion()}
	code := app.Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
