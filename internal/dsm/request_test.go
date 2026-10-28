package dsm

import (
	"context"
	"net"
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
