package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	capture "github.com/lineahq/linea/internal/fleetplugins/livecapture"
	"github.com/lineahq/linea/internal/fleetplugins/livecapture/client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "capture consumer:", err)
		os.Exit(1)
	}
}
func run() error {
	endpoint := flag.String("endpoint", "", "public plugin API ingress prefix")
	tokenFile := flag.String("token-file", "", "management bearer secret file")
	channel := flag.String("channel", "", "previously opened owner-bound channel")
	duration := flag.Duration("duration", 10*time.Second, "maximum receive duration")
	ratio := flag.Float64("accept-ratio", 1, "fraction of offered records explicitly accepted")
	verify := flag.Bool("verify-redaction", false, "verify standard denied attributes are absent")
	allowHTTP := flag.Bool("allow-insecure-http", false, "acknowledge protected HTTP transport")
	flag.Parse()
	if flag.NArg() != 0 || *duration <= 0 || *duration > 5*time.Minute {
		return errors.New("invalid consumer arguments")
	}
	token, err := capture.Secret(*tokenFile)
	if err != nil {
		return err
	}
	c, err := client.New(*endpoint, token, *allowHTTP)
	if err != nil {
		return err
	}
	defer c.Close()
	c.OnReceipt = func(summary client.Summary) { _ = json.NewEncoder(os.Stdout).Encode(summary) }
	ctx, cancelSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancelSignal()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"state": "consumer_ready", "channel_id": *channel})
	summary, err := c.Consume(ctx, *channel, *ratio, *verify)
	_ = json.NewEncoder(os.Stdout).Encode(summary)
	return err
}
