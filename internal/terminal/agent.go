package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
	"github.com/ryantenney/synodock/internal/wire"
)

// pumpAgent reads synodock-agent's records (package wire): output in any order and cut
// anywhere, put back in sequence, acknowledged as it arrives so the agent keeps sending.
// With stdin, it's sent alongside (sendInput), paced by the agent's K records.
func pumpAgent(ctx context.Context, s *Session, write func(stream int, data []byte) error, stdin io.Reader) (int, error) {
	d := wire.NewDecoder()
	gz := map[int]*gunzip{} // C records: one gzip stream per output stream
	defer func() {
		for _, g := range gz {
			g.abort()
		}
	}()
	var acks chan inputAck
	if stdin != nil {
		acks = make(chan inputAck, 64)
		inCtx, stop := context.WithCancel(ctx)
		sent := make(chan error, 1)
		go func() { sent <- sendInput(inCtx, s, stdin, acks) }()
		defer func() { stop(); <-sent }()
	}
	acked := 0
	status, exited := 0, false
	last := time.Now()
	for {
		text, err := s.Recv(ctx, stallTimeout)
		if errors.Is(err, errQuiet) {
			if d.Pending() && time.Since(last) >= stallTimeout {
				return 0, dsm.Errorf("%s: exec output stopped: %s", s.Container, d.Leftover())
			}
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			_ = s.Send("\x03")
			return 0, err
		}
		last = time.Now()
		for _, r := range d.Feed(text) {
			switch r.Kind {
			case 'D':
				if err := write(r.Stream, r.Payload); err != nil {
					return 0, err
				}
			case 'C':
				stream := r.Stream
				if gz[stream] == nil {
					gz[stream] = newGunzip(func(b []byte) error { return write(stream, b) })
				}
				if err := gz[stream].feed(r.Payload); err != nil {
					return 0, err
				}
			case 'K':
				if k, ok := parseInputAck(string(r.Payload)); ok && acks != nil {
					select {
					case acks <- k:
					default: // the sender is behind or done; a later K says as much
					}
				}
			case 'X':
				status, _ = strconv.Atoi(string(r.Payload))
				exited = true
				for stream, g := range gz {
					err := g.finish()
					delete(gz, stream)
					if err == io.ErrUnexpectedEOF {
						return 0, dsm.Errorf("%s: exec output ended mid-gzip stream", s.Container)
					} else if err != nil {
						return 0, err
					}
				}
			}
		}
		if d.Next() > acked {
			acked = d.Next()
			if err := s.Send(fmt.Sprintf("A%d\n", acked)); err != nil {
				return 0, err
			}
		}
	}
	if err := s.checkClose(); err != nil {
		return 0, err
	}
	if !exited {
		return 0, dsm.Errorf("%s: exec ended without an exit status (%s)", s.Container, d.Leftover())
	}
	return status, nil
}

// inputAck is a K record: input below w is in the command's pipe, below a has arrived;
// nack: one came out of order or damaged, so send again from a.
type inputAck struct {
	w, a int
	nack bool
}

func parseInputAck(p string) (inputAck, bool) {
	f := strings.Fields(p)
	if len(f) < 2 {
		return inputAck{}, false
	}
	w, err1 := strconv.Atoi(f[0])
	a, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return inputAck{}, false
	}
	return inputAck{w: w, a: a, nack: len(f) > 2 && f[2] == "n"}, true
}

// resendAfter is how long sent input may go unacknowledged before it's sent again, from
// where the agent last said it had got to: for a record the relay lost outright.
const resendAfter = 500 * time.Millisecond

// sendInput sends r to synodock-agent as I records, then an E record, with no more than
// wire.InWindow beyond what the agent has written to the command, and sends again from
// where the agent asks. It returns once the E record is in, or ctx ends.
func sendInput(ctx context.Context, s *Session, r io.Reader, acks <-chan inputAck) error {
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, wire.MaxPayload)
			n, err := r.Read(buf)
			if n > 0 {
				select {
				case chunks <- buf[:n]:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				return // end of input (or it can't be read further): E
			}
		}
	}()
	var recs []wire.Record // recs[0] is record base; base+len(recs) the next to make
	base, next, arrived := 0, 0, 0
	eof := false
	progress := time.Now()
	tick := time.NewTicker(resendAfter / 4)
	defer tick.Stop()
	for {
		for next < base+len(recs) && next < base+wire.InWindow {
			if err := s.Send(wire.Encode(recs[next-base])); err != nil {
				return err
			}
			next++
		}
		in := chunks
		if eof || len(recs) >= wire.InWindow {
			in = nil // read ahead no further than can be sent
		}
		select {
		case <-ctx.Done():
			return nil
		case b, ok := <-in:
			if !ok {
				eof = true
				recs = append(recs, wire.Record{Kind: 'E', Seq: base + len(recs)})
				continue
			}
			recs = append(recs, wire.Record{Kind: 'I', Seq: base + len(recs), Payload: b})
		case k := <-acks:
			if k.w > base {
				recs = recs[min(k.w-base, len(recs)):]
				base = k.w
				next = max(next, base)
				progress = time.Now()
			}
			if k.a > arrived {
				arrived = k.a
				progress = time.Now()
			}
			if k.nack && k.a < next {
				next = max(k.a, base)
			}
			if eof && len(recs) == 0 {
				return nil
			}
		case <-tick.C:
			if next > base && time.Since(progress) >= resendAfter {
				next = max(arrived, base)
				progress = time.Now()
			}
		}
	}
}
