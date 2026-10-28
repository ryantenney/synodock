package dsm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// saveTar writes a docker save-style tar: manifest.json names the tags and config blob.
func saveTar(t *testing.T, gz bool) string {
	t.Helper()
	var buf bytes.Buffer
	var out io.Writer = &buf
	var gw *gzip.Writer
	if gz {
		gw = gzip.NewWriter(&buf)
		out = gw
	}
	tw := tar.NewWriter(out)
	manifest := []byte(`[{"Config":"blobs/sha256/0123456789abcdef0123456789abcdef","RepoTags":["omni/app:1","omni/app:latest"]}]`)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gw != nil {
		if err := gw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "app.tar")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadImageTar(t *testing.T) {
	for _, gz := range []bool{false, true} {
		info, err := ReadImageTar(saveTar(t, gz))
		if err != nil {
			t.Fatalf("gzip=%v: %v", gz, err)
		}
		if got := info.ShortID; got != "0123456789ab" {
			t.Errorf("gzip=%v: short id %q", gz, got)
		}
		if len(info.Tags) != 2 || info.Tags[0] != "omni/app:1" || info.Tags[1] != "omni/app:latest" {
			t.Errorf("gzip=%v: tags %v", gz, info.Tags)
		}
	}
}

func TestReadImageTarRejectsOtherFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.tar")
	if err := os.WriteFile(path, []byte("not a tar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadImageTar(path); err == nil {
		t.Fatal("expected an error for a file that isn't a docker save tar")
	}
}
