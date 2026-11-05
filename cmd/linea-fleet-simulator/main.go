// Optional protocol-capacity tool. It runs one official opamp-go client per
// simulated agent and is never linked into the fleet binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lineahq/linea/internal/fleet/agentsimulator"
)

func run() error {
	var c agentsimulator.Config
	flag.StringVar(&c.Endpoint, "endpoint", "", "OpAMP WebSocket endpoint")
	path := flag.String("token-file", "", "deployment bearer secret file")
	flag.StringVar(&c.CheckpointDirectory, "checkpoint-dir", "", "optional read-only driver phase-release directory")
	flag.BoolVar(&c.AllowInsecure, "allow-insecure-transport", false, "acknowledge protected plaintext transport")
	flag.IntVar(&c.Agents, "agents", 100, "independent simulated sessions (1-20000)")
	flag.IntVar(&c.ConnectRate, "connect-rate", 10, "connection ramp per second (1-1000)")
	flag.IntVar(&c.PayloadBytes, "payload-bytes", 1024, "synthetic discarded agent description bytes (0-65536)")
	flag.IntVar(&c.ReconnectPercent, "reconnect-percent", 10, "planned reconnect percentage (0-25)")
	flag.IntVar(&c.HealthChangeEvery, "health-change-every", 3, "toggle health every N heartbeat intervals; 0 disables")
	flag.DurationVar(&c.Heartbeat, "heartbeat", 30*time.Second, "opamp-go heartbeat interval; 30s matches the Collector")
	flag.DurationVar(&c.Duration, "duration", 30*time.Second, "steady phase duration")
	flag.DurationVar(&c.Hold, "hold", 10*time.Second, "verification hold after reconnect")
	flag.IntVar(&c.ChurnRate, "churn-rate", 0, "identities replaced per second during steady reporting (0-200)")
	storm := flag.Bool("storm", false, "shorthand for -storms 1")
	flag.IntVar(&c.Storms, "storms", 0, "outage windows after reconnects in which the driver breaks sessions and recovery is timed (0-8)")
	flag.DurationVar(&c.StormTimeout, "storm-timeout", 2*time.Minute, "recovery limit per storm (10s-10m)")
	flag.BoolVar(&c.AcceptConfig, "accept-config", false, "accept remote config like a supervised Collector")
	flag.IntVar(&c.FailPercent, "fail-percent", 0, "with -accept-config, agents that report FAILED for a config (0-100)")
	flag.DurationVar(&c.ApplyDelay, "apply-delay", 0, "with -accept-config, time from APPLYING to the outcome (0-1m)")
	flag.IntVar(&c.Builds, "builds", 0, "report AvailableComponents from this many builds; 0 disables (0-64)")
	flag.IntVar(&c.MinimalPercent, "minimal-percent", 0, "with -builds, agents on a build without the filelog receiver (0-100)")
	flag.Parse()
	if *storm && c.Storms == 0 {
		c.Storms = 1
	}
	if flag.NArg() != 0 {
		return errors.New("invalid simulator arguments")
	}
	f, err := os.Open(*path)
	if err != nil {
		return errors.New("deployment secret file unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(b) > 16384 {
		return errors.New("deployment secret file invalid")
	}
	c.Token = strings.TrimSpace(string(b))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	encoder := json.NewEncoder(os.Stdout)
	_, err = agentsimulator.Run(ctx, c, func(o agentsimulator.Observation) { _ = encoder.Encode(o) })
	return err
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "agent simulator:", err)
		os.Exit(1)
	}
}
