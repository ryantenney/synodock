package main

import (
	"testing"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
)

func TestIdleExitChoice(t *testing.T) {
	ten := 10 * time.Minute
	for _, tc := range []struct {
		name     string
		o        options
		config   *time.Duration
		onDemand bool
		want     time.Duration
	}{
		{"flag wins", options{idle: time.Minute, idleSet: true}, &ten, true, time.Minute},
		{"flag 0", options{idleSet: true}, &ten, true, 0},
		{"config", options{}, &ten, true, ten},
		{"on demand default", options{}, nil, true, defaultIdleExit},
		{"by hand: never", options{}, nil, false, 0},
	} {
		if got := idleExit(tc.o, &dsm.Config{IdleExit: tc.config}, tc.onDemand); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}
