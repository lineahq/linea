package server

import (
	"bytes"
	"context"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// envOnly is a complete server config as environment variables: the secret
// files of writeTeamSecrets in a directory, and trusted-header sign-in.
func envOnly(t *testing.T) (map[string]string, string) {
	t.Helper()
	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	return map[string]string{
		"LINEA_STORE_DATABASE_URL_FILE":                filepath.Join(dir, "db-url"),
		"LINEA_STORE_OBJECTS_ENDPOINT":                 "http://127.0.0.1:8333",
		"LINEA_STORE_OBJECTS_BUCKET":                   "linea",
		"LINEA_STORE_OBJECTS_PATH_STYLE":               "true",
		"LINEA_STORE_OBJECTS_ACCESS_KEY_FILE":          filepath.Join(dir, "s3-access"),
		"LINEA_STORE_OBJECTS_SECRET_KEY_FILE":          filepath.Join(dir, "s3-secret"),
		"LINEA_AUTHENTICATION_TRUSTED_HEADERS_PROXIES": "[127.0.0.1]",
		"LINEA_AUTHENTICATION_TRUSTED_HEADERS_ISSUER":  "proxy",
		"LINEA_AUTHENTICATION_TRUSTED_HEADERS_USER":    "X-Forwarded-User",
		"LINEA_BINDINGS":                               `[{group: platform, role: viewer, scope: "*"}]`,
	}, dir
}

// TestEnvironmentVariablesFollowTheConfigKeys: one rule names a key's
// variable, every key has one, they are unique, and the file format's version
// has none because LINEA_VERSION names the release.
func TestEnvironmentVariablesFollowTheConfigKeys(t *testing.T) {
	for key, want := range map[string]string{
		"store.storage_limit":         "LINEA_STORE_STORAGE_LIMIT",
		"store.objects.bucket":        "LINEA_STORE_OBJECTS_BUCKET",
		"store.objects.create_bucket": "LINEA_STORE_OBJECTS_CREATE_BUCKET",
		"authentication.oidc.issuer":  "LINEA_AUTHENTICATION_OIDC_ISSUER",
		"store.database_url_file":     "LINEA_STORE_DATABASE_URL_FILE",
		"bindings":                    "LINEA_BINDINGS",
		"retention.age":               "LINEA_RETENTION_AGE",
	} {
		if envName(key) != want {
			t.Errorf("envName(%s) = %s, want %s", key, envName(key), want)
		}
		if !slices.Contains(configKeys(), key) {
			t.Errorf("%s is not a config key", key)
		}
	}
	seen := map[string]string{}
	for _, key := range configKeys() {
		name := envName(key)
		if other, dup := seen[name]; dup {
			t.Errorf("%s and %s share %s", key, other, name)
		}
		seen[name] = key
		if name == "LINEA_VERSION" {
			t.Errorf("%s would read LINEA_VERSION, the release", key)
		}
	}
}

func TestEnvironmentSetsValuesLikeTheFileDoes(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		vars  map[string]string
		check func(t *testing.T, c serverConfig)
		set   []string
	}{
		{"string", map[string]string{"LINEA_STORE_OBJECTS_BUCKET": " linea "}, func(t *testing.T, c serverConfig) {
			if c.Store.Objects.Bucket != "linea" {
				t.Errorf("bucket %q", c.Store.Objects.Bucket)
			}
		}, []string{"store.objects.bucket"}},
		{"a string is kept as written", map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea@db:5432/linea?sslmode=disable"}, func(t *testing.T, c serverConfig) {
			if c.Store.DatabaseURL != "postgres://linea@db:5432/linea?sslmode=disable" {
				t.Errorf("url %q", c.Store.DatabaseURL)
			}
		}, []string{"store.database_url"}},
		{"duration and size stay strings for the validation", map[string]string{"LINEA_RETENTION_AGE": "36h", "LINEA_STORE_STORAGE_LIMIT": "64MiB", "LINEA_LIMITS_GIT_CLONE_SIZE": "32MiB"}, func(t *testing.T, c serverConfig) {
			if c.Retention.Age != "36h" || c.Store.StorageLimit != "64MiB" || c.Limits.GitCloneSize != "32MiB" {
				t.Errorf("%+v", c)
			}
		}, []string{"limits.git_clone_size", "retention.age", "store.storage_limit"}},
		{"bool", map[string]string{"LINEA_STORE_OBJECTS_PATH_STYLE": "true", "LINEA_STORE_OBJECTS_CREATE_BUCKET": "TRUE"}, func(t *testing.T, c serverConfig) {
			if !c.Store.Objects.PathStyle || !c.Store.Objects.CreateBucket {
				t.Errorf("%+v", c.Store.Objects)
			}
		}, []string{"store.objects.create_bucket", "store.objects.path_style"}},
		{"optional number", map[string]string{"LINEA_LIMITS_QUEUE": "8", "LINEA_RETENTION_RUNS": "500"}, func(t *testing.T, c serverConfig) {
			if c.Limits.Queue == nil || *c.Limits.Queue != 8 || c.Retention.Runs == nil || *c.Retention.Runs != 500 || c.Limits.Connections != nil {
				t.Errorf("%+v", c)
			}
		}, []string{"limits.queue", "retention.runs"}},
		{"flow list of strings", map[string]string{"LINEA_AUTHENTICATION_TRUSTED_HEADERS_PROXIES": "[10.0.0.0/8, 127.0.0.1]"}, func(t *testing.T, c serverConfig) {
			if h := c.Authentication.TrustedHeaders; h == nil || !slices.Equal(h.Proxies, []string{"10.0.0.0/8", "127.0.0.1"}) {
				t.Errorf("%+v", h)
			}
		}, []string{"authentication.trusted_headers.proxies"}},
		{"flow list of bindings", map[string]string{"LINEA_BINDINGS": `[{group: admins, role: admin, scope: "*"}, {principal: "service:ci", role: engineer, scope: "project:p"}]`}, func(t *testing.T, c serverConfig) {
			if len(c.Bindings) != 2 || c.Bindings[0].Group != "admins" || c.Bindings[0].Scope != "*" || c.Bindings[1].Principal != "service:ci" || c.Bindings[1].Scope != "project:p" {
				t.Errorf("%+v", c.Bindings)
			}
		}, []string{"bindings"}},
		{"a section is allocated by its first key", map[string]string{"LINEA_AUTHENTICATION_OIDC_ISSUER": "https://idp.example.com", "LINEA_AUTHENTICATION_OIDC_SCOPES": "[openid, groups]"}, func(t *testing.T, c serverConfig) {
			if o := c.Authentication.OIDC; o == nil || o.Issuer != "https://idp.example.com" || !slices.Equal(o.Scopes, []string{"openid", "groups"}) || c.Authentication.TrustedHeaders != nil {
				t.Errorf("%+v", o)
			}
		}, []string{"authentication.oidc.issuer", "authentication.oidc.scopes"}},
		{"empty and blank values are unset and open no section", map[string]string{
			"LINEA_AUTHENTICATION_OIDC_ISSUER": "", "LINEA_AUTHENTICATION_TRUSTED_HEADERS_USER": "  \t", "LINEA_LIMITS_QUEUE": "", "LINEA_BINDINGS": " ",
			"LINEA_STORE_OBJECTS_PATH_STYLE": "", "LINEA_STORE_OBJECTS_BUCKET": "",
		}, func(t *testing.T, c serverConfig) {
			if c.Authentication.OIDC != nil || c.Authentication.TrustedHeaders != nil || c.Limits.Queue != nil || c.Bindings != nil || c.Store.Objects.Bucket != "" {
				t.Errorf("%+v", c)
			}
		}, nil},
		{"absolute file paths stay", map[string]string{"LINEA_STORE_DATABASE_URL_FILE": filepath.Join(dir, "abs")}, func(t *testing.T, c serverConfig) {
			if c.Store.DatabaseURLFile != filepath.Join(dir, "abs") {
				t.Errorf("%q", c.Store.DatabaseURLFile)
			}
		}, []string{"store.database_url_file"}},
		{"relative file paths resolve against the directory", map[string]string{"LINEA_STORE_DATABASE_URL_FILE": "secrets/db"}, func(t *testing.T, c serverConfig) {
			if c.Store.DatabaseURLFile != filepath.Join(dir, "secrets", "db") {
				t.Errorf("%q", c.Store.DatabaseURLFile)
			}
		}, []string{"store.database_url_file"}},
		{"variables that are not keys are ignored", map[string]string{"LINEA_VERSION": "0.2.0", "LINEA_BIND": "0.0.0.0", "LINEA_SERVER_MAIN": "1"}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c serverConfig
			set, err := environment{lookup: mapEnv(tc.vars), dir: dir}.apply(&c)
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, o := range set {
				keys = append(keys, o.key)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, tc.set) {
				t.Errorf("set %v, want %v", keys, tc.set)
			}
			if c.Version != 0 {
				t.Errorf("version %d", c.Version)
			}
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}

// TestEnvironmentErrorsNameTheVariable: a value that does not parse, and a
// value that parses but fails the server's checks, are named by their
// variable, never only by the YAML key.
func TestEnvironmentErrorsNameTheVariable(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value, want string
	}{
		{"bool", "LINEA_STORE_OBJECTS_PATH_STYLE", "sometimes", "LINEA_STORE_OBJECTS_PATH_STYLE: give true or false"},
		{"number", "LINEA_LIMITS_QUEUE", "many", "LINEA_LIMITS_QUEUE: give a whole number"},
		{"list not flow", "LINEA_AUTHENTICATION_TRUSTED_HEADERS_PROXIES", "10.0.0.0/8", "LINEA_AUTHENTICATION_TRUSTED_HEADERS_PROXIES: give a YAML flow value"},
		{"unknown binding key", "LINEA_BINDINGS", "[{grup: a, role: admin, scope: '*'}]", "LINEA_BINDINGS: give a YAML flow value"},
		{"bad size", "LINEA_STORE_STORAGE_LIMIT", "lots", "LINEA_STORE_STORAGE_LIMIT: invalid size"},
		{"bad endpoint", "LINEA_STORE_OBJECTS_ENDPOINT", "s3.example.com", "LINEA_STORE_OBJECTS_ENDPOINT: "},
		{"bad retention", "LINEA_RETENTION_AGE", "1w", "LINEA_RETENTION_AGE "},
		{"limit range", "LINEA_LIMITS_QUEUE", "4096", "LINEA_LIMITS_QUEUE 4096"},
		{"runs over queue", "LINEA_LIMITS_QUEUE", "8", "LINEA_LIMITS_RUNS_PER_PRINCIPAL 32 exceeds LINEA_LIMITS_QUEUE 8"},
		{"binding scope", "LINEA_BINDINGS", `[{group: a, role: admin, scope: team}]`, "LINEA_BINDINGS[0]"},
		{"missing secret file", "LINEA_STORE_OBJECTS_SECRET_KEY_FILE", "missing-file", "LINEA_STORE_OBJECTS_SECRET_KEY_FILE: "},
		{"oidc incomplete", "LINEA_AUTHENTICATION_OIDC_ISSUER", "https://idp.example.com", "authentication.oidc: issuer, client_id and redirect_url are required (LINEA_AUTHENTICATION_OIDC_ISSUER"},
		{"oidc issuer", "LINEA_AUTHENTICATION_OIDC_ISSUER", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars, dir := envOnly(t)
			if tc.name == "runs over queue" {
				vars["LINEA_LIMITS_RUNS_PER_PRINCIPAL"] = "32"
			}
			vars[tc.variable] = tc.value
			_, err := loadConfig("", environment{lookup: mapEnv(vars), dir: dir})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("an empty value is unset: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.HasPrefix(err.Error(), "server config (environment): ") {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestEnvironmentConfiguresTheServerWithoutAFile: nothing but variables
// composes the same server a file does, and a missing key is reported with
// the variables that set it.
func TestEnvironmentConfiguresTheServerWithoutAFile(t *testing.T) {
	vars, dir := envOnly(t)
	vars["LINEA_STORE_STORAGE_LIMIT"] = "64MiB"
	vars["LINEA_STORE_OBJECTS_CREATE_BUCKET"] = "true"
	vars["LINEA_LIMITS_QUEUE"] = "8"
	vars["LINEA_LIMITS_RUNS_PER_PRINCIPAL"] = "4"
	vars["LINEA_RETENTION_AGE"] = "7d"
	tm, err := loadConfig("", environment{lookup: mapEnv(vars), dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if tm.store.DatabaseURL != "postgres://linea@127.0.0.1/linea" || tm.store.S3.Bucket != "linea" || tm.store.S3.SecretKey != "secret" || !tm.store.S3.UsePathStyle || !tm.store.S3.CreateBucket ||
		tm.limits.storage != 64<<20 || tm.queue != 8 || tm.principals.Runs != 4 || tm.retention.Age != 7*24*time.Hour || tm.headers.Proxies[0].String() != "127.0.0.1/32" {
		t.Fatalf("team = %+v", tm)
	}

	if _, err := loadConfig("", environment{lookup: noEnv}); err != errNoConfig {
		t.Fatalf("no file and no variable: %v", err)
	}
	if _, err := loadConfig("", environment{lookup: mapEnv(map[string]string{"LINEA_VERSION": "dev", "LINEA_AUTHENTICATION_OIDC_ISSUER": " "})}); err != errNoConfig {
		t.Fatalf("only variables that set no key: %v", err)
	}
	_, err = loadConfig("", environment{lookup: mapEnv(map[string]string{"LINEA_RETENTION_AGE": "7d"})})
	if err == nil {
		t.Fatal("a server without a store started")
	}
	for _, want := range []string{"server config (environment): store needs database_url_file (LINEA_STORE_DATABASE_URL_FILE)", "LINEA_STORE_DATABASE_URL, LINEA_STORE_DATABASE_PASSWORD_FILE",
		"objects.bucket (LINEA_STORE_OBJECTS_BUCKET)", "objects.access_key_file (LINEA_STORE_OBJECTS_ACCESS_KEY_FILE)", "objects.secret_key_file (LINEA_STORE_OBJECTS_SECRET_KEY_FILE)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	for name, drop := range map[string][]string{
		"sign-in":  {"LINEA_AUTHENTICATION_TRUSTED_HEADERS_PROXIES", "LINEA_AUTHENTICATION_TRUSTED_HEADERS_ISSUER", "LINEA_AUTHENTICATION_TRUSTED_HEADERS_USER"},
		"bindings": {"LINEA_BINDINGS"},
	} {
		v := maps.Clone(vars)
		for _, d := range drop {
			delete(v, d)
		}
		if _, err := loadConfig("", environment{lookup: mapEnv(v), dir: dir}); err == nil || !strings.Contains(err.Error(), "LINEA_") {
			t.Errorf("without %s: %v", name, err)
		}
	}
}

// TestEnvironmentOverridesTheFile: a variable wins over the file's value, an
// empty one does not, a section's keys combine, a list replaces the file's
// list, and the file's relative paths still resolve against the file while a
// variable's resolve against the directory given.
func TestEnvironmentOverridesTheFile(t *testing.T) {
	path := writeTeam(t, teamYAML()+"retention: {runs: 500, age: 7d}\nlimits: {queue: 64}\n")
	for _, tc := range []struct {
		name  string
		vars  map[string]string
		check func(t *testing.T, tm *team)
	}{
		{"nothing set", nil, func(t *testing.T, tm *team) {
			if tm.limits.storage != 64<<20 || tm.retention.Runs != 500 || tm.retention.Age != 7*24*time.Hour || tm.store.S3.Bucket != "linea" || tm.store.S3.SecretKey != "secret" {
				t.Errorf("%+v", tm)
			}
		}},
		{"scalar overrides", map[string]string{"LINEA_STORE_STORAGE_LIMIT": "128MiB", "LINEA_STORE_OBJECTS_BUCKET": "other", "LINEA_LIMITS_QUEUE": "100"}, func(t *testing.T, tm *team) {
			if tm.limits.storage != 128<<20 || tm.store.S3.Bucket != "other" || tm.queue != 100 || tm.retention.Runs != 500 || tm.store.S3.SecretKey != "secret" {
				t.Errorf("%+v", tm)
			}
		}},
		{"empty values override nothing", map[string]string{"LINEA_STORE_STORAGE_LIMIT": "", "LINEA_STORE_OBJECTS_BUCKET": "  ", "LINEA_RETENTION_AGE": "", "LINEA_LIMITS_QUEUE": ""}, func(t *testing.T, tm *team) {
			if tm.limits.storage != 64<<20 || tm.store.S3.Bucket != "linea" || tm.retention.Age != 7*24*time.Hour || tm.queue != 64 {
				t.Errorf("%+v", tm)
			}
		}},
		{"an empty value does not turn OIDC on", map[string]string{"LINEA_AUTHENTICATION_OIDC_ISSUER": "", "LINEA_AUTHENTICATION_OIDC_CLIENT_ID": "", "LINEA_AUTHENTICATION_OIDC_REDIRECT_URL": ""}, func(t *testing.T, tm *team) {
			if tm.oidc != nil || tm.headers == nil {
				t.Errorf("oidc %+v, headers %+v", tm.oidc, tm.headers)
			}
		}},
		{"a list replaces the file's", map[string]string{"LINEA_BINDINGS": `[{group: admins, role: admin, scope: "*"}]`}, func(t *testing.T, tm *team) {
			if b := tm.access.Bindings(); len(b) != 1 || b[0].Subject.Group != "admins" {
				t.Errorf("bindings %+v", b)
			}
		}},
		{"a key of a section the file has", map[string]string{"LINEA_AUTHENTICATION_TRUSTED_HEADERS_ISSUER": "edge"}, func(t *testing.T, tm *team) {
			if tm.headers == nil || tm.headers.Issuer != "edge" || tm.headers.User != "X-Forwarded-User" {
				t.Errorf("%+v", tm.headers)
			}
		}},
		{"a file key given relative to the working directory", map[string]string{"LINEA_STORE_OBJECTS_SECRET_KEY_FILE": "from-cwd"}, func(t *testing.T, tm *team) {
			if tm.store.S3.SecretKey != "cwd-secret" {
				t.Errorf("secret %q", tm.store.S3.SecretKey)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(cwd, "from-cwd"), []byte("cwd-secret\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			tm, err := loadConfig(path, environment{lookup: mapEnv(tc.vars), dir: cwd})
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, tm)
		})
	}

	// An error about an overriding variable names the variable, and the file.
	_, err := loadConfig(path, environment{lookup: mapEnv(map[string]string{"LINEA_RETENTION_AGE": "1w"})})
	if err == nil || !strings.Contains(err.Error(), "server config "+path+": LINEA_RETENTION_AGE ") {
		t.Fatalf("error = %v", err)
	}
	_, err = loadConfig(path, environment{lookup: mapEnv(map[string]string{"LINEA_LIMITS_CONNECTIONS": "4"})})
	if err == nil || !strings.Contains(err.Error(), "LINEA_LIMITS_CONNECTIONS 4: give a number from 16") {
		t.Fatalf("error = %v", err)
	}
	// An error about the file's own value still names its key.
	bad := writeTeam(t, strings.Replace(teamYAML(), "64MiB", "lots", 1))
	if _, err := loadConfig(bad, environment{lookup: mapEnv(map[string]string{"LINEA_RETENTION_AGE": "7d"})}); err == nil || !strings.Contains(err.Error(), "store.storage_limit") || strings.Contains(err.Error(), "LINEA_") {
		t.Fatalf("a file's value is named by its key: %v", err)
	}
	// The environment does not stand in for the file's version.
	if _, err := loadConfig(writeTeam(t, strings.Replace(teamYAML(), "version: 1", "version: 2", 1)), environment{lookup: mapEnv(map[string]string{"LINEA_VERSION": "1"})}); err == nil {
		t.Fatal("version 2 accepted")
	}
}

// TestStoreDatabaseURLTakesItsPasswordFromAFile: store.database_url has no
// password and store.database_password_file supplies it, escaped, and
// exactly one way of naming the database is accepted.
func TestStoreDatabaseURLTakesItsPasswordFromAFile(t *testing.T) {
	vars, dir := envOnly(t)
	delete(vars, "LINEA_STORE_DATABASE_URL_FILE")
	password := "p@ss:w/rd#?%&= \u00e9"
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	special := write("pw-special", password+"\n\n")
	empty := write("pw-empty", "\n")
	plain := write("pw-plain", "hunter2hunter2")
	urlFile := filepath.Join(dir, "db-url")
	for _, tc := range []struct {
		name     string
		set      map[string]string
		wantPass string
		wantURL  string // exact, when there is no password
		wantErr  string
	}{
		{name: "password inserted and escaped", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea@postgres:5432/linea?sslmode=disable", "LINEA_STORE_DATABASE_PASSWORD_FILE": special}, wantPass: password},
		{name: "plain password", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgresql://linea@postgres/linea", "LINEA_STORE_DATABASE_PASSWORD_FILE": plain}, wantPass: "hunter2hunter2"},
		{name: "no password file leaves the URL alone", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea@postgres/linea"}, wantURL: "postgres://linea@postgres/linea"},
		{name: "the URL file as before", set: map[string]string{"LINEA_STORE_DATABASE_URL_FILE": urlFile}, wantURL: "postgres://linea@127.0.0.1/linea"},
		{name: "neither", set: map[string]string{}, wantErr: "store needs database_url_file (LINEA_STORE_DATABASE_URL_FILE), or database_url with database_password_file (LINEA_STORE_DATABASE_URL, LINEA_STORE_DATABASE_PASSWORD_FILE)"},
		{name: "both", set: map[string]string{"LINEA_STORE_DATABASE_URL_FILE": urlFile, "LINEA_STORE_DATABASE_URL": "postgres://linea@postgres/linea"},
			wantErr: "LINEA_STORE_DATABASE_URL_FILE and LINEA_STORE_DATABASE_URL are alternatives"},
		{name: "password file with the URL file", set: map[string]string{"LINEA_STORE_DATABASE_URL_FILE": urlFile, "LINEA_STORE_DATABASE_PASSWORD_FILE": plain},
			wantErr: "LINEA_STORE_DATABASE_PASSWORD_FILE completes store.database_url"},
		{name: "password already in the URL", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea:leakedpassword@postgres/linea", "LINEA_STORE_DATABASE_PASSWORD_FILE": plain},
			wantErr: "LINEA_STORE_DATABASE_URL must not carry a password"},
		{name: "password in the URL without a file", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea:leakedpassword@postgres/linea"},
			wantErr: "LINEA_STORE_DATABASE_URL must not carry a password"},
		{name: "password as a query parameter", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea@postgres/linea?password=leakedpassword"},
			wantErr: "LINEA_STORE_DATABASE_URL must not carry a password"},
		{name: "not a postgres URL", set: map[string]string{"LINEA_STORE_DATABASE_URL": "host=postgres user=linea"}, wantErr: "LINEA_STORE_DATABASE_URL: give a postgres:// URL"},
		{name: "no user for the password", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://postgres/linea", "LINEA_STORE_DATABASE_PASSWORD_FILE": plain},
			wantErr: "needs a user name"},
		{name: "empty password file", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea@postgres/linea", "LINEA_STORE_DATABASE_PASSWORD_FILE": empty},
			wantErr: "LINEA_STORE_DATABASE_PASSWORD_FILE: " + empty + " is empty"},
		{name: "missing password file", set: map[string]string{"LINEA_STORE_DATABASE_URL": "postgres://linea@postgres/linea", "LINEA_STORE_DATABASE_PASSWORD_FILE": filepath.Join(dir, "nope")},
			wantErr: "LINEA_STORE_DATABASE_PASSWORD_FILE: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := maps.Clone(vars)
			maps.Copy(v, tc.set)
			tm, err := loadConfig("", environment{lookup: mapEnv(v)})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				for _, secret := range []string{"leakedpassword", password, "hunter2hunter2"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("the error repeats a password: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantURL != "" {
				if tm.store.DatabaseURL != tc.wantURL {
					t.Fatalf("URL %q, want %q", tm.store.DatabaseURL, tc.wantURL)
				}
				return
			}
			u, err := url.Parse(tm.store.DatabaseURL)
			if err != nil {
				t.Fatal(err)
			}
			if pw, _ := u.User.Password(); u.User.Username() != "linea" || pw != tc.wantPass || u.Host == "" || !strings.HasPrefix(u.Path, "/linea") {
				t.Fatalf("user %q password %q host %q path %q", u.User.Username(), pw, u.Host, u.Path)
			}
			if strings.Contains(tm.store.DatabaseURL, "p@ss") {
				t.Fatalf("the password is not escaped in %q", tm.store.DatabaseURL)
			}
		})
	}
}

// TestServeStartsFromTheEnvironmentAlone: no --config, and nothing but
// variables, composes a server; its startup notices hold no secret, and the
// bucket notice says when the server creates it.
func TestServeStartsFromTheEnvironmentAlone(t *testing.T) {
	vars, dir := envOnly(t)
	secrets := map[string]string{"db-url": "postgres://linea@127.0.0.1/linea", "s3-access": "access", "s3-secret": "secret"}
	for _, extra := range []map[string]string{nil, {"LINEA_STORE_OBJECTS_CREATE_BUCKET": "true"}} {
		v := maps.Clone(vars)
		maps.Copy(v, extra)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		if code := run(ctx, []string{"--addr", "127.0.0.1:0", "--mcp"}, &stdout, &stderr, environment{lookup: mapEnv(v), dir: dir}); code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		startup := stderr.String()
		for _, want := range []string{"serving API on http://127.0.0.1:", "storage: PostgreSQL, payloads in object storage bucket linea"} {
			if !strings.Contains(startup, want) {
				t.Errorf("startup lacks %q:\n%s", want, startup)
			}
		}
		if created := strings.Contains(startup, "bucket linea (created at startup if missing)"); created != (extra != nil) {
			t.Errorf("create_bucket %v, startup:\n%s", extra, startup)
		}
		for name, secret := range secrets {
			if strings.Contains(startup, secret) || strings.Contains(stdout.String(), secret) {
				t.Errorf("the startup output holds the content of %s", name)
			}
		}
	}
}

// TestServeRefusalsFromTheEnvironmentHoldNoSecrets: a refused start names
// the variable to fix and never a secret, and a start with no configuration
// says what to set.
func TestServeRefusalsFromTheEnvironmentHoldNoSecrets(t *testing.T) {
	vars, dir := envOnly(t)
	vars["LINEA_STORE_STORAGE_LIMIT"] = "lots"
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"--addr", "127.0.0.1:0"}, &stdout, &stderr, environment{lookup: mapEnv(vars), dir: dir}); code != exitError {
		t.Fatalf("exit %d", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "LINEA_STORE_STORAGE_LIMIT") || strings.Contains(out, "postgres://") || strings.Contains(out, "secret\n") {
		t.Fatalf("stderr: %s", out)
	}
	stderr.Reset()
	if code := run(context.Background(), nil, &stdout, &stderr, environment{lookup: noEnv}); code != exitError || !strings.Contains(stderr.String(), noConfig) {
		t.Fatalf("no configuration: exit %d: %s", code, stderr.String())
	}
}
