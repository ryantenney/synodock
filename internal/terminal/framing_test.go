package terminal

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// testdata/golden.json holds the command lines the Python version generated for
// the same argv: the container side only works with them byte for byte.
func TestCommandsMatchGolden(t *testing.T) {
	data, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]string
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	argv := []string{"echo", "a=b", "it's", "$x", ""}
	got := map[string]string{
		"framed_gzip":          CommandFor(argv, Options{Gzip: true}),
		"framed_nogzip":        CommandFor(argv, Options{}),
		"detach":               CommandFor(argv, Options{Detach: true}),
		"detach_raw":           CommandFor(argv, Options{Detach: true, Raw: true}),
		"raw":                  CommandFor(argv, Options{Raw: true}),
		"unframed_interactive": CommandFor(argv, Options{Unframed: true, Interactive: true, Stdin: true}),
		"unframed_stdin":       CommandFor(argv, Options{Unframed: true, Stdin: true}),
		"unframed":             CommandFor(argv, Options{Unframed: true}),
		"join":                 Join(argv),
		"framed_user":          CommandFor(argv, Options{User: "nobody"}),
		"detach_user":          CommandFor(argv, Options{Detach: true, User: "1000:1000"}),
	}
	if os.Getenv("UPDATE_GOLDEN") == "1" { // after a deliberate change to the scripts
		out, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile("testdata/golden.json", append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	for name, want := range golden {
		if got[name] != want {
			t.Errorf("%s:\n got %q\nwant %q", name, got[name], want)
		}
	}
}

func TestExitFilter(t *testing.T) {
	f := &exitFilter{}
	out := f.feed("hello\n\x1e__dsm")
	out += f.feed("_exit=7")
	if f.known {
		t.Fatal("known before the newline")
	}
	out += f.feed("\r\nmore")
	if out != "hello\nmore" || !f.known || f.code != 7 {
		t.Fatalf("got %q known=%v code=%d", out, f.known, f.code)
	}
	f = &exitFilter{}
	out = f.feed("x\x1e__dsm_exit=3")
	out += f.flush()
	if out != "x" || f.code != 3 {
		t.Fatalf("flush: got %q code=%d", out, f.code)
	}
}

func rec(kind byte, seq, n int, hexdata string) string {
	if kind == 'D' || kind == 'R' {
		data, _ := hex.DecodeString(hexdata)
		return fmt.Sprintf("\x1e%c %d %d %s %d\n", kind, seq, n, hexdata, fletcher16(data))
	}
	return fmt.Sprintf("\x1e%c %d %d %s\n", kind, seq, n, hexdata)
}

// The relay can split a record into several pieces and reorder them. Swapped pieces from
// the middle are all hex, so without the checksum they'd still look like a record, and
// the bytes would come out in the wrong order with nothing said.
func TestDeframerMiddlePiecesSwapped(t *testing.T) {
	data := []byte("the quick brown fox jumps over the lazy dog")
	whole := rec('D', 0, len(data), hex.EncodeToString(data))
	// head | mid1 | mid2 | tail, arriving head, mid2, mid1, tail
	head, mid1, mid2, tail := whole[:12], whole[12:40], whole[40:70], whole[70:]
	d := &deframer{}
	var got *frame
	for _, piece := range []string{head, mid2, mid1, tail} {
		r, err := d.feed(piece)
		if err != nil {
			t.Fatal(err)
		}
		if r != nil {
			got = r
		}
	}
	if got == nil || string(got.data) != string(data) {
		t.Fatalf("got %q, want %q", got.data, data)
	}
}

func TestFletcher16(t *testing.T) {
	// Fletcher-16 of "abcde" is 0xC8F0; swapping two bytes changes it.
	if got := fletcher16([]byte("abcde")); got != 0xC8F0 {
		t.Errorf("fletcher16(abcde) = %#x, want 0xc8f0", got)
	}
	if fletcher16([]byte("ab")) == fletcher16([]byte("ba")) {
		t.Error("order doesn't change the checksum")
	}
}

func TestDeframerReordered(t *testing.T) {
	d := &deframer{}
	whole := rec('D', 0, 3, "616263")
	if r, err := d.feed(whole[8:]); r != nil || err != nil {
		t.Fatalf("half a record decoded: %v %v", r, err)
	}
	r, err := d.feed(whole[:8])
	if err != nil || r == nil || string(r.data) != "abc" {
		t.Fatalf("got %v %v", r, err)
	}
	if r, _ := d.feed(rec('E', 1, 0, "")); r == nil || r.kind != 'E' {
		t.Fatalf("end: %v", r)
	}
	if r, _ := d.feed(rec('S', 0, 42, "")); r == nil || !d.hasStatus || d.status != 42 {
		t.Fatalf("status: %v", r)
	}
	if _, err := (&deframer{}).feed(rec('D', 5, 1, "61")); err == nil {
		t.Fatal("out-of-sequence record accepted")
	}
}

func TestGunzipMembers(t *testing.T) {
	var members []byte
	for _, part := range []string{"one\n", "two\n", strings.Repeat("x", 100000)} {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		w.Write([]byte(part))
		w.Close()
		members = append(members, b.Bytes()...)
	}
	var out bytes.Buffer
	g := newGunzip(func(b []byte) error { out.Write(b); return nil })
	for len(members) > 0 {
		n := min(recordBytes, len(members))
		if err := g.feed(members[:n]); err != nil {
			t.Fatal(err)
		}
		members = members[n:]
	}
	if err := g.finish(); err != nil {
		t.Fatal(err)
	}
	if want := "one\ntwo\n" + strings.Repeat("x", 100000); out.String() != want {
		t.Fatalf("got %d bytes", out.Len())
	}

	// A member's output is written before the next one starts arriving.
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte("live\n"))
	w.Close()
	got := make(chan string, 1)
	g = newGunzip(func(b []byte) error { got <- string(b); return nil })
	g.feed(b.Bytes())
	if s := <-got; s != "live\n" {
		t.Fatalf("got %q", s)
	}
	g.feed(b.Bytes()[:5])
	if err := g.finish(); err == nil {
		t.Fatal("a cut-off member wasn't an error")
	}
}

func TestTextChunksHoldsSplitRune(t *testing.T) {
	var got []string
	r := &splitReader{parts: [][]byte{[]byte("a\xc3"), []byte("\xa9b")}}
	if err := textChunks(r, func(s string) error { got = append(got, s); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "a|éb" {
		t.Fatalf("got %q", got)
	}
}

type splitReader struct{ parts [][]byte }

func (r *splitReader) Read(p []byte) (int, error) {
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.parts[0])
	r.parts = r.parts[1:]
	return n, nil
}

func TestTruthy(t *testing.T) {
	for raw, want := range map[string]bool{"true": true, "1": true, `"x"`: true, "false": false, "0": false,
		"null": false, `""`: false, "[]": false, "0.0": false} {
		if truthy(json.RawMessage(raw)) != want {
			t.Errorf("truthy(%s) != %v", raw, want)
		}
	}
}

// Many small pieces, a few out of place: what the NAS's relay does to a full record.
func TestDeframerManyPieces(t *testing.T) {
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i * 7)
	}
	whole := rec('D', 0, len(data), hex.EncodeToString(data))
	var pieces []string
	for i := 0; i < len(whole); i += len(whole)/10 + 1 {
		pieces = append(pieces, whole[i:min(i+len(whole)/10+1, len(whole))])
	}
	pieces[3], pieces[4] = pieces[4], pieces[3] // two middle pieces swapped
	pieces[7], pieces[8] = pieces[8], pieces[7]
	d := &deframer{}
	var got *frame
	for _, piece := range pieces {
		r, err := d.feed(piece)
		if err != nil {
			t.Fatal(err)
		}
		if r != nil {
			got = r
		}
	}
	if got == nil || string(got.data) != string(data) {
		t.Fatalf("%d pieces: record not put back together", len(pieces))
	}
}
