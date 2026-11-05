package api

import (
	"fmt"
	"net/http"

	"github.com/lineahq/linea/internal/debugger/platform"
)

// The upgrade check: a manifest moved to another release, and a
// configuration with its scenarios checked on the current and a target
// runtime.

// UpgradeCheckRequest checks a configuration, and the scenarios that
// exercise it, on the current runtime (from; empty is the built-in one) and
// a target runtime (to).
type UpgradeCheckRequest struct {
	From        string                `json:"from,omitempty"`
	To          string                `json:"to"`
	ConfigYAML  string                `json:"config_yaml,omitempty"`
	ConfigRef   *platform.ArtifactRef `json:"config_ref,omitempty"`
	ConfigLabel string                `json:"config_label,omitempty"`
	// Scenarios each run on both runtimes on one captured input.
	Scenarios []SuiteScenario `json:"scenarios,omitempty"`
	// Ignore lists OTTL paths whose differences do not count.
	Ignore []string `json:"ignore,omitempty"`
}

func (r UpgradeCheckRequest) check() platform.UpgradeCheckRequest {
	req := platform.UpgradeCheckRequest{From: r.From, To: r.To, Ignore: r.Ignore,
		Config:    platform.Source{Text: r.ConfigYAML, Artifact: r.ConfigRef, Label: r.ConfigLabel},
		Scenarios: make([]platform.ScenarioInput, 0, len(r.Scenarios))}
	for _, s := range r.Scenarios {
		req.Scenarios = append(req.Scenarios, platform.ScenarioInput{
			Scenario: platform.Source{Text: s.ScenarioYAML, Artifact: s.ScenarioRef, Label: s.ScenarioLabel}})
	}
	return req
}

// ManifestRetargetRequest moves a manifest, text or a saved revision, to a
// verified release (empty: the newest).
type ManifestRetargetRequest struct {
	Version      string                `json:"version,omitempty"`
	ManifestYAML string                `json:"manifest_yaml,omitempty"`
	ManifestRef  *platform.ArtifactRef `json:"manifest_ref,omitempty"`
}

// RetargetedManifest is the moved manifest. Moved names each module whose
// version changed; Missing the core and contrib modules the release does
// not have (unchanged; a build refuses them); Custom the other modules,
// which keep their version until the user confirms one.
type RetargetedManifest struct {
	Manifest string   `json:"manifest"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Moved    []string `json:"moved"`
	Missing  []string `json:"missing"`
	Custom   []string `json:"custom"`
	Notes    []string `json:"notes"`
}

func (s *Server) routeUpgrades() {
	s.mux.HandleFunc("POST /api/v1/upgrade-checks", s.checkUpgrade)
	s.mux.HandleFunc("POST /api/v1/manifests/retarget", s.retargetManifest)
}

// checkUpgrade runs the check and answers when it is done, as JSON or, with
// format=markdown, as its Markdown. Ending the request abandons the check,
// which cancels the runs it was waiting for.
func (s *Server) checkUpgrade(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format != "" && format != "json" && format != "markdown" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown format %q: use json or markdown", format))
		return
	}
	var req UpgradeCheckRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := s.runs.CheckUpgrade(r.Context(), callerOf(r), req.check())
	if err != nil {
		if r.Context().Err() == nil {
			writeOperationError(w, err)
		}
		return
	}
	if format == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(c.Markdown()))
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) retargetManifest(w http.ResponseWriter, r *http.Request) {
	var req ManifestRetargetRequest
	if !decode(w, r, &req) {
		return
	}
	m, err := s.runs.RetargetManifest(r.Context(), callerOf(r), platform.RetargetManifestRequest{Version: req.Version,
		Manifest: platform.Source{Text: req.ManifestYAML, Artifact: req.ManifestRef}})
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, RetargetedManifest{Manifest: m.Manifest, From: m.From, To: m.To,
		Moved: orEmpty(m.Moved), Missing: orEmpty(m.Missing), Custom: orEmpty(m.Custom), Notes: orEmpty(m.Notes)})
}

func upgradePaths() map[string]any {
	return map[string]any{
		"/api/v1/upgrade-checks": map[string]any{"post": op("checkUpgrade",
			"Check a Collector upgrade: inspect the configuration on the current runtime (from; empty is the built-in one) and the target (to), compare their feature gates for the configuration's components and the Collector's own, and, when both accept the configuration, run each scenario on both runtimes on one captured input and compare the runs. The verdict is compatible, configuration_breaks, behavior_changes or inconclusive. The request waits for the check; ending it abandons the check, which cancels the runs it was waiting for. With format=markdown the answer is the Markdown for a pull request.",
			[]any{map[string]any{"name": "format", "in": "query", "schema": map[string]any{"type": "string", "enum": []string{"json", "markdown"}}}},
			"UpgradeCheckRequest", map[string]any{
				"200": map[string]any{"description": "The check.", "content": map[string]any{
					"application/json": map[string]any{"schema": ref("UpgradeCheck")},
					"text/markdown":    map[string]any{"schema": map[string]any{"type": "string"}},
				}},
				"400": operationFailure("Malformed request, the same runtime twice, or an unknown format."),
				"403": operationFailure(forbiddenPath),
				"422": operationFailure("A source could not be read."),
				"503": operationFailure("A runtime is not available, or the queue is full; " + noDocker),
			})},
		"/api/v1/manifests/retarget": map[string]any{"post": op("retargetManifest",
			"Move an OCB manifest to a verified release: its core and contrib modules take the release's versions and the rest of the text is unchanged. It names modules the release lacks and custom modules, which keep their version. It never saves or builds.",
			nil, "ManifestRetargetRequest", map[string]any{
				"200": reply("The moved manifest.", ref("RetargetedManifest")),
				"400": operationFailure("Malformed request."),
				"403": operationFailure(forbiddenPath),
				"404": operationFailure("The release is not verified by this debugger."),
				"422": operationFailure("The manifest could not be read or parsed; diagnostics say why."),
			})},
	}
}
