package runresult

import (
	"time"

	"github.com/lineahq/linea/internal/debugger/config"
	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/mock"
	"github.com/lineahq/linea/internal/debugger/telemetry"
	"go.opentelemetry.io/collector/component"
	"go.uber.org/zap"
)

// Input is one fixture submission.
type Input struct {
	// Receiver selects the receiver to inject into. When nil, the single
	// receiver feeding pipelines of the payload's signal is used.
	Receiver *component.ID
	Payload  telemetry.Payload
	// ClientAddress, when set, is the IP address the receiver reports as the
	// sender (client.Info), as for a connection from that address. It is
	// what connection-based pod association in k8sattributes uses.
	ClientAddress string
	// After is how long to wait, after the previous input (or the
	// Collector's start), before this input is injected.
	After time.Duration
}

// Options configures a single isolated run.
type Options struct {
	// ConfigPath is the user's Collector YAML. It is read, never written.
	ConfigPath string
	// ConfigYAML is the configuration as text, used instead of ConfigPath.
	ConfigYAML string
	// ConfigSnapshot seals provider responses for planning/startup/reference.
	// Nil captures once at this runtime invocation; it never rewrites user YAML.
	ConfigSnapshot *config.Snapshot
	// Inputs are injected in order, each after the previous one was accepted
	// and its After wait.
	Inputs []Input
	// Settle is how long to wait after the last input before the Collector
	// shuts down. Zero lets the run derive the wait its components need
	// (the runtime's settleFor).
	Settle time.Duration
	// Mocks is the mock environment: for example the files filelog
	// receivers read. Components with an external dependency always run
	// against a mock, even when Mocks supplies no fixture for it.
	Mocks mock.Spec
	// Env supplies ${env:NAME} values, taking precedence over the process
	// environment.
	Env map[string]string
	// Logger receives Collector logs. Defaults to a no-op logger.
	Logger *zap.Logger
	// RunID, if set, is used as the run ID instead of a random one.
	RunID string
	// Observer, if set, receives every event as it is recorded. It must
	// return quickly (see instrument.Recorder.SetObserver).
	Observer func(instrument.Event)
	// Budget, if set, bounds the evidence the run keeps: events, snapshots
	// and captured output. It is shared with the reference run of the same
	// request, since both land in one evidence archive.
	Budget *instrument.Budget
}
