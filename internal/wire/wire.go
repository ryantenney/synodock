// Package wire is the protocol between synodock and synodock-agent, the helper that
// can run inside a container in place of the framing script.
//
// Both ends talk through Container Manager's Terminal: the agent writes to its tty, the
// NAS relay reads that pty and sends what it read over the websocket. Measured on the
// NAS, the relay reads up to 4 KiB at a time, merging writes and cutting anywhere, and
// it reorders those reads. So nothing can rely on where a read begins or ends, or on
// the order they arrive in. Instead every record is self-contained and checked:
//
//	\x1e<kind> <seq> <stream> <base64 length> <base64 payload> <crc32>\n
//
// A record that lands whole inside one read decodes on its own. One cut by a read
// boundary leaves fragments, a head (from \x1e), a tail (to \n) and perhaps middles
// between them. A head says how long its record is, so the decoder only tries fragments
// that add up to that, and keeps the combination whose CRC (over the kind, seq, stream
// and payload) agrees.
//
// Records: D = output (stream 1 stdout, 2 stderr), C = compressed output (the stream's
// gzip stream, in pieces; the agent sends C or D, not both), X = the command exited (payload: its
// status; seq: the number of records before it). synodock acknowledges with "A<n>\n"
// on the tty: every record below n has arrived. Its go-ahead after the hello says what it
// can take: "z", C records; and "i", that stdin follows (protocol 3).
//
// Stdin goes the other way in the same lines, stream 0: I (data) and E (end of input).
// The agent takes them only in order and whole, and answers with K records, "<w> <a>":
// every input record below w is in the command's stdin pipe (so InWindow more may be
// sent), every one below a has arrived; with " n" when one came out of order or
// damaged, so synodock sends again from a.
package wire

import (
	"encoding/base64"
	"fmt"
	"hash/crc32"
	"sort"
	"strconv"
	"strings"
)

const (
	Mark = '\x1e'

	// MaxPayload is a record's most bytes: its line, about 1 KiB, is a quarter of the
	// relay's 4 KiB reads, so most records arrive whole and a cut one spans few reads.
	MaxPayload = 768

	// Hello is what the agent writes first, in the framing script's record syntax, so a
	// synodock reading that format knows to switch: kind V, protocol 2.
	Hello = "\x1eV 0 2 \n"

	// InWindow is how many input records may be on their way beyond what the agent has
	// written to the command (synodock-agent's IN_WINDOW).
	InWindow = 8
)

// Record is one decoded record.
type Record struct {
	Kind    byte
	Seq     int
	Stream  int
	Payload []byte
}

func sum(kind byte, seq, stream int, b64 string) uint32 {
	return crc32.ChecksumIEEE([]byte(fmt.Sprintf("%c %d %d %s", kind, seq, stream, b64)))
}

// Encode is a record's line.
func Encode(r Record) string {
	b64 := base64.StdEncoding.EncodeToString(r.Payload)
	return fmt.Sprintf("%c%c %d %d %d %s %08x\n", Mark, r.Kind, r.Seq, r.Stream, len(b64), b64, sum(r.Kind, r.Seq, r.Stream, b64))
}

// lineLength is how long the record that head begins is, from its header; 0 when the
// head was cut before its header ended.
func lineLength(head string) int {
	f := strings.SplitN(head[1:], " ", 5)
	if len(f) < 5 {
		return 0
	}
	n, err := strconv.Atoi(f[3])
	if err != nil {
		return 0
	}
	header := 1 + len(f[0]) + 1 + len(f[1]) + 1 + len(f[2]) + 1 + len(f[3]) + 1
	return header + n + 1 + 8 + 1 // payload, space, crc, newline
}

// parse decodes one candidate line (\x1e...\n); ok is false unless it's well formed
// and its CRC agrees.
func parse(line string) (Record, bool) {
	if len(line) < 2 || line[0] != Mark || line[len(line)-1] != '\n' {
		return Record{}, false
	}
	f := strings.Split(strings.TrimSuffix(line[1:], "\n"), " ")
	if len(f) != 6 || len(f[0]) != 1 {
		return Record{}, false
	}
	seq, err1 := strconv.Atoi(f[1])
	stream, err2 := strconv.Atoi(f[2])
	n, err3 := strconv.Atoi(f[3])
	want, err4 := strconv.ParseUint(f[5], 16, 32)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || n != len(f[4]) ||
		sum(f[0][0], seq, stream, f[4]) != uint32(want) {
		return Record{}, false
	}
	payload, err := base64.StdEncoding.DecodeString(f[4])
	if err != nil {
		return Record{}, false
	}
	return Record{Kind: f[0][0], Seq: seq, Stream: stream, Payload: payload}, true
}

// Decoder turns reads, in any order and cut anywhere, back into records in sequence.
type Decoder struct {
	next    int            // the lowest seq not yet handed on
	ready   map[int]Record // decoded, waiting for the ones before them
	heads   []string       // fragments that start a record
	middles []string       // fragments with neither end
	tails   []string       // fragments that end a record
}

func NewDecoder() *Decoder { return &Decoder{ready: map[int]Record{}} }

// Next is the lowest seq not yet handed on: what to acknowledge.
func (d *Decoder) Next() int { return d.next }

// Pending says whether fragments are waiting for their other parts.
func (d *Decoder) Pending() bool {
	return len(d.heads)+len(d.middles)+len(d.tails) > 0 || len(d.ready) > 0
}

// Feed takes one read and returns the records now in order.
func (d *Decoder) Feed(text string) []Record {
	d.split(text)
	d.join()
	var out []Record
	for {
		r, ok := d.ready[d.next]
		if !ok {
			return out
		}
		delete(d.ready, d.next)
		d.next++
		out = append(out, r)
	}
}

// split takes the whole records out of a read and files what's left as fragments.
func (d *Decoder) split(text string) {
	for text != "" {
		start := strings.IndexByte(text, Mark)
		if start != 0 {
			// Before the first mark: the end of some record (or all of a middle).
			end := strings.IndexByte(text, '\n')
			if start < 0 {
				if end < 0 {
					d.middles = append(d.middles, text)
				} else {
					d.tails = append(d.tails, text[:end+1])
					if rest := text[end+1:]; rest != "" {
						d.middles = append(d.middles, rest) // garbage after a tail; kept for matching
					}
				}
				return
			}
			if end >= 0 && end < start {
				d.tails = append(d.tails, text[:end+1])
			} else {
				d.middles = append(d.middles, text[:start])
			}
			text = text[start:]
			continue
		}
		end := strings.IndexByte(text, '\n')
		if end < 0 {
			d.heads = append(d.heads, text)
			return
		}
		if r, ok := parse(text[:end+1]); ok {
			d.keep(r)
		} else {
			// A mark and a newline that don't make a record: a head cut short, then a tail.
			d.heads = append(d.heads, text[:end+1])
		}
		text = text[end+1:]
	}
}

func (d *Decoder) keep(r Record) {
	if r.Seq >= d.next {
		d.ready[r.Seq] = r
	}
}

// maxMiddles is how many fragments with neither end one record may take.
const maxMiddles = 4

// join puts heads, middles and tails back together until no more make a record.
func (d *Decoder) join() {
	for d.joinOne() {
	}
}

func (d *Decoder) joinOne() bool {
	for hi, head := range d.heads {
		head = strings.TrimSuffix(head, "\n") // a head mistaken as whole by split
		want := lineLength(head)
		for ti, tail := range d.tails {
			if want > 0 && len(head)+len(tail) > want {
				continue
			}
			if d.extend(hi, head, nil, ti, want) {
				return true
			}
		}
	}
	return false
}

// extend tries head + middles + tail, adding middles that keep the length in reach (when
// the head says it), up to maxMiddles.
func (d *Decoder) extend(hi int, prefix string, used []int, ti int, want int) bool {
	tail := d.tails[ti]
	if want == 0 || len(prefix)+len(tail) == want {
		if r, ok := parse(prefix + tail); ok {
			d.keep(r)
			d.heads = remove(d.heads, hi)
			d.tails = remove(d.tails, ti)
			sort.Sort(sort.Reverse(sort.IntSlice(used)))
			for _, m := range used {
				d.middles = remove(d.middles, m)
			}
			return true
		}
	}
	if len(used) == maxMiddles || (want == 0 && len(used) == 1) { // without a length, stay shallow
		return false
	}
next:
	for mi, mid := range d.middles {
		if want > 0 && len(prefix)+len(mid)+len(tail) > want {
			continue
		}
		for _, u := range used {
			if u == mi {
				continue next
			}
		}
		if d.extend(hi, prefix+mid, append(append([]int(nil), used...), mi), ti, want) {
			return true
		}
	}
	return false
}

func remove(s []string, i int) []string { return append(s[:i:i], s[i+1:]...) }

// Leftover describes what's waiting, for an error message.
func (d *Decoder) Leftover() string {
	return fmt.Sprintf("%d heads, %d middles, %d tails, %d records waiting for seq %d",
		len(d.heads), len(d.middles), len(d.tails), len(d.ready), d.next)
}
