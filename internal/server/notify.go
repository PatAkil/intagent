package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// WebhookConfig sends selected activities to an HTTP endpoint, such as a Slack
// incoming webhook, so people hear about stuck agents and refused collisions
// without watching the dashboard.
type WebhookConfig struct {
	URL string `json:"url"`
	// Events lists activity kinds to send. Empty means DefaultWebhookEvents.
	// "conflict" sends only refused or asked edits, never warnings.
	Events []board.ActivityKind `json:"events,omitempty"`
}

// DefaultWebhookEvents are the activities worth interrupting a person for.
var DefaultWebhookEvents = []board.ActivityKind{board.ActivitySessionStalled, board.ActivitySessionGone, board.ActivityConflict}

// webhookPayload is Slack-compatible ("text") and carries the activity for other consumers.
type webhookPayload struct {
	Text     string         `json:"text"`
	Activity board.Activity `json:"activity"`
}

type notifier struct {
	cfg    WebhookConfig
	queue  chan board.Activity
	client *http.Client
	log    *slog.Logger
}

func newNotifier(cfg WebhookConfig, log *slog.Logger) *notifier {
	if len(cfg.Events) == 0 {
		cfg.Events = DefaultWebhookEvents
	}
	return &notifier{cfg: cfg, queue: make(chan board.Activity, 256), client: &http.Client{Timeout: 5 * time.Second}, log: log}
}

func (n *notifier) wants(a board.Activity) bool {
	if !slices.Contains(n.cfg.Events, a.Kind) {
		return false
	}
	if a.Kind == board.ActivityConflict {
		// Refusals and questions, and changes made inside a teammate's
		// reservation without a check.
		return a.Decision == board.Refuse || a.Decision == board.DecideAsk || a.Severity == board.Block
	}
	return true
}

// enqueue never blocks the board; when the endpoint is slow, activities are dropped.
func (n *notifier) enqueue(acts []board.Activity) {
	for _, a := range acts {
		if !n.wants(a) {
			continue
		}
		select {
		case n.queue <- a:
		default:
			n.log.Warn("webhook queue full; dropping activity", "kind", a.Kind)
		}
	}
}

func (n *notifier) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case a := <-n.queue:
			if err := n.send(ctx, a); err != nil {
				n.log.Warn("webhook failed", "kind", a.Kind, "err", err)
			}
		}
	}
}

func (n *notifier) send(ctx context.Context, a board.Activity) error {
	body, err := json.Marshal(webhookPayload{Text: slackText.Replace(describeActivity(a)), Activity: a})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint answered %s", resp.Status)
	}
	return nil
}

// slackText escapes the three characters Slack reads as markup, so a member's
// summary or tool name cannot mention @channel or disguise a link. The line's
// own wording uses none of them.
var slackText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// describeActivity writes one line a person can act on.
func describeActivity(a board.Activity) string {
	who := a.Member + "'s agent"
	if a.Agent != "" {
		who = fmt.Sprintf("%s's %s agent", a.Member, a.Agent)
	}
	switch a.Kind {
	case "session.stalled":
		return fmt.Sprintf("intagent: %s in %s looks stuck: %s.", who, a.Repo, a.Text)
	case "session.gone":
		return fmt.Sprintf("intagent: %s in %s stopped reporting (%s) without ending its session.", who, a.Repo, a.Text)
	case "conflict":
		verb := "was refused an edit"
		switch a.Decision {
		case board.DecideAsk:
			verb = "was asked to confirm an edit"
		case board.Allow:
			verb = "changed a reserved file without a check"
		}
		return fmt.Sprintf("intagent: %s %s in %s: %s.", who, verb, a.Repo, a.Text)
	}
	return fmt.Sprintf("intagent: %s in %s: %s %s", who, a.Repo, a.Kind, a.Text)
}
