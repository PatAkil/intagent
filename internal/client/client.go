package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/patakil/intagent/internal/board"
)

// Client calls a team server.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the server at base, authenticating with token.
func New(base, token string, timeout time.Duration) *Client {
	return &Client{base: NormalizeURL(base), token: token, http: &http.Client{Timeout: timeout}}
}

// APIError is an error the server returned.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("server said %d: %s", e.Status, e.Message) }

// IsUnauthorized reports whether err is the server rejecting the token.
func IsUnauthorized(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("User-Agent", "intagent")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return err
	}
	if len(data) > maxResponse {
		return fmt.Errorf("%s %s: the server's answer is larger than %d MB", method, path, maxResponse>>20)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(data)
		return nil
	}
	return json.Unmarshal(data, out)
}

// Hook sends a lifecycle event.
func (c *Client) Hook(ctx context.Context, ev board.HookEvent) (board.HookResult, error) {
	var res board.HookResult
	err := c.do(ctx, http.MethodPost, "/v1/hook", ev, &res)
	return res, err
}

// Declare declares intents.
func (c *Client) Declare(ctx context.Context, r board.DeclareRequest) (board.DeclareResult, error) {
	var res board.DeclareResult
	err := c.do(ctx, http.MethodPost, "/v1/intents", r, &res)
	return res, err
}

// Release releases intents and returns how many were released.
func (c *Client) Release(ctx context.Context, r board.ReleaseRequest) (int, error) {
	var res board.ReleaseResult
	err := c.do(ctx, http.MethodPost, "/v1/intents/release", r, &res)
	return res.Released, err
}

// Check asks who else is working on paths.
func (c *Client) Check(ctx context.Context, r board.CheckRequest) (board.CheckResult, error) {
	var res board.CheckResult
	err := c.do(ctx, http.MethodPost, "/v1/check", r, &res)
	return res, err
}

// Note sends a note.
func (c *Client) Note(ctx context.Context, r board.NoteRequest) (board.NoteResult, error) {
	var res board.NoteResult
	err := c.do(ctx, http.MethodPost, "/v1/notes", r, &res)
	return res, err
}

// Board fetches a repository's view.
func (c *Client) Board(ctx context.Context, repo string) (board.View, error) {
	var v board.View
	err := c.do(ctx, http.MethodGet, "/v1/board?repo="+url.QueryEscape(repo), nil, &v)
	return v, err
}

// BoardText fetches a repository's view rendered as text.
func (c *Client) BoardText(ctx context.Context, repo string) (string, error) {
	var s string
	err := c.do(ctx, http.MethodGet, "/v1/board?format=text&repo="+url.QueryEscape(repo), nil, &s)
	return s, err
}

// Repos lists repositories with claims.
func (c *Client) Repos(ctx context.Context) ([]board.RepoSummary, error) {
	var rs []board.RepoSummary
	err := c.do(ctx, http.MethodGet, "/v1/repos", nil, &rs)
	return rs, err
}

// Whoami asks the server who the token belongs to.
func (c *Client) Whoami(ctx context.Context) (board.Whoami, error) {
	var w board.Whoami
	err := c.do(ctx, http.MethodGet, "/v1/whoami", nil, &w)
	return w, err
}

// maxResponse bounds what the client reads from the server.
const maxResponse = 32 << 20
