package engine

import (
	"time"

	"github.com/lineahq/linea/internal/debugger/runresult"

	"github.com/lineahq/linea/internal/debugger/scenario"
	"github.com/lineahq/linea/internal/debugger/telemetry"
)

// Modes of Report.Timestamps.
const (
	// TimestampsRecorded: the inputs were injected with their timestamps as
	// written.
	TimestampsRecorded = scenario.TimestampsRecorded
	// TimestampsRebased: every input timestamp was shifted by Offset
	// (execution.timestamps: rebase).
	TimestampsRebased = "rebased"
)

// Timestamps relates the input timestamps to the wall clock of the run.
type Timestamps struct {
	Mode string `json:"mode"`
	// LatestInput is the latest record timestamp of the inputs, as written.
	LatestInput *time.Time `json:"latest_input,omitempty"`
	// Offset is the run's start minus LatestInput, truncated to whole
	// seconds: how old the inputs were (recorded), or what was added to each
	// input timestamp (rebased). Normalized reports omit it.
	Offset string `json:"offset,omitempty"`
	// WallClock lists the configuration expressions that read the wall
	// clock (OTTL Now()); their outcome depends on Offset unless rebased.
	WallClock []runresult.ClockRead `json:"wall_clock,omitempty"`
}

// timestamps describes the inputs' timestamps relative to now, the run's
// start, and with rebase shifts them in place first, so that the instrumented
// and the reference run see the same records. It returns nil when there is
// nothing to report.
func timestamps(inputs []runresult.Input, rebase bool, reads []runresult.ClockRead, now time.Time) *Timestamps {
	payloads := make([]telemetry.Payload, len(inputs))
	for i, in := range inputs {
		payloads[i] = in.Payload
	}
	latest := telemetry.Latest(payloads...)
	if latest.IsZero() && !rebase && len(reads) == 0 {
		return nil
	}
	t := &Timestamps{Mode: TimestampsRecorded, WallClock: reads}
	if rebase {
		t.Mode = TimestampsRebased
	}
	if latest.IsZero() {
		return t
	}
	l := latest.UTC()
	t.LatestInput = &l
	offset := now.Sub(latest).Truncate(time.Second)
	t.Offset = offset.String()
	if rebase {
		for _, p := range payloads {
			p.Shift(offset)
		}
	}
	return t
}

// RebaseShift is how far a rebased run moved its input timestamps; zero when
// they were recorded as written. Comparing two runs' telemetry undoes it.
func (t *Timestamps) RebaseShift() time.Duration { return t.rebaseShift() }

// rebaseShift is the shift Normalize undoes: the Offset of a rebased run.
func (t *Timestamps) rebaseShift() time.Duration {
	if t == nil || t.Mode != TimestampsRebased || t.Offset == "" {
		return 0
	}
	d, err := time.ParseDuration(t.Offset)
	if err != nil {
		return 0
	}
	return d
}
