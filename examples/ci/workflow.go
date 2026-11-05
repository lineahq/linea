// Package ci holds the CI examples the debugger ships. The server's Export
// for CI writes the GitHub Actions workflow into its archive.
package ci

import _ "embed"

// GitHubActions is github-actions.yml, the workflow docs/ci.md describes.
//
//go:embed github-actions.yml
var GitHubActions []byte
