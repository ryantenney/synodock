package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/ryantenney/synodock/internal/dsm"
	"github.com/ryantenney/synodock/internal/wire"
)

// pumpAgent reads synodock-agent's records (package wire): output in any order and cut
// anywhere, put back in sequence, acknowledged as it arrives so the agent keeps sending.
func pumpAgent(ctx context.Context, s *Session, write func(stream int, data []byte) error) (int, error) {
	d := wire.NewDecoder()
	gz := map[int]*gunzip{} // C records: one gzip stream per output stream
	defer func() {
		for _, g := range gz {
			g.abort()
		}
	}()
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
