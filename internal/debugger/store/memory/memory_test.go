package memory_test

import (
	"sync"
	"testing"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/artifact/contract"
	"github.com/lineahq/linea/internal/debugger/store/memory"
)

// TestMemoryStoreContract runs the shared repository contract; every open of a
// backend sees the same data.
func TestMemoryStoreContract(t *testing.T) {
	contract.Run(t, func(t *testing.T) contract.Backend {
		data := memory.NewData()
		return func(limits artifact.Limits) (artifact.Repository, error) { return memory.Open(data, limits), nil }
	})
}

// TestConcurrentUse writes from several goroutines; the race detector and the
// final sequence check the store's locking.
func TestConcurrentUse(t *testing.T) {
	r := memory.New(artifact.Limits{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				if _, err := r.Save(t.Context(), artifact.SaveRequest{WorkspaceID: r.Info().ID, Project: artifact.DefaultProject, Kind: artifact.Scenario, Actor: "user:test", Content: []byte("x")}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if got := r.Info().Generation; got != 160 {
		t.Fatalf("generation = %d, want 160", got)
	}
}
