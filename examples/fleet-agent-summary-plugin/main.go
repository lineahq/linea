// Command agent-summary is a minimal, complete fleet plugin built only on
// pluginsdk and the standard library. It is a template, not a product:
//
//   - it serves host protocol 3 (manifest, status, opaque API) behind the
//     service token,
//   - it keeps a host session and fences its work on every epoch change,
//   - it reads the agent directory and resolves groups through HostClient,
//   - it owns its API schema and authorization (readers read, admins write).
//
// Public routes, reached through fleet at /api/v1/plugins/agent-summary/api/:
//
//	GET  v1/summary      agent counts refreshed every 10 s (reader or admin)
//	POST v1/groups/count connected members of a group or selector (admin)
//
// See docs/plugins.md and pluginsdk/README.md in the fleet repository.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lineahq/linea/pluginsdk"
)

// The registration must pin the same id, version and api_version, or fleet
// reports the plugin incompatible.
const (
	pluginID   = "agent-summary"
	version    = "0.1.0"
	apiVersion = "1"
)

type summary struct {
	Agents     int       `json:"agents"`
	Connected  int       `json:"connected"`
	Healthy    int       `json:"healthy"`
	ObservedAt time.Time `json:"observed_at"`
}

type plugin struct {
	instance     string
	serviceToken string
	host         pluginsdk.HostClient

	mu      sync.Mutex
	session pluginsdk.Session // valid only while current is true
	current bool
	// epochCtx is canceled when the epoch it was created for ends, so work
	// admitted under an old epoch stops.
	epochCtx    context.Context
	endEpoch    context.CancelFunc
	lastSummary *summary      // computed under the current epoch only
	newSession  chan struct{} // wakes the refresher when a session starts
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "plugin service listener (fleet calls it)")
	hostEndpoint := flag.String("host-endpoint", "http://127.0.0.1:8081", "fleet private host listener")
	hostTokenFile := flag.String("host-token-file", "host_token", "token the plugin presents to fleet")
	serviceTokenFile := flag.String("service-token-file", "service_token", "token fleet presents to the plugin")
	allowInsecure := flag.Bool("allow-insecure-host", false, "allow plain HTTP to a non-loopback host on a private network")
	flag.Parse()

	hostToken, err := readToken(*hostTokenFile)
	if err != nil {
		log.Fatal("host token: ", err)
	}
	serviceToken, err := readToken(*serviceTokenFile)
	if err != nil {
		log.Fatal("service token: ", err)
	}
	if hostToken == serviceToken {
		log.Fatal("host and service tokens must differ")
	}
	endpoint, err := pluginsdk.Endpoint(*hostEndpoint, *allowInsecure)
	if err != nil {
		log.Fatal("host endpoint must be https, or http to loopback unless -allow-insecure-host")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	instance := pluginsdk.RandomID() // new per process start
	p := &plugin{instance: instance, serviceToken: serviceToken,
		host:       pluginsdk.HostClient{Endpoint: endpoint, Token: hostToken, InstanceID: instance, HTTP: pluginsdk.NewHTTPClient()},
		newSession: make(chan struct{}, 1)}
	p.epochCtx, p.endEpoch = context.WithCancel(ctx)
	go p.watchSession(ctx)
	go p.refreshSummaries(ctx)

	server := &http.Server{Addr: *listen, Handler: p, ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("agent-summary %s listening on %s", version, *listen) // never log tokens
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// readToken loads a 64-hex-character secret written by new-plugin-secrets.
func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("unreadable")
	}
	token := strings.TrimSpace(string(data))
	if !pluginsdk.ValidToken(token) {
		return "", errors.New("not 64 lowercase hex characters")
	}
	return token, nil
}

// watchSession polls the host session. Fleet answers only after its manifest
// probe has seen this instance and the plugin is enabled and ready, so 409
// and 403 are normal while starting, draining or disabled.
func (p *plugin) watchSession(ctx context.Context) {
	for {
		session, err := p.host.Session(ctx)
		p.mu.Lock()
		if p.current && (err != nil || session.Epoch != p.session.Epoch) {
			// A lifecycle change, a restart or lost readiness fenced the old
			// epoch: stop its work and forget what it computed.
			p.endEpoch()
			p.epochCtx, p.endEpoch = context.WithCancel(ctx)
			p.current, p.lastSummary = false, nil
			log.Print("host epoch ended")
		}
		if err == nil && !p.current {
			p.session, p.current = session, true
			log.Printf("host session ready, permissions %v", session.Permissions)
			select {
			case p.newSession <- struct{}{}:
			default:
			}
		}
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// admitted returns the current session with a context that ends with its
// epoch, or false without a session.
func (p *plugin) admitted() (pluginsdk.Session, context.Context, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session, p.epochCtx, p.current
}

// refreshSummaries pages the agent directory in the background. Public API
// calls have a 2 s ceiling in fleet, too short to page a large fleet inline.
func (p *plugin) refreshSummaries(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		if session, epochCtx, ok := p.admitted(); ok {
			if s, err := p.countAgents(epochCtx, session); err == nil {
				p.mu.Lock()
				if p.current && p.session.Epoch == session.Epoch {
					p.lastSummary = &s
				}
				p.mu.Unlock()
			} else {
				log.Printf("agent directory read failed (status %d)", pluginsdk.StatusOf(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-p.newSession:
		}
	}
}

func (p *plugin) countAgents(ctx context.Context, session pluginsdk.Session) (summary, error) {
	out := summary{ObservedAt: time.Now().UTC()}
	query := pluginsdk.AgentQuery{Limit: 100}
	for range 1000 { // at most 100,000 agents, the fleet inventory bound
		page, err := p.host.Agents(ctx, session, query)
		if err != nil {
			return summary{}, err
		}
		for _, a := range page.Items {
			out.Agents++
			if a.Connected {
				out.Connected++
			}
			if a.Health != nil && a.Health.Healthy {
				out.Healthy++
			}
		}
		if page.NextAfter == "" || len(page.Items) == 0 {
			return out, nil
		}
		query.After = page.NextAfter
	}
	return out, nil
}

func (p *plugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Every route, including the manifest, requires the service token.
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.serviceToken)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, 401, "unauthorized")
		return
	}
	if r.URL.Path == pluginsdk.ManifestPath && r.Method == "GET" {
		// Fleet decodes the manifest strictly: no extra fields.
		reply(w, 200, pluginsdk.Manifest{ID: pluginID, Version: version, ProtocolVersion: pluginsdk.ProtocolVersion, InstanceID: p.instance, APIVersion: apiVersion})
		return
	}
	// Status and API calls carry the authenticated fleet caller.
	caller, role := r.Header.Values(pluginsdk.CallerIDHeader), r.Header.Values(pluginsdk.CallerRoleHeader)
	if len(caller) != 1 || caller[0] == "" || len(role) != 1 || (role[0] != "reader" && role[0] != "admin") {
		fail(w, 403, "forbidden")
		return
	}
	if r.URL.Path == pluginsdk.StatusPath && r.Method == "GET" {
		_, _, ok := p.admitted()
		p.mu.Lock()
		pending := p.lastSummary == nil
		p.mu.Unlock()
		reply(w, 200, map[string]any{"host_session": ok, "summary_pending": pending})
		return
	}
	route, ok := strings.CutPrefix(r.URL.Path, pluginsdk.APIPath)
	if !ok {
		fail(w, 404, "not_found")
		return
	}
	// API calls name the instance and epoch fleet authorized them under.
	session, epochCtx, ok := p.admitted()
	if !ok || r.Header.Get(pluginsdk.InstanceHeader) != p.instance || r.Header.Get(pluginsdk.EpochHeader) != session.Epoch {
		fail(w, 409, "stale_epoch")
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(epochCtx, cancel)()
	if r.URL.RawQuery != "" {
		fail(w, 400, "query_not_supported")
		return
	}
	switch {
	case route == "v1/summary" && r.Method == "GET":
		p.mu.Lock()
		s := p.lastSummary
		p.mu.Unlock()
		if s == nil {
			w.Header().Set("Retry-After", "1")
			fail(w, 503, "summary_pending")
			return
		}
		reply(w, 200, s)
	case route == "v1/groups/count" && r.Method == "POST":
		if role[0] != "admin" {
			fail(w, 403, "admin_required")
			return
		}
		p.countGroup(ctx, w, r, session)
	case route == "v1/summary" || route == "v1/groups/count":
		fail(w, 405, "method_not_allowed") // fleet turns 405 into 503; see the passthrough list
	default:
		fail(w, 404, "not_found")
	}
}

func (p *plugin) countGroup(ctx context.Context, w http.ResponseWriter, r *http.Request, session pluginsdk.Session) {
	var in struct {
		GroupID  string `json:"group_id,omitempty"`
		Selector string `json:"selector,omitempty"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	if err != nil || len(body) > 4096 || strictJSON(body, &in) != nil || (in.GroupID == "") == (in.Selector == "") {
		fail(w, 400, "invalid_request")
		return
	}
	connected := true
	out, err := p.host.ResolveGroup(ctx, session, pluginsdk.GroupResolveRequest{GroupID: in.GroupID, Selector: in.Selector, Connected: &connected})
	switch pluginsdk.StatusOf(err) {
	case 0:
		if err != nil {
			fail(w, 503, "host_unavailable")
			return
		}
		reply(w, 200, map[string]any{"connected_members": len(out.AgentIDs), "group_revision": out.Revision})
	case 400:
		fail(w, 400, "invalid_selector")
	case 404:
		fail(w, 404, "unknown_group")
	case 422:
		// 422 is not forwarded by fleet, so map it to a forwarded status.
		fail(w, 409, "group_too_large")
	default:
		fail(w, 503, "host_unavailable")
	}
}

func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing data")
	}
	return nil
}

func reply(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

// fail writes a fixed error body; never echo request data, tokens or upstream text.
func fail(w http.ResponseWriter, code int, reason string) {
	reply(w, code, map[string]any{"error": map[string]string{"code": reason, "message": fmt.Sprintf("request refused: %s", strings.ReplaceAll(reason, "_", " "))}})
}
