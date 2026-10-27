package dsm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fake2FA is DSM with 2FA on: a login needs the password and either the code
// 123456 or the device token that code minted. Calls need the latest session.
type fake2FA struct {
	logins   atomic.Int32
	mu       sync.Mutex
	session  string // the one valid sid; "" after expire()
	lastForm map[string]string
}

func (f *fake2FA) expire() {
	f.mu.Lock()
	f.session = ""
	f.mu.Unlock()
}

func (f *fake2FA) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	reply := func(body any) { _ = json.NewEncoder(w).Encode(body) }
	fail := func(code int) { reply(map[string]any{"success": false, "error": map[string]int{"code": code}}) }
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Form.Get("method") != "login" {
		if r.Form.Get("_sid") == "" || r.Form.Get("_sid") != f.session {
			fail(106)
			return
		}
		reply(map[string]any{"success": true, "data": map[string]any{"containers": []any{}}})
		return
	}
	otp, device := r.Form.Get("otp_code"), r.Form.Get("device_id")
	f.lastForm = map[string]string{"otp_code": otp, "device_id": device}
	switch {
	case r.Form.Get("passwd") != "hunter2":
		fail(400)
		return
	case device == "dev-token":
	case otp == "":
		fail(403)
		return
	case otp != "123456":
		fail(404)
		return
	}
	f.session = fmt.Sprintf("sid%d", f.logins.Add(1))
	data := map[string]any{"sid": f.session, "synotoken": "t"}
	if otp != "" {
		data["did"] = "dev-token"
	}
	reply(map[string]any{"success": true, "data": data})
}

func newFake2FA(t *testing.T, config Config) (*fake2FA, *Client) {
	t.Helper()
	fake := &fake2FA{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	old := ConfigDir
	t.Cleanup(func() { ConfigDir = old })
	ConfigDir = t.TempDir()
	t.Setenv("SYNODOCK_PASSWORD", "")
	config.URL, config.User = srv.URL, "docker"
	c := NewClient(context.Background(), &config)
	c.secrets.hasKeychain = false // the device token goes to a file under ConfigDir
	return fake, c
}

func TestPasswordAndOTPCommands(t *testing.T) {
	fake, c := newFake2FA(t, Config{PasswordCommand: "printf 'hunter2\\nignored\\n'", OTPCommand: "echo 123456"})
	if err := c.Login(""); err != nil {
		t.Fatal(err)
	}
	if fake.lastForm["otp_code"] != "123456" {
		t.Fatalf("login sent %v", fake.lastForm)
	}
	token, _ := os.ReadFile(filepath.Join(ConfigDir, "device-docker@127.0.0.1"))
	if string(token) != "dev-token" {
		t.Fatalf("device token file: %q", token)
	}
	// The next login uses the device token, not a code.
	c.Config.OTPCommand = "exit 1"
	if err := c.Login(""); err != nil {
		t.Fatal(err)
	}
	if fake.lastForm["device_id"] != "dev-token" || fake.lastForm["otp_code"] != "" {
		t.Fatalf("second login sent %v", fake.lastForm)
	}
}

func TestOTPRequiredWithoutSource(t *testing.T) {
	_, c := newFake2FA(t, Config{PasswordCommand: "echo hunter2"})
	if !strings.Contains(c.Login("").Error(), "otp_command") { // go test's stdin isn't a terminal
		t.Fatal("no hint about otp_command")
	}
	_, c = newFake2FA(t, Config{PasswordCommand: "echo hunter2", OTPCommand: "echo 000000"})
	if err := c.Login(""); err == nil || !strings.Contains(err.Error(), "code 404") {
		t.Fatalf("wrong code: %v", err)
	}
	_, c = newFake2FA(t, Config{PasswordCommand: "exit 3"})
	if err := c.Login(""); err == nil || !strings.Contains(err.Error(), "password_command failed") {
		t.Fatalf("failing password_command: %v", err)
	}
}

// An expired session seen by several calls at once is logged in again once.
func TestReloginOnce(t *testing.T) {
	fake, c := newFake2FA(t, Config{PasswordCommand: "echo hunter2", OTPCommand: "echo 123456"})
	if err := c.Login(""); err != nil {
		t.Fatal(err)
	}
	fake.expire()
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Containers(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := fake.logins.Load(); n != 2 {
		t.Fatalf("%d logins, want 2", n)
	}
}

// serve started on demand doesn't log in up front: the first call does.
func TestFirstCallLogsIn(t *testing.T) {
	fake, c := newFake2FA(t, Config{PasswordCommand: "echo hunter2", OTPCommand: "echo 123456"})
	if _, err := c.Containers(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Containers(); err != nil {
		t.Fatal(err)
	}
	if n := fake.logins.Load(); n != 1 {
		t.Fatalf("%d logins, want 1", n)
	}
}
