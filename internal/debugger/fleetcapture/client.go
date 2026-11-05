// Package fleetcapture is the debugger's client of linea-fleet: the
// livecapture plugin API, reached through fleet's authenticated plugin
// ingress, and the core group and agent reads that select capture targets.
// The debugger runs no Collector listener; fleet owns Collector connections,
// authorization and delivery.
package fleetcapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPluginID is the plugin's registration ID in fleet.
	DefaultPluginID = "livecapture"
	// requestBytes and responseBytes are fleet's generic ingress bounds.
	requestBytes  = 64 << 10
	responseBytes = 1 << 20
	// requestTimeout covers one request; fleet's ingress answers within 2 s.
	requestTimeout = 5 * time.Second
)

// Config is trusted composition: it comes from linea-server's flags and the token
// file, never from API or MCP requests.
type Config struct {
	// Endpoint is fleet's management API base URL, such as https://fleet:8080.
	Endpoint string
	// Token is a fleet admin credential. Captures and channels are owned by
	// it, so every debugger caller shares them.
	Token string
	// AllowHTTP permits plaintext to a non-loopback host on a protected network.
	AllowHTTP bool
	// PluginID overrides DefaultPluginID.
	PluginID string
}

// Client calls one fleet server.
type Client struct {
	base, plugin, token string
	http                *http.Client
}

// Error is a request fleet refused, with its status and sanitized error.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("fleet returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("fleet returned HTTP %d: %s", e.Status, e.Message)
}

// StatusOf returns the HTTP status of a fleet refusal, or 0 for transport
// and decoding failures.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// New validates the configuration. Plain HTTP needs a loopback host or
// AllowHTTP. Proxies and redirects are never followed, so the token is only
// sent to the configured host.
func New(c Config) (*Client, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("fleet endpoint must be a scheme, host and port, such as https://fleet:8080")
	}
	switch u.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(u.Hostname())
		if !c.AllowHTTP && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("plain HTTP to a remote fleet needs the explicit protected-network option")
		}
	default:
		return nil, errors.New("fleet endpoint must use https or http")
	}
	token := strings.TrimSpace(c.Token)
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("fleet token is missing or malformed")
	}
	plugin := c.PluginID
	if plugin == "" {
		plugin = DefaultPluginID
	}
	if plugin != url.PathEscape(plugin) || strings.Contains(plugin, "/") {
		return nil, errors.New("invalid fleet plugin ID")
	}
	base := u.Scheme + "://" + u.Host
	return &Client{
		base:   base,
		plugin: base + "/api/v1/plugins/" + plugin + "/api/",
		token:  token,
		http: &http.Client{
			Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Timeout:       requestTimeout,
		},
	}, nil
}

// Endpoint is the configured fleet base URL; it carries no secret.
func (c *Client) Endpoint() string { return c.base }

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// do sends one request and decodes a 2xx body into out unless it is 204.
func (c *Client) do(ctx context.Context, method, target string, header http.Header, body, out any) (int, error) {
	return c.send(ctx, method, target, header, body, out, requestBytes)
}

// send is do with a request body bound.
func (c *Client) send(ctx context.Context, method, target string, header http.Header, body, out any, limit int) (int, error) {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return 0, fmt.Errorf("encode fleet request: %w", err)
		}
		if len(encoded) > limit {
			return 0, fmt.Errorf("fleet request exceeds %d KiB", limit>>10)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(encoded))
	if err != nil {
		return 0, errors.New("invalid fleet request")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	setActing(ctx, req.Header)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		// The transport error can name the URL; it never contains the token.
		return 0, fmt.Errorf("fleet unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, responseBytes+1))
	if err != nil {
		return resp.StatusCode, errors.New("fleet response was interrupted")
	}
	if len(data) > responseBytes {
		return resp.StatusCode, errors.New("fleet response exceeds 1 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, refusal(resp.StatusCode, data)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, errors.New("invalid fleet response")
		}
	}
	return resp.StatusCode, nil
}

// refusal decodes fleet's {"error":{"code","message"}} body; the plugin's
// older routes send {"error":"message"}.
func refusal(status int, data []byte) error {
	e := &Error{Status: status}
	var structured struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &structured) == nil && len(structured.Error) > 0 {
		var detail struct{ Code, Message string }
		if json.Unmarshal(structured.Error, &detail) == nil {
			e.Code, e.Message = detail.Code, detail.Message
		} else {
			_ = json.Unmarshal(structured.Error, &e.Message)
		}
	}
	if len(e.Message) > 512 {
		e.Message = e.Message[:512]
	}
	return e
}

func (c *Client) pluginURL(path string, query url.Values) string {
	if len(query) > 0 {
		return c.plugin + path + "?" + query.Encode()
	}
	return c.plugin + path
}

// Peers reads one page of connected peers. A 410 means the plugin restarted;
// restart from an empty cursor.
func (c *Client) Peers(ctx context.Context, cursor string, limit int) (*PeerPage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var page PeerPage
	_, err := c.do(ctx, http.MethodGet, c.pluginURL("v1/peers", q), nil, nil, &page)
	return &page, err
}

// CreateChannel opens a delivery channel; fleet caps its life at 5 minutes.
func (c *Client) CreateChannel(ctx context.Context, expiresAt time.Time) (*Channel, error) {
	var ch Channel
	_, err := c.do(ctx, http.MethodPost, c.pluginURL("v1/channels", nil), nil, channelRequest{ExpiresAt: expiresAt.UTC()}, &ch)
	return &ch, err
}

// DeleteChannel closes a channel; batches still queued are discarded.
func (c *Client) DeleteChannel(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, c.pluginURL("v1/channels/"+url.PathEscape(id), nil), nil, nil, nil)
	return err
}

// Next takes the channel's next batch, or nil when none is waiting. Polling
// renews the channel's consumer heartbeat; it does not acknowledge.
func (c *Client) Next(ctx context.Context, channel string) (*Envelope, error) {
	var e Envelope
	status, err := c.do(ctx, http.MethodGet, c.pluginURL("v1/channels/"+url.PathEscape(channel)+"/next", nil), nil, nil, &e)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	return &e, nil
}

// Acknowledge reports a batch's accepted records. Repeating an identical
// acknowledgement is safe.
func (c *Client) Acknowledge(ctx context.Context, channel string, ack Ack) error {
	_, err := c.do(ctx, http.MethodPost, c.pluginURL("v1/channels/"+url.PathEscape(channel)+"/acks", nil), nil, ack, nil)
	return err
}

// CreateCapture admits a capture. Admission is not activation: read the
// operation until its targets are active or terminal.
func (c *Client) CreateCapture(ctx context.Context, req CaptureRequest) (*Operation, error) {
	var op Operation
	_, err := c.do(ctx, http.MethodPost, c.pluginURL("v1/captures", nil), nil, req, &op)
	return &op, err
}

// GetCapture reads a capture's operation and target outcomes.
func (c *Client) GetCapture(ctx context.Context, id string) (*Operation, error) {
	var op Operation
	_, err := c.do(ctx, http.MethodGet, c.pluginURL("v1/captures/"+url.PathEscape(id), nil), nil, nil, &op)
	return &op, err
}

// StopCapture asks fleet to stop a capture at the given revision. A 412
// means the revision moved; read the operation and retry.
func (c *Client) StopCapture(ctx context.Context, id string, revision int64) (*Operation, error) {
	var op Operation
	h := http.Header{}
	h.Set("If-Match", strconv.Quote(strconv.FormatInt(revision, 10)))
	_, err := c.do(ctx, http.MethodPost, c.pluginURL("v1/captures/"+url.PathEscape(id)+"/stop", nil), h, nil, &op)
	return &op, err
}

func pageQuery(after string, limit int) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if after != "" {
		q.Set("after", after)
	}
	return q
}

func (c *Client) coreURL(path string, query url.Values) string {
	if len(query) > 0 {
		return c.base + path + "?" + query.Encode()
	}
	return c.base + path
}

// Groups reads one page of fleet groups.
func (c *Client) Groups(ctx context.Context, after string, limit int) (*GroupPage, error) {
	var page GroupPage
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/groups", pageQuery(after, limit)), nil, nil, &page)
	return &page, err
}

// Group reads one fleet group with its static members or selector.
func (c *Client) Group(ctx context.Context, id string) (*Group, error) {
	var g Group
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/groups/"+url.PathEscape(id), nil), nil, nil, &g)
	return &g, err
}

// GroupAgents reads one page of a group's current members.
func (c *Client) GroupAgents(ctx context.Context, id, after string, limit int) (*AgentPage, error) {
	var page AgentPage
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/groups/"+url.PathEscape(id)+"/agents", pageQuery(after, limit)), nil, nil, &page)
	return &page, err
}

// Agents reads one page of connected fleet agents matching a selector, which
// previews what a selector capture would resolve.
func (c *Client) Agents(ctx context.Context, selector, after string, limit int) (*AgentPage, error) {
	q := pageQuery(after, limit)
	q.Set("connected", "true")
	if selector != "" {
		q.Set("selector", selector)
	}
	var page AgentPage
	_, err := c.do(ctx, http.MethodGet, c.coreURL("/api/v1/agents", q), nil, nil, &page)
	return &page, err
}
