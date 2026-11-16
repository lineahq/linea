package runresult

// SelfCheck is the outcome of one runtime self-check: an assumption about
// Collector internals that the instrumentation relies on, probed once per
// process on a small pipeline of stand-in components. A failed check means
// evidence that depends on the assumption may be wrong in this runtime.
type SelfCheck struct {
	// Code names the assumption (SelfCheck* codes).
	Code string `json:"code"`
	OK   bool   `json:"ok"`
	// Detail says what the probe found; set on failures only.
	Detail string `json:"detail,omitempty"`
}

// Self-check codes.
const (
	// SelfCheckProbe: the probe itself could not run.
	SelfCheckProbe = "probe"
	// SelfCheckPipelineAttribution: component loggers carry the pipeline
	// ID, and a processor in two pipelines runs as two instances.
	SelfCheckPipelineAttribution = "pipeline_attribution"
	// SelfCheckConnectorRoutes: a connector's deliveries name the pipeline
	// they went to.
	SelfCheckConnectorRoutes = "connector_routes"
	// SelfCheckEmissionContext: the Collector passes the caller's context
	// through, so outputs link to the input that caused them.
	SelfCheckEmissionContext = "emission_context"
	// SelfCheckObjectIdentity: pdata record objects identify records.
	SelfCheckObjectIdentity = "object_identity"
	// SelfCheckFanoutClone: a fan-out copies data for a consumer that
	// mutates it and passes the original to the others.
	SelfCheckFanoutClone = "fanout_clone"
	// SelfCheckExtensionStatus: status reports reach a watching extension,
	// so custom extensions' starts and stops are recorded.
	SelfCheckExtensionStatus = "extension_status"
	// SelfCheckLifecycle: the Collector starts, delivers every injected
	// record to its exporters and shuts down cleanly.
	SelfCheckLifecycle = "lifecycle"
)

// FailedSelfChecks returns the checks that did not pass.
func FailedSelfChecks(checks []SelfCheck) []SelfCheck {
	var out []SelfCheck
	for _, c := range checks {
		if !c.OK {
			out = append(out, c)
		}
	}
	return out
}
