package dsm

import (
	"reflect"
	"testing"
)

func TestBindFolders(t *testing.T) {
	compose := `
services:
  web:
    volumes:
      - ./data:/var/lib/data
      - "./logs/nginx:/var/log/nginx"
      - ./logs/app:/var/log/app:ro
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
      - ./conf.d/site.conf:/etc/nginx/conf.d/site.conf
      - named-volume:/srv
      - /volume1/abs:/abs
      - ./.cache:/root/.cache
`
	folders, skipped := bindFolders(compose)
	// Parents are always folders; a leaf that looks like a file is left alone.
	wantFolders := []string{"data", "logs", "logs/nginx", "logs/app", "conf.d", ".cache"}
	wantSkipped := []string{"./nginx.conf", "./conf.d/site.conf"}
	if !reflect.DeepEqual(folders, wantFolders) {
		t.Errorf("folders %v, want %v", folders, wantFolders)
	}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Errorf("skipped %v, want %v", skipped, wantSkipped)
	}
}
