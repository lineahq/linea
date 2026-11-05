package pg

import (
	"context"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/artifact/contract"
)

// TestContract runs the shared repository contract on PostgreSQL and
// S3-compatible storage. Every case gets its own schema and object prefix.
func TestContract(t *testing.T) {
	environment(t)
	contract.Run(t, func(t *testing.T) contract.Backend {
		f := newFixture(t)
		return func(limits artifact.Limits) (artifact.Repository, error) {
			return Open(context.Background(), f.config(limits))
		}
	})
}
