package wire

import (
	"bytes"
	"math/rand"
	"strings"
	"testing"
)

// stream encodes n records of random sizes and returns them with their wire text.
func stream(rng *rand.Rand, n int) ([]Record, string) {
	var recs []Record
	var b strings.Builder
	for i := 0; i < n; i++ {
		payload := make([]byte, 1+rng.Intn(MaxPayload))
		rng.Read(payload)
		r := Record{Kind: 'D', Seq: i, Stream: 1 + rng.Intn(2), Payload: payload}
		recs = append(recs, r)
		b.WriteString(Encode(r))
	}
	return recs, b.String()
}

// cut splits text the way the relay reads it: mostly 4 KiB, sometimes less, anywhere.
func cut(rng *rand.Rand, text string, small bool) []string {
	var pieces []string
	for text != "" {
		n := 4096
		if small || rng.Intn(4) == 0 {
			n = 1 + rng.Intn(4096)
		}
		if small {
			n = 300 + rng.Intn(3800) // the smallest reads measured were about 500 bytes
		}
		n = min(n, len(text))
		pieces = append(pieces, text[:n])
		text = text[n:]
	}
	return pieces
}

// shuffle reorders pieces within windows of up to w, as the relay reorders reads in flight.
func shuffle(rng *rand.Rand, pieces []string, w int) {
	for i := 0; i < len(pieces); i += w {
		j := min(i+w, len(pieces))
		rng.Shuffle(j-i, func(a, b int) { pieces[i+a], pieces[i+b] = pieces[i+b], pieces[i+a] })
	}
}

func TestDecoderPutsCutAndReorderedReadsBack(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		recs, text := stream(rng, 300)
		pieces := cut(rng, text, seed%10 == 0) // every tenth: all small reads
		shuffle(rng, pieces, 1+rng.Intn(8))
		d := NewDecoder()
		var got []Record
		for _, p := range pieces {
			got = append(got, d.Feed(p)...)
		}
		if len(got) != len(recs) {
			t.Fatalf("seed %d: %d of %d records (%s)", seed, len(got), len(recs), d.Leftover())
		}
		for i := range recs {
			if got[i].Seq != i || got[i].Stream != recs[i].Stream || !bytes.Equal(got[i].Payload, recs[i].Payload) {
				t.Fatalf("seed %d: record %d differs", seed, i)
			}
		}
		if d.Pending() {
			t.Fatalf("seed %d: left over: %s", seed, d.Leftover())
		}
	}
}

func TestEncodeParseRoundTrip(t *testing.T) {
	for _, r := range []Record{
		{Kind: 'D', Seq: 0, Stream: 1, Payload: []byte("hello\n")},
		{Kind: 'D', Seq: 12345, Stream: 2, Payload: []byte{0, 0x1e, '\n', 0xff}},
		{Kind: 'X', Seq: 7, Stream: 0, Payload: []byte("3")},
	} {
		got, ok := parse(Encode(r))
		if !ok || got.Kind != r.Kind || got.Seq != r.Seq || got.Stream != r.Stream || !bytes.Equal(got.Payload, r.Payload) {
			t.Errorf("%+v: got %+v %v", r, got, ok)
		}
	}
	line := Encode(Record{Kind: 'D', Seq: 1, Stream: 1, Payload: []byte("abc")})
	if _, ok := parse(strings.Replace(line, " 1 1 ", " 2 1 ", 1)); ok {
		t.Error("a changed seq passed the CRC")
	}
}

func TestHelloMatchesTheScriptsRecordSyntax(t *testing.T) {
	// The framing script's decoder reads this before it knows an agent is there.
	if !strings.HasPrefix(Hello, "\x1eV ") || !strings.HasSuffix(Hello, "\n") {
		t.Errorf("hello %q", Hello)
	}
}

// Wider reordering than measured, for margin: windows of up to 16 reads.
func TestDecoderWideReordering(t *testing.T) {
	for seed := int64(1000); seed < 1100; seed++ {
		rng := rand.New(rand.NewSource(seed))
		recs, text := stream(rng, 400)
		pieces := cut(rng, text, true)
		shuffle(rng, pieces, 1+rng.Intn(16))
		d := NewDecoder()
		n := 0
		for _, p := range pieces {
			n += len(d.Feed(p))
		}
		if n != len(recs) || d.Pending() {
			t.Fatalf("seed %d: %d of %d records (%s)", seed, n, len(recs), d.Leftover())
		}
	}
}
