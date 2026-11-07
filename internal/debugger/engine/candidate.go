package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lineahq/linea/internal/debugger/recommend"
)

// ErrNotVerifiable rejects a recommendation whose change cannot be expressed
// as provider overrides.
var ErrNotVerifiable = errors.New("recommendation cannot be verified")

// CandidateOverrides are the provider overrides that apply a recommendation's
// change on top of the user's unchanged configuration: yaml: URIs layered
// after its root. A list value replaces the list, so a swap is one override
// of the pipeline's processors. Only swaps are verifiable for now.
func CandidateOverrides(rec recommend.Recommendation) ([]string, error) {
	c := rec.Change
	switch {
	case c == nil:
		return nil, fmt.Errorf("%w: %s suggests no change", ErrNotVerifiable, rec.Rule)
	case c.Op != recommend.OpSwap:
		return nil, fmt.Errorf("%w: a %s change is not verified yet", ErrNotVerifiable, c.Op)
	case rec.Pipeline == "" || len(c.Processors) == 0:
		return nil, fmt.Errorf("%w: the swap names no pipeline or processors", ErrNotVerifiable)
	}
	// confmap keys are separated by "::"; IDs never contain it.
	if strings.Contains(rec.Pipeline, "::") {
		return nil, fmt.Errorf("%w: pipeline %q", ErrNotVerifiable, rec.Pipeline)
	}
	for _, id := range c.Processors {
		if id == "" || strings.Contains(id, "::") {
			return nil, fmt.Errorf("%w: processor %q", ErrNotVerifiable, id)
		}
	}
	// A JSON list is YAML whatever the IDs hold.
	list, err := json.Marshal(c.Processors)
	if err != nil {
		return nil, err
	}
	return []string{"yaml:service::pipelines::" + rec.Pipeline + "::processors: " + string(list)}, nil
}

// CandidateRequest prepares a new execution from a run's captured bundle with
// the given provider overrides layered on its configuration snapshot: the
// same inputs, setup, limits and runtime, read from the bundle and never from
// the host. The user's configuration is unchanged; the candidate's snapshot
// records the overrides (snapshot schema 4). A bundle for an out-of-process
// runtime keeps its target, so the candidate runs on the same Collector
// build; the caller binds it again. The returned request has no run ID.
func CandidateRequest(ctx context.Context, raw []byte, overrides []string) (Request, error) {
	if err := ctx.Err(); err != nil {
		return Request{}, err
	}
	if len(overrides) == 0 {
		return Request{}, fmt.Errorf("%w: a candidate needs overrides", ErrInvalidRequest)
	}
	a, snapshot, err := readBundle(raw)
	if err != nil {
		return Request{}, err
	}
	if snapshot, err = snapshot.WithOverrides(overrides); err != nil {
		return Request{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	runtime := ""
	if a.Runtime != nil {
		runtime = a.Runtime.ID
	} else if err := a.Distribution.compatible(); err != nil {
		return Request{}, err
	}
	return decodedRequest(ctx, a, snapshot, runtime)
}
