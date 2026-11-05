package mcp

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Evidence resources. Each ID is one percent-encoded path segment, since
// pipeline and component IDs contain "/"; a connector stage has an empty
// pipeline segment and names its signal.
const (
	runTemplate        = "otel://runs/{run_id}"
	stageTemplate      = "otel://runs/{run_id}/stages/{pipeline}/{component}{?signal,cursor}"
	statementsTemplate = "otel://runs/{run_id}/statements/{processor}{?cursor}"
)

func runURI(id string) string { return "otel://runs/" + url.PathEscape(id) }

func stageURI(id, pipeline, component, signal string) string {
	uri := runURI(id) + "/stages/" + url.PathEscape(pipeline) + "/" + url.PathEscape(component)
	if pipeline == "" {
		uri += "?" + url.Values{"signal": {signal}}.Encode()
	}
	return uri
}

func statementsURI(id, processor string) string {
	return runURI(id) + "/statements/" + url.PathEscape(processor)
}

// withURIs adds the resource URIs of a run summary; each resolves to the same
// answer as the tool call it names.
func withURIs(id string, s *platform.RunSummary) *platform.RunSummary {
	if s == nil {
		return nil
	}
	s.URI = runURI(id)
	for i := range s.Stages {
		st := &s.Stages[i]
		st.URI = stageURI(id, st.Pipeline, st.Component, st.Signal)
		if st.Kind == "processor" && st.Type == "transform" {
			st.StatementsURI = statementsURI(id, st.Component)
		}
	}
	return s
}

// evidenceTarget is a parsed evidence resource URI.
type evidenceTarget struct {
	runID  string
	query  *platform.EvidenceQuery // nil for the run itself
	cursor string
}

// parseEvidenceURI accepts exactly the URIs the templates describe.
func parseEvidenceURI(raw string) (evidenceTarget, error) {
	bad := invalidRequest("the resource URI is not an evidence resource")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "otel" || u.Host != "runs" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.ForceQuery {
		return evidenceTarget{}, bad
	}
	segments := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	for i, s := range segments {
		if segments[i], err = url.PathUnescape(s); err != nil {
			return evidenceTarget{}, bad
		}
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return evidenceTarget{}, bad
	}
	allowed := map[string]bool{}
	target := evidenceTarget{runID: segments[0]}
	switch {
	case len(segments) == 1:
	case len(segments) == 4 && segments[1] == "stages":
		allowed = map[string]bool{"signal": true, "cursor": true}
		target.query = &platform.EvidenceQuery{Family: platform.EvidenceStageChanges, Pipeline: segments[2], Component: segments[3], Signal: values.Get("signal")}
	case len(segments) == 3 && segments[1] == "statements":
		allowed = map[string]bool{"cursor": true}
		target.query = &platform.EvidenceQuery{Family: platform.EvidenceStatements, Component: segments[2]}
	default:
		return evidenceTarget{}, bad
	}
	for key, v := range values {
		if !allowed[key] || len(v) != 1 {
			return evidenceTarget{}, bad
		}
	}
	if target.runID == "" {
		return evidenceTarget{}, bad
	}
	target.cursor = values.Get("cursor")
	return target, nil
}

// readEvidence answers a resource read with the same JSON as the matching
// tool: linea_get_run for a run, linea_get_execution_trace for a stage or
// statements resource.
func readEvidence(ctx context.Context, service *platform.Service, web webLinks, uri string) (any, error) {
	target, err := parseEvidenceURI(uri)
	if err != nil {
		return nil, err
	}
	if target.query == nil {
		return getRun(ctx, service, web, target.runID)
	}
	return queryEvidence(ctx, service, target.runID, *target.query, platform.EvidencePage{Cursor: target.cursor})
}

func addEvidenceResources(server *sdk.Server, service *platform.Service, web webLinks) {
	read := func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		out, err := readEvidence(ctx, service, web, req.Params.URI)
		if err != nil {
			return nil, resourceError(err)
		}
		data, err := json.Marshal(out)
		if err != nil {
			return nil, resourceError(err)
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(data)}}}, nil
	}
	for _, t := range []*sdk.ResourceTemplate{
		{Name: "run", URITemplate: runTemplate, MIMEType: "application/json",
			Description: "A run's state, mode, timestamps and, once terminal, its summary: the linea_get_run result."},
		{Name: "stage", URITemplate: stageTemplate, MIMEType: "application/json",
			Description: "One page of a finished run's stage changes: linea_get_execution_trace with family stage_changes. IDs are percent-encoded path segments."},
		{Name: "statements", URITemplate: statementsTemplate, MIMEType: "application/json",
			Description: "One page of a transform processor's OTTL replay in a finished run: linea_get_execution_trace with family statements."},
	} {
		server.AddResourceTemplate(t, read)
	}
}

// resourceError carries the same structured error as a tool error, as the
// JSON-RPC error's data; its message is the same fixed text.
func resourceError(err error) error {
	out := errorFor(err)
	code := int64(jsonrpc.CodeInternalError)
	switch out.Code {
	// MCP reports an unknown resource as invalid params (SEP-2164).
	case platform.CodeNotFound, platform.CodeInvalidRequest:
		code = jsonrpc.CodeInvalidParams
	}
	data, _ := json.Marshal(out)
	return &jsonrpc.Error{Code: code, Message: out.summary(), Data: data}
}
