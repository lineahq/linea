// Package client is the CLI's connected mode: it sends inspect, run, test
// and diff requests to a running linea-server over its HTTP API instead of
// starting a local platform. Without an API token it talks only to loopback
// servers; with one, also to team servers over HTTPS.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lineahq/linea/internal/debugger/api"
	"github.com/lineahq/linea/internal/debugger/assertion"
	"github.com/lineahq/linea/internal/debugger/platform"
)

// cancelTimeout bounds the cancel requests sent after an interrupt.
const cancelTimeout = 5 * time.Second

// Client sends requests to one debugger server.
type Client struct {
	base  string
	token string
	http  *http.Client
	// project is the project every request works in; empty is the
	// default project.
	project string
}

// Error is a request the server refused, with its status and message.
type Error struct {
	Status  int
	Message string
	// Code and Diagnostics come from the server's OperationError body, when
	// it sent one.
	Code        string
	Diagnostics []platform.Diagnostic
	// Current is a saved artifact's metadata on an artifact conflict.
	Current *platform.ArtifactMetadata
}

func (e *Error) Error() string {
	return fmt.Sprintf("server refused the request (%d %s): %s", e.Status, http.StatusText(e.Status), e.Message)
}

// New accepts only http URLs on a loopback host (127.0.0.1, ::1 or
// localhost) without credentials, path, query or fragment. Proxies and
// redirects are never followed, so requests stay on this machine.
func New(server string) (*Client, error) { return NewWithToken(server, "") }

// NewWithToken is New presenting an API token on every request. With a
// token it also accepts https URLs on any host; a token never travels in
// plain HTTP off this machine.
func NewWithToken(server, token string) (*Client, error) { return NewWithCA(server, token, nil) }

// NewWithCA is NewWithToken also trusting the PEM certificates in ca, beside
// the system roots, for a team server whose certificate a private CA signs.
func NewWithCA(server, token string, ca []byte) (*Client, error) {
	if token != "" && strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r >= 0x7f }) {
		return nil, errors.New("the API token holds spaces or control characters")
	}
	base, err := serverBase(server, token != "")
	if err != nil {
		return nil, err
	}
	hc, err := serverHTTP(ca)
	if err != nil {
		return nil, err
	}
	return &Client{base: base, token: token, http: hc}, nil
}

// InProject returns a client whose requests work in project: they create
// work and saved artifacts there, use its runtimes and saved work, and
// narrow lists to it. Empty is the default project.
func (c *Client) InProject(project string) *Client {
	out := *c
	out.project = project
	return &out
}

// serverBase checks --server and returns its scheme, host and port. A team
// server's https URL is accepted only when the request will carry a
// credential (team).
func serverBase(server string, team bool) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", fmt.Errorf("--server %s: %w", server, err)
	}
	refuse := func(why string) (string, error) {
		if team {
			return "", fmt.Errorf("--server %s: %s; give a team server's https URL or a local http://127.0.0.1:7070", server, why)
		}
		return "", fmt.Errorf("--server %s: %s; connected mode talks only to a local linea-server, for example http://127.0.0.1:7070, unless you sign in with linea login or LINEA_TOKEN holds an API token", server, why)
	}
	switch {
	case u.Scheme == "https" && team && u.Hostname() != "":
	case u.Scheme != "http":
		return refuse("use an http URL")
	case !loopbackName(u.Hostname()):
		return refuse("not a loopback host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return refuse("give only the scheme, host and port")
	}
	host := u.Host
	if u.Port() == "" && u.Scheme == "http" {
		host = net.JoinHostPort(u.Hostname(), "80")
	}
	return u.Scheme + "://" + host, nil
}

func loopbackName(host string) bool {
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// serverHTTP reaches a debugger server directly: no proxy, no redirects. A
// non-empty ca is trusted beside the system roots.
func serverHTTP(ca []byte) (*http.Client, error) {
	t := &http.Transport{Proxy: nil}
	if len(ca) > 0 {
		pool, err := caPool(ca)
		if err != nil {
			return nil, err
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// caPool is the system roots plus the PEM certificates in ca.
func caPool(ca []byte) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the CA file holds no PEM certificate")
	}
	return pool, nil
}

// Inspect validates a configuration. It returns the server's JSON bytes,
// which are what local inspect prints, and the decoded inspection.
func (c *Client) Inspect(ctx context.Context, req api.InspectRequest) ([]byte, *platform.Inspection, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/config/validate", req, http.StatusOK)
	if err != nil {
		return nil, nil, err
	}
	var ins platform.Inspection
	if err := json.Unmarshal(raw, &ins); err != nil {
		return nil, nil, c.malformed(err)
	}
	return raw, &ins, nil
}

// Run submits a run and waits for its report. It returns the report's JSON
// bytes, which are what local run prints, and the decoded report. An
// interrupted wait cancels the run.
func (c *Client) Run(ctx context.Context, req api.Request) ([]byte, *platform.Report, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/runs", req, http.StatusAccepted)
	if err != nil {
		return nil, nil, err
	}
	var run api.Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return nil, nil, c.malformed(err)
	}
	raw, err = c.awaited(ctx, "/api/v1/runs/"+url.PathEscape(run.ID)+"/report?wait=true")
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, c.interrupted(ctx, []string{run.ID})
		}
		return nil, nil, err
	}
	var rep platform.Report
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&rep); err != nil {
		return nil, nil, c.malformed(err)
	}
	return raw, &rep, nil
}

// Export returns a run as a reviewable scenario file; the caller writes it.
func (c *Client) Export(ctx context.Context, runID string, req api.ExportRequest) (*api.ScenarioExport, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(runID)+"/export", req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.ScenarioExport
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// CompareConfigs runs two configurations on one input, waits for both runs
// and compares them. It returns the comparison's JSON bytes, which are what
// local diff prints, and the decoded comparison. An interrupted wait cancels
// both runs.
func (c *Client) CompareConfigs(ctx context.Context, req api.ComparisonRequest, ignore []string) ([]byte, *platform.Comparison, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/comparisons", req, http.StatusAccepted)
	if err != nil {
		return nil, nil, err
	}
	var runs api.ComparisonRuns
	if err := json.Unmarshal(raw, &runs); err != nil {
		return nil, nil, c.malformed(err)
	}
	raw, cmp, err := c.CompareRuns(ctx, runs.Before.ID, runs.After.ID, ignore)
	if err != nil && ctx.Err() != nil {
		return nil, nil, c.interrupted(ctx, []string{runs.Before.ID, runs.After.ID})
	}
	return raw, cmp, err
}

// CompareRuns waits for two runs and compares them; ending the wait never
// cancels them.
func (c *Client) CompareRuns(ctx context.Context, before, after string, ignore []string) ([]byte, *platform.Comparison, error) {
	q := url.Values{"wait": {"true"}}
	if len(ignore) > 0 {
		q["ignore"] = ignore
	}
	raw, err := c.awaited(ctx, "/api/v1/runs/"+url.PathEscape(before)+"/compare/"+url.PathEscape(after)+"?"+q.Encode())
	if err != nil {
		return nil, nil, err
	}
	// Numbers stay as written, so values render as the server rendered them.
	var cmp platform.Comparison
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&cmp); err != nil {
		return nil, nil, c.malformed(err)
	}
	return raw, &cmp, nil
}

// RunSuite submits the scenarios as consecutive suites of at most
// platform.MaxSuiteMembers and concatenates their reports, with the worst
// outcome. An interrupt cancels every run it submitted.
func (c *Client) RunSuite(ctx context.Context, req api.SuiteRequest, config string) (*platform.SuiteReport, error) {
	var rep *platform.SuiteReport
	var submitted []string
	all := req.Scenarios
	for offset := 0; offset < len(all); {
		end := min(offset+platform.MaxSuiteMembers, len(all))
		req.Scenarios = all[offset:end]
		raw, err := c.do(ctx, http.MethodPost, "/api/v1/suites", req, http.StatusAccepted)
		if err != nil {
			if ctx.Err() != nil {
				return nil, c.interrupted(ctx, submitted)
			}
			return nil, err
		}
		var suite api.Suite
		if err := json.Unmarshal(raw, &suite); err != nil {
			return nil, c.malformed(err)
		}
		for _, m := range suite.Members {
			if m.RunID != "" {
				submitted = append(submitted, m.RunID)
			}
		}
		raw, err = c.awaited(ctx, "/api/v1/suites/"+url.PathEscape(suite.ID)+"/report?wait=true")
		if err != nil {
			if ctx.Err() != nil {
				return nil, c.interrupted(ctx, submitted)
			}
			return nil, err
		}
		var part platform.SuiteReport
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&part); err != nil {
			return nil, c.malformed(err)
		}
		scenarios := part.Scenarios
		if rep == nil {
			// The first part carries the schema; its empty, non-nil slice
			// keeps a report without results encoding [] as local test does.
			rep = &part
			rep.Config, rep.Outcome, rep.Scenarios = config, assertion.Pass, scenarios[:0:0]
		}
		for _, s := range scenarios {
			rep.Scenarios = append(rep.Scenarios, s)
			rep.Outcome = assertion.Worst(rep.Outcome, s.Outcome)
		}
		offset = end
	}
	return rep, nil
}

// interrupted cancels the runs this client submitted, outside the ended
// context, and reports the interrupt.
func (c *Client) interrupted(ctx context.Context, runs []string) error {
	cctx, stop := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer stop()
	var errs []error
	for _, id := range runs {
		// A finished run refuses cancellation with a conflict; that is fine.
		if _, err := c.do(cctx, http.MethodPost, "/api/v1/runs/"+url.PathEscape(id)+"/cancel", nil, http.StatusAccepted); err != nil {
			var refused *Error
			if !errors.As(err, &refused) || refused.Status != http.StatusConflict {
				errs = append(errs, err)
			}
		}
	}
	err := fmt.Errorf("interrupted: cancelled %d submitted run(s)", len(runs))
	if len(errs) > 0 {
		err = fmt.Errorf("interrupted: cancelling submitted runs failed: %w", errors.Join(errs...))
	}
	return errors.Join(err, context.Cause(ctx))
}

func (c *Client) malformed(err error) error {
	return fmt.Errorf("unexpected response from the debugger server at %s: %w", c.base, err)
}

// do sends one request and returns the body of a response with the wanted
// status, or one of also. Other statuses become *Error with the server's
// message.
func (c *Client) do(ctx context.Context, method, path string, body any, want int, also ...int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	// The server refuses any POST or PUT that is not application/json,
	// bodyless ones such as cancel, rerun and keep included.
	contentType := ""
	if body != nil || method == http.MethodPost || method == http.MethodPut {
		contentType = "application/json"
	}
	return c.exchange(ctx, method, path, reader, contentType, nil, want, also...)
}

// exchange sends one request with its body and extra headers and returns the
// response body when its status is want or one of also; any other status is
// an *Error.
func (c *Client) exchange(ctx context.Context, method, path string, reader io.Reader, contentType string, header map[string]string, want int, also ...int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if c.project != "" {
		q := req.URL.Query()
		q.Set("project", c.project)
		req.URL.RawQuery = q.Encode()
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("cannot reach the debugger server at %s (is linea-server running?): %w", c.base, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("reading the response of the debugger server at %s: %w", c.base, err)
	}
	if resp.StatusCode != want && !slices.Contains(also, resp.StatusCode) {
		var e struct {
			api.OperationError
			Current *platform.ArtifactMetadata `json:"current"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return nil, &Error{Status: resp.StatusCode, Message: msg, Code: e.Code, Diagnostics: e.Diagnostics, Current: e.Current}
	}
	return raw, nil
}

// waitRetry bounds how long awaited retries a wait whose server went away.
const waitRetry = 30 * time.Second

// awaited sends a GET that waits on the server. A team server instance that
// stops while it waits (a lost connection, or a 502, 503 or 504 from the load
// balancer without an operation error) does not end the wait: it is sent
// again, reaching another instance, until such failures have lasted
// waitRetry. A wait that failed only after a while, such as a gateway's
// timeout, starts that count again.
func (c *Client) awaited(ctx context.Context, path string) ([]byte, error) {
	deadline := time.Now().Add(waitRetry)
	delay := 250 * time.Millisecond
	for {
		sent := time.Now()
		raw, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
		if err == nil || ctx.Err() != nil || !instanceGone(err) {
			return raw, err
		}
		if time.Since(sent) > waitRetry {
			deadline, delay = time.Now().Add(waitRetry), 250*time.Millisecond
		}
		if time.Now().After(deadline) {
			return raw, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, 4*time.Second)
	}
}

// instanceGone reports whether err is a wait the server did not answer: no
// response, or a gateway's error page rather than the server's refusal.
func instanceGone(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return true
	}
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return e.Code == ""
	}
	return false
}

// ListRuns returns a page of the server's runs newest first, each with its
// summary once it has a report, narrowed by rq's filter. The page's
// NextCursor, passed back in rq.Cursor, continues the list.
func (c *Client) ListRuns(ctx context.Context, rq platform.RunQuery) (*api.RunPage, error) {
	q := url.Values{}
	for k, v := range map[string]string{"scenario": rq.Scenario, "runtime": rq.Runtime, "result": rq.Result, "cursor": rq.Cursor} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if rq.Limit > 0 {
		q.Set("limit", strconv.Itoa(rq.Limit))
	}
	path := "/api/v1/runs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out api.RunPage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.malformed(err)
	}
	return &out, nil
}

// RerunSuite admits a new suite from an earlier suite's recorded inputs and
// waits for its report. An interrupt cancels the rerun's runs.
func (c *Client) RerunSuite(ctx context.Context, id string) (*platform.SuiteReport, error) {
	raw, err := c.do(ctx, http.MethodPost, "/api/v1/suites/"+url.PathEscape(id)+"/rerun", nil, http.StatusAccepted)
	if err != nil {
		return nil, err
	}
	var suite api.Suite
	if err := json.Unmarshal(raw, &suite); err != nil {
		return nil, c.malformed(err)
	}
	raw, err = c.do(ctx, http.MethodGet, "/api/v1/suites/"+url.PathEscape(suite.ID)+"/report?wait=true", nil, http.StatusOK)
	if err != nil {
		if ctx.Err() != nil {
			var runs []string
			for _, m := range suite.Members {
				if m.RunID != "" {
					runs = append(runs, m.RunID)
				}
			}
			return nil, c.interrupted(ctx, runs)
		}
		return nil, err
	}
	var rep platform.SuiteReport
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&rep); err != nil {
		return nil, c.malformed(err)
	}
	return &rep, nil
}

// SuiteJUnit returns a finished suite's report as JUnit XML.
func (c *Client) SuiteJUnit(ctx context.Context, id string) ([]byte, error) {
	return c.do(ctx, http.MethodGet, "/api/v1/suites/"+url.PathEscape(id)+"/junit", nil, http.StatusOK)
}
