package cli

import (
	"bufio"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/client"
	"github.com/patakil/intagent/internal/server"
	"github.com/patakil/intagent/internal/web"
)

func (a *App) serve(ctx context.Context, args []string) error {
	fs := a.flags("serve", "serve --config team.json [--addr :7400] [--data DIR] [--tls-cert FILE --tls-key FILE]...")
	addr := fs.String("addr", ":7400", "address to listen on")
	cfgPath := fs.String("config", "team.json", "team configuration: members and policy")
	data := fs.String("data", "intagent-data", "directory for the board snapshot (\"\" keeps it in memory)")
	public := fs.Bool("public-read", false, "let anyone who can reach the server see the board without a token")
	level := fs.String("log-level", "info", "debug, info, warn or error")
	var tlsCerts, tlsKeys listFlag
	fs.Var(&tlsCerts, "tls-cert", "serve HTTPS with this certificate chain (PEM), with --tls-key; give an ECDSA and an RSA one "+
		"each with its key to serve both, ECDSA to the clients that take it")
	fs.Var(&tlsKeys, "tls-key", "the certificate's private key (PEM)")
	maxConns := fs.Int("max-connections", server.DefaultMaxConnections, "connections open at once; more are closed as soon as they are accepted")
	maxStreams := fs.Int("max-streams", 500, "dashboards connected at once, in all (0 for none); more are refused until one closes")
	memberStreams := fs.Int("max-member-streams", 20, "dashboards one member may have connected at once (0 for none)")
	publicStreams := fs.Int("max-public-streams", 100, "dashboards connected at once without a token, with --public-read (0 for none)")
	if err := parse(fs, args); err != nil {
		return err
	}
	var streams server.StreamLimits
	for _, l := range []struct {
		flag  string
		n     int
		field *int
	}{
		{"max-streams", *maxStreams, &streams.Total},
		{"max-member-streams", *memberStreams, &streams.PerMember},
		{"max-public-streams", *publicStreams, &streams.Anonymous},
	} {
		if l.n < 0 {
			return fmt.Errorf("--%s cannot be negative: 0 allows no dashboards", l.flag)
		}
		*l.field = l.n
		if l.n == 0 {
			*l.field = -1 // in StreamLimits zero takes the default, and less allows none
		}
	}
	if len(tlsCerts) != len(tlsKeys) {
		return errors.New("--tls-cert and --tls-key go together")
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*level)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	logger := slog.New(slog.NewTextHandler(a.Err, &slog.HandlerOptions{Level: lvl}))
	loaded := teamStamp(*cfgPath) // before reading, so a change while starting is not missed
	fc, err := server.LoadFileConfig(*cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s not found: create it with 'intagent token add <name> --config %s'", *cfgPath, *cfgPath)
	}
	if err != nil {
		return err
	}
	var tlsConfig *tls.Config
	if len(tlsCerts) > 0 {
		certs := make([]tls.Certificate, len(tlsCerts))
		for i := range tlsCerts {
			if certs[i], err = tls.LoadX509KeyPair(tlsCerts[i], tlsKeys[i]); err != nil {
				return fmt.Errorf("TLS: %w", err)
			}
		}
		tlsConfig = server.TLSConfig(certs)
		if server.RSAOnly(tlsConfig) {
			// Every hook is a new connection, so the handshake is most of
			// what a hook costs the server.
			logger.Warn("the TLS certificate has an RSA key: each hook's connection costs the server about 1.5 ms of CPU, " +
				"three times as much as with an ECDSA P-256 certificate, or 1.5 cores at 1000 hooks a second; add an ECDSA " +
				"certificate with a second --tls-cert and --tls-key, which intagent's clients will use")
		}
	}
	srv, err := server.New(server.Options{
		Members: fc.Members, Version: a.Version, Board: fc.BoardConfig(), DataDir: *data, PublicRead: *public,
		Logger: logger, Dashboard: web.Handler(), Webhook: fc.Webhook, MaxConnections: *maxConns, TLS: tlsConfig,
		Streams: streams,
	})
	if err != nil {
		return err
	}
	ln, err := server.Listen(ctx, *addr)
	if err != nil {
		return err
	}
	if a.Listening != nil {
		a.Listening(ln.Addr().String())
	}
	url := "http://" + ln.Addr().String()
	if tlsConfig != nil {
		url = "https://" + ln.Addr().String()
	}
	policy := fc.BoardConfig().Policy
	logger.Info("intagent server listening", "url", url, "members", len(fc.Members),
		"policy", fmt.Sprintf("block=%s overlap=%s nearby=%s", policy.Block, policy.Overlap, policy.Nearby), "data", *data)
	go watchTeamFile(ctx, *cfgPath, loaded, srv, logger, cmp.Or(a.TeamFilePoll, 2*time.Second))
	err = srv.Serve(ctx, ln)
	logger.Info("intagent server stopped", "err", err)
	return err
}

// listFlag is a flag that may be given more than once.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ", ") }

func (l *listFlag) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// watchTeamFile applies changes to the team file's members while the server
// runs: 'token add' and '--rotate' take effect within seconds, and a rotated
// token stops working. Policy and other settings apply at the next start.
func watchTeamFile(ctx context.Context, path, last string, srv *server.Server, logger *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := teamStamp(path)
		if now == last || now == "" {
			continue
		}
		last = now
		fc, err := server.LoadFileConfig(path)
		if err == nil {
			err = srv.SetMembers(fc.Members)
		}
		if err != nil {
			logger.Warn("team file changed but was not applied", "file", path, "err", err)
			continue
		}
		logger.Info("members reloaded", "file", path, "members", len(fc.Members))
	}
}

// teamStamp identifies a version of the team file.
func teamStamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprint(fi.ModTime().UnixNano(), fi.Size())
}

func (a *App) token(_ context.Context, args []string) error {
	if len(args) == 0 || args[0] != "add" {
		fmt.Fprintln(a.Err, "usage: intagent token add <name> [--config team.json] [--rotate]")
		return errUsage
	}
	fs := a.flags("token add", "token add <name> [--config team.json] [--rotate]")
	cfgPath := fs.String("config", "team.json", "team configuration file to update")
	rotate := fs.Bool("rotate", false, "issue a new token for an existing member")
	if err := parse(fs, args[1:]); err != nil {
		return err
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
	fmt.Fprintln(a.Out, "\nA running 'intagent serve' picks this up within a few seconds; a replaced token stops working then.")
	return nil
}

func (a *App) login(ctx context.Context, args []string) error {
	fs := a.flags("login", "login --url <server> [--token <token>] [--share-prompts on|off]")
	url := fs.String("url", "", "the team server, e.g. https://intagent.example.com")
	token := fs.String("token", "", "your token (read from stdin if omitted)")
	noVerify := fs.Bool("no-verify", false, "save without contacting the server")
	share := fs.String("share-prompts", "", "on: the first line of a session's first prompt becomes its task on the board; off: it does not")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *share != "" && *share != "on" && *share != "off" {
		return errors.New("--share-prompts is on or off")
	}
	u := client.NormalizeURL(*url)
	if u == "" && *share != "" {
		return a.setSharePrompts(*share == "on")
	}
	if u == "" {
		fs.Usage()
		return errUsage
	}
	if err := client.CheckURL(u); err != nil {
		return err
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
		if w.Member == "" {
			return fmt.Errorf("%s answers anyone, but does not know this token; check it with whoever runs the server", u)
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
	if *share != "" {
		return a.setSharePrompts(*share == "on")
	}
	if uc.SharePrompts == nil || *uc.SharePrompts {
		fmt.Fprintln(a.Out, "The first line of each agent session's first prompt is shown to your team as its task; "+
			"'intagent login --share-prompts off' keeps prompts to yourself.")
	}
	return nil
}

// setSharePrompts saves whether prompts may describe a session's task.
func (a *App) setSharePrompts(on bool) error {
	uc, p, err := client.LoadUserConfig()
	if err != nil {
		return err
	}
	uc.SharePrompts = &on
	if err := client.SaveUserConfig(p, uc); err != nil {
		return err
	}
	if on {
		fmt.Fprintln(a.Out, "Prompts are shared: the first line of a session's first prompt becomes its task on the board.")
	} else {
		fmt.Fprintln(a.Out, "Prompts stay private: a session's task comes only from the intents its agent declares.")
	}
	return nil
}
