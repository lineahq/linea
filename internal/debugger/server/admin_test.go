package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// adminYAML is a server config whose only sign-in method is the built-in
// admin account, with no bindings: the account's binding is the platform's.
const adminYAML = "version: 1\n" + teamStoreYAML + `authentication:
  admin:
    password_file: admin-password
`

const adminPassword = "correct horse battery staple"

func TestConfigReadsTheAdminPassword(t *testing.T) {
	for name, tc := range map[string]struct {
		file, password string
		env            map[string]string
		want, refusal  string
	}{
		"file":                  {file: adminYAML, password: adminPassword + "\n", want: adminPassword},
		"crlf":                  {file: adminYAML, password: adminPassword + "\r\n", want: adminPassword},
		"too short":             {file: adminYAML, password: "tiny-pass\n", refusal: "at least 16 characters"},
		"two lines":             {file: adminYAML, password: adminPassword + "\nsecond line\n", refusal: "one line"},
		"missing file":          {file: adminYAML, refusal: "authentication.admin.password_file"},
		"from the environment":  {file: "version: 1\n" + teamStoreYAML, password: adminPassword, env: map[string]string{"LINEA_AUTHENTICATION_ADMIN_PASSWORD_FILE": "admin-password"}, want: adminPassword},
		"empty variable is off": {file: "version: 1\n" + teamStoreYAML, password: adminPassword, env: map[string]string{"LINEA_AUTHENTICATION_ADMIN_PASSWORD_FILE": ""}, refusal: "LINEA_AUTHENTICATION_ADMIN_PASSWORD_FILE"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeTeamSecrets(t, dir)
			if tc.password != "" {
				if err := os.WriteFile(filepath.Join(dir, "admin-password"), []byte(tc.password), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			path := writeTeamIn(t, dir, "team", tc.file)
			tm, err := loadConfig(path, environment{lookup: mapEnv(tc.env), dir: dir})
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) {
					t.Fatalf("err = %v, want it to name %q", err, tc.refusal)
				}
				if first, _, _ := strings.Cut(tc.password, "\n"); first != "" && strings.Contains(err.Error(), first) {
					t.Fatalf("the refusal shows the password: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tm.adminPassword != tc.want {
				t.Fatalf("adminPassword = %q, want %q", tm.adminPassword, tc.want)
			}
		})
	}
}

func TestServeSignsInTheAdminWithItsPassword(t *testing.T) {
	dir := t.TempDir()
	writeTeamSecrets(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "admin-password"), []byte(adminPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, notices := startServe(t, writeTeamIn(t, dir, "team", adminYAML))
	if !strings.Contains(notices, "the built-in admin account") {
		t.Errorf("startup does not name the admin account: %q", notices)
	}
	if strings.Contains(notices, adminPassword) {
		t.Fatal("startup shows the admin password")
	}

	resp := get(t, server+"/auth/methods", "")
	var methods struct {
		Password bool `json:"password"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&methods); err != nil || !methods.Password {
		t.Fatalf("/auth/methods = %+v, %v", methods, err)
	}
	resp.Body.Close()
	if resp := get(t, server+"/auth/login?return_to=/runs", ""); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/sign-in?return_to=%2Fruns" {
		t.Errorf("/auth/login = %d to %q, want the sign-in page", resp.StatusCode, resp.Header.Get("Location"))
	}

	if resp := signInAdmin(t, server, "wrong password, long enough", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong password = %d, want 401", resp.StatusCode)
	}
	if resp := get(t, server+"/api/v1/session", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no session = %d, want 401", resp.StatusCode)
	}

	// The web client's sign-in: a session cookie the API accepts.
	resp = signInAdmin(t, server, adminPassword, "")
	if resp.StatusCode != http.StatusNoContent || len(resp.Cookies()) == 0 {
		t.Fatalf("sign-in = %d with %d cookies, want 204 and a session", resp.StatusCode, len(resp.Cookies()))
	}
	cookie := resp.Cookies()[0]
	session := get(t, server+"/api/v1/session", cookie.Name+"="+cookie.Value)
	body, _ := io.ReadAll(session.Body)
	if session.StatusCode != http.StatusOK || !strings.Contains(string(body), `"admin"`) {
		t.Fatalf("session = %d %s, want the admin", session.StatusCode, body)
	}

	// linea login's sign-in: an API token with the admin's permissions.
	resp = signInAdmin(t, server, adminPassword, "cli")
	var token struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil || token.Token == "" {
		t.Fatalf("CLI sign-in = %d, %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	req, _ := http.NewRequest(http.MethodGet, server+"/api/v1/role-bindings", nil)
	req.Header.Set("Authorization", "Bearer "+token.Token)
	bindings, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bindings.Body.Close()
	if bindings.StatusCode != http.StatusOK {
		t.Errorf("the admin's token reading role bindings = %d, want 200", bindings.StatusCode)
	}
}

// startServe runs Main on a free loopback port until the test ends and
// returns the server's URL and its startup notices.
func startServe(t *testing.T, path string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	notices := make(serveNotices, 16)
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, []string{"--config", path, "--addr", "127.0.0.1:0"}, io.Discard, notices)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != exitOK {
				t.Errorf("serve exit = %d", code)
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	var server, seen string
	for server == "" {
		select {
		case notice := <-notices:
			seen += notice
			if rest, ok := strings.CutPrefix(notice, "serving API on "); ok {
				server = strings.TrimSuffix(strings.Fields(rest)[0], "/api/v1")
			}
		case code := <-done:
			t.Fatalf("serve exited %d: %s", code, seen)
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not start")
		}
	}
	// The rest of the startup notices, until the server serves.
	deadline := time.After(5 * time.Second)
	for !strings.Contains(seen, "storage:") {
		select {
		case notice := <-notices:
			seen += notice
		case <-deadline:
			t.Fatalf("no storage notice: %s", seen)
		}
	}
	go func() {
		for range notices {
		}
	}()
	return server, seen
}

func get(t *testing.T, url, cookie string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func signInAdmin(t *testing.T, server, password, client string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"user": "admin", "password": password, "client": client})
	resp, err := http.Post(server+"/auth/password", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
