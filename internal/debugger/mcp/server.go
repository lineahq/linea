// Package mcp maps the local MCP transport to shared platform operations.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Options supplies the service owned by the composing application.
type Options struct {
	Platform *platform.Service
	// WebURL is the base URL of the web client served beside this adapter,
	// such as http://127.0.0.1:7070. When set, run results carry web_url, the
	// page that opens the run; leave it empty when no web client is served.
	WebURL string
	// Authenticator identifies the principal of each MCP request; MCP clients
	// then present a bearer token. Nil trusts every request as the local
	// caller's, which only tests and in-process adapters use.
	Authenticator platform.Authenticator
	// Logger records each refused authentication; nil discards it.
	Logger *zap.Logger
}

// requestIDKey keys the ID a refused request is logged and answered with.
type requestIDKey struct{}

// callerKey keys a request's caller in the context of its handler.
type callerKey struct{}

// projectHeader names the project a client's requests work in: where new
// work and saved artifacts go, whose runtimes and saved work they use, and
// what lists show. ?project= on the endpoint URL sets it too, for clients
// that cannot send headers.
const projectHeader = "Linea-Project"

// withProject moves ?project= into the project header, refusing two
// different projects.
func withProject(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values, headers := r.URL.Query()["project"], r.Header.Values(projectHeader)
		if len(values) > 1 || len(headers) > 1 || (len(values) == 1 && len(headers) == 1 && values[0] != headers[0]) {
			http.Error(w, "a request names at most one project", http.StatusBadRequest)
			return
		}
		if len(values) == 1 {
			r = r.Clone(r.Context())
			r.Header.Set(projectHeader, values[0])
		}
		next.ServeHTTP(w, r)
	})
}

// principalKey keys the authenticated principal in a bearer token's info.
const principalKey = "linea/principal"

// callerOf is the caller identify set for the request being handled. A
// handler outside a request has none, and the platform refuses it.
func callerOf(ctx context.Context) platform.Caller {
	c, _ := ctx.Value(callerKey{}).(platform.Caller)
	return c
}

// identify gives every received request its caller: the principal the
// bearer token was verified as, or the local owner in local mode. Without
// either the caller stays zero and every operation refuses it.
func identify(local bool) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			caller := platform.Caller{Client: "mcp", RequestID: newRequestID()}
			if extra := req.GetExtra(); extra != nil && extra.Header != nil {
				caller.Project = extra.Header.Get(projectHeader)
			}
			if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
				if p, ok := extra.TokenInfo.Extra[principalKey].(platform.Principal); ok {
					caller.Principal = p
				}
			} else if local {
				caller.Principal = platform.LocalPrincipal()
			}
			return next(context.WithValue(ctx, callerKey{}, caller), method, req)
		}
	}
}

// bearer verifies each MCP request with the authenticator. The token itself
// is the authenticator's to check; the request carries it.
func bearer(a platform.Authenticator, logger *zap.Logger) func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
	return func(_ context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
		p, err := a.Authenticate(r)
		if err != nil {
			// The reason never carries the credential.
			id, _ := r.Context().Value(requestIDKey{}).(string)
			logger.Warn("authentication refused", zap.String("request_id", id), zap.String("client", r.RemoteAddr),
				zap.String("interface", "mcp"), zap.String("reason", err.Error()))
			if errors.Is(err, platform.ErrUnauthenticated) {
				return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
			}
			return nil, errors.New("authentication is unavailable; retry later")
		}
		return &auth.TokenInfo{UserID: p.ID, Extra: map[string]any{principalKey: p}}, nil
	}
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "req-" + hex.EncodeToString(b[:])
}

// New returns a Streamable HTTP adapter. It never closes the injected platform.
// The composing application is responsible for listener and access policy.
func New(opts Options) http.Handler {
	if opts.Platform == nil {
		panic("mcp: an application-owned platform is required")
	}
	server := sdk.NewServer(&sdk.Implementation{
		Name: "linea", Version: platform.CurrentVersions().Product,
	}, &sdk.ServerOptions{Instructions: serverInstructions})
	server.AddReceivingMiddleware(structuredArgumentErrors, identify(opts.Authenticator == nil))
	addInspectionTools(server, opts.Platform)
	addCollectorTools(server, opts.Platform)
	addCaptureManagementTools(server, opts.Platform)
	addFleetTools(server, opts.Platform)
	addCaptureTools(server, opts.Platform)
	addCaptureDataTools(server, opts.Platform)
	web := webLinks(strings.TrimRight(opts.WebURL, "/"))
	addRunTools(server, opts.Platform, web)
	addRunReadTools(server, opts.Platform)
	addDeletionTools(server, opts.Platform)
	addRetentionTools(server, opts.Platform)
	addSavedWorkTools(server, opts.Platform)
	addComparisonTools(server, opts.Platform, web)
	addAssertionTools(server, opts.Platform)
	addDevelopmentTools(server, opts.Platform, web)
	addTraceTools(server, opts.Platform)
	addRuntimeTools(server, opts.Platform)
	addRuntimeManagementTools(server, opts.Platform)
	addUpgradeTools(server, opts.Platform)
	addBisectTools(server, opts.Platform)
	addArtifactTools(server, opts.Platform)
	addAuditTools(server, opts.Platform)
	addProjectTools(server, opts.Platform)
	addEvidenceResources(server, opts.Platform, web)
	addDevelopmentPrompt(server)
	// An authenticated server (team mode) answers any Host, like the HTTP
	// API: every request carries a credential, so a rebound DNS name gains
	// nothing, and a proxy in front of it forwards its public name. It also
	// keeps no session state, so any of its instances answers any request; no
	// tool sends the client requests of its own.
	team := opts.Authenticator != nil
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return server
	}, &sdk.StreamableHTTPOptions{DisableLocalhostProtection: team, Stateless: team})
	if opts.Authenticator == nil {
		return withProject(handler)
	}
	logger := opts.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	authenticated := auth.RequireBearerToken(bearer(opts.Authenticator, logger), &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(withProject(handler))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		authenticated.ServeHTTP(refusalID{w, id}, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// refusalID answers a refused request with the ID it was logged under; an
// accepted request's operations carry IDs of their own.
type refusalID struct {
	http.ResponseWriter
	id string
}

func (w refusalID) WriteHeader(code int) {
	if code == http.StatusUnauthorized {
		w.Header().Set("X-Request-Id", w.id)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w refusalID) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps streamed responses streaming.
func (w refusalID) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
