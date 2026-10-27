# synodock exec: roadmap

Author: Ryan Tenney

`synodock exec` and `docker exec` through `synodock serve` run commands over
Container Manager's Terminal websocket (protocol and quirks in
`internal/terminal/session.go`).
The default mode is framed: in the container the command's output is cut into
chunks by `dd`, each gzipped and hex-encoded by `od`, and sent by `awk` as records
acked one at a time, with the exit status last. This is what's still missing
compared with `docker exec`, roughly in the order to build it.

## Status (2026-10-26)

| Feature | Feasible | How | Effort | State |
|---|---|---|---|---|
| `-d` (detach) | yes | the command starts in the background under `setsid` (where the image has it) with SIGHUP ignored, output discarded; returns in ~0.5 s. A missing binary fails with 127. `docker exec -d` too; inspect can't follow it afterwards | small | done |
| Faster output: gzip | yes | `gzip -1` per chunk when the image has gzip: 100k lines of `seq` 1.3 s instead of 4.8 s; 256 KB of random data ~0.2 s slower. On by default (`--no-gzip` or `SYNODOCK_GZIP=0` turns it off) | small | done |
| Live slow output | yes | streaming `od`, mawk and the 1 KiB records held slow output back (a line a second for 5 s arrived all at 5 s); `dd` chunks flushed through every stage fix it, gzip included | small | done |
| Faster output: window | yes | a few records in flight, each small enough to stay one pty read, sorted by sequence number. Helps incompressible output, which gzip doesn't | medium | |
| stdout and stderr apart | yes, framed mode only | stderr through its own `od \| awk` tagger into the same framer; each tagged line is one `write()` under PIPE_BUF, so lines don't mix. Order between the streams is approximate, as with real docker. `-t` keeps them merged, as real docker does | small | |
| `docker top` | yes | `SYNO.Docker.Container get_process` (the UI's Processes view), no exec needed: pid, cpu %, memory bytes and %, start (HH:MM), command; no UID or PPID, so ps options get a 501. `synodock top` too | small | done |
| `-u user` | when the exec runs as root | wrap in whatever the image has: `setpriv`, `su-exec`, `gosu`, `chroot --userspec`, busybox `su`; a numeric uid with no passwd entry needs `setpriv`. Fail clearly otherwise | medium | |
| `docker cp` out | yes | `GET /containers/{id}/archive` = `tar -cf - -C dir name` through framed exec (binary-safe); the stat header from `stat`. Bound by exec throughput | medium | |
| Framed stdin (`-i` without `-t`, ordered, binary-safe) | yes, hardest | one `sh` loop is the only tty reader: ack lines go to the framer through a FIFO, stdin lines (octal escapes, under the tty's 4,095-character line limit) become bytes with `printf` into a FIFO feeding the command. Needs `mkfifo` and a writable `/tmp`. Risk: if the command stops reading stdin, the loop blocks and output acks stall, so stdin needs its own flow control | large | |
| `docker cp` in | after framed stdin | tar through framed stdin; for paths on a bind mount, FileStation upload is faster and already works | medium | |
| `docker attach` | technically | the Terminal tab's first entry (id = container name) is PID 1's console. A stray `^C` stops the container, so keep refusing it, or require an explicit flag | small, risky | not planned |
| `--privileged`, exec in a stopped container | no | DSM's exec has no such option; docker can't exec in a stopped container either | | not possible |
