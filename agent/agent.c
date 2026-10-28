/*
 * synodock-agent runs a command inside a container for synodock exec, in place of the
 * shell framing script, when it's in the container at /opt/synodock/agent.
 *
 * Author: Ryan Tenney
 *
 * It speaks package wire's protocol (internal/wire) over its tty, through the NAS
 * relay: a hello, synodock's go-ahead, then records
 *
 *	\x1e<kind> <seq> <stream> <base64 length> <base64 payload> <crc32>\n
 *
 * with up to WINDOW unacknowledged ("A<n>" lines from synodock). D records carry output
 * (stream 1 stdout, 2 stderr); with SD_GZIP=1 and a go-ahead that says "z", C records
 * carry each stream as one gzip stream instead, flushed after every read so slow output
 * stays live. X carries the exit status. A ^C byte on the tty interrupts the command.
 *
 *	synodock-agent run COMMAND [ARG]...
 *
 * One thread and a poll loop: read the tty for acks, and the command's pipes while the
 * window has room. Build static with musl and zlib (agent/build.sh); with -DNO_ZLIB it
 * needs nothing but libc and never compresses (it sends D records whatever SD_GZIP says).
 */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <spawn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <termios.h>
#include <time.h>
#include <unistd.h>
#ifndef NO_ZLIB
#include <zlib.h>
#endif

#define WINDOW 32           /* records in flight: about 32 KiB */
#define MAX_PAYLOAD 768     /* wire.MaxPayload */
#define ZREAD (16 << 10)    /* a read to compress: a few records once compressed */
#define EXIT_WAIT_MS 30000  /* how long the exit status may take to be acknowledged */

static const char hello[] = "\x1eV 0 2 \n";

extern char **environ;

static int seq;          /* the next record's */
static int acked;        /* every record below this has arrived */
static int tty_open = 1; /* synodock is still there */
static pid_t child = -1;

static struct termios saved_tty;
static int tty_saved;

static void restore_tty(void) {
	if (tty_saved)
		tcsetattr(0, TCSANOW, &saved_tty);
}

/* write_all writes everything: a write to a tty can come back short. */
static void write_all(const char *p, size_t n) {
	while (n > 0) {
		ssize_t w = write(1, p, n);
		if (w < 0) {
			if (errno == EINTR || errno == EAGAIN)
				continue;
			exit(1);
		}
		p += w;
		n -= (size_t)w;
	}
}

static const char b64chars[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

static size_t base64(char *dst, const unsigned char *src, size_t n) {
	char *d = dst;
	size_t i = 0;
	for (; i + 2 < n; i += 3) {
		unsigned v = (unsigned)src[i] << 16 | (unsigned)src[i + 1] << 8 | src[i + 2];
		*d++ = b64chars[v >> 18];
		*d++ = b64chars[(v >> 12) & 63];
		*d++ = b64chars[(v >> 6) & 63];
		*d++ = b64chars[v & 63];
	}
	if (i < n) {
		unsigned v = (unsigned)src[i] << 16 | (i + 1 < n ? (unsigned)src[i + 1] << 8 : 0);
		*d++ = b64chars[v >> 18];
		*d++ = b64chars[(v >> 12) & 63];
		*d++ = i + 1 < n ? b64chars[(v >> 6) & 63] : '=';
		*d++ = '=';
	}
	return (size_t)(d - dst);
}

/* crc32 is CRC-32/IEEE, as Go's hash/crc32 and zlib compute it. */
static uint32_t crc_table[256];

static uint32_t crc32_update(uint32_t crc, const void *data, size_t n) {
	if (!crc_table[1])
		for (uint32_t i = 0; i < 256; i++) {
			uint32_t c = i;
			for (int k = 0; k < 8; k++)
				c = c & 1 ? 0xedb88320u ^ (c >> 1) : c >> 1;
			crc_table[i] = c;
		}
	const unsigned char *p = data;
	crc = ~crc;
	while (n--)
		crc = crc_table[(crc ^ *p++) & 0xff] ^ (crc >> 8);
	return ~crc;
}

/* send writes one record, in one write; the caller keeps to the window. */
static void send(char kind, int stream, const unsigned char *data, size_t n) {
	static char line[64 + (MAX_PAYLOAD + 2) / 3 * 4 + 16];
	char sum[48];
	int h = snprintf(sum, sizeof sum, "%c %d %d ", kind, seq, stream);
	int pre = snprintf(line, sizeof line, "\x1e%c %d %d %zu ", kind, seq, stream, (n + 2) / 3 * 4);
	size_t blen = base64(line + pre, data, n);
	/* the CRC is over "<kind> <seq> <stream> <base64>" */
	uint32_t crc = crc32_update(crc32_update(0, sum, (size_t)h), line + pre, blen);
	size_t len = (size_t)pre + blen;
	len += (size_t)snprintf(line + len, sizeof line - len, " %08x\n", (unsigned)crc);
	seq++;
	write_all(line, len);
}

/* on_tty takes synodock's "A<n>" lines; a ^C byte interrupts the command. */
static void on_tty(const char *buf, ssize_t n) {
	static char line[32];
	static size_t len;
	for (ssize_t i = 0; i < n; i++) {
		char c = buf[i];
		if (c == 3) {
			if (child > 0)
				kill(child, SIGINT);
			continue;
		}
		if (c != '\n' && c != '\r') {
			if (len < sizeof line - 1)
				line[len++] = c;
			continue;
		}
		line[len] = 0;
		if (len > 1 && line[0] == 'A') {
			char *end;
			long v = strtol(line + 1, &end, 10);
			if (*end == 0 && v > acked)
				acked = (int)v;
		}
		len = 0;
	}
}

/* read_tty waits up to timeout ms (-1: no limit) for the tty and takes what's there. */
static void read_tty(int timeout) {
	if (!tty_open)
		return;
	struct pollfd p = {.fd = 0, .events = POLLIN};
	int r = poll(&p, 1, timeout);
	if (r <= 0)
		return;
	char buf[256];
	ssize_t n = read(0, buf, sizeof buf);
	if (n > 0)
		on_tty(buf, n);
	else if (n == 0 || (errno != EINTR && errno != EAGAIN))
		tty_open = 0;
}

/* Output waiting for room in the window: one read's worth, raw or compressed. */
static unsigned char *pend;
static size_t pend_len, pend_off, pend_cap;
static char pend_kind;
static int pend_stream;

static void pend_reserve(size_t extra) {
	if (pend_len + extra <= pend_cap)
		return;
	pend_cap = (pend_len + extra) * 2;
	pend = realloc(pend, pend_cap);
	if (!pend)
		exit(1);
}

/* flush sends what's waiting while the window has room; true when nothing is left. */
static int flush(void) {
	while (pend_off < pend_len && seq - acked < WINDOW) {
		size_t n = pend_len - pend_off;
		if (n > MAX_PAYLOAD)
			n = MAX_PAYLOAD;
		send(pend_kind, pend_stream, pend + pend_off, n);
		pend_off += n;
	}
	if (pend_off < pend_len)
		return 0;
	pend_len = pend_off = 0;
	return 1;
}

/* drain sends everything waiting, waiting for acks as needed. */
static void drain(void) {
	while (!flush()) {
		if (!tty_open)
			exit(1); /* synodock is gone; nothing more will be acknowledged */
		read_tty(-1);
	}
}

#ifndef NO_ZLIB
/* Each stream's gzip stream, started at its first output. */
static z_stream zs[3];
static int zs_started[3];

static void deflate_into_pend(int stream, const unsigned char *data, size_t n, int mode) {
	z_stream *z = &zs[stream];
	if (!zs_started[stream]) {
		/* 15 + 16: a gzip header and trailer; level 1, as gzip -1 */
		if (deflateInit2(z, 1, Z_DEFLATED, 15 + 16, 8, Z_DEFAULT_STRATEGY) != Z_OK)
			exit(1);
		zs_started[stream] = 1;
	}
	z->next_in = (Bytef *)data;
	z->avail_in = (uInt)n;
	do {
		pend_reserve(4096 + n);
		z->next_out = pend + pend_len;
		z->avail_out = (uInt)(pend_cap - pend_len);
		int r = deflate(z, mode);
		if (r == Z_STREAM_ERROR)
			exit(1);
		pend_len = pend_cap - z->avail_out;
	} while (z->avail_out == 0);
	pend_kind = 'C';
	pend_stream = stream;
}
#endif

/* finish sends the exit status and waits (a while) for it to arrive, so the tty isn't
 * closed under it. */
static int finish(int code) {
	char s[16];
	int n = snprintf(s, sizeof s, "%d", code);
	while (seq - acked >= WINDOW && tty_open)
		read_tty(-1);
	send('X', 0, (const unsigned char *)s, (size_t)n);
	struct timespec start, now;
	clock_gettime(CLOCK_MONOTONIC, &start);
	while (acked < seq && tty_open) {
		clock_gettime(CLOCK_MONOTONIC, &now);
		long ms = (now.tv_sec - start.tv_sec) * 1000 + (now.tv_nsec - start.tv_nsec) / 1000000;
		if (ms >= EXIT_WAIT_MS)
			break;
		read_tty((int)(EXIT_WAIT_MS - ms));
	}
	return 0;
}

/* read_line reads synodock's go-ahead, a byte at a time so nothing after it is taken. */
static int read_line(char *buf, size_t size) {
	size_t len = 0;
	for (;;) {
		char c;
		ssize_t n = read(0, &c, 1);
		if (n < 0 && errno == EINTR)
			continue;
		if (n <= 0)
			return -1;
		if (c == '\n' || c == '\r')
			break;
		if (len < size - 1)
			buf[len++] = c;
	}
	buf[len] = 0;
	return 0;
}

/* command_env is ours without the framing script's switches. */
static char **command_env(void) {
	size_t n = 0;
	while (environ[n])
		n++;
	char **env = calloc(n + 1, sizeof *env);
	if (!env)
		exit(1);
	size_t j = 0;
	for (size_t i = 0; i < n; i++)
		if (strncmp(environ[i], "SD_GZIP=", 8) && strncmp(environ[i], "SD_AGENT=", 9))
			env[j++] = environ[i];
	return env;
}

static int spawn(char **argv, int out[3]) {
	int fds[3][2];
	for (int s = 1; s <= 2; s++)
		if (pipe2(fds[s], O_CLOEXEC) < 0)
			return errno;
	posix_spawn_file_actions_t fa;
	posix_spawnattr_t attr;
	posix_spawn_file_actions_init(&fa);
	posix_spawn_file_actions_addopen(&fa, 0, "/dev/null", O_RDONLY, 0);
	posix_spawn_file_actions_adddup2(&fa, fds[1][1], 1);
	posix_spawn_file_actions_adddup2(&fa, fds[2][1], 2);
	posix_spawnattr_init(&attr);
	sigset_t none, def;
	sigemptyset(&none);
	sigemptyset(&def);
	sigaddset(&def, SIGINT);
	sigaddset(&def, SIGQUIT);
	sigaddset(&def, SIGPIPE);
	sigaddset(&def, SIGTERM);
	posix_spawnattr_setsigmask(&attr, &none);
	posix_spawnattr_setsigdefault(&attr, &def);
	posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETSIGMASK | POSIX_SPAWN_SETSIGDEF);
	int err = posix_spawnp(&child, argv[0], &fa, &attr, argv, command_env());
	posix_spawn_file_actions_destroy(&fa);
	posix_spawnattr_destroy(&attr);
	for (int s = 1; s <= 2; s++) {
		close(fds[s][1]);
		out[s] = fds[s][0];
		if (err)
			close(fds[s][0]);
	}
	if (err)
		child = -1;
	return err;
}

/* status is the command's exit status as a shell reports it. */
static int status(void) {
	int ws;
	while (waitpid(child, &ws, 0) < 0)
		if (errno != EINTR)
			return 126;
	if (WIFSIGNALED(ws))
		return 128 + WTERMSIG(ws);
	return WEXITSTATUS(ws);
}

int main(int argc, char **argv) {
	if (argc < 3 || strcmp(argv[1], "run")) {
		fputs("usage: synodock-agent run COMMAND [ARG]...\n", stderr);
		return 2;
	}
	argv += 2;
	if (!strcmp(argv[0], "--"))
		argv++;
	if (!argv[0]) {
		fputs("usage: synodock-agent run COMMAND [ARG]...\n", stderr);
		return 2;
	}
	signal(SIGPIPE, SIG_IGN);

	/* Raw: no echo, no output processing, ^C a byte rather than a signal to us. */
	if (tcgetattr(0, &saved_tty) == 0) {
		struct termios raw = saved_tty;
		cfmakeraw(&raw);
		if (tcsetattr(0, TCSANOW, &raw) == 0) {
			tty_saved = 1;
			atexit(restore_tty);
		}
	}
	write_all(hello, sizeof hello - 1);
	char go[32];
	if (read_line(go, sizeof go) < 0)
		return 1;
	const char *gz = getenv("SD_GZIP");
#ifdef NO_ZLIB
	int compress = 0;
	(void)gz;
#else
	int compress = gz && !strcmp(gz, "1") && strchr(go, 'z');
#endif

	int pipes[3] = {-1, -1, -1};
	int err = spawn(argv, pipes);
	if (err) {
		char msg[512];
		int n = snprintf(msg, sizeof msg, "synodock exec: %s: %s\n", argv[0], strerror(err));
		if (n >= (int)sizeof msg)
			n = sizeof msg - 1;
		send('D', 2, (const unsigned char *)msg, (size_t)n);
		return finish(err == ENOENT ? 127 : 126);
	}

	size_t rsize = compress ? ZREAD : MAX_PAYLOAD;
	unsigned char *buf = malloc(rsize);
	if (!buf)
		return 1;
	while (pipes[1] >= 0 || pipes[2] >= 0) {
		if (!flush()) {
			if (!tty_open)
				return 1;
			read_tty(-1);
			continue;
		}
		struct pollfd p[3];
		int np = 0;
		for (int s = 1; s <= 2; s++)
			if (pipes[s] >= 0)
				p[np++] = (struct pollfd){.fd = pipes[s], .events = POLLIN};
		if (tty_open)
			p[np++] = (struct pollfd){.fd = 0, .events = POLLIN};
		if (poll(p, (nfds_t)np, -1) < 0) {
			if (errno == EINTR)
				continue;
			return 1;
		}
		for (int i = 0; i < np; i++) {
			if (!p[i].revents)
				continue;
			if (p[i].fd == 0) {
				char t[256];
				ssize_t n = read(0, t, sizeof t);
				if (n > 0)
					on_tty(t, n);
				else if (n == 0 || (errno != EINTR && errno != EAGAIN))
					tty_open = 0;
				continue;
			}
			int s = p[i].fd == pipes[1] ? 1 : 2;
			if (pend_len) /* the other stream's read came first; this one waits */
				continue;
			ssize_t n = read(pipes[s], buf, rsize); /* what the pipe holds now */
			if (n < 0 && errno == EINTR)
				continue;
			if (n <= 0) {
				close(pipes[s]);
				pipes[s] = -1;
				continue;
			}
#ifndef NO_ZLIB
			if (compress) {
				deflate_into_pend(s, buf, (size_t)n, Z_SYNC_FLUSH);
				continue;
			}
#endif
			{
				pend_reserve((size_t)n);
				memcpy(pend, buf, (size_t)n);
				pend_len = (size_t)n;
				pend_kind = 'D';
				pend_stream = s;
			}
		}
	}
	drain();
#ifndef NO_ZLIB
	for (int s = 1; s <= 2; s++) {
		if (!zs_started[s])
			continue;
		deflate_into_pend(s, NULL, 0, Z_FINISH); /* the trailer: its CRC checks the whole stream */
		drain();
		deflateEnd(&zs[s]);
	}
#endif
	return finish(status());
}
