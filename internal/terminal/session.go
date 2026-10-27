// Package terminal runs commands in NAS containers through Container Manager's
// Terminal websocket.
//
// Author: Ryan Tenney
//
// DSM has no exec in the SYNO.Docker.* web API; the Terminal tab in Container
// Manager uses a websocket at /docker/ws instead (docker.js, TermSocket/PanelTerm).
// Messages are JSON arrays, ["event", *args], in both directions:
//
//	monitor socket   -> ["enter", container, "monitor"]   <- ["enter", ok]
//	                                                       <- ["list", [id, title, id, title, ...]]
//	                 -> ["create", command]                <- ["create", ok], then a new "list"
//	                 -> ["delete", tty_id]
//	terminal socket  -> ["enter", container, "terminal"]  <- ["enter", ok]
//	                 -> ["attach", tty_id, force]          <- ["attach", 1503 ok | 1504 in use | 1505 failed]
//	                 -> ["resize", rows, cols]
//	                 -> ["data", text]                     <- ["data", text]
//	                                                       <- ["close", 1501 ended | 1500 taken over | 1502]
//
// A tty id is a Docker exec id. "create" splits its command like a shell (no
// expansion), except that it cuts the command off at the first unquoted "="
// (`echo a=b c` runs `echo a`), so Join single-quotes every argument. The exec
// starts at "attach", so nothing is lost before we listen. It always runs on a
// TTY. Ended execs drop off the list by themselves; "delete" is only needed when
// we give up early.
//
// The handshake needs the DSM session as the `id` cookie and an Origin header
// matching the DSM address (HTTP 400 otherwise).
//
// The list also has an entry whose id is the container's own name: that's PID 1's
// console. Never attach to it, and never attach with force=true (that kicks off
// whoever is using the Terminal tab).
//
// **The relay reorders output.** Each "data" message is one read from the pty and
// arrives intact, but when several are in flight they can arrive in any order (seen
// from 500 lines of `seq` up; intermittent; the UI terminal has the same bug), and
// "close" can overtake them too. So by default the command's output is cut into
// chunks in the container (dd, one pipe read each), each gzipped and hex-encoded
// (od) and sent by awk as records, one in flight at a time: awk waits for our ack
// on /dev/tty before sending the next. A record split over several pty reads is put
// back together (only its pieces can be in flight). The exit status comes in a last
// record. Per-chunk compression keeps slow output live (a line a second arrives a
// second apart) while bulk output still compresses. Hex also keeps binary output
// and \n intact and stderr in order with stdout. This needs sh, dd, od and awk in
// the image (gzip optional); without them, or with stdin (-i, which would share the
// tty with the acks), output is passed through as it comes and can be out of order.
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ryantenney/synodock/internal/dsm"
)

const (
	attachOK, attachInUse, attachFailed = 1503, 1504, 1505
	closeTakenOver, closeEnded          = 1500, 1501
	waitTimeout                         = 15 * time.Second
	pingEvery                           = 20 * time.Second
)

var execID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// errQuiet is Recv's answer when nothing came within the poll interval.
var errQuiet = errors.New("no output")

type message struct {
	event string
	args  []json.RawMessage
}

// conn is one websocket. A goroutine reads it into msgs, so waits can time out
// without a read deadline (which would break the connection for good); writes
// take a lock, since acks, stdin, resizes and ^C can come from several goroutines.
type conn struct {
	ws        *websocket.Conn
	wmu       sync.Mutex
	msgs      chan message
	done      chan struct{}
	closeOnce sync.Once
}

func dial(client *dsm.Client, container, role string) (*conn, error) {
	endpoint := "ws" + strings.TrimPrefix(client.URL, "http") + "/docker/ws"
	dialer := websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: waitTimeout}
	header := http.Header{"Cookie": {"id=" + client.SID()}, "Origin": {client.URL}}
	ws, resp, err := dialer.Dial(endpoint, header)
	if err != nil {
		if resp != nil {
			return nil, dsm.Errorf("terminal websocket: %v (HTTP %d)", err, resp.StatusCode)
		}
		return nil, dsm.Errorf("terminal websocket: %v", err)
	}
	ws.SetReadLimit(0) // DSM's messages are one pty read each; no cap
	c := &conn{ws: ws, msgs: make(chan message), done: make(chan struct{})}
	go c.read()
	go c.keepalive()
	if err := c.send("enter", container, role); err != nil {
		c.close()
		return nil, err
	}
	args, err := c.wait(context.Background(), "enter")
	if err != nil {
		c.close()
		return nil, err
	}
	if len(args) == 0 || !truthy(args[0]) {
		c.close()
		return nil, dsm.Errorf("can't open a terminal in %s (not running, or no such container)", container)
	}
	return c, nil
}

func (c *conn) read() {
	defer close(c.msgs)
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		var raw []json.RawMessage
		if json.Unmarshal(data, &raw) != nil || len(raw) == 0 {
			continue
		}
		var event string
		_ = json.Unmarshal(raw[0], &event)
		select {
		case c.msgs <- message{event, raw[1:]}:
		case <-c.done:
			return
		}
	}
}

func (c *conn) keepalive() {
	tick := time.NewTicker(pingEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			_ = c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
		case <-c.done:
			return
		}
	}
}

func (c *conn) send(args ...any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(args); err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.WriteMessage(websocket.TextMessage, bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

// wait returns the args of the next message for event, skipping others.
func (c *conn) wait(ctx context.Context, event string) ([]json.RawMessage, error) {
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-c.msgs:
			if !ok {
				return nil, dsm.Errorf("terminal websocket closed while waiting for %q", event)
			}
			if msg.event == event {
				return msg.args, nil
			}
		case <-timer.C:
			return nil, dsm.Errorf("terminal websocket: no %q within %s", event, waitTimeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (c *conn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.wmu.Lock()
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		c.wmu.Unlock()
		_ = c.ws.Close()
	})
}

// truthy is Python truthiness for a JSON value: DSM's acks aren't always booleans.
func truthy(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func intArg(args []json.RawMessage) int {
	var n int
	if len(args) > 0 {
		_ = json.Unmarshal(args[0], &n)
	}
	return n
}

// Session is one exec in one container. Close it when done: an exec that hasn't
// ended is deleted.
type Session struct {
	Container string
	ttyID     string
	ended     bool
	closeCode int // 0 until a "close" arrives
	monitor   *conn
	term      *conn
}

// Open creates the exec and attaches to it; the command starts now.
func Open(ctx context.Context, client *dsm.Client, container, command string, rows, cols int) (s *Session, err error) {
	s = &Session{Container: container}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	if s.monitor, err = dial(client, container, "monitor"); err != nil {
		return nil, err
	}
	args, err := s.monitor.wait(ctx, "list")
	if err != nil {
		return nil, err
	}
	before := ttyIDs(args)
	if err := s.monitor.send("create", command); err != nil {
		return nil, err
	}
	if args, err = s.monitor.wait(ctx, "create"); err != nil {
		return nil, err
	}
	if len(args) == 0 || !truthy(args[0]) {
		return nil, dsm.Errorf("Container Manager refused to create an exec in %s", container)
	}
	for s.ttyID == "" {
		if args, err = s.monitor.wait(ctx, "list"); err != nil {
			return nil, err
		}
		var fresh []string
		for id := range ttyIDs(args) {
			if !before[id] {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) > 1 {
			return nil, dsm.Errorf("several new terminals appeared in %s at once; not guessing which is ours", container)
		}
		if len(fresh) == 1 {
			if !execID.MatchString(fresh[0]) { // e.g. the container's own console
				return nil, dsm.Errorf("unexpected terminal id %q", fresh[0])
			}
			s.ttyID = fresh[0]
		}
	}
	if s.term, err = dial(client, container, "terminal"); err != nil {
		return nil, err
	}
	if err := s.term.send("attach", s.ttyID, false); err != nil {
		return nil, err
	}
	if args, err = s.term.wait(ctx, "attach"); err != nil {
		return nil, err
	}
	if code := intArg(args); code != attachOK {
		return nil, dsm.Errorf("attach to the new exec in %s failed (%d)", container, code)
	}
	if err := s.Resize(rows, cols); err != nil {
		return nil, err
	}
	// Keep reading the monitor socket (more "list"s come), so pings are answered
	// and its reader isn't stuck.
	go func(msgs <-chan message) {
		for range msgs {
		}
	}(s.monitor.msgs)
	return s, nil
}

// ttyIDs are the ids in a "list": [id, title, id, title, ...].
func ttyIDs(args []json.RawMessage) map[string]bool {
	ids := map[string]bool{}
	var entries []json.RawMessage
	if len(args) > 0 {
		_ = json.Unmarshal(args[0], &entries)
	}
	for i := 0; i < len(entries); i += 2 {
		var id string
		if json.Unmarshal(entries[i], &id) == nil {
			ids[id] = true
		}
	}
	return ids
}

func (s *Session) Close() {
	if s.term != nil {
		s.term.close()
	}
	if s.monitor != nil {
		if s.ttyID != "" && !s.ended {
			_ = s.monitor.send("delete", s.ttyID)
		}
		s.monitor.close()
	}
}

// Send types text into the exec's tty.
func (s *Session) Send(text string) error { return s.term.send("data", text) }

func (s *Session) Resize(rows, cols int) error { return s.term.send("resize", rows, cols) }

// Recv returns the next output text, io.EOF once the exec has ended, and with
// poll > 0, errQuiet after poll without any.
func (s *Session) Recv(ctx context.Context, poll time.Duration) (string, error) {
	var quiet <-chan time.Time
	if poll > 0 {
		timer := time.NewTimer(poll)
		defer timer.Stop()
		quiet = timer.C
	}
	for {
		select {
		case msg, ok := <-s.term.msgs:
			if !ok {
				return "", io.EOF
			}
			switch msg.event {
			case "data":
				var text string
				if len(msg.args) > 0 {
					_ = json.Unmarshal(msg.args[0], &text)
				}
				return text, nil
			case "close":
				s.closeCode = intArg(msg.args)
				s.ended = s.closeCode == closeEnded
				return "", io.EOF
			}
		case <-quiet:
			return "", errQuiet
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func (s *Session) checkClose() error {
	switch s.closeCode {
	case 0, closeEnded:
		return nil
	case closeTakenOver:
		return dsm.Errorf("someone else attached to this exec (Container Manager's Terminal tab?)")
	default:
		return dsm.Errorf("terminal closed with code %d", s.closeCode)
	}
}
