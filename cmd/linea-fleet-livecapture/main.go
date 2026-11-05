package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	capture "github.com/lineahq/linea/internal/fleetplugins/livecapture"
	"github.com/lineahq/linea/internal/fleetplugins/livecapture/authority"
)

// version is the release, set at link time with -X main.version=X.Y.Z. The
// manifest reports the plugin version (capture.Version) instead.
var version = "0.0.0-dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "livecapture plugin:", err)
		os.Exit(1)
	}
}
func run() error {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := fs.String("config", "/etc/livecapture/config.json", "operator plugin configuration")
	runtimeRole := fs.String("runtime-role", "capture_app", "existing restricted database role")
	beforeFlag := fs.String("before", "", "offline metadata purge cutoff (RFC3339)")
	haListen := fs.String("ha-listen", "", "enable active/standby supervision on a private probe listener")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return errors.New("invalid plugin arguments")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command == "purge" {
		before, err := time.Parse(time.RFC3339, *beforeFlag)
		if err != nil {
			return capture.ErrInvalid
		}
		url, err := capture.Secret(os.Getenv("LINEA_FLEET_CAPTURE_DATABASE_URL_FILE"))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		count, err := capture.Purge(ctx, url, before)
		if err != nil {
			return err
		}
		fmt.Printf("pruned capture metadata: %d\n", count)
		return nil
	}
	if command == "migrate" {
		url, err := capture.Secret(os.Getenv("LINEA_FLEET_CAPTURE_DATABASE_URL_FILE"))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		if err = capture.Migrate(ctx, url, *runtimeRole); err != nil {
			return err
		}
		fmt.Println("capture plugin schema is current")
		return nil
	}
	config, err := capture.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "probe" {
		token, err := capture.Secret(config.ServiceTokenFile)
		if err != nil {
			return err
		}
		r, _ := http.NewRequestWithContext(ctx, "GET", "http://127.0.0.1:8090/readyz", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if *haListen != "" {
			r, _ = http.NewRequestWithContext(ctx, "GET", "http://"+*haListen+"/healthz", nil)
		}
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Do(r)
		if err != nil {
			return capture.ErrUnavailable
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return capture.ErrUnavailable
		}
		return nil
	}
	if command != "serve" {
		return errors.New("unknown plugin command")
	}
	url, err := capture.Secret(os.Getenv("LINEA_FLEET_CAPTURE_DATABASE_URL_FILE"))
	if err != nil {
		return err
	}
	if *haListen != "" {
		host, _, err := net.SplitHostPort(*haListen)
		if err != nil || net.ParseIP(host) == nil {
			return capture.ErrInvalid
		}
		return authority.Run(ctx, *haListen, func(attempt context.Context) (authority.Lease, error) {
			store, err := capture.Open(attempt, url, config.ID)
			if err != nil {
				return authority.Lease{}, err
			}
			return authority.Lease{Close: store.Close, Serve: func(active context.Context) error {
				return capture.Run(active, config, store)
			}}, nil
		}, func(err error) bool {
			return errors.Is(err, capture.ErrOwnerBusy) || errors.Is(err, capture.ErrUnavailable)
		}, nil)
	}
	startup, cancelStartup := context.WithTimeout(ctx, 5*time.Second)
	store, err := capture.Open(startup, url, config.ID)
	cancelStartup()
	if err != nil {
		return err
	}
	defer store.Close()
	return capture.Run(ctx, config, store)
}
