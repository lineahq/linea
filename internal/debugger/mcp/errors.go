package mcp

import (
	"context"
	"errors"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lineahq/linea/internal/debugger/platform"
)

type errorResult struct {
	Code             platform.Code              `json:"code"`
	Message          string                     `json:"message"`
	Retryable        bool                       `json:"retryable"`
	Diagnostics      []platform.Diagnostic      `json:"diagnostics,omitempty"`
	Current          *platform.ArtifactMetadata `json:"current,omitempty"`
	CurrentStatement *string                    `json:"current_statement,omitempty"`
}

func invalidRequest(message string) error {
	return &platform.Error{Code: platform.CodeInvalidRequest, Message: message}
}

// toolError maps a failure to an MCP tool error.
func toolError(err error) *sdk.CallToolResult {
	out := errorFor(err)
	result := toolResult(out.summary(), out)
	result.IsError = true
	return result
}

// errorFor is the one mapping of platform failures to the structured error of
// tools and resources. Unclassified failures never disclose their original
// text or cause.
func errorFor(err error) errorResult {
	out := errorResult{Code: platform.CodeInternal, Message: "internal error"}
	var e *platform.Error
	if errors.As(err, &e) {
		switch e.Code {
		case platform.CodeInvalidRequest, platform.CodeValidationFailed,
			platform.CodeNotFound, platform.CodeConflict, platform.CodeResourceExhausted,
			platform.CodeUnsupportedBuild, platform.CodeForbidden, platform.CodeUnavailable,
			platform.CodeRunFailed, platform.CodeCancelled, platform.CodeStorageError,
			platform.CodeInternal:
			out.Code, out.Message, out.Retryable = e.Code, e.Message, e.Retryable
			out.Diagnostics = e.Diagnostics
			if e.Code == platform.CodeConflict {
				out.Current = e.Current
				if e.CurrentStatement != "" {
					out.CurrentStatement = &e.CurrentStatement
				}
			}
		}
	}
	return out
}

// summary names the code (a fixed platform value, never the message) and
// what else the structured error holds.
func (e errorResult) summary() string {
	text := "Tool request failed: " + string(e.Code)
	if e.Retryable {
		text += " (retryable)"
	}
	if len(e.Diagnostics) != 0 {
		text += "; " + diagnosticCounts(e.Diagnostics)
	}
	if e.Current != nil {
		text += "; the current reference is in the result"
	}
	if e.CurrentStatement != nil {
		text += "; the current statement is in the result"
	}
	return text + "."
}

// diagnosticCounts counts findings by severity without repeating them.
func diagnosticCounts(diagnostics []platform.Diagnostic) string {
	errs, warnings := 0, 0
	for _, d := range diagnostics {
		switch d.Severity {
		case "error":
			errs++
		case "warning":
			warnings++
		}
	}
	return plural(errs, "error") + ", " + plural(warnings, "warning")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func toolResult(text string, structured any) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: text}}, StructuredContent: structured,
	}
}

// The SDK validates inferred schemas before invoking typed handlers. Its
// default failure is text containing validation details. Map that failure to
// the same structured invalid_request envelope without echoing arguments.
func structuredArgumentErrors(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		result, err := next(ctx, method, req)
		if method == "tools/call" && err == nil {
			if r, ok := result.(*sdk.CallToolResult); ok && r.IsError && r.StructuredContent == nil {
				return toolError(invalidRequest("tool arguments do not match the input schema")), nil
			}
		}
		return result, err
	}
}
