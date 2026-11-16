package fleet_test

import (
	"testing"

	"github.com/lineahq/linea/internal/fleet/fleet"
	"github.com/lineahq/linea/internal/fleet/fleettest"
)

func TestMemoryInventoryContract(t *testing.T) {
	fleettest.RunInventory(t, func(*testing.T) fleettest.InventorySubject {
		return fleettest.InventorySubject{Store: fleet.NewStore(8), Credentials: [2]string{"alpha", "bravo"}}
	})
}
