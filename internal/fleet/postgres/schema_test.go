package postgres

import (
	"os"
	"strings"
	"testing"
)

// Every migration file is applied: the schema version follows the list, and
// the list names each file once, in order.
func TestSchemaVersionCoversEveryMigration(t *testing.T) {
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	files := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	if len(files) != len(migrations) || schemaVersion != len(migrations) {
		t.Fatalf("%d migration files, %d embedded, schema version %d", len(files), len(migrations), schemaVersion)
	}
	for i, name := range files {
		source, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ReplaceAll(string(source), "\r\n", "\n") != strings.ReplaceAll(migrations[i], "\r\n", "\n") {
			t.Fatalf("migration %d is not %s", i+1, name)
		}
	}
}
