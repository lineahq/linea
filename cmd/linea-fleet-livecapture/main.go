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
	"slices"
	"syscall"
	"time"

	capture "github.com/lineahq/linea/internal/fleetplugins/livecapture"
	"github.com/lineahq/linea/internal/fleetplugins/livecapture/authority"
	"github.com/lineahq/linea/internal/fleetplugins/livecapture/translog"
)

// version is the release, set at link time with -X main.version=X.Y.Z. The
// manifest reports the plugin version (capture.Version) instead.
var version = "0.0.0-dev"

// logFlushWait bounds how long exit waits for the last log lines.
const logFlushWait = 2 * time.Second

// main logs through translog only: a failure reaches stderr as one JSON line
// with an enum cause, never as error text.
func main() {
	log := translog.New(os.Stderr)
	err := run(log)
	log.Close(logFlushWait)
	if err != nil {
		os.Exit(1)
	}
}

// run executes one command. A failed serve logs plugin_stopped with its cause;
// a failed other command logs command_failed with its cause.
func run(log *translog.Logger) (result error) {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	known := slices.Contains([]string{"serve", "version", "purge", "migrate", "probe"}, command)
	if command == "serve" {
		log.Log(translog.EventPluginStarted, translog.Version(version))
		defer func() {
			log.Log(translog.EventPluginStopped, translog.Version(version), translog.Why(capture.CauseOf(result)))
		}()
	} else {
		defer func() {
			if result != nil {
				log.Log(translog.EventCommandFailed, translog.Cause(capture.CauseOf(result)))
			}
		}()
	}
	if !known {
		return capture.WithCause(translog.ReasonInvalidInput, errors.New("unknown plugin command"))
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := fs.String("config", "/etc/livecapture/config.json", "operator plugin configuration")
	runtimeRole := fs.String("runtime-role", "capture_app", "existing restricted database role")
	beforeFlag := fs.String("before", "", "metadata purge cutoff (RFC3339); runs beside the plugin")
	haListen := fs.String("ha-listen", "", "enable active/standby supervision on a private probe listener")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return capture.WithCause(translog.ReasonInvalidInput, errors.New("invalid plugin arguments"))
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
		return capture.WithCause(translog.ReasonConfigInvalid, err)
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
	url, err := capture.Secret(os.Getenv("LINEA_FLEET_CAPTURE_DATABASE_URL_FILE"))
	if err != nil {
		return capture.WithCause(translog.ReasonConfigInvalid, err)
	}
	if *haListen != "" {
		host, _, err := net.SplitHostPort(*haListen)
		if err != nil || net.ParseIP(host) == nil {
			return capture.WithCause(translog.ReasonInvalidInput, capture.ErrInvalid)
		}
		// standby is true while this instance waits for the authority, so the
		// wait is logged once, not once per attempt.
		standby := false
		err = authority.Run(ctx, *haListen, func(attempt context.Context) (authority.Lease, error) {
			store, err := capture.Open(attempt, url, config.ID)
			if err != nil {
				if !standby {
					standby = true
					log.Log(translog.EventAuthorityStandby, translog.Why(capture.CauseOf(err)))
				}
				return authority.Lease{}, err
			}
			standby = false
			return authority.Lease{Close: store.Close, Serve: func(active context.Context) error {
				return capture.Run(active, config, store, log)
			}}, nil
		}, func(err error) bool {
			return errors.Is(err, capture.ErrOwnerBusy) || errors.Is(err, capture.ErrUnavailable)
		}, nil)
		return supervisorFailure(err)
	}
	startup, cancelStartup := context.WithTimeout(ctx, 5*time.Second)
	store, err := capture.Open(startup, url, config.ID)
	cancelStartup()
	if err != nil {
		return openFailure(err)
	}
	defer store.Close()
	return capture.Run(ctx, config, store, log)
}

// openFailure gives a failed database open its cause: owner_busy when another
// instance holds the authority, storage_unavailable otherwise.
func openFailure(err error) error {
	if errors.Is(err, capture.ErrOwnerBusy) {
		return err
	}
	return capture.WithCause(translog.ReasonStorageUnavailable, err)
}

// supervisorFailure keeps the cause of the error a lease returned and gives
// the supervisor's own failures the supervisor_failed cause.
func supervisorFailure(err error) error {
	if err == nil || capture.CauseOf(err) != translog.ReasonFailed {
		return err
	}
	return capture.WithCause(translog.ReasonSupervisorFailed, err)
}
