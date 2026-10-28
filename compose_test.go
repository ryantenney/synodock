package main

import (
	"reflect"
	"testing"
)

const composeSample = `
services:
  web:
    image: nginx
    container_name: site
  db:
    image: "registry.example/app/db:16"
  job:
    image: busybox:1.36
    profiles: [tools]
  worker:
    image: app/worker
`

func TestComposeImages(t *testing.T) {
	got := composeImages(composeSample)
	want := []string{"nginx:latest", "registry.example/app/db:16", "busybox:1.36", "app/worker:latest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("composeImages = %v, want %v", got, want)
	}
}

func TestComposeContainerNames(t *testing.T) {
	got, err := composeContainerNames(composeSample, "proj")
	if err != nil {
		t.Fatal(err)
	}
	// container_name where set, else <project>-<service>-1; profiles don't start on a build.
	want := []string{"proj-db-1", "proj-worker-1", "site"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("composeContainerNames = %v, want %v", got, want)
	}
}

func TestComposeContainerNamesRejectsBadYAML(t *testing.T) {
	if _, err := composeContainerNames("services: [", "proj"); err == nil {
		t.Error("expected an error for invalid YAML")
	}
}
