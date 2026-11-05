package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/lineahq/linea/internal/debugger/artifact"
	"github.com/lineahq/linea/internal/debugger/platform"
	"github.com/lineahq/linea/internal/debugger/store/pg"
)

// The server config composes linea-server --config: a shared server that
// authenticates every request and authorizes it by role bindings. It is the
// only source of host state on a team server: the store, the secrets and the
// sign-in are named here. Requests read no host file and no host
// environment variable. Relative paths are resolved against the file's
// directory.

const (
	serverConfigVersion = 1
	maxServerConfig     = 1 << 20
	// The default limits share a team server: a queue of 64,
	// 32 runs per principal (one full suite) and 1 open capture.
	defaultTeamQueue    = 64
	defaultTeamRuns     = 32
	defaultTeamCaptures = 1
	maxTeamLimit        = 1024
	// The default connection limit and its range: every browser tab,
	// CLI, worker and builder holds connections.
	defaultTeamConnections = 1024
	minTeamConnections     = 16
	maxTeamConnections     = 65536
	// defaultGitCloneSize bounds a git source the server clones, its
	// history and checkout together.
	defaultGitCloneSize = 256 << 20
	// The default retention: 10,000 terminal runs, 30 days.
	defaultTeamRetentionRuns = 10_000
	defaultTeamRetentionAge  = 30 * 24 * time.Hour
	maxTeamRetentionRuns     = 1_000_000
	minTeamRetentionAge      = time.Hour
	maxTeamRetentionDays     = 100 * 365
)

type serverConfig struct {
	Version int `yaml:"version"`
	// Store is PostgreSQL for records and S3-compatible object storage for
	// payloads; secrets are read from files.
	Store struct {
		DatabaseURLFile string `yaml:"database_url_file"`
		Objects         struct {
			Endpoint      string `yaml:"endpoint"`
			Bucket        string `yaml:"bucket"`
			Prefix        string `yaml:"prefix"`
			Region        string `yaml:"region"`
			PathStyle     bool   `yaml:"path_style"`
			AccessKeyFile string `yaml:"access_key_file"`
			SecretKeyFile string `yaml:"secret_key_file"`
		} `yaml:"objects"`
		StorageLimit string `yaml:"storage_limit"`
		RunLimit     string `yaml:"run_limit"`
	} `yaml:"store"`
	// WorkerTokenFile holds the registration token workers that dial the
	// server present; without it the server accepts none.
	WorkerTokenFile string `yaml:"worker_token_file"`
	// Builds configures server builds: builders that dial the
	// server present the token in TokenFile; without it the server takes
	// no builder, and queued builds wait. Export.Repository, when set, is
	// the image repository builders push each built runtime to, tagged
	// with the build's name; the builders hold its credentials.
	Builds struct {
		TokenFile string `yaml:"token_file"`
		Export    struct {
			Repository string `yaml:"repository"`
		} `yaml:"export"`
	} `yaml:"builds"`
	Authentication struct {
		TrustedHeaders *struct {
			Proxies []string `yaml:"proxies"`
			Issuer  string   `yaml:"issuer"`
			User    string   `yaml:"user"`
			Groups  string   `yaml:"groups"`
			Name    string   `yaml:"name"`
		} `yaml:"trusted_headers"`
		OIDC *struct {
			Issuer           string   `yaml:"issuer"`
			ClientID         string   `yaml:"client_id"`
			ClientSecretFile string   `yaml:"client_secret_file"`
			RedirectURL      string   `yaml:"redirect_url"`
			CLIClientID      string   `yaml:"cli_client_id"`
			Audiences        []string `yaml:"audiences"`
			Scopes           []string `yaml:"scopes"`
			GroupsClaim      string   `yaml:"groups_claim"`
			NameClaim        string   `yaml:"name_claim"`
			SessionTTL       string   `yaml:"session_ttl"`
		} `yaml:"oidc"`
	} `yaml:"authentication"`
	Bindings []struct {
		Group     string `yaml:"group"`
		Principal string `yaml:"principal"`
		Role      string `yaml:"role"`
		Scope     string `yaml:"scope"`
	} `yaml:"bindings"`
	Retention struct {
		Runs *int   `yaml:"runs"`
		Age  string `yaml:"age"`
	} `yaml:"retention"`
	Limits struct {
		Queue                *int `yaml:"queue"`
		RunsPerPrincipal     *int `yaml:"runs_per_principal"`
		CapturesPerPrincipal *int `yaml:"captures_per_principal"`
		// Connections bounds the API listener's open connections.
		Connections *int `yaml:"connections"`
		// GitCloneSize bounds each git source the server clones.
		GitCloneSize string `yaml:"git_clone_size"`
	} `yaml:"limits"`
}

// team is a loaded server config, ready to compose.
type team struct {
	store       pg.Config
	limits      storageLimits
	workerToken string
	buildToken  string
	buildExport string
	access      *platform.Policy
	headers     *platform.TrustedHeaders
	oidc        *platform.OIDCOptions
	queue       int
	principals  platform.PrincipalLimits
	retention   platform.RetentionPolicy
	connections int
	gitClone    int64
}

// loadConfig reads and checks a server config; every error names the file.
func loadConfig(path string) (*team, error) {
	t, err := readConfig(path)
	if err != nil {
		return nil, fmt.Errorf("server config %s: %w", path, err)
	}
	return t, nil
}

func readConfig(path string) (*team, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxServerConfig+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxServerConfig {
		return nil, errors.New("larger than 1 MiB")
	}
	var file serverConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	if file.Version != serverConfigVersion {
		return nil, fmt.Errorf("version %d: want version: %d", file.Version, serverConfigVersion)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	t := &team{}
	if t.store, err = teamStore(file, resolve); err != nil {
		return nil, err
	}
	var storage, run sizeFlag
	for _, s := range []struct {
		name, v string
		flag    *sizeFlag
	}{{"store.storage_limit", file.Store.StorageLimit, &storage}, {"store.run_limit", file.Store.RunLimit, &run}} {
		if s.v != "" {
			if err := s.flag.Set(s.v); err != nil {
				return nil, fmt.Errorf("%s: %w", s.name, err)
			}
		}
	}
	if t.limits, err = serveStorage(storage, run); err != nil {
		return nil, err
	}
	t.store.Limits = artifact.Limits{StorageBytes: t.limits.storage, RunBytes: t.limits.run,
		BuildBytes: min(artifact.BuildAllowance, t.limits.storage/8)}
	if file.WorkerTokenFile != "" {
		if t.workerToken, err = teamWorkerToken(resolve(file.WorkerTokenFile)); err != nil {
			return nil, fmt.Errorf("worker_token_file: %w", err)
		}
	}
	if file.Builds.TokenFile != "" {
		if t.buildToken, err = teamWorkerToken(resolve(file.Builds.TokenFile)); err != nil {
			return nil, fmt.Errorf("builds.token_file: %w", err)
		}
		if t.buildToken == t.workerToken {
			return nil, errors.New("builds.token_file: builders and workers need different tokens")
		}
	}
	// The platform checks the repository when it opens.
	t.buildExport = file.Builds.Export.Repository

	if h := file.Authentication.TrustedHeaders; h != nil {
		if len(h.Proxies) == 0 || h.Issuer == "" || h.User == "" {
			return nil, errors.New("authentication.trusted_headers needs proxies, issuer and user")
		}
		th := &platform.TrustedHeaders{Issuer: h.Issuer, User: h.User, Groups: h.Groups, Name: h.Name}
		for _, p := range h.Proxies {
			prefix, err := netip.ParsePrefix(p)
			if err != nil {
				addr, aerr := netip.ParseAddr(p)
				if aerr != nil {
					return nil, fmt.Errorf("authentication.trusted_headers.proxies: %q is not an address or CIDR prefix", p)
				}
				prefix = netip.PrefixFrom(addr, addr.BitLen())
			}
			th.Proxies = append(th.Proxies, prefix.Masked())
		}
		t.headers = th
	}
	if o := file.Authentication.OIDC; o != nil {
		if t.oidc, err = teamOIDC(o.Issuer, o.ClientID, resolve(o.ClientSecretFile), o.RedirectURL, o.CLIClientID, o.SessionTTL); err != nil {
			return nil, fmt.Errorf("authentication.oidc: %w", err)
		}
		t.oidc.Audiences, t.oidc.Scopes, t.oidc.GroupsClaim, t.oidc.NameClaim = o.Audiences, o.Scopes, o.GroupsClaim, o.NameClaim
	}
	if t.headers == nil && t.oidc == nil {
		return nil, errors.New("authentication needs a sign-in method, oidc or trusted_headers; API tokens are created by signed-in users")
	}

	bindings := make([]platform.Binding, 0, len(file.Bindings))
	for i, b := range file.Bindings {
		scope, err := platform.ParseScope(b.Scope)
		if err != nil {
			return nil, fmt.Errorf("bindings[%d]: %w", i, err)
		}
		bindings = append(bindings, platform.Binding{Subject: platform.Subject{Group: b.Group, Principal: b.Principal}, Role: b.Role, Scope: scope})
	}
	if len(bindings) == 0 {
		return nil, errors.New("bindings are required: without one, nobody may do anything")
	}
	if t.access, err = platform.NewPolicy(bindings); err != nil {
		return nil, fmt.Errorf("bindings: %w", err)
	}

	t.queue, t.principals = defaultTeamQueue, platform.PrincipalLimits{Runs: defaultTeamRuns, Captures: defaultTeamCaptures}
	for _, l := range []struct {
		name string
		v    *int
		dst  *int
	}{
		{"limits.queue", file.Limits.Queue, &t.queue},
		{"limits.runs_per_principal", file.Limits.RunsPerPrincipal, &t.principals.Runs},
		{"limits.captures_per_principal", file.Limits.CapturesPerPrincipal, &t.principals.Captures},
	} {
		if l.v == nil {
			continue
		}
		if *l.v < 1 || *l.v > maxTeamLimit {
			return nil, fmt.Errorf("%s %d: give a number from 1 to %d", l.name, *l.v, maxTeamLimit)
		}
		*l.dst = *l.v
	}
	if t.principals.Runs > t.queue {
		return nil, fmt.Errorf("limits.runs_per_principal %d exceeds limits.queue %d", t.principals.Runs, t.queue)
	}
	t.connections = defaultTeamConnections
	if c := file.Limits.Connections; c != nil {
		if *c < minTeamConnections || *c > maxTeamConnections {
			return nil, fmt.Errorf("limits.connections %d: give a number from %d to %d", *c, minTeamConnections, maxTeamConnections)
		}
		t.connections = *c
	}
	t.gitClone = defaultGitCloneSize
	if v := file.Limits.GitCloneSize; v != "" {
		if t.gitClone, err = parseSize(v); err != nil {
			return nil, fmt.Errorf("limits.git_clone_size: %w", err)
		}
	}
	if t.retention, err = teamRetention(file.Retention.Runs, file.Retention.Age); err != nil {
		return nil, err
	}
	return t, nil
}

// teamRetention reads the retention section; what it leaves out keeps the
// default. An age is a number of days ("30d") or a Go duration ("12h").
func teamRetention(runs *int, age string) (platform.RetentionPolicy, error) {
	p := platform.RetentionPolicy{Runs: defaultTeamRetentionRuns, Age: defaultTeamRetentionAge}
	if runs != nil {
		if *runs < 1 || *runs > maxTeamRetentionRuns {
			return p, fmt.Errorf("retention.runs %d: give a number from 1 to %d", *runs, maxTeamRetentionRuns)
		}
		p.Runs = *runs
	}
	if age != "" {
		d, err := time.ParseDuration(age)
		if n, ok := strings.CutSuffix(age, "d"); ok {
			var days int
			days, err = strconv.Atoi(n)
			d = time.Duration(min(max(days, 0), maxTeamRetentionDays)) * 24 * time.Hour
		}
		if err != nil || d < minTeamRetentionAge {
			return p, fmt.Errorf("retention.age %q: give days such as 30d, or a duration of at least 1h", age)
		}
		p.Age = d
	}
	return p, nil
}

// minWorkerToken is the shortest registration token a server config may hold.
const minWorkerToken = 32

// teamWorkerToken reads the registration token of dialing workers.
func teamWorkerToken(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if len(token) < minWorkerToken || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("the token is one line of at least %d characters without spaces", minWorkerToken)
	}
	return token, nil
}

// teamOIDC checks the OIDC settings; the provider itself is read at
// startup. The client secret is read from its file, never the server config.
func teamOIDC(issuer, clientID, secretFile, redirect, cliClientID, sessionTTL string) (*platform.OIDCOptions, error) {
	if issuer == "" || clientID == "" || redirect == "" {
		return nil, errors.New("issuer, client_id and redirect_url are required")
	}
	for name, v := range map[string]string{"issuer": issuer, "redirect_url": redirect} {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname()))) {
			return nil, fmt.Errorf("%s %q: give an https URL, or http on loopback", name, v)
		}
	}
	if u, _ := url.Parse(redirect); u.Path != "/auth/callback" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("redirect_url %q: give this server's URL ending in /auth/callback", redirect)
	}
	o := &platform.OIDCOptions{Issuer: issuer, ClientID: clientID, RedirectURL: redirect, CLIClientID: cliClientID}
	if secretFile != "" {
		raw, err := os.ReadFile(secretFile)
		if err != nil {
			return nil, fmt.Errorf("client_secret_file: %w", err)
		}
		if o.ClientSecret = strings.TrimSpace(string(raw)); o.ClientSecret == "" {
			return nil, fmt.Errorf("client_secret_file %s is empty", secretFile)
		}
	}
	if sessionTTL != "" {
		d, err := time.ParseDuration(sessionTTL)
		if err != nil || d <= 0 || d > platform.MaxSessionTTL {
			return nil, fmt.Errorf("session_ttl %q: give a duration up to %s, such as 12h", sessionTTL, platform.MaxSessionTTL)
		}
		o.SessionTTL = d
	}
	return o, nil
}

// teamStore reads store's connection settings. The database URL and the
// object storage keys hold secrets, so the server config names files holding
// them; each is read once, at startup.
func teamStore(file serverConfig, resolve func(string) string) (pg.Config, error) {
	st, o := file.Store, file.Store.Objects
	if st.DatabaseURLFile == "" || o.Bucket == "" || o.AccessKeyFile == "" || o.SecretKeyFile == "" {
		return pg.Config{}, errors.New("store needs database_url_file and objects with bucket, access_key_file and secret_key_file: a team server keeps its records in PostgreSQL and its payloads in object storage")
	}
	secret := func(name, path string) (string, error) {
		raw, err := os.ReadFile(resolve(path))
		if err != nil {
			return "", fmt.Errorf("store.%s: %w", name, err)
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", fmt.Errorf("store.%s: %s is empty", name, path)
		}
		return v, nil
	}
	var c pg.Config
	var err error
	if c.DatabaseURL, err = secret("database_url_file", st.DatabaseURLFile); err != nil {
		return pg.Config{}, err
	}
	if c.S3.AccessKey, err = secret("objects.access_key_file", o.AccessKeyFile); err != nil {
		return pg.Config{}, err
	}
	if c.S3.SecretKey, err = secret("objects.secret_key_file", o.SecretKeyFile); err != nil {
		return pg.Config{}, err
	}
	c.S3.Bucket, c.S3.Prefix, c.S3.Region, c.S3.UsePathStyle = o.Bucket, o.Prefix, o.Region, o.PathStyle
	if o.Endpoint != "" {
		u, err := url.Parse(o.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return pg.Config{}, fmt.Errorf("store.objects.endpoint: %q is not an http or https URL", o.Endpoint)
		}
		c.S3.Endpoint, c.S3.UseTLS = o.Endpoint, u.Scheme == "https"
	}
	return c, nil
}
