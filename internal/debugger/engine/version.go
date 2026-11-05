package engine

import (
	goruntime "runtime"
	"slices"

	"github.com/lineahq/linea/internal/debugger/instrument"
	"github.com/lineahq/linea/internal/debugger/release"
)

// Versions describes the debugger build and the shapes of what it emits.
type Versions struct {
	Product          string   `json:"product_version"`
	Collector        string   `json:"collector_version"`
	Go               string   `json:"go_version"`
	ReportSchema     string   `json:"report_schema"`
	InspectionSchema string   `json:"inspection_schema"`
	EventSchema      string   `json:"event_schema"`
	Modules          []string `json:"modules"`
}

// CurrentVersions returns the versions of this build.
func CurrentVersions() Versions {
	return Versions{
		Product:          release.ProductVersion,
		Collector:        release.CollectorVersion,
		Go:               goruntime.Version(),
		ReportSchema:     ReportSchemaVersion,
		InspectionSchema: InspectionSchemaVersion,
		EventSchema:      instrument.EventSchemaVersion,
		Modules:          slices.Clone(release.Modules),
	}
}
