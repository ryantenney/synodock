package dsm

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransportErrorHidesSessionID(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens: the request fails in transport, with its URL in the error
	c := NewClient(context.Background(), &Config{URL: "http://" + addr, User: "u"})
	c.sid = "SECRETSID123"
	err = c.FSPutBytes([]byte("x"), "f", "/docker", false, true) // an upload: params in the URL
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "SECRETSID123") {
		t.Errorf("session id in error: %v", err)
	}
}

func TestUploadSendsSessionAsCookie(t *testing.T) {
	var gotURL, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		if ck, err := r.Cookie("id"); err == nil {
			gotCookie = ck.Value
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	c := NewClient(context.Background(), &Config{URL: srv.URL, User: "u"})
	c.sid = "SECRETSID123"
	if err := c.FSPutBytes([]byte("x"), "f", "/docker", false, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotURL, "SECRETSID123") || gotCookie != "SECRETSID123" {
		t.Errorf("url %q, cookie %q: the session belongs in the cookie, not the URL", gotURL, gotCookie)
	}
}
