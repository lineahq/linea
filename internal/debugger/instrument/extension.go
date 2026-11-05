package instrument

import (
	"context"
	"sync"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/extension"
)

// StatusWatcherType is the type of the debugger's own status watcher, the
// extension a run adds when a custom extension runs for real.
var StatusWatcherType = component.MustNewType("linea_status")

// StatusWatcher returns the factory of an extension that records
// component.started and component.stopped for each watched extension from
// the Collector's component status reports. An extension cannot be wrapped
// like a processor: components look extensions up by their concrete type
// (host.GetExtensions()[id].(*T)), so the instance the Collector holds must
// be the real one. The Collector reports every extension's status around
// its Start and Shutdown calls, synchronously, to each extension that is a
// componentstatus.Watcher, started or not.
func StatusWatcher(rec *Recorder, watched func(component.ID) bool) extension.Factory {
	return extension.NewFactory(StatusWatcherType,
		func() component.Config { return &struct{}{} },
		func(context.Context, extension.Settings, component.Config) (extension.Extension, error) {
			return &statusWatcher{rec: rec, watched: watched, state: map[component.ID]componentstatus.Status{}}, nil
		},
		component.StabilityLevelDevelopment)
}

type statusWatcher struct {
	component.StartFunc
	component.ShutdownFunc

	rec     *Recorder
	watched func(component.ID) bool

	mu    sync.Mutex
	state map[component.ID]componentstatus.Status
}

// ComponentStatusChanged records the end of a watched extension's Start
// (the first status after Starting) and Shutdown (the first after
// Stopping). A permanent or fatal error then is the call's error.
func (w *statusWatcher) ComponentStatusChanged(source *componentstatus.InstanceID, event *componentstatus.Event) {
	if source.Kind() != component.KindExtension || !w.watched(source.ComponentID()) {
		return
	}
	id := source.ComponentID()
	w.mu.Lock()
	previous := w.state[id]
	w.state[id] = event.Status()
	w.mu.Unlock()

	var kind Kind
	switch {
	case previous == componentstatus.StatusStarting && event.Status() != componentstatus.StatusStarting:
		kind = KindComponentStarted
	case previous == componentstatus.StatusStopping && event.Status() != componentstatus.StatusStopping:
		kind = KindComponentStopped
	default:
		return
	}
	e := Event{Kind: kind, Component: id.String(), ComponentKind: ComponentExtension, Mode: ModeReal, Source: SourceStatusWatcher}
	if s := event.Status(); (s == componentstatus.StatusPermanentError || s == componentstatus.StatusFatalError) && event.Err() != nil {
		e.Error = event.Err().Error()
	}
	w.rec.Record(e)
}
