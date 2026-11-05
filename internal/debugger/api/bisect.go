package api

import (
	"fmt"
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// Fleet config bisect: which version of a fleet config made the
// scenarios fail on an agent.

// ConfigBisectRequest searches one fleet config's versions on an agent.
type ConfigBisectRequest struct {
	// Agent is the fleet agent whose configuration is rebuilt.
	Agent string `json:"agent"`
	// Config is the searched fleet config; empty when the agent receives
	// exactly one layer.
	Config string `json:"config,omitempty"`
	// Good and Bad bound the range; zero good is the oldest version fleet
	// keeps, zero bad the version fleet sends the agent now.
	Good int `json:"good,omitempty"`
	Bad  int `json:"bad,omitempty"`
	// Scenarios run on each probed version; each must declare assertions.
	Scenarios []SuiteScenario `json:"scenarios"`
	// Local are the supervisor's local config files, merged under fleet's
	// layers in order.
	Local   []BisectLocalFile `json:"local,omitempty"`
	Runtime string            `json:"runtime,omitempty"`
	// Ignore lists OTTL paths whose differences do not count when the
	// culprit's runs are compared.
	Ignore []string `json:"ignore,omitempty"`
}

// BisectLocalFile is one local supervisor config file: text or a saved
// revision.
type BisectLocalFile struct {
	YAML  string                `json:"yaml,omitempty"`
	Ref   *platform.ArtifactRef `json:"ref,omitempty"`
	Label string                `json:"label,omitempty"`
}

func (r ConfigBisectRequest) bisect() platform.BisectRequest {
	req := platform.BisectRequest{AgentID: r.Agent, ConfigID: r.Config, Good: r.Good, Bad: r.Bad, Runtime: r.Runtime, Ignore: r.Ignore,
		Scenarios: make([]platform.ScenarioInput, 0, len(r.Scenarios))}
	for _, s := range r.Scenarios {
		req.Scenarios = append(req.Scenarios, platform.ScenarioInput{
			Scenario: platform.Source{Text: s.ScenarioYAML, Artifact: s.ScenarioRef, Label: s.ScenarioLabel}})
	}
	for _, l := range r.Local {
		req.Local = append(req.Local, platform.Source{Text: l.YAML, Artifact: l.Ref, Label: l.Label})
	}
	return req
}

func (s *Server) routeBisects() {
	s.mux.HandleFunc("POST /api/v1/config-bisects", s.bisectConfig)
}

// bisectConfig runs the bisect and answers when it is done, as JSON or,
// with format=markdown, as its Markdown. Ending the request abandons the
// bisect, which cancels the run it was waiting for.
func (s *Server) bisectConfig(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "markdown" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown format %q: use json or markdown", format))
		return
	}
	var req ConfigBisectRequest
	if !decode(w, r, &req) {
		return
	}
	b, err := s.runs.BisectFleetConfig(r.Context(), callerOf(r), req.bisect())
	if err != nil {
		if r.Context().Err() == nil {
			writeOperationError(w, err)
		}
		return
	}
	if format == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(b.Markdown()))
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func bisectPaths() map[string]any {
	return map[string]any{
		"/api/v1/config-bisects": map[string]any{"post": op("bisectConfig",
			"Find the fleet config version that broke the scenarios on an agent. It reads the config layers fleet sends the agent, rebuilds the agent's configuration as the OpAMP supervisor merges it for each probed version of the searched config, with the other layers at the versions fleet sends now and the given local files underneath, and runs every scenario on it. It probes the bad bound, the good bound and then halves the range; a version whose configuration is invalid or whose runs are inconclusive is skipped. The verdict is found, no_regression, already_bad or inconclusive; once a last good and first bad version are known, the culprit diffs their bodies and compares the changed scenarios' runs. Needs the server's fleet connection with an admin credential. The request waits for the bisect; ending it cancels the run it was waiting for. With format=markdown the answer is the Markdown for an incident note.",
			[]any{map[string]any{"name": "format", "in": "query", "schema": map[string]any{"type": "string", "enum": []string{"json", "markdown"}}}},
			"ConfigBisectRequest", map[string]any{
				"200": map[string]any{"description": "The bisection.", "content": map[string]any{
					"application/json": map[string]any{"schema": ref("Bisection")},
					"text/markdown":    map[string]any{"schema": map[string]any{"type": "string"}},
				}},
				"400": operationFailure("Malformed request, no scenarios, a reversed or single-version range, a config the agent does not receive, an unmanaged agent, or an unknown format."),
				"403": operationFailure(forbiddenPath + " Or fleet refused to return a config body; reading bodies needs a fleet admin credential."),
				"404": operationFailure("Fleet does not know the agent, or no longer keeps a bound's version."),
				"409": operationFailure("Fleet's layers for the agent are not consistent, such as too many matching assignments."),
				"422": operationFailure("A source could not be read."),
				"503": operationFailure("The server has no fleet connection, fleet is unreachable, a runtime is not available, or the queue is full."),
			})},
	}
}
