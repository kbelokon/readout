// Package hooks performs the outbound JSON calls to the optional external
// authorization and resource-prerender endpoints. The wire formats here are a
// public, documented API (README and the Helm chart): the request and response
// field names must not change. The package owns its own HTTP client with a
// fixed timeout so a hung or slow hook endpoint cannot stall a login or a
// detail render past that budget.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/kbelokon/readout/internal/config"
)

// hookTimeout bounds every hook call. These are interactive paths (a login or a
// detail render waits on them), and today they run with no timeout at all, so a
// generous bound is deliberate: it guards only against a wedged endpoint, not
// against a merely slow one.
const hookTimeout = 10 * time.Second

// responseCap limits how much of a hook response body is read. The prerender
// hook may echo back a modified resource, and a single Kubernetes object can
// legitimately approach the ~1.5 MiB etcd object ceiling, with JSON inflation
// on top; 4 MiB leaves room for that while still stopping a runaway hook.
const responseCap = 4 << 20

// Observer is the callback the web layer hands a Client so it can record hook
// call duration without internal/hooks importing Prometheus. It carries the hook
// name (authorization or prerender), the call error (nil on success), and the
// elapsed time. It is always nil-safe — a Client without an observer simply
// makes its calls.
type Observer func(hook string, err error, elapsed time.Duration)

// Hook names. They are a fixed enum (bounded metric cardinality) naming each
// observed hook call.
const (
	HookAuthorization = "authorization"
	HookPrerender     = "prerender"
)

// Client makes the outbound hook calls over a timeout-bounded HTTP client.
type Client struct {
	httpClient *http.Client
	// observe, when set, records each hook call's name, error, and elapsed time.
	// It is nil by default (the no-hook and test paths build Clients without it)
	// and every recording goes through the nil-safe observe helper.
	observe Observer
}

// NewClient returns a Client whose HTTP calls time out after hookTimeout.
func NewClient() *Client {
	return newClient(hookTimeout)
}

// newClient builds a Client whose HTTP calls time out after the given budget.
// Tests inject a small timeout to exercise the timeout lane without waiting the
// full hookTimeout.
func newClient(timeout time.Duration) *Client {
	return &Client{httpClient: &http.Client{Timeout: timeout}}
}

// SetObserver installs the per-call duration callback. The web layer wires it to
// the metrics histogram; passing nil clears it.
func (c *Client) SetObserver(observe Observer) {
	c.observe = observe
}

// observed records a finished hook call through the observer when one is set. It
// is nil-safe: with no observer it is a no-op. An empty url means no hook is
// configured, so the no-op call is NOT recorded — only real outbound calls are.
func (c *Client) observed(hook string, start time.Time, err error) {
	if c.observe != nil {
		c.observe(hook, err, time.Since(start))
	}
}

// AuthorizationRequest is the body posted to the authorization hook. Session is
// the caller's session serialized as-is, preserving its exact JSON shape.
type AuthorizationRequest struct {
	Token   map[string]any  `json:"token"`
	Session json.RawMessage `json:"session"`
}

// AuthorizationResult is the hook's decision. Allowed is a pointer so a missing
// field (nil) is distinguishable from an explicit false and defaults to allow.
type AuthorizationResult struct {
	Allowed *bool    `json:"allowed"`
	User    string   `json:"user"`
	Email   string   `json:"email"`
	Groups  []string `json:"groups"`
}

// Authorization posts req to the authorization hook at url and returns its
// decision. An empty url means no hook is configured: access is allowed and the
// result is otherwise empty.
func (c *Client) Authorization(ctx context.Context, url string, req AuthorizationRequest) (AuthorizationResult, error) {
	var result AuthorizationResult
	if url == "" {
		allowed := true
		result.Allowed = &allowed
		return result, nil
	}
	start := time.Now()
	err := c.postJSON(ctx, url, req, &result)
	c.observed(HookAuthorization, start, err)
	if err != nil {
		return AuthorizationResult{}, err
	}
	return result, nil
}

// PrerenderRequest is the body posted to the resource-prerender hook.
type PrerenderRequest struct {
	Cluster   string         `json:"cluster"`
	Namespace string         `json:"namespace"`
	Plural    string         `json:"plural"`
	Resource  map[string]any `json:"resource"`
	Links     []config.Link  `json:"links"`
}

// PrerenderResult is the hook's reply: extra links to append and an optional
// replacement resource. A nil Resource means the caller keeps the original.
type PrerenderResult struct {
	Links    []config.Link  `json:"links"`
	Resource map[string]any `json:"resource"`
}

// Prerender posts req to the resource-prerender hook at url and returns its
// reply. An empty url means no hook is configured: an empty result is returned.
func (c *Client) Prerender(ctx context.Context, url string, req *PrerenderRequest) (PrerenderResult, error) {
	var result PrerenderResult
	if url == "" {
		return result, nil
	}
	start := time.Now()
	err := c.postJSON(ctx, url, req, &result)
	c.observed(HookPrerender, start, err)
	if err != nil {
		return PrerenderResult{}, err
	}
	return result, nil
}

func (c *Client) postJSON(ctx context.Context, endpoint string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, responseCap))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The hook response body can carry endpoint-internal detail (and, on a
		// compromised hook, attacker-chosen content) that must never reach the
		// browser through a surfaced error. Log status+body server-side and return
		// an error carrying only the status -- no body. Callers map this to a
		// generic client message.
		slog.Error("hook call failed", "endpoint", endpoint, "status", resp.Status, "body", string(data))
		return fmt.Errorf("hook returned %s", resp.Status)
	}
	if len(data) == 0 || out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
