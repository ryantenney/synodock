package dockerapi

import (
	"io/fs"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	for _, tc := range []struct {
		line string
		want pathStat
	}{
		{"1234 81a4 1700000000 \n", pathStat{Name: "nginx.conf", Size: 1234, Mode: 0o644, Mtime: time.Unix(1700000000, 0).UTC()}},
		{"4096 41ed 1700000000 \n", pathStat{Name: "nginx.conf", Size: 4096, Mode: fs.ModeDir | 0o755, Mtime: time.Unix(1700000000, 0).UTC()}},
		{"5 a1ff 1700000000 a b.txt\n", pathStat{Name: "nginx.conf", Size: 5, Mode: fs.ModeSymlink | 0o777,
			Mtime: time.Unix(1700000000, 0).UTC(), LinkTarget: "a b.txt"}},
		{"0 89ed 1 \n", pathStat{Name: "nginx.conf", Mode: fs.ModeSetuid | 0o755, Mtime: time.Unix(1, 0).UTC()}},
	} {
		got, err := parseStat("/etc/nginx.conf", tc.line)
		if err != nil || got != tc.want {
			t.Errorf("%q: %+v %v, want %+v", tc.line, got, err, tc.want)
		}
	}
	if _, err := parseStat("/x", "garbage\n"); err == nil {
		t.Error("garbage parsed")
	}
}
