// Package pluginsdk defines fleet's HTTP/JSON plugin host protocol 3: the
// routes a plugin serves, the host services it calls, their wire types and
// bounds, and bounded HTTP helpers. It depends only on the standard library.
//
// A plugin serves ManifestPath, StatusPath and, when granted, APIPath behind
// its service token; it calls the host through HostClient with its host
// token. The developer guide is docs/fleet/plugins.md and the package
// README; a complete example plugin is examples/fleet-agent-summary-plugin.
package pluginsdk

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ProtocolVersion is the host protocol a manifest must declare.
const ProtocolVersion = "3"

// AgentsRead is the registration permission for the agent directory and the
// agent and group resolvers.
const AgentsRead = "agents.read"

// Routes the plugin serves. Fleet sends the service token on each.
const (
	ManifestPath = "/fleet-plugin/v3/manifest"
	StatusPath   = "/fleet-plugin/v3/status"
)

// AgentsPath is the host's paged agent directory.
const AgentsPath = "/fleet-host/v3/agents"

// Headers fleet sets on status and API calls: the authenticated management
// credential ID and its role, "reader" or "admin".
const (
	CallerIDHeader   = "X-Fleet-Caller-ID"
	CallerRoleHeader = "X-Fleet-Caller-Role"
)

// Headers a delegating management client may send to name the user it acts
// for. Fleet validates and records them and forwards them on plugin API
// calls; neither fleet nor a plugin authorizes on them.
const (
	ActingIDHeader      = "X-Fleet-Acting-ID"
	ActingNameHeader    = "X-Fleet-Acting-Name"
	ActingRequestHeader = "X-Fleet-Acting-Request"
)

// Acting bounds: the principal ID, display name and request ID.
const (
	MaxActingIDBytes      = 256
	MaxActingNameBytes    = 128
	MaxActingRequestBytes = 128
)

// ErrActing is returned for a malformed acting-for header set.
var ErrActing = errors.New("invalid acting-for headers")

// Acting names the user a delegating credential acts for. It is attribution
// only: the credential's own ID and role stay the authority.
type Acting struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// ReadActing returns the acting-for headers, or nil when none are present.
// Each header appears at most once; the ID is required when any is sent.
func ReadActing(h http.Header) (*Acting, error) {
	var a Acting
	for _, f := range []struct {
		name  string
		max   int
		value *string
	}{{ActingIDHeader, MaxActingIDBytes, &a.ID}, {ActingNameHeader, MaxActingNameBytes, &a.Name}, {ActingRequestHeader, MaxActingRequestBytes, &a.RequestID}} {
		values := h.Values(f.name)
		if len(values) > 1 || len(values) == 1 && !actingValue(values[0], f.max) {
			return nil, ErrActing
		}
		if len(values) == 1 {
			*f.value = values[0]
		}
	}
	if a == (Acting{}) {
		return nil, nil
	}
	if a.ID == "" {
		return nil, ErrActing
	}
	return &a, nil
}

// Set writes the acting-for headers; a nil Acting removes them.
func (a *Acting) Set(h http.Header) {
	h.Del(ActingIDHeader)
	h.Del(ActingNameHeader)
	h.Del(ActingRequestHeader)
	if a == nil {
		return
	}
	h.Set(ActingIDHeader, a.ID)
	if a.Name != "" {
		h.Set(ActingNameHeader, a.Name)
	}
	if a.RequestID != "" {
		h.Set(ActingRequestHeader, a.RequestID)
	}
}

func actingValue(v string, max int) bool {
	return v != "" && len(v) <= max && utf8.ValidString(v) && strings.TrimSpace(v) == v && strings.IndexFunc(v, unicode.IsControl) < 0
}

// RequestTimeout bounds manifest, status and host requests on both sides.
const RequestTimeout = 750 * time.Millisecond

// ErrProtocol is returned for transport, size and decoding failures. It
// carries no upstream text.
var ErrProtocol = errors.New("plugin protocol request failed")

// HTTPError is a non-200 answer; StatusOf extracts the status.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return "plugin protocol request refused" }
func (e *HTTPError) Unwrap() error { return ErrProtocol }

// Manifest is the plugin's answer on ManifestPath. Fleet decodes it strictly:
// an unknown field makes the plugin unavailable, and an ID, version, protocol
// or API version different from the registration makes it incompatible.
type Manifest struct {
	ID              string `json:"id"`
	Version         string `json:"version"`
	ProtocolVersion string `json:"protocol_version"`
	InstanceID      string `json:"instance_id"`
	APIVersion      string `json:"api_version,omitempty"`
}

// Health is the agent's last reported component health.
type Health struct {
	Healthy    bool      `json:"healthy"`
	ObservedAt time.Time `json:"observed_at"`
}

// Agent is one entry of the host agent directory.
type Agent struct {
	ID             string     `json:"id"`
	Generation     uint64     `json:"generation"`
	Connected      bool       `json:"connected"`
	Presence       string     `json:"presence"`
	ConnectedAt    time.Time  `json:"connected_at"`
	DisconnectedAt *time.Time `json:"disconnected_at,omitempty"`
	LastReportAt   *time.Time `json:"last_report_at,omitempty"`
	Capabilities   uint64     `json:"capabilities"`
	Health         *Health    `json:"health,omitempty"`
	// Attributes are the allowlisted inventory attributes.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// AgentPage is one directory page; pass NextAfter as AgentQuery.After to
// continue. An empty NextAfter ends the listing.
type AgentPage struct {
	Items     []Agent `json:"items"`
	NextAfter string  `json:"next_after,omitempty"`
}

// ValidID accepts plugin IDs: 1-64 characters of a-z, 0-9 and '-'.
func ValidID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// ValidVersion accepts plugin and API versions: 1-64 characters of letters,
// digits and ".-+_".
func ValidVersion(version string) bool {
	if len(version) < 1 || len(version) > 64 {
		return false
	}
	for _, c := range version {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(".-+_", c)) {
			return false
		}
	}
	return true
}

// ValidToken accepts host and service tokens: 64 lowercase hex characters.
func ValidToken(token string) bool {
	data, err := hex.DecodeString(token)
	return err == nil && len(data) == 32 && hex.EncodeToString(data) == token
}

// Endpoint validates a base URL without path, query or user info. Plain HTTP
// is allowed to a loopback IP, or anywhere when allowHTTP is set.
func Endpoint(raw string, allowHTTP bool) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", ErrProtocol
	}
	if u.Scheme == "http" && !allowHTTP {
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return "", ErrProtocol
		}
	}
	return u.String(), nil
}

// NewHTTPClient has no redirect or environment-proxy credential forwarding.
func NewHTTPClient() *http.Client {
	return &http.Client{Timeout: RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{
		DialContext:     (&net.Dialer{Timeout: RequestTimeout}).DialContext,
		MaxConnsPerHost: 2, MaxIdleConnsPerHost: 2, MaxIdleConns: 2,
		IdleConnTimeout: 15 * time.Second, TLSHandshakeTimeout: RequestTimeout,
		ResponseHeaderTimeout: RequestTimeout, MaxResponseHeaderBytes: 16 << 10, DisableCompression: true,
	}}
}

// GetJSON omits upstream response/error text and limits the body before decoding.
func GetJSON(ctx context.Context, client *http.Client, endpoint, token string, headers http.Header, maxBytes int64, out any) error {
	return RequestJSON(ctx, client, "GET", endpoint, token, headers, nil, maxBytes, out)
}

// RequestJSON sends the bearer token, the headers and an optional JSON body,
// requires 200 and strictly decodes at most maxBytes into out.
func RequestJSON(ctx context.Context, client *http.Client, method, endpoint, token string, headers http.Header, body []byte, maxBytes int64, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return ErrProtocol
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	response, err := client.Do(req)
	if err != nil {
		return ErrProtocol
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return &HTTPError{Status: response.StatusCode}
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil || int64(len(responseBody)) > maxBytes {
		return ErrProtocol
	}
	decoder := json.NewDecoder(strings.NewReader(string(responseBody)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrProtocol
	}
	return nil
}
