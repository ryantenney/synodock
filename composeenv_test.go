package main

import (
	"reflect"
	"sort"
	"testing"
)

func strp(s string) *string { return &s }

func show(m map[string]*string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		if v == nil {
			out[k] = "<unknown>"
		} else {
			out[k] = *v
		}
	}
	return out
}

func TestDotenv(t *testing.T) {
	got := show(dotenv(`# comment
A=plain
export B = spaced # a comment
C='single $kept #too'
D="double # kept"
E=has$var
F="quoted ${X}"
G=
not a line
`))
	want := map[string]string{"A": "plain", "B": "spaced", "C": "single $kept #too", "D": "double # kept",
		"E": "<unknown>", "F": "<unknown>", "G": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestInterpolate(t *testing.T) {
	env := map[string]*string{"SET": strp("v"), "EMPTY": strp(""), "LATER": nil}
	for _, tc := range []struct {
		in, out string
		known   bool
	}{
		{"plain", "plain", true},
		{"$SET and ${SET}", "v and v", true},
		{"${UNSET}", "", true},
		{"${UNSET:-def} ${EMPTY:-def} ${SET:-def}", "def def v", true},
		{"${UNSET-def} ${EMPTY-def}", "def ", true},
		{"${SET:+alt} ${EMPTY:+alt} ${UNSET:+alt}", "alt  ", true},
		{"${SET+alt} ${EMPTY+alt} ${UNSET+alt}", "alt alt ", true},
		{"$$SET", "$SET", true},
		{"${LATER}", "", false},            // its own value needs interpolating
		{"${UNSET:-$SET}", "", false},      // a nested default isn't worked out
		{"${SET:?must be set}", "v", true}, // compose refuses to start when it isn't
	} {
		out, known := interpolate(tc.in, env)
		if known != tc.known || (known && out != tc.out) {
			t.Errorf("%q: %q %v, want %q %v", tc.in, out, known, tc.out, tc.known)
		}
	}
}

func TestComposeServiceParsing(t *testing.T) {
	services, err := parseServices(`
services:
  api:
    image: omni/api
    env_file: [.env, ./extra.env]
    environment:
      - PLAIN=x
      - FROM_SHELL
      - IFACE=${HOST:-0.0.0.0}
    depends_on: [db]
  worker:
    image: omni/worker@sha256:abc
    env_file: {path: ./w.env}
    environment:
      DEBUG: true
      COUNT: 3
      SHELLVAR:
    depends_on:
      db: {condition: service_healthy}
      cache: {condition: service_started}
  db:
    image: postgres:16
`)
	if err != nil {
		t.Fatal(err)
	}
	api, worker := services["api"], services["worker"]
	if !reflect.DeepEqual(api.envFiles(), []string{".env", "extra.env"}) || !reflect.DeepEqual(worker.envFiles(), []string{"w.env"}) {
		t.Errorf("env files %v %v", api.envFiles(), worker.envFiles())
	}
	deps := worker.dependsOn()
	sort.Strings(deps)
	if !reflect.DeepEqual(api.dependsOn(), []string{"db"}) || !reflect.DeepEqual(deps, []string{"cache", "db"}) {
		t.Errorf("depends_on %v %v", api.dependsOn(), deps)
	}
	if imageRef(api.Image) != "omni/api:latest" || imageRef(worker.Image) != "omni/worker@sha256:abc" || imageRef("reg:5000/x") != "reg:5000/x:latest" {
		t.Error("imageRef")
	}
	files := map[string]map[string]*string{".env": {"FROM_SHELL": strp("file"), "A": strp("a")}, "extra.env": {"B": strp("b")},
		"w.env": {"SHELLVAR": strp("from file")}}
	got := show(api.expectedEnv(map[string]*string{}, files))
	want := map[string]string{"A": "a", "B": "b", "PLAIN": "x", "IFACE": "0.0.0.0"} // FROM_SHELL comes from the shell
	if !reflect.DeepEqual(got, want) {
		t.Errorf("api env %v, want %v", got, want)
	}
	got = show(worker.expectedEnv(map[string]*string{}, files))
	want = map[string]string{"DEBUG": "true", "COUNT": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("worker env %v, want %v", got, want)
	}
}
