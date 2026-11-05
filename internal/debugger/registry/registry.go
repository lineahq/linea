// Package registry builds the Collector component factories supported by the
// debugger. components.go is generated from build/linea-runtime.yaml, so
// the in-process runtime executes exactly the components the manifest pins;
// runtime builds generate it from the user's manifest. The release line and
// modules are generated into package release, which links no component.
package registry

//go:generate go run ./gen -manifest ../../../build/linea-runtime.yaml -source build/linea-runtime.yaml -out components.go -ottl ../compat/ottl/variant.go -features ../featureset/featureset.go -providers providers.go -gates gateowners.go -release ../release/collector.go

import "go.opentelemetry.io/collector/component"

// MustType is a convenience for package-level component types.
func MustType(name string) component.Type { return component.MustNewType(name) }

// modulesOf completes a kind's module map, keyed by component type, with
// the deprecated type alias of each factory that has one, as OCB's
// generated registry does.
func modulesOf[T component.Factory](factories map[component.Type]T, modules map[component.Type]string) map[component.Type]string {
	for _, f := range factories {
		if a, ok := any(f).(interface{ DeprecatedAlias() component.Type }); ok && a.DeprecatedAlias().String() != "" {
			modules[a.DeprecatedAlias()] = modules[f.Type()]
		}
	}
	return modules
}
