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

// The server config composes linea-server: a shared server that
// authenticates every request and authorizes it by role bindings. It is the
// only source of host state on a team server: the store, the secrets and the
// sign-in are named here, in a file (--config), in LINEA_* environment
// variables (envconfig.go), or both, where the environment wins. Requests
// read no host file and no host environment variable. A file's relative
// paths are resolved against its directory, an environment variable's against
// the working directory.

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
		// DatabaseURLFile holds the whole URL, password included.
		// DatabaseURL is a URL without a password that
		// DatabasePasswordFile completes: give one of the two ways.
		DatabaseURLFile      string `yaml:"database_url_file"`
		DatabaseURL          string `yaml:"database_url"`
		DatabasePasswordFile string `yaml:"database_password_file"`
		Objects              struct {
			Endpoint  string `yaml:"endpoint"`
			Bucket    string `yaml:"bucket"`
			Prefix    string `yaml:"prefix"`
			Region    string `yaml:"region"`
			PathStyle bool   `yaml:"path_style"`
			// CreateBucket makes the bucket at startup when it is
			// missing and the credentials allow it.
			CreateBucket  bool   `yaml:"create_bucket"`
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
		// Admin is the built-in admin account, on when it has a password.
		Admin *struct {
			PasswordFile string `yaml:"password_file"`
		} `yaml:"admin"`
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
	// adminPassword turns on the built-in admin account; empty is off.
	adminPassword string
	queue         int
	principals    platform.PrincipalLimits
	retention     platform.RetentionPolicy
	connections   int
	gitClone      int64
}

// errNoConfig is loadConfig's answer to neither a file nor a single
// environment variable that sets a key.
var errNoConfig = errors.New("no server config")

// loadConfig reads and checks a server config from the file at path, which
// may be empty, and the environment, which overrides it. Every error names
// the file, or the environment when there is none, and a key that a variable
// set is named by its variable.
func loadConfig(path string, env environment) (*team, error) {
	where := "server config " + path
	if path == "" {
		where = "server config (environment)"
	}
	var file serverConfig
	if path != "" {
		var err error
		if file, err = readConfigFile(path); err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
	}
	set, err := env.apply(&file)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if path == "" && len(set) == 0 {
		return nil, errNoConfig
	}
	resolve := func(p string) string { return p }
	if path != "" {
		base, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		resolve = func(p string) string {
			if p == "" || filepath.IsAbs(p) {
				return p
			}
			return filepath.Join(base, p)
		}
	}
	t, err := composeTeam(file, resolve)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", where, nameVariables(err.Error(), set))
	}
	return t, nil
}

// readConfigFile reads the server config file at path.
func readConfigFile(path string) (serverConfig, error) {
	var file serverConfig
	f, err := os.Open(path)
	if err != nil {
		return file, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxServerConfig+1))
	if err != nil {
		return file, err
	}
	if len(raw) > maxServerConfig {
		return file, errors.New("larger than 1 MiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return file, err
	}
	if file.Version != serverConfigVersion {
		return file, fmt.Errorf("version %d: want version: %d", file.Version, serverConfigVersion)
	}
	return file, nil
}

// composeTeam checks a server config, from whichever source, and reads the
// secret files it names through resolve.
func composeTeam(file serverConfig, resolve func(string) string) (*team, error) {
	var err error
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
			return nil, fmt.Errorf("authentication.trusted_headers needs proxies, issuer and user (%s, %s, %s)",
				envName("authentication.trusted_headers.proxies"), envName("authentication.trusted_headers.issuer"), envName("authentication.trusted_headers.user"))
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
			return nil, err
		}
		t.oidc.Audiences, t.oidc.Scopes, t.oidc.GroupsClaim, t.oidc.NameClaim = o.Audiences, o.Scopes, o.GroupsClaim, o.NameClaim
	}
	if a := file.Authentication.Admin; a != nil && a.PasswordFile != "" {
		if t.adminPassword, err = teamAdminPassword(resolve(a.PasswordFile)); err != nil {
			return nil, fmt.Errorf("authentication.admin.password_file: %w", err)
		}
	}
	if t.headers == nil && t.oidc == nil && t.adminPassword == "" {
		return nil, fmt.Errorf("authentication needs a sign-in method, the built-in admin account (%s), oidc (%s) or trusted_headers (%s); API tokens are created by signed-in users",
			envName("authentication.admin.password_file"), envName("authentication.oidc.issuer"), envName("authentication.trusted_headers.proxies"))
	}

	bindings := make([]platform.Binding, 0, len(file.Bindings))
	for i, b := range file.Bindings {
		scope, err := platform.ParseScope(b.Scope)
		if err != nil {
			return nil, fmt.Errorf("bindings[%d]: %w", i, err)
		}
		bindings = append(bindings, platform.Binding{Subject: platform.Subject{Group: b.Group, Principal: b.Principal}, Role: b.Role, Scope: scope})
	}
	// The admin account's own binding is the platform's.
	if len(bindings) == 0 && t.adminPassword == "" {
		return nil, fmt.Errorf("bindings are required (%s): without one, nobody may do anything", envName("bindings"))
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

// teamAdminPassword reads the built-in admin account's password: the file's
// first line, without its line ending, as the postgres image reads
// POSTGRES_PASSWORD_FILE.
func teamAdminPassword(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if strings.ContainsAny(password, "\r\n") || len(password) < platform.MinAdminPassword {
		return "", fmt.Errorf("the password is one line of at least %d characters", platform.MinAdminPassword)
	}
	return password, nil
}

// teamOIDC checks the OIDC settings; the provider itself is read at
// startup. The client secret is read from its file, never the server config.
func teamOIDC(issuer, clientID, secretFile, redirect, cliClientID, sessionTTL string) (*platform.OIDCOptions, error) {
	if issuer == "" || clientID == "" || redirect == "" {
		return nil, fmt.Errorf("authentication.oidc: issuer, client_id and redirect_url are required (%s, %s, %s)",
			envName("authentication.oidc.issuer"), envName("authentication.oidc.client_id"), envName("authentication.oidc.redirect_url"))
	}
	for name, v := range map[string]string{"issuer": issuer, "redirect_url": redirect} {
		u, err := url.Parse(v)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && loopbackHost(u.Hostname()))) {
			return nil, fmt.Errorf("authentication.oidc.%s %q: give an https URL, or http on loopback", name, v)
		}
	}
	if u, _ := url.Parse(redirect); u.Path != "/auth/callback" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("authentication.oidc.redirect_url %q: give this server's URL ending in /auth/callback", redirect)
	}
	o := &platform.OIDCOptions{Issuer: issuer, ClientID: clientID, RedirectURL: redirect, CLIClientID: cliClientID}
	if secretFile != "" {
		raw, err := os.ReadFile(secretFile)
		if err != nil {
			return nil, fmt.Errorf("authentication.oidc.client_secret_file: %w", err)
		}
		if o.ClientSecret = strings.TrimSpace(string(raw)); o.ClientSecret == "" {
			return nil, fmt.Errorf("authentication.oidc.client_secret_file %s is empty", secretFile)
		}
	}
	if sessionTTL != "" {
		d, err := time.ParseDuration(sessionTTL)
		if err != nil || d <= 0 || d > platform.MaxSessionTTL {
			return nil, fmt.Errorf("authentication.oidc.session_ttl %q: give a duration up to %s, such as 12h", sessionTTL, platform.MaxSessionTTL)
		}
		o.SessionTTL = d
	}
	return o, nil
}

// teamStore reads store's connection settings. The database URL's password
// and the object storage keys are secrets, so the server config names files
// holding them; each is read once, at startup.
func teamStore(file serverConfig, resolve func(string) string) (pg.Config, error) {
	st, o := file.Store, file.Store.Objects
	var missing []string
	if st.DatabaseURLFile == "" && st.DatabaseURL == "" {
		missing = append(missing, "database_url_file ("+envName("store.database_url_file")+"), or database_url with database_password_file ("+
			envName("store.database_url")+", "+envName("store.database_password_file")+")")
	}
	for _, k := range []struct{ key, v string }{{"bucket", o.Bucket}, {"access_key_file", o.AccessKeyFile}, {"secret_key_file", o.SecretKeyFile}} {
		if k.v == "" {
			missing = append(missing, "objects."+k.key+" ("+envName("store.objects."+k.key)+")")
		}
	}
	if len(missing) > 0 {
		return pg.Config{}, fmt.Errorf("store needs %s: a team server keeps its records in PostgreSQL and its payloads in object storage", strings.Join(missing, "; "))
	}
	switch {
	case st.DatabaseURLFile != "" && st.DatabaseURL != "":
		return pg.Config{}, errors.New("store.database_url_file and store.database_url are alternatives: give one")
	case st.DatabasePasswordFile != "" && st.DatabaseURL == "":
		return pg.Config{}, errors.New("store.database_password_file completes store.database_url; a store.database_url_file holds the password itself")
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
	if st.DatabaseURL != "" {
		c.DatabaseURL, err = databaseURL(st.DatabaseURL, st.DatabasePasswordFile, resolve)
	} else {
		c.DatabaseURL, err = secret("database_url_file", st.DatabaseURLFile)
	}
	if err != nil {
		return pg.Config{}, err
	}
	if c.S3.AccessKey, err = secret("objects.access_key_file", o.AccessKeyFile); err != nil {
		return pg.Config{}, err
	}
	if c.S3.SecretKey, err = secret("objects.secret_key_file", o.SecretKeyFile); err != nil {
		return pg.Config{}, err
	}
	c.S3.Bucket, c.S3.Prefix, c.S3.Region, c.S3.UsePathStyle, c.S3.CreateBucket = o.Bucket, o.Prefix, o.Region, o.PathStyle, o.CreateBucket
	if o.Endpoint != "" {
		u, err := url.Parse(o.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return pg.Config{}, fmt.Errorf("store.objects.endpoint: %q is not an http or https URL", o.Endpoint)
		}
		c.S3.Endpoint, c.S3.UseTLS = o.Endpoint, u.Scheme == "https"
	}
	return c, nil
}

// databaseURL is store.database_url with the password in passwordFile, if
// any, set as the URL's password. The URL names no secret, so it is plain
// configuration and must not carry one; messages never repeat it, for a
// mistaken password in it would otherwise reach the logs. The password's
// trailing line breaks are dropped, as the postgres image does for
// POSTGRES_PASSWORD_FILE, and net/url escapes the rest.
func databaseURL(raw, passwordFile string, resolve func(string) string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("store.database_url: give a postgres:// URL without a password, such as postgres://linea@postgres:5432/linea")
	}
	if _, has := u.User.Password(); has || u.Query().Has("password") {
		return "", errors.New("store.database_url must not carry a password: give it in store.database_password_file")
	}
	if passwordFile == "" {
		return raw, nil
	}
	if u.User.Username() == "" {
		return "", errors.New("store.database_url needs a user name when store.database_password_file gives the password")
	}
	pw, err := os.ReadFile(resolve(passwordFile))
	if err != nil {
		return "", fmt.Errorf("store.database_password_file: %w", err)
	}
	password := strings.TrimRight(string(pw), "\r\n")
	if password == "" {
		return "", fmt.Errorf("store.database_password_file: %s is empty", passwordFile)
	}
	u.User = url.UserPassword(u.User.Username(), password)
	return u.String(), nil
}
