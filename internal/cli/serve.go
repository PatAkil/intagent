package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/server"
	"github.com/patakil/intagent/internal/web"
)

func (a *App) serve(ctx context.Context, args []string) error {
	fs := a.flags("serve", "serve --config team.json [--addr :7400] [--data DIR]")
	addr := fs.String("addr", ":7400", "address to listen on")
	cfgPath := fs.String("config", "team.json", "team configuration: members and policy")
	data := fs.String("data", "intagent-data", "directory for the board snapshot (\"\" keeps it in memory)")
	public := fs.Bool("public-read", false, "let anyone who can reach the server see the board without a token")
	level := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*level)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(a.Err, &slog.HandlerOptions{Level: lvl}))
	fc, err := server.LoadFileConfig(*cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s not found: create it with 'intagent token add <name> --config %s'", *cfgPath, *cfgPath)
	}
	if err != nil {
		return err
	}
	server.Version = a.Version
	srv, err := server.New(server.Options{
		Members: fc.Members, Board: fc.BoardConfig(), DataDir: *data, PublicRead: *public,
		Logger: logger, Dashboard: web.Handler(),
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	policy := fc.BoardConfig().Policy
	logger.Info("intagent server listening", "addr", ln.Addr().String(), "members", len(fc.Members),
		"policy", fmt.Sprintf("block=%s overlap=%s nearby=%s", policy.Block, policy.Overlap, policy.Nearby), "data", *data)
	return srv.Serve(ctx, ln)
}

func (a *App) token(_ context.Context, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(a.Err, "usage: intagent token add <name> [--config team.json] [--rotate]")
		return errUsage
	}
	fs := a.flags("token add", "token add <name> [--config team.json] [--rotate]")
	cfgPath := fs.String("config", "team.json", "team configuration file to update")
	rotate := fs.Bool("rotate", false, "issue a new token for an existing member")
	if err := fs.Parse(reorder(args[1:])); err != nil {
		return errUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errUsage
	}
	name := fs.Arg(0)
	tok, err := server.AddMember(*cfgPath, name, *rotate)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Token for %s (shown once; the server keeps only its hash):\n\n  %s\n\n", name, tok)
	fmt.Fprintf(a.Out, "%s connects with:\n\n  intagent login --url <server-url> --token %s\n", name, tok)
	return nil
}

// reorder moves flags before positional arguments, so "add alice --rotate" works.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && args[i] != "--rotate" && args[i] != "-rotate" && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, args[i])
	}
	return append(flags, pos...)
}

func (a *App) login(ctx context.Context, args []string) error {
	fs := a.flags("login", "login --url <server> [--token <token>]")
	url := fs.String("url", "", "the team server, e.g. https://intagent.example.com")
	token := fs.String("token", "", "your token (read from stdin if omitted)")
	noVerify := fs.Bool("no-verify", false, "save without contacting the server")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	u := client.NormalizeURL(*url)
	if u == "" {
		fs.Usage()
		return errUsage
	}
	tok := strings.TrimSpace(*token)
	if tok == "" {
		fmt.Fprint(a.Err, "Token: ")
		line, err := bufio.NewReader(a.In).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no token given")
		}
		tok = strings.TrimSpace(line)
	}
	who := ""
	if !*noVerify {
		w, err := client.New(u, tok, client.DefaultTimeout).Whoami(ctx)
		if err != nil {
			if client.IsUnauthorized(err) {
				return fmt.Errorf("%s does not recognise this token", u)
			}
			return fmt.Errorf("could not reach %s: %w (use --no-verify to save anyway)", u, err)
		}
		who = w.Member
	}
	uc, p, err := client.LoadUserConfig()
	if err != nil {
		return err
	}
	if uc.Servers == nil {
		uc.Servers = map[string]client.ServerCredentials{}
	}
	uc.Servers[u] = client.ServerCredentials{Token: tok}
	if uc.Default == "" {
		uc.Default = u
	}
	if err := client.SaveUserConfig(p, uc); err != nil {
		return err
	}
	if who != "" {
		fmt.Fprintf(a.Out, "Logged in to %s as %s. Saved to %s.\n", u, who, p)
	} else {
		fmt.Fprintf(a.Out, "Saved your token for %s to %s.\n", u, p)
	}
	return nil
}
