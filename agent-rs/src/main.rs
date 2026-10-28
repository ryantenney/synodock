//! synodock-agent runs a command inside a container for synodock exec, in place of the
//! shell framing script, when it's in the container at /opt/synodock/agent. This is the
//! Rust port of the C agent (agent/agent.c, removed after 676a932): the same protocol and
//! behaviour, no_std, calling libc.
//!
//! Author: Ryan Tenney
//!
//! It speaks package wire's protocol (internal/wire) over its tty, through the NAS
//! relay: a hello, synodock's go-ahead, then records
//!
//! ```text
//! \x1e<kind> <seq> <stream> <base64 length> <base64 payload> <crc32>\n
//! ```
//!
//! with up to WINDOW unacknowledged ("A<n>" lines from synodock). D records carry output
//! (stream 1 stdout, 2 stderr); with SD_GZIP=1 and a go-ahead that says "z", C records
//! carry each stream as one gzip stream instead, flushed after every read so slow output
//! stays live. X carries the exit status. A ^C byte on the tty interrupts the command.
//!
//! ```text
//! synodock-agent run COMMAND [ARG]...
//! ```
//!
//! One thread and a poll loop: read the tty for acks, and the command's pipes while the
//! window has room. Build it static with musl (agent-rs/build.sh). Without the gzip
//! feature it needs nothing but libc and never compresses (it sends D records whatever
//! SD_GZIP says).
#![no_std]
#![no_main]

use core::mem::MaybeUninit;
use core::ptr;
use libc::{c_char, c_int, pid_t};

const WINDOW: i32 = 32; // records in flight: about 32 KiB
const MAX_PAYLOAD: usize = 768; // wire.MaxPayload
#[cfg(feature = "gzip")]
const ZREAD: usize = 16 << 10; // a read to compress: a few records once compressed
const EXIT_WAIT_MS: i64 = 30000; // how long the exit status may take to be acknowledged
const HELLO: &[u8] = b"\x1eV 0 2 \n";
const USAGE: &[u8] = b"usage: synodock-agent run COMMAND [ARG]...\n";

/// Output waiting for room in the window holds one read's worth, raw or compressed;
/// deflate output past this much is sent before the rest is made.
#[cfg(feature = "gzip")]
const PEND_CAP: usize = ZREAD + 1024;
#[cfg(not(feature = "gzip"))]
const PEND_CAP: usize = MAX_PAYLOAD;

/// A record's line: header, base64 payload, CRC.
const LINE_CAP: usize = 64 + MAX_PAYLOAD.div_ceil(3) * 4 + 16;

#[panic_handler]
fn panic(_: &core::panic::PanicInfo) -> ! {
    exit(1)
}

#[cfg(feature = "gzip")]
mod malloc {
    use core::alloc::{GlobalAlloc, Layout};

    /// Malloc is libc's allocator, for miniz_oxide's compressor state.
    struct Malloc;

    unsafe impl GlobalAlloc for Malloc {
        unsafe fn alloc(&self, l: Layout) -> *mut u8 {
            if l.align() <= 16 {
                return libc::malloc(l.size()).cast();
            }
            let mut p = core::ptr::null_mut();
            if libc::posix_memalign(&mut p, l.align(), l.size()) != 0 {
                return core::ptr::null_mut();
            }
            p.cast()
        }
        unsafe fn dealloc(&self, p: *mut u8, _: Layout) {
            libc::free(p.cast())
        }
    }

    #[global_allocator]
    static A: Malloc = Malloc;
}

fn exit(code: c_int) -> ! {
    // SAFETY: exit runs the atexit handlers (restore_tty) and doesn't return.
    unsafe { libc::exit(code) }
}

fn errno() -> c_int {
    // SAFETY: musl's errno location is always valid for this thread.
    unsafe { *libc::__errno_location() }
}

/// read is read(2) into buf.
fn read(fd: c_int, buf: &mut [u8]) -> isize {
    // SAFETY: buf is valid for buf.len() bytes.
    unsafe { libc::read(fd, buf.as_mut_ptr().cast(), buf.len()) }
}

/// write_all writes everything: a write to a tty can come back short.
fn write_all(fd: c_int, mut p: &[u8]) {
    while !p.is_empty() {
        // SAFETY: p is valid for p.len() bytes.
        let w = unsafe { libc::write(fd, p.as_ptr().cast(), p.len()) };
        if w < 0 {
            let e = errno();
            if e == libc::EINTR || e == libc::EAGAIN {
                continue;
            }
            exit(1);
        }
        p = &p[w as usize..];
    }
}

/// now_ms is CLOCK_MONOTONIC in milliseconds.
fn now_ms() -> i64 {
    let mut t = libc::timespec { tv_sec: 0, tv_nsec: 0 };
    // SAFETY: t is a valid timespec.
    unsafe { libc::clock_gettime(libc::CLOCK_MONOTONIC, &mut t) };
    t.tv_sec * 1000 + t.tv_nsec / 1_000_000 // both i64 on the 64-bit targets
}

/// cstr is a C string's bytes, without the NUL.
fn cstr<'a>(p: *const c_char) -> &'a [u8] {
    // SAFETY: callers pass NUL-terminated strings that outlive the slice (argv, environ,
    // getenv's and strerror's results).
    unsafe { core::slice::from_raw_parts(p.cast(), libc::strlen(p)) }
}

/// Out appends to a fixed buffer, dropping what doesn't fit.
struct Out<'a> {
    b: &'a mut [u8],
    n: usize,
}

impl Out<'_> {
    fn put(&mut self, s: &[u8]) {
        let k = s.len().min(self.b.len() - self.n);
        self.b[self.n..self.n + k].copy_from_slice(&s[..k]);
        self.n += k;
    }
    fn byte(&mut self, c: u8) {
        self.put(&[c]);
    }
    fn int(&mut self, v: i64) {
        if v < 0 {
            self.byte(b'-');
        }
        let mut d = [0u8; 20];
        let mut i = d.len();
        let mut v = v.unsigned_abs();
        loop {
            i -= 1;
            d[i] = b'0' + (v % 10) as u8;
            v /= 10;
            if v == 0 {
                break;
            }
        }
        self.put(&d[i..]);
    }
}

const B64: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

fn base64(o: &mut Out, src: &[u8]) {
    for c in src.chunks(3) {
        let at = |i: usize| c.get(i).copied().unwrap_or(0) as usize;
        let v = at(0) << 16 | at(1) << 8 | at(2);
        o.byte(B64[v >> 18 & 63]);
        o.byte(B64[v >> 12 & 63]);
        o.byte(if c.len() > 1 { B64[v >> 6 & 63] } else { b'=' });
        o.byte(if c.len() > 2 { B64[v & 63] } else { b'=' });
    }
}

/// Crc is CRC-32/IEEE, as Go's hash/crc32 and zlib compute it.
struct Crc([u32; 256]);

impl Crc {
    fn new() -> Self {
        let mut t = [0u32; 256];
        for (i, e) in t.iter_mut().enumerate() {
            let mut c = i as u32;
            for _ in 0..8 {
                c = if c & 1 != 0 { 0xedb88320 ^ (c >> 1) } else { c >> 1 };
            }
            *e = c;
        }
        Crc(t)
    }
    fn update(&self, crc: u32, data: &[u8]) -> u32 {
        !data.iter().fold(!crc, |c, &b| self.0[((c ^ b as u32) & 0xff) as usize] ^ (c >> 8))
    }
}

/// Tx writes records to the tty.
struct Tx {
    seq: i32, // the next record's
    crc: Crc,
    line: [u8; LINE_CAP],
}

impl Tx {
    /// send writes one record, in one write; the caller keeps to the window.
    fn send(&mut self, kind: u8, stream: usize, data: &[u8]) {
        let mut o = Out { b: &mut self.line, n: 0 };
        o.put(&[0x1e, kind, b' ']);
        o.int(self.seq as i64);
        o.byte(b' ');
        o.int(stream as i64);
        o.byte(b' ');
        let h = o.n;
        o.int(data.len().div_ceil(3) as i64 * 4);
        o.byte(b' ');
        let pre = o.n;
        base64(&mut o, data);
        // the CRC is over "<kind> <seq> <stream> <base64>"
        let crc = self.crc.update(self.crc.update(0, &o.b[1..h]), &o.b[pre..o.n]);
        o.byte(b' ');
        for i in (0..8).rev() {
            o.byte(b"0123456789abcdef"[(crc >> (i * 4)) as usize & 15]);
        }
        o.byte(b'\n');
        let n = o.n;
        self.seq += 1;
        write_all(1, &self.line[..n]);
    }
}

/// Gz is one output stream's gzip stream, started at its first output.
#[cfg(feature = "gzip")]
struct Gz {
    z: miniz_oxide::deflate::core::CompressorOxide,
    crc: u32,
    size: u32,
}

struct Agent {
    tx: Tx,
    acked: i32,     // every record below this has arrived
    tty_open: bool, // synodock is still there
    child: pid_t,
    line: [u8; 32], // a tty line in progress
    line_len: usize,
    pend: [u8; PEND_CAP], // output waiting for room in the window
    pend_len: usize,
    pend_off: usize,
    pend_kind: u8,
    pend_stream: usize,
    #[cfg(feature = "gzip")]
    gz: [Option<Gz>; 3],
}

impl Agent {
    /// on_tty takes synodock's "A<n>" lines; a ^C byte interrupts the command.
    fn on_tty(&mut self, buf: &[u8]) {
        for &c in buf {
            if c == 3 {
                if self.child > 0 {
                    // SAFETY: plain syscall.
                    unsafe { libc::kill(self.child, libc::SIGINT) };
                }
                continue;
            }
            if c != b'\n' && c != b'\r' {
                if self.line_len < self.line.len() - 1 {
                    self.line[self.line_len] = c;
                    self.line_len += 1;
                }
                continue;
            }
            let l = &self.line[..self.line_len];
            if l.len() > 1 && l[0] == b'A' && l[1..].iter().all(u8::is_ascii_digit) {
                let v = l[1..].iter().fold(0i32, |v, &d| v.saturating_mul(10).saturating_add((d - b'0') as i32));
                self.acked = self.acked.max(v);
            }
            self.line_len = 0;
        }
    }

    /// tty_read takes what's on the tty, once poll has said there's something.
    fn tty_read(&mut self) {
        let mut buf = [0u8; 256];
        let n = read(0, &mut buf);
        if n > 0 {
            self.on_tty(&buf[..n as usize]);
        } else if n == 0 || (errno() != libc::EINTR && errno() != libc::EAGAIN) {
            self.tty_open = false;
        }
    }

    /// read_tty waits up to timeout ms (-1: no limit) for the tty and takes what's there.
    fn read_tty(&mut self, timeout: c_int) {
        if !self.tty_open {
            return;
        }
        let mut p = libc::pollfd { fd: 0, events: libc::POLLIN, revents: 0 };
        // SAFETY: p is one valid pollfd.
        if unsafe { libc::poll(&mut p, 1, timeout) } > 0 {
            self.tty_read();
        }
    }

    /// room is whether the window takes another record.
    fn room(&self) -> bool {
        self.tx.seq - self.acked < WINDOW
    }

    /// flush sends what's waiting while the window has room; true when nothing is left.
    fn flush(&mut self) -> bool {
        while self.pend_off < self.pend_len && self.room() {
            let n = (self.pend_len - self.pend_off).min(MAX_PAYLOAD);
            self.tx.send(self.pend_kind, self.pend_stream, &self.pend[self.pend_off..self.pend_off + n]);
            self.pend_off += n;
        }
        if self.pend_off < self.pend_len {
            return false;
        }
        self.pend_len = 0;
        self.pend_off = 0;
        true
    }

    /// drain sends everything waiting, waiting for acks as needed.
    fn drain(&mut self) {
        while !self.flush() {
            if !self.tty_open {
                exit(1); // synodock is gone; nothing more will be acknowledged
            }
            self.read_tty(-1);
        }
    }

    /// pend_put adds to what's waiting, sending it first if there isn't room.
    #[cfg(feature = "gzip")]
    fn pend_put(&mut self, b: &[u8]) {
        if self.pend_len + b.len() > PEND_CAP {
            self.drain();
        }
        self.pend[self.pend_len..self.pend_len + b.len()].copy_from_slice(b);
        self.pend_len += b.len();
    }

    /// deflate adds data to stream's gzip stream, onto what's waiting: level 1, as gzip -1,
    /// with a sync flush, so the reader gets everything so far; or with finish, ends the
    /// stream (its trailer's CRC checks the whole of it).
    #[cfg(feature = "gzip")]
    fn deflate(&mut self, stream: usize, data: &[u8], finish: bool) {
        use miniz_oxide::deflate::core::{
            compress, create_comp_flags_from_zip_params, CompressorOxide, TDEFLFlush, TDEFLStatus,
        };
        self.pend_kind = b'C';
        self.pend_stream = stream;
        if self.gz[stream].is_none() {
            // -15: raw deflate; the gzip header and trailer are ours, as zlib writes them
            let z = CompressorOxide::new(create_comp_flags_from_zip_params(1, -15, 0));
            self.gz[stream] = Some(Gz { z, crc: 0, size: 0 });
            self.pend_put(&[0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 4, 3]);
        }
        let Some(gz) = &mut self.gz[stream] else { exit(1) };
        gz.crc = self.tx.crc.update(gz.crc, data);
        gz.size = gz.size.wrapping_add(data.len() as u32);
        let flush = if finish { TDEFLFlush::Finish } else { TDEFLFlush::Sync };
        let mut off = 0;
        loop {
            let Some(gz) = &mut self.gz[stream] else { exit(1) };
            let (st, used, made) = compress(&mut gz.z, &data[off..], &mut self.pend[self.pend_len..], flush);
            off += used;
            self.pend_len += made;
            if (st as i32) < 0 {
                exit(1);
            }
            let full = self.pend_len == PEND_CAP;
            if if finish { st == TDEFLStatus::Done } else { off == data.len() && !full } {
                break;
            }
            if full {
                self.drain();
            }
        }
        if finish {
            let Some(gz) = self.gz[stream].take() else { exit(1) };
            let mut t = [0u8; 8];
            t[..4].copy_from_slice(&gz.crc.to_le_bytes());
            t[4..].copy_from_slice(&gz.size.to_le_bytes());
            self.pend_put(&t);
        }
    }

    /// finish sends the exit status and waits (a while) for it to arrive, so the tty
    /// isn't closed under it.
    fn finish(&mut self, code: i32) -> c_int {
        let mut s = [0u8; 16];
        let mut o = Out { b: &mut s, n: 0 };
        o.int(code as i64);
        let n = o.n;
        while !self.room() && self.tty_open {
            self.read_tty(-1);
        }
        self.tx.send(b'X', 0, &s[..n]);
        let start = now_ms();
        while self.acked < self.tx.seq && self.tty_open {
            let ms = now_ms() - start;
            if ms >= EXIT_WAIT_MS {
                break;
            }
            self.read_tty((EXIT_WAIT_MS - ms) as c_int);
        }
        0
    }

    /// status is the command's exit status as a shell reports it.
    fn status(&self) -> i32 {
        let mut ws = 0;
        // SAFETY: ws is a valid int.
        while unsafe { libc::waitpid(self.child, &mut ws, 0) } < 0 {
            if errno() != libc::EINTR {
                return 126;
            }
        }
        if libc::WIFSIGNALED(ws) {
            return 128 + libc::WTERMSIG(ws);
        }
        libc::WEXITSTATUS(ws)
    }
}

/// SAVED_TTY is the tty as it was, for restore_tty at exit.
static mut SAVED_TTY: MaybeUninit<libc::termios> = MaybeUninit::uninit();

extern "C" fn restore_tty() {
    // SAFETY: registered only once SAVED_TTY was filled in; one thread.
    unsafe { libc::tcsetattr(0, libc::TCSANOW, (*ptr::addr_of!(SAVED_TTY)).as_ptr()) };
}

/// raw_tty puts fd 0 in raw mode, if it's a tty: no echo, no output processing, ^C a
/// byte rather than a signal to us. It's restored at exit.
fn raw_tty() {
    // SAFETY: SAVED_TTY is only touched here and in restore_tty; one thread.
    unsafe {
        let saved = (*ptr::addr_of_mut!(SAVED_TTY)).as_mut_ptr();
        if libc::tcgetattr(0, saved) != 0 {
            return;
        }
        let mut raw = *saved;
        libc::cfmakeraw(&mut raw);
        if libc::tcsetattr(0, libc::TCSANOW, &raw) == 0 {
            libc::atexit(restore_tty);
        }
    }
}

/// read_line reads synodock's go-ahead, a byte at a time so nothing after it is taken.
fn read_line(buf: &mut [u8; 32]) -> Option<usize> {
    let mut len = 0;
    loop {
        let mut c = [0u8];
        let n = read(0, &mut c);
        if n < 0 && errno() == libc::EINTR {
            continue;
        }
        if n <= 0 {
            return None;
        }
        if c[0] == b'\n' || c[0] == b'\r' {
            return Some(len);
        }
        if len < buf.len() - 1 {
            buf[len] = c[0];
            len += 1;
        }
    }
}

// Without std, nothing else asks for libc (static, as musl targets link).
#[link(name = "c")]
extern "C" {
    static environ: *const *mut c_char;
}

/// command_env is ours without the framing script's switches.
fn command_env() -> *const *mut c_char {
    // SAFETY: environ is a NULL-terminated array of C strings; env has room for all of
    // them and the NULL (calloc zeroes it).
    unsafe {
        let mut n = 0;
        while !(*environ.add(n)).is_null() {
            n += 1;
        }
        let env: *mut *mut c_char = libc::calloc(n + 1, size_of::<*mut c_char>()).cast();
        if env.is_null() {
            exit(1);
        }
        let mut j = 0;
        for i in 0..n {
            let e = *environ.add(i);
            let s = cstr(e);
            if !s.starts_with(b"SD_GZIP=") && !s.starts_with(b"SD_AGENT=") {
                *env.add(j) = e;
                j += 1;
            }
        }
        env
    }
}

/// spawn starts the command (argv, NULL-terminated): stdin /dev/null, stdout and stderr
/// to pipes[1] and pipes[2], no signals blocked, and the ones a shell would reset at
/// their defaults. It returns posix_spawnp's error.
fn spawn(argv: *const *const c_char, child: &mut pid_t, pipes: &mut [c_int; 3]) -> c_int {
    let mut fds = [[-1 as c_int; 2]; 3];
    // SAFETY: libc calls on live locals; argv and the env are NULL-terminated arrays of
    // C strings.
    unsafe {
        for f in &mut fds[1..] {
            if libc::pipe2(f.as_mut_ptr(), libc::O_CLOEXEC) < 0 {
                return errno();
            }
        }
        let mut fa = MaybeUninit::<libc::posix_spawn_file_actions_t>::uninit();
        let mut attr = MaybeUninit::<libc::posix_spawnattr_t>::uninit();
        let (fa, attr) = (fa.as_mut_ptr(), attr.as_mut_ptr());
        libc::posix_spawn_file_actions_init(fa);
        libc::posix_spawn_file_actions_addopen(fa, 0, c"/dev/null".as_ptr(), libc::O_RDONLY, 0);
        libc::posix_spawn_file_actions_adddup2(fa, fds[1][1], 1);
        libc::posix_spawn_file_actions_adddup2(fa, fds[2][1], 2);
        libc::posix_spawnattr_init(attr);
        let mut none = MaybeUninit::<libc::sigset_t>::uninit();
        let mut def = MaybeUninit::<libc::sigset_t>::uninit();
        libc::sigemptyset(none.as_mut_ptr());
        libc::sigemptyset(def.as_mut_ptr());
        for s in [libc::SIGINT, libc::SIGQUIT, libc::SIGPIPE, libc::SIGTERM] {
            libc::sigaddset(def.as_mut_ptr(), s);
        }
        libc::posix_spawnattr_setsigmask(attr, none.as_ptr());
        libc::posix_spawnattr_setsigdefault(attr, def.as_ptr());
        libc::posix_spawnattr_setflags(attr, (libc::POSIX_SPAWN_SETSIGMASK | libc::POSIX_SPAWN_SETSIGDEF) as _);
        let err = libc::posix_spawnp(child, *argv, fa, attr, argv.cast(), command_env());
        libc::posix_spawn_file_actions_destroy(fa);
        libc::posix_spawnattr_destroy(attr);
        for s in 1..=2 {
            libc::close(fds[s][1]);
            pipes[s] = fds[s][0];
            if err != 0 {
                libc::close(fds[s][0]);
                pipes[s] = -1;
            }
        }
        if err != 0 {
            *child = -1;
        }
        err
    }
}

/// # Safety
///
/// The C runtime calls it, with argv's argc entries and then a NULL.
#[no_mangle]
pub unsafe extern "C" fn main(argc: c_int, argv: *const *const c_char) -> c_int {
    // SAFETY: argv has argc entries and then a NULL, from the C runtime.
    let arg = |i: usize| unsafe { *argv.add(i) };
    if argc < 3 || cstr(arg(1)) != b"run" {
        write_all(2, USAGE);
        return 2;
    }
    let first = if cstr(arg(2)) == b"--" { 3 } else { 2 };
    if arg(first).is_null() {
        write_all(2, USAGE);
        return 2;
    }
    // SAFETY: plain syscall.
    unsafe { libc::signal(libc::SIGPIPE, libc::SIG_IGN) };
    raw_tty();

    write_all(1, HELLO);
    let mut go = [0u8; 32];
    let Some(go_len) = read_line(&mut go) else { return 1 };
    #[cfg(feature = "gzip")]
    let compress = {
        // SAFETY: getenv's result is NULL or a C string in environ.
        let gz = unsafe { libc::getenv(c"SD_GZIP".as_ptr()) };
        !gz.is_null() && cstr(gz) == b"1" && go[..go_len].contains(&b'z')
    };
    #[cfg(not(feature = "gzip"))]
    let _ = go_len;

    let mut a = Agent {
        tx: Tx { seq: 0, crc: Crc::new(), line: [0; LINE_CAP] },
        acked: 0,
        tty_open: true,
        child: -1,
        line: [0; 32],
        line_len: 0,
        pend: [0; PEND_CAP],
        pend_len: 0,
        pend_off: 0,
        pend_kind: b'D',
        pend_stream: 1,
        #[cfg(feature = "gzip")]
        gz: [None, None, None],
    };

    // SAFETY: argv + first is still NULL-terminated.
    let cmd = unsafe { argv.add(first) };
    let mut pipes = [-1 as c_int; 3];
    let err = spawn(cmd, &mut a.child, &mut pipes);
    if err != 0 {
        let mut msg = [0u8; 511];
        let mut o = Out { b: &mut msg, n: 0 };
        o.put(b"synodock exec: ");
        o.put(cstr(arg(first)));
        o.put(b": ");
        // SAFETY: strerror returns a C string.
        o.put(cstr(unsafe { libc::strerror(err) }));
        o.byte(b'\n');
        let n = o.n;
        a.tx.send(b'D', 2, &msg[..n]);
        return a.finish(if err == libc::ENOENT { 127 } else { 126 });
    }

    #[cfg(feature = "gzip")]
    let mut buf = [0u8; ZREAD];
    while pipes[1] >= 0 || pipes[2] >= 0 {
        if !a.flush() {
            if !a.tty_open {
                return 1;
            }
            a.read_tty(-1);
            continue;
        }
        let mut p = [libc::pollfd { fd: -1, events: libc::POLLIN, revents: 0 }; 3];
        let mut np = 0;
        for fd in [pipes[1], pipes[2], if a.tty_open { 0 } else { -1 }] {
            if fd >= 0 {
                p[np].fd = fd;
                np += 1;
            }
        }
        // SAFETY: p holds np valid pollfds.
        if unsafe { libc::poll(p.as_mut_ptr(), np as libc::nfds_t, -1) } < 0 {
            if errno() == libc::EINTR {
                continue;
            }
            return 1;
        }
        for q in &p[..np] {
            if q.revents == 0 {
                continue;
            }
            if q.fd == 0 {
                a.tty_read();
                continue;
            }
            let s = if q.fd == pipes[1] { 1 } else { 2 };
            if a.pend_len != 0 {
                continue; // the other stream's read came first; this one waits
            }
            // what the pipe holds now
            #[cfg(feature = "gzip")]
            let n = read(pipes[s], if compress { &mut buf[..] } else { &mut a.pend[..MAX_PAYLOAD] });
            #[cfg(not(feature = "gzip"))]
            let n = read(pipes[s], &mut a.pend[..MAX_PAYLOAD]);
            if n < 0 && errno() == libc::EINTR {
                continue;
            }
            if n <= 0 {
                // SAFETY: plain syscall on our pipe.
                unsafe { libc::close(pipes[s]) };
                pipes[s] = -1;
                continue;
            }
            #[cfg(feature = "gzip")]
            if compress {
                a.deflate(s, &buf[..n as usize], false);
                continue;
            }
            a.pend_len = n as usize;
            a.pend_kind = b'D';
            a.pend_stream = s;
        }
    }
    a.drain();
    #[cfg(feature = "gzip")]
    for s in 1..=2 {
        if a.gz[s].is_some() {
            a.deflate(s, &[], true);
            a.drain();
        }
    }
    let code = a.status();
    a.finish(code)
}
