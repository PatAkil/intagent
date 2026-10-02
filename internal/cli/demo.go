package cli

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"time"

	"github.com/patakil/intagent/internal/board"
	"github.com/patakil/intagent/internal/server"
	"github.com/patakil/intagent/internal/web"
)

// demo runs an in-memory server with simulated agents, so the dashboard can be
// tried without wiring up a team.
func (a *App) demo(ctx context.Context, args []string) error {
	fs := a.flags("demo", "demo [--addr 127.0.0.1:7400]")
	addr := fs.String("addr", "127.0.0.1:7400", "address to listen on")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	var members []server.Member
	for _, m := range []string{"alice", "bob", "carol", "dana"} {
		_, hash, err := server.NewToken()
		if err != nil {
			return err
		}
		members = append(members, server.Member{Name: m, TokenSHA256: hash})
	}
	cfg := board.DefaultConfig()
	cfg.StallAfter = 40 * time.Second // so a stall shows up within the demo
	cfg.ToolStallAfter = 90 * time.Second
	server.Version = a.Version
	srv, err := server.New(server.Options{
		Members: members, Board: cfg, PublicRead: true, Dashboard: web.Handler(),
		Logger: slog.New(slog.NewTextHandler(a.Err, &slog.HandlerOptions{Level: slog.LevelWarn})), SweepEvery: 2 * time.Second,
	})
	if err != nil {
		return err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "intagent demo: open http://%s/ to watch four simulated agents collide and coordinate.\nNothing is saved. Ctrl-C to stop.\n", ln.Addr())
	go simulate(ctx, srv.Board(), 3*time.Second)
	return srv.Serve(ctx, ln)
}

const demoRepo = "github.com/acme/mono"

type actor struct {
	member, session, branch string
	agent                   board.Agent
}

func (ac actor) where() board.Where {
	return board.Where{Repo: demoRepo, Host: ac.member + "-laptop", Worktree: "/home/" + ac.member + "/mono", Branch: ac.branch}
}

func (ac actor) hook(b *board.Board, kind board.Kind, tool string, paths ...string) board.HookResult {
	ev := board.HookEvent{Kind: kind, Member: ac.member, Agent: ac.agent, SessionID: ac.session, Where: ac.where(), Tool: tool}
	for _, p := range paths {
		ev.Paths = append(ev.Paths, board.PathRef{Path: p, Area: demoArea(p)})
	}
	res, _ := b.Hook(time.Now(), ev)
	return res
}

func (ac actor) prompt(b *board.Board, text string) {
	ev := board.HookEvent{Kind: board.KindPrompt, Member: ac.member, Agent: ac.agent, SessionID: ac.session, Where: ac.where(), Prompt: text}
	_, _ = b.Hook(time.Now(), ev)
}

// edit tries a write like an agent would: on a bump it retries once.
func (ac actor) edit(b *board.Board, paths ...string) bool {
	res := ac.hook(b, board.KindPreEdit, "Edit", paths...)
	if res.Decision == board.Refuse && len(res.Conflicts) > 0 && res.Conflicts[0].Severity != board.Block {
		res = ac.hook(b, board.KindPreEdit, "Edit", paths...)
	}
	if res.Decision != board.Allow {
		return false
	}
	ac.hook(b, board.KindPostEdit, "Edit", paths...)
	return true
}

func demoArea(p string) string {
	for _, area := range []string{"services/payments", "services/billing", "apps/web", "libs/http"} {
		if len(p) > len(area) && p[:len(area)+1] == area+"/" {
			return area
		}
	}
	return ""
}

// simulate plays a short story, then keeps the agents busy with small edits.
func simulate(ctx context.Context, b *board.Board, every time.Duration) {
	alice := actor{"alice", "a-1", "feat/retry-policy", board.AgentClaudeCode}
	bob := actor{"bob", "b-1", "fix/payment-timeouts", board.AgentCodex}
	carol := actor{"carol", "c-1", "feat/web-checkout", board.AgentCursor}
	dana := actor{"dana", "d-1", "chore/http-client", board.AgentClaudeCode}
	where := func(ac actor) board.Where { return ac.where() }

	steps := []func(){
		func() { alice.hook(b, board.KindSessionStart, "") },
		func() { alice.prompt(b, "Move the retry policy into its own package") },
		func() {
			_, _ = b.Declare(time.Now(), board.DeclareRequest{Member: "alice", Where: where(alice), Summary: "Move the retry policy into its own package",
				Patterns: []string{"services/payments/retry/**", "services/payments/client.go"}, Mode: board.Exclusive})
		},
		func() { alice.edit(b, "services/payments/retry/policy.go") },
		func() { alice.edit(b, "services/payments/client.go") },
		func() { bob.hook(b, board.KindSessionStart, ""); bob.prompt(b, "Raise the payment client timeout to 30s") },
		func() { bob.edit(b, "services/payments/client.go") }, // refused: alice holds it
		func() {
			_, _ = b.Note(time.Now(), board.NoteRequest{Member: "bob", Where: where(bob), To: "alice", Text: "I need client.go for the timeout fix; can you take it in your change?"})
		},
		func() { bob.edit(b, "services/billing/invoice.go") },
		func() { carol.hook(b, board.KindSessionStart, ""); carol.prompt(b, "Add Apple Pay to web checkout") },
		func() { carol.edit(b, "apps/web/src/checkout/ApplePay.tsx") },
		func() { carol.edit(b, "libs/http/client.ts") },
		func() { dana.hook(b, board.KindSessionStart, ""); dana.prompt(b, "Upgrade the shared HTTP client") },
		func() {
			_, _ = b.Declare(time.Now(), board.DeclareRequest{Member: "dana", Where: where(dana), Summary: "Upgrade the shared HTTP client", Patterns: []string{"libs/http/**"}, Mode: board.Shared})
		},
		func() { dana.edit(b, "libs/http/client.ts") }, // bumped: carol changed it
		func() { alice.hook(b, board.KindToolEnd, "Bash") }, // alice reads bob's note
		func() { alice.hook(b, board.KindToolStart, "Bash") },
		func() { dana.hook(b, board.KindToolStart, "Bash") }, // dana's agent will stall in this tool call
		func() { bob.hook(b, board.KindStop, "") },
		func() { carol.edit(b, "apps/web/src/checkout/Summary.tsx") },
	}
	idle := []func(){
		func() { carol.edit(b, "apps/web/src/checkout/Summary.tsx") },
		func() { alice.edit(b, "services/payments/retry/backoff.go") },
		func() { bob.prompt(b, "Also cover the refund path"); bob.edit(b, "services/billing/refunds.go") },
		func() { bob.edit(b, "services/payments/client.go") },
		func() { alice.hook(b, board.KindToolEnd, "Bash"); alice.hook(b, board.KindToolStart, "Bash") },
		func() { carol.hook(b, board.KindStop, "") },
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if i < len(steps) {
			steps[i]()
			continue
		}
		idle[rand.IntN(len(idle))]()
	}
}
