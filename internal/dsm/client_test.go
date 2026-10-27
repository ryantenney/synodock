package dsm

import (
	"encoding/json"
	"testing"
)

// DSM's entries carry Docker's keys beside its own, differing only in case.
func TestContainerKeysExact(t *testing.T) {
	var c Container
	err := json.Unmarshal([]byte(`{"name": "web", "id": "abc", "status": "running", "created": 1700000000,
		"image": "nginx:latest", "Name": "/web", "Id": "other", "Status": "Up 2 hours",
		"Created": "2026-10-26T10:00:00Z", "Image": "sha256:ff", "State": {"Running": true}}`), &c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "web" || c.ID != "abc" || c.Status != "running" || string(c.Created) != "1700000000" ||
		c.Image != "nginx:latest" || c.ImageUpper != "sha256:ff" || !c.Running() {
		t.Fatalf("got %+v", c)
	}
	var img Image
	if err := json.Unmarshal([]byte(`{"id": "sha256:aa", "Id": "x", "created": 5, "Created": "2026"}`), &img); err != nil {
		t.Fatal(err)
	}
	if img.ID != "sha256:aa" || img.Created != "5" {
		t.Fatalf("got %+v", img)
	}
}
