package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

func TestDiscoverFindsComponentPackages(t *testing.T) {
	file := func(src string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(src)} }
	files := fstest.MapFS{
		"go.mod": file("module example.com/components\n\ngo 1.26\n"),
		"captureprocessor/factory.go": file(`package captureprocessor

import "go.opentelemetry.io/collector/processor"

func NewFactory() processor.Factory { return nil }
`),
		"captureprocessor/factory_test.go": file("package captureprocessor\n"),
		"liveextension/factory.go": file(`package liveextension

import ext "go.opentelemetry.io/collector/extension"

func NewFactory() ext.Factory { return nil }
`),
		"exp/x.go": file(`package exp

import "go.opentelemetry.io/collector/connector/xconnector"

func NewFactory() xconnector.Factory { return nil }
`),
		// Not components: a helper NewFactory, a method, test data, an
		// internal package, a nested module and a main package.
		"helper/h.go":     file("package helper\n\nimport \"go.opentelemetry.io/collector/component\"\n\nfunc NewFactory() component.Factory { return nil }\n"),
		"method/m.go":     file("package method\n\nimport \"go.opentelemetry.io/collector/receiver\"\n\ntype T struct{}\n\nfunc (T) NewFactory() receiver.Factory { return nil }\n"),
		"testdata/r/r.go": file("package r\n\nimport \"go.opentelemetry.io/collector/receiver\"\n\nfunc NewFactory() receiver.Factory { return nil }\n"),
		"internal/r/r.go": file("package r\n\nimport \"go.opentelemetry.io/collector/receiver\"\n\nfunc NewFactory() receiver.Factory { return nil }\n"),
		"nested/go.mod":   file("module example.com/nested\n"),
		"nested/r.go":     file("package nested\n\nimport \"go.opentelemetry.io/collector/receiver\"\n\nfunc NewFactory() receiver.Factory { return nil }\n"),
		"cmd/main.go":     file("package main\n\nimport \"go.opentelemetry.io/collector/receiver\"\n\nfunc NewFactory() receiver.Factory { return nil }\n"),
		"broken/b.go":     file("package broken\n\nfunc {"),
		"root.go":         file("package components\n"),
	}
	module, got, err := Discover(files)
	if err != nil {
		t.Fatal(err)
	}
	want := []Component{
		{Kind: Processors, Import: "example.com/components/captureprocessor"},
		{Kind: Connectors, Import: "example.com/components/exp"},
		{Kind: Extensions, Import: "example.com/components/liveextension"},
	}
	if module != "example.com/components" || !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover = %s %+v, want %+v", module, got, want)
	}
	if e := got[0].Entry(module); e.GoMod != "example.com/components v0.0.0" || e.Import != "example.com/components/captureprocessor" {
		t.Fatalf("entry %+v", e)
	}
	if e := (Component{Kind: Receivers, Import: module}).Entry(module); e.Import != "" {
		t.Fatalf("a root package needs no import: %+v", e)
	}
	if _, _, err := Discover(fstest.MapFS{"a.go": file("package a\n")}); err == nil {
		t.Fatal("a source without go.mod was accepted")
	}
}

func TestDiscoverFindsTheStampProcessor(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "testdata", "runtimes", "stampprocessor")
	module, got, err := Discover(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	if want := []Component{{Kind: Processors, Import: module, Type: "stamp"}}; module != "example.com/linea/stampprocessor" || !reflect.DeepEqual(got, want) {
		t.Fatalf("Discover = %s %+v", module, got)
	}
}

func TestDiscoverReadsComponentTypes(t *testing.T) {
	file := func(src string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(src)} }
	files := fstest.MapFS{
		"go.mod": file("module example.com/c\n\ngo 1.26\n"),
		"literal/f.go": file(`package literal

import (
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/processor"
)

func NewFactory() processor.Factory {
	return processor.NewFactory(component.MustNewType("capture"), nil)
}
`),
		"constant/f.go": file(`package constant

import (
	"go.opentelemetry.io/collector/component"
	ext "go.opentelemetry.io/collector/extension"
)

const typeStr = "live" + "capture"
const name = "livecapture"

func NewFactory() ext.Factory { return ext.NewFactory(component.MustNewType(name), nil, nil, 0) }
`),
		// mdatagen's layout: the type is a variable of an internal package.
		"generated/internal/metadata/generated_status.go": file(`package metadata

import "go.opentelemetry.io/collector/component"

var (
	Type      = component.MustNewType("stamp")
	ScopeName = "example.com/c/generated"
)
`),
		"generated/f.go": file(`package generated

import (
	"go.opentelemetry.io/collector/connector/xconnector"
	"example.com/c/generated/internal/metadata"
)

func NewFactory() xconnector.Factory { return xconnector.NewFactory(metadata.Type, nil) }
`),
		"computed/f.go": file(`package computed

import (
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/receiver"
)

func NewFactory() receiver.Factory { return receiver.NewFactory(component.MustNewType(pick()), nil) }
`),
	}
	_, got, err := Discover(files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"example.com/c/literal": "capture", "example.com/c/constant": "livecapture",
		"example.com/c/generated": "stamp", "example.com/c/computed": "",
	}
	if len(got) != len(want) {
		t.Fatalf("Discover = %+v", got)
	}
	for _, c := range got {
		if typ, ok := want[c.Import]; !ok || c.Type != typ {
			t.Errorf("%s: type %q, want %q", c.Import, c.Type, typ)
		}
	}
}
