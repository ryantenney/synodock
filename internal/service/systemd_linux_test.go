package service

import (
	"strings"
	"testing"
)

func TestUnits(t *testing.T) {
	socket, svc := units(Spec{Name: "home", Exe: "/opt/syno dock", Args: []string{"--nas", "home", "serve"},
		Socket: "/home/x/.cache/synodock/home.sock", Env: map[string]string{"PATH": "/usr/bin:$HOME/100%"}})
	for _, want := range []string{"ListenStream=/home/x/.cache/synodock/home.sock", "SocketMode=0600", "WantedBy=sockets.target"} {
		if !strings.Contains(socket, want) {
			t.Errorf("socket unit lacks %s:\n%s", want, socket)
		}
	}
	for _, want := range []string{`ExecStart="/opt/syno dock" "--nas" "home" "serve"`, "Requires=synodock-home.socket",
		`Environment="PATH=/usr/bin:$$HOME/100%%"`} {
		if !strings.Contains(svc, want) {
			t.Errorf("service unit lacks %s:\n%s", want, svc)
		}
	}
}
