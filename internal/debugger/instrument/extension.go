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
// status watcher (componentstatus.Watcher up to v0.164.0,
// extensioncapabilities.ComponentStatusWatcher from v0.165.0), started or not.
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

// Changed is ComponentStatusChanged under the name v0.165.0 calls: its
// service notifies extensions that are an
// extensioncapabilities.ComponentStatusWatcher, which replaced
// componentstatus.Watcher. That interface does not exist in the releases
// before, so the watcher implements both methods without naming either.
func (w *statusWatcher) Changed(source *componentstatus.InstanceID, event *componentstatus.Event) {
	w.ComponentStatusChanged(source, event)
}

// ComponentStatusChanged records the end of a watched extension's Start
// (the first status after Starting) and Shutdown (the first after
// Stopping). A permanent or fatal error then is the call's error. Releases
// up to v0.164.0 call it as componentstatus.Watcher.
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
