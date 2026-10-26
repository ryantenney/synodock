"""Run commands in NAS containers through Container Manager's Terminal websocket.

Author: Ryan Tenney

DSM has no exec in the SYNO.Docker.* web API; the Terminal tab in Container
Manager uses a websocket at /docker/ws instead (docker.js, TermSocket/PanelTerm).
Messages are JSON arrays, ["event", *args], in both directions:

  monitor socket   -> ["enter", container, "monitor"]   <- ["enter", ok]
                                                         <- ["list", [id, title, id, title, ...]]
                   -> ["create", command]                <- ["create", ok], then a new "list"
                   -> ["delete", tty_id]
  terminal socket  -> ["enter", container, "terminal"]  <- ["enter", ok]
                   -> ["attach", tty_id, force]          <- ["attach", 1503 ok | 1504 in use | 1505 failed]
                   -> ["resize", rows, cols]
                   -> ["data", text]                     <- ["data", text]
                                                         <- ["close", 1501 ended | 1500 taken over | 1502]

A tty id is a Docker exec id. "create" splits its command like a shell (no
expansion), except that it cuts the command off at the first unquoted "="
(`echo a=b c` runs `echo a`), so join() single-quotes every argument. The exec
starts at "attach", so nothing is lost before we listen. It always runs on a TTY. Ended execs drop
off the list by themselves; "delete" is only needed when we give up early.

The handshake needs the DSM session as the `id` cookie and an Origin header
matching the DSM address (HTTP 400 otherwise).

The list also has an entry whose id is the container's own name: that's PID 1's
console. Never attach to it, and never attach with force=true (that kicks off
whoever is using the Terminal tab).

**The relay reorders output.** Each "data" message is one read from the pty and
arrives intact, but when several are in flight they can arrive in any order (seen
from 500 lines of `seq` up; intermittent; the UI terminal has the same bug), and
"close" can overtake them too. So by default the command's output is cut into
chunks in the container (dd, one pipe read each), each gzipped and hex-encoded
(od) and sent by awk as records, one in flight at a time: awk waits for our ack
on /dev/tty before sending the next. A record split over several pty reads is put
back together (only its pieces can be in flight). The exit status comes in a last
record. Per-chunk compression keeps slow output live (a line a second arrives a
second apart) while bulk output still compresses. Hex also keeps binary output
and \\n intact and stderr in order with stdout. This needs sh, dd, od and awk in
the image (gzip optional); without them, or with stdin (-i, which would share the
tty with the acks), output is passed through as it comes and can be out of order.
"""

import codecs
import itertools
import json
import re
import sys
import time
import zlib

from .client import SSL_CTX, DsmError

ATTACH_OK, ATTACH_IN_USE, ATTACH_FAILED = 1503, 1504, 1505
CLOSE_TAKEN_OVER, CLOSE_ENDED = 1500, 1501
EXEC_ID = re.compile(r"[0-9a-f]{64}")
RECORD_BYTES = 1024  # 2 KiB of hex: one pty read in practice, so rarely split
STALL_SECONDS = 10  # half a record and nothing more for this long: give up

# Records: \x1e<kind> <seq> <n> <hex>\n. D = n bytes of output, E = end of output
# (seq = number of D records), S = exit status n, Z = each chunk that follows is
# its own gzip member (sent first, when the image has gzip and it wasn't turned off).
RECORD = re.compile(r"\x1e([DESZ]) (\d+) (\d+) ([0-9a-f]*)\r?\n")
# A "." line from the chunk loop ends a chunk: send what's held, however little.
AWK = r"""
function ack() { fflush(); if ((getline line < "/dev/tty") <= 0) exit 1 }
function emit(n) { printf "\036D %d %d %s\n", seq++, n, substr(buf, 1, 2 * n); buf = substr(buf, 2 * n + 1); ack() }
$0 == "." { if (buf != "") emit(length(buf) / 2); next }
{ gsub(/[^0-9a-f]/, ""); buf = buf $0; while (length(buf) >= 2 * B) emit(B) }
END { if (buf != "") emit(length(buf) / 2); printf "\036E %d 0 \n", seq; ack() }
"""
# The output is cut into chunks by dd, which takes what the pipe holds right now
# (one read, up to 64 KiB), and each chunk goes through its own gzip and od, so
# nothing waits on a buffer: a line a second arrives a second apart, and bulk output
# still compresses in 64 KiB pieces. mawk reads pipes in blocks unless -W interactive;
# busybox awk ignores that with a warning, so it's only passed when awk is silent.
# The tools' stderr stays off the tty: one stray line there jams the framing.
FRAMED = f"""stty -echo -opost 2>/dev/null
if ! command -v od >/dev/null || ! command -v awk >/dev/null || ! command -v dd >/dev/null; then
  echo "synodock exec: the container has no od, awk or dd (use --raw)"; exit 127; fi
case $(printf x | LC_ALL=C dd bs=1 count=1 2>&1 >/dev/null) in *"records in"*) ;; *)
  echo "synodock exec: this dd doesn't report what it read, so the end of output can't be seen (use --raw)"; exit 127;; esac
Z=cat
if [ "$SD_GZIP" = 1 ] && command -v gzip >/dev/null; then Z='gzip -1'; printf '\\036Z 0 1 \\n'; read -r line </dev/tty; fi
AI=
if [ -z "$(awk -W interactive 'BEGIN {{}}' </dev/null 2>&1)" ]; then AI='-W interactive'; fi
exec 4>&1
s=$( {{ {{ "$@" </dev/null 2>&1 3>&- 4>&-; echo $? >&3; }} | {{
       while :; do
         st=$( {{ LC_ALL=C dd bs=65536 count=1 2>&3 | $Z 2>/dev/null | od -An -v -tx1 2>/dev/null >&5; }} 3>&1 )
         echo .
         case $st in "0+0 records in"*) break;; esac
       done 5>&1
     }} | awk $AI -v B={RECORD_BYTES} '{AWK.strip()}' 2>/dev/null >&4; }} 3>&1 )
printf '\\036S 0 %d \\n' "$s"
read -r line </dev/tty"""

# -d: start the command in the background, out of the exec's session (setsid where
# the image has it, and SIGHUP ignored either way, so the exec ending doesn't take
# it down), output discarded, as docker exec -d does.
DETACH = """command -v "$1" >/dev/null 2>&1 || { echo "synodock exec: $1: not found"; exit 127; }
trap '' HUP
if command -v setsid >/dev/null 2>&1; then setsid "$@" </dev/null >/dev/null 2>&1 &
else "$@" </dev/null >/dev/null 2>&1 &
fi"""

# Unframed: the exit status is a marker after the output (it can be overtaken too).
MARKER = "\x1e__dsm_exit="
READY = "\x1e__dsm_ready\n"
MARKER_RE = re.compile(re.escape(MARKER) + r"(\d+)\r?\n?")


def join(argv):
    """shlex.join, but quoting every argument: shlex leaves a=b bare, and DSM
    drops everything from a bare "=" on."""
    return " ".join("'" + a.replace("'", "'\"'\"'") + "'" for a in argv)


def _printf(text):
    return "printf '" + text.replace("\x1e", "\\036").replace("\n", "\\n") + "'"


def wrap(argv, interactive, stdin=False):
    """Unframed: argv under sh, ending with the exit-code marker. Without a TTY on
    our side, turn off echo (piped stdin would come back in the output) and output
    post-processing (\\n -> \\r\\n); with stdin, say when that's done, so input isn't
    sent (and echoed) before."""
    setup = ""
    if not interactive:
        setup = "stty -echo -opost 2>/dev/null; " + (_printf(READY) + "; " if stdin else "")
    script = f"{setup}\"$@\"; {_printf(MARKER + '%d' + chr(10))} $?"
    return join(["sh", "-c", script, "sh", *argv])


class ExitFilter:
    """Unframed: pass output through, holding back anything that could be the
    start of the marker, and pull the exit code out once it arrives."""

    def __init__(self):
        self.pending = ""
        self.exit_code = None

    def feed(self, text):
        if self.exit_code is not None:
            return text  # out of order behind the marker; still output
        buf = self.pending + text
        m = MARKER_RE.search(buf)
        if m and (m.end() < len(buf) or buf.endswith("\n")):
            self.exit_code = int(m.group(1))
            self.pending = ""
            return buf[:m.start()] + buf[m.end():]
        i = buf.find(MARKER)
        if i < 0:
            i = len(buf)
            for k in range(min(len(MARKER) - 1, len(buf)), 0, -1):
                if MARKER.startswith(buf[-k:]):
                    i = len(buf) - k
                    break
        self.pending = buf[i:]
        return buf[:i]

    def flush(self):
        out, self.pending = self.pending, ""
        m = MARKER_RE.search(out)
        if m and self.exit_code is None:
            self.exit_code = int(m.group(1))
            return out[:m.start()] + out[m.end():]
        return out


class Deframer:
    """Framed: turn pty reads back into records. Only one record is ever in
    flight, so the pieces on hand all belong to it, in some order."""

    MAX_PIECES = 4

    def __init__(self):
        self.pieces = []
        self.seq = 0
        self.status = None

    def feed(self, text):
        """Returns the record's (kind, value) once complete, else None."""
        self.pieces.append(text)
        if len(self.pieces) > self.MAX_PIECES:
            raise DsmError("exec output didn't decode (not from the framing script?): "
                                  + repr("".join(self.pieces)[:300]))
        for order in itertools.permutations(self.pieces):
            m = RECORD.fullmatch("".join(order))
            if m:
                break
        else:
            return None
        self.pieces = []
        kind, seq, n, hexdata = m.group(1), int(m.group(2)), int(m.group(3)), m.group(4)
        if kind == "S":
            self.status = n
            return kind, n
        if kind == "Z":
            return kind, None
        if seq != self.seq:
            raise DsmError(f"exec output record {seq} arrived, expected {self.seq}")
        if kind == "E":
            return kind, None
        if len(hexdata) != 2 * n:
            raise DsmError(f"exec output record {seq} has {len(hexdata) // 2} bytes, said {n}")
        self.seq += 1
        return kind, bytes.fromhex(hexdata)

    def leftover(self):
        return "".join(self.pieces)


class Session:
    """One exec in one container. Use as a context manager: on the way out, an exec
    that hasn't ended is deleted."""

    def __init__(self, dsm, container, command, rows=24, cols=200):
        from websockets.sync.client import connect  # only exec needs it
        self._connect = connect
        self.dsm = dsm
        self.container = container
        self.command = command
        self.size = (rows, cols)
        self.tty_id = None
        self.ended = False
        self.close_code = None
        self.monitor = self.term = None

    def _socket(self, role):
        url = re.sub(r"^http", "ws", self.dsm.url) + "/docker/ws"
        ws = self._connect(url, ssl=SSL_CTX, open_timeout=15, max_size=None,
                           additional_headers={"Cookie": f"id={self.dsm.sid}", "Origin": self.dsm.url})
        ws.send(json.dumps(["enter", self.container, role]))
        if not self._wait(ws, "enter")[0]:
            ws.close()
            raise DsmError(f"can't open a terminal in {self.container} (not running, or no such container)")
        return ws

    @staticmethod
    def _wait(ws, event, timeout=15):
        while True:
            msg = json.loads(ws.recv(timeout=timeout))
            if msg[0] == event:
                return msg[1:]

    def __enter__(self):
        try:
            self.monitor = self._socket("monitor")
            before = set(self._wait(self.monitor, "list")[0][0::2])
            self.monitor.send(json.dumps(["create", self.command]))
            if not self._wait(self.monitor, "create")[0]:
                raise DsmError(f"Container Manager refused to create an exec in {self.container}")
            while self.tty_id is None:
                new = set(self._wait(self.monitor, "list")[0][0::2]) - before
                if len(new) > 1:
                    raise DsmError(f"several new terminals appeared in {self.container} at once; "
                                          "not guessing which is ours")
                if new:
                    tty_id = new.pop()
                    if not EXEC_ID.fullmatch(tty_id):  # e.g. the container's own console
                        raise DsmError(f"unexpected terminal id {tty_id!r}")
                    self.tty_id = tty_id
            self.term = self._socket("terminal")
            self.term.send(json.dumps(["attach", self.tty_id, False]))
            code = self._wait(self.term, "attach")[0]
            if code != ATTACH_OK:
                raise DsmError(f"attach to the new exec in {self.container} failed ({code})")
            self.resize(*self.size)
            return self
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def __exit__(self, *exc):
        if self.term is not None:
            self.term.close()
        if self.monitor is not None:
            if self.tty_id and not self.ended:
                try:
                    self.monitor.send(json.dumps(["delete", self.tty_id]))
                except Exception:
                    pass
            self.monitor.close()

    def send(self, text):
        self.term.send(json.dumps(["data", text]))

    def resize(self, rows, cols):
        self.size = (rows, cols)
        self.term.send(json.dumps(["resize", rows, cols]))

    def output(self, poll=None):
        """Yield output text until the exec ends; with poll, also None after poll
        seconds without any."""
        from websockets.exceptions import ConnectionClosed
        try:
            while True:
                try:
                    raw = self.term.recv(timeout=poll)
                except TimeoutError:
                    yield None
                    continue
                msg = json.loads(raw)
                if msg[0] == "data":
                    yield msg[1]
                elif msg[0] == "close":
                    self.close_code = msg[1]
                    self.ended = msg[1] == CLOSE_ENDED
                    return
        except ConnectionClosed:
            return

    def check_close(self):
        if self.close_code == CLOSE_TAKEN_OVER:
            raise DsmError("someone else attached to this exec (Container Manager's Terminal tab?)")
        if self.close_code not in (CLOSE_ENDED, None):
            raise DsmError(f"terminal closed with code {self.close_code}")


# --- running a command: shared by `synodock exec` and docker_api's exec ----

def command_for(argv, framed=True, interactive=False, stdin=False, raw=False, detach=False, gzip=False):
    if detach:
        argv = ["sh", "-c", DETACH, "sh", *argv]
    if raw:
        return join(argv)
    if framed:
        # env, not an sh variable: the framing script reads SD_GZIP; "$@" is argv.
        return join(["env", f"SD_GZIP={int(gzip)}", "sh", "-c", FRAMED, "sh", *argv])
    return wrap(argv, interactive, stdin)


class Gunzip:
    """gzip members back to back (one per chunk) -> the bytes they hold."""

    def __init__(self):
        self.member = zlib.decompressobj(wbits=31)
        self.partial = False  # a member has begun and not ended

    def feed(self, data):
        out = b""
        while data:
            out += self.member.decompress(data)
            data = self.member.unused_data
            self.partial = not self.member.eof
            if self.member.eof:
                self.member = zlib.decompressobj(wbits=31)
        return out

    def complete(self):
        return not self.partial


def pump_framed(session, write):
    """Framed mode: write(bytes) gets the output in order; returns the exit status."""
    deframer = Deframer()
    gunzip = None
    ended = False
    last = time.monotonic()
    try:
        for text in session.output(poll=STALL_SECONDS):
            if text is None:
                # Quiet is fine (the command may sleep), but not halfway through a
                # record: something that isn't a record got in, and nothing more is
                # coming until we ack, so both ends would wait forever.
                if deframer.pieces and time.monotonic() - last >= STALL_SECONDS:
                    raise DsmError(f"{session.container}: exec output stopped mid-record: "
                                          + repr(deframer.leftover()[:300]))
                continue
            last = time.monotonic()
            record = deframer.feed(text)
            if record is None:
                continue
            kind, value = record
            if kind == "Z":
                gunzip = Gunzip()
            elif kind == "D":
                write(gunzip.feed(value) if gunzip else value)
            elif kind == "E":
                ended = True
                if gunzip and not gunzip.complete():
                    raise DsmError(f"{session.container}: exec output ended mid-gzip stream")
            elif kind == "S" and not ended:
                raise DsmError(f"{session.container}: the framing in the container failed "
                                      f"(output incomplete); exit status was {value}")
            session.send("\n")
    except KeyboardInterrupt:
        session.send("\x03")  # stop the remote command too; __exit__ deletes the exec
        raise
    session.check_close()
    if deframer.status is None:
        left = deframer.leftover().strip()
        raise DsmError(f"{session.container}: exec ended without an exit status"
                              + (f": {left}" if left else ""))
    return deframer.status


def pump_unframed(session, write, stdin_chunks=None, wait_ready=False, send_eof=True, raw=False):
    """Unframed mode: write(text) gets output as it comes. stdin_chunks (text) are
    sent from a thread, after wrap()'s ready line if wait_ready, then ^D if
    send_eof. Returns the exit code, or None if it can't be known."""
    import threading

    filt = None if raw else ExitFilter()
    ready = threading.Event()
    if not wait_ready:
        ready.set()
    head = ""

    def feed():
        last = "\n"
        try:
            ready.wait()
            for text in stdin_chunks:
                session.send(text)
                last = text[-1]
            if send_eof:
                # Canonical mode: ^D is EOF at the start of a line; mid-line it only
                # hands over the partial line, so that needs a second one.
                session.send("\x04" if last == "\n" else "\x04\x04")
        except Exception:
            pass

    if stdin_chunks is not None:
        threading.Thread(target=feed, daemon=True).start()
    try:
        for text in session.output():
            if not ready.is_set():
                head += text
                if READY not in head:
                    continue
                text = head.replace(READY, "", 1)
                ready.set()
            write(filt.feed(text) if filt else text)
    except KeyboardInterrupt:
        session.send("\x03")
        raise
    if not ready.is_set():  # ended before saying ready (no sh or no stty?)
        write(filt.feed(head) if filt else head)
    if filt:
        write(filt.flush())
    session.check_close()
    return filt.exit_code if filt else None


def _text_chunks(read):
    """read() -> bytes ('' at EOF) as UTF-8 text chunks."""
    decoder = codecs.getincrementaldecoder("utf-8")("replace")
    while data := read():
        if text := decoder.decode(data):
            yield text


def gzip_default():
    """Compress framed output? On unless SYNODOCK_GZIP=0. Each chunk is its own gzip
    member, so it costs no latency; it costs ~0.2s on 256 KB that won't compress."""
    import os
    return os.environ.get("SYNODOCK_GZIP", "1") != "0"


def run(dsm, container, argv, stdin=False, tty=False, raw=False, detach=False, gzip=None):
    """`synodock exec`: returns the command's exit code (0 once started, with detach)."""
    import os
    import shutil

    cols, rows = shutil.get_terminal_size((200, 50))
    if detach or not (stdin or tty or raw):
        out = sys.stdout.buffer
        command = command_for(argv, raw=raw, detach=detach, gzip=gzip_default() if gzip is None else gzip)
        with Session(dsm, container, command, rows, cols) as session:
            write = lambda b: (out.write(b), out.flush())
            if raw:  # --raw -d: no framing, so no exit status either
                pump_unframed(session, lambda t: write(t.encode()), raw=True)
                return 0
            return pump_framed(session, write)

    interactive = tty and sys.stdin.isatty()
    stdin = stdin or interactive
    if not interactive:
        print("synodock exec: unframed mode, output may arrive out of order", file=sys.stderr)
    command = command_for(argv, framed=False, interactive=interactive, stdin=stdin, raw=raw)
    chunks = _text_chunks(lambda: os.read(sys.stdin.fileno(), 4096)) if stdin else None
    with Session(dsm, container, command, rows, cols) as session:
        restore = None
        if interactive:
            import signal
            import termios
            import tty as ttylib
            fd = sys.stdin.fileno()
            saved = termios.tcgetattr(fd)
            ttylib.setraw(fd)
            restore = lambda: termios.tcsetattr(fd, termios.TCSADRAIN, saved)
            signal.signal(signal.SIGWINCH, lambda *_: session.resize(*reversed(shutil.get_terminal_size())))
        try:
            code = pump_unframed(session, lambda t: (sys.stdout.write(t), sys.stdout.flush()), chunks,
                                 wait_ready=stdin and not interactive and not raw,
                                 send_eof=not interactive, raw=raw)
        finally:
            if restore:
                restore()
    if raw:
        return 0
    if code is None:
        print(f"synodock exec: {container}: no exit status (no sh in the image? try --raw)", file=sys.stderr)
        return 255
    return code
