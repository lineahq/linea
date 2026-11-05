// Package stampdebug is the stamp processor's debugger provider for the
// runtime build tests: it counts the run's inputs and
// reports the configured value with the scenario's note.
package stampdebug

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/lineahq/linea/collector/provider"
)

// DebuggerProvider is what the debugger compiles into a runtime built
// with this module.
func DebuggerProvider() provider.Provider {
	return provider.Provider{
		Name:       "stamp",
		Components: []provider.Type{{Kind: "processor", Type: "stamp"}},
		Prepare:    prepare,
		Check: func(run provider.Static) []provider.Finding {
			var s settings
			if err := run.Settings(&s); err != nil || s.Note == "" {
				var out []provider.Finding
				for _, c := range run.Components() {
					out = append(out, provider.Finding{Component: c.ID, Message: "the scenario gives the stamp provider no note", Hint: "set mocks.providers.stamp.note"})
				}
				return out
			}
			return nil
		},
	}
}

type settings struct {
	Note string `json:"note"`
}

func prepare(_ context.Context, run provider.Run) error {
	var s settings
	if err := run.Settings(&s); err != nil {
		return err
	}
	var inputs atomic.Int32
	run.OnInput(func(context.Context, provider.Input) error {
		inputs.Add(1)
		return nil
	})
	run.OnInputsDone(func(context.Context) error {
		for _, c := range run.Components() {
			run.Supplied(c.ID, fmt.Sprintf("value %v, %d inputs, note %s", c.Config["value"], inputs.Load(), s.Note))
		}
		return nil
	})
	return nil
}
