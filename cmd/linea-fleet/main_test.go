package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A failed service is reported by the transition log with an enumerated
// reason. The error run returns, which main prints, carries none of the
// database error text, and neither stream shows the database secret.
func TestServiceFailureReachesTheLogAsAReasonOnly(t *testing.T) {
	const password = "s3cret-database-password"
	t.Setenv("LINEA_FLEET_DATABASE_URL", "postgres://fleet:"+password+"@127.0.0.1:1/fleet?sslmode=disable")
	t.Setenv("LINEA_FLEET_DATABASE_URL_FILE", "")
	var stdout, stderr bytes.Buffer
	err := run([]string{"--server-id", "test-fleet"}, &stdout, &stderr)
	if err != errServiceFailed {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error()+stdout.String()+stderr.String(), password) {
		t.Fatal("the database password was written")
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stderr line is not JSON: %q", line)
		}
		events = append(events, m)
	}
	if len(events) != 2 || events[0]["msg"] != "process_started" || events[0]["version"] != version {
		t.Fatalf("events = %v", events)
	}
	if stop := events[1]; stop["msg"] != "process_stopped" || stop["reason"] != "database_unavailable" || stop["level"] != "ERROR" {
		t.Fatalf("stop line = %v", stop)
	}
}

// A usage or configuration error is refused before the service starts, with
// its authored message and no log line.
func TestConfigurationErrorsAreRefusedBeforeLogging(t *testing.T) {
	t.Setenv("LINEA_FLEET_DATABASE_URL", "postgres://x@127.0.0.1:1/x")
	t.Setenv("LINEA_FLEET_DATABASE_URL_FILE", "")
	var stdout, stderr bytes.Buffer
	err := run([]string{"--drain-timeout", "5m", "--server-id", "test-fleet"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "drain-timeout") {
		t.Fatalf("err = %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
