// Package linkboundary holds the test that keeps Collector component
// implementations out of the packages the team server links. The server
// executes nothing in process: runtimes run on workers. It may link the
// Collector's data model and API packages, never a component implementation,
// the generated registry or the in-process executor.
package linkboundary
