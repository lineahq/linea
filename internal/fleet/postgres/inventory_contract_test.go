package postgres

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lineahq/linea/internal/fleet/auth"
	"github.com/lineahq/linea/internal/fleet/fleettest"
)

// Run only against a disposable database supplied by scripts/fleet/test-postgres.ps1.
// Each case gets its own database, since the contract expects an empty
// inventory.
func TestPostgresInventoryContract(t *testing.T) {
	n := 0
	fleettest.RunInventory(t, func(t *testing.T) fleettest.InventorySubject {
		n++
		f := newPluginFixture(t, fmt.Sprintf("fleet_inventorycontract%d", n))
		var ids [2]string
		for i := range ids {
			c, err := auth.Generate(auth.CredentialRequest{ID: strings.Repeat(fmt.Sprintf("5%d", i), 16), Name: fmt.Sprintf("agent-%d", i), Role: "agent",
				ExpiresAt: new(time.Now().Add(time.Hour))}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.InsertCredential(context.Background(), f.admin, c.Credential); err != nil {
				t.Fatal(err)
			}
			ids[i] = c.Credential.ID
		}
		return fleettest.InventorySubject{Store: f.store, Credentials: ids}
	})
}
