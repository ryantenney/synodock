# synodock: roadmap

Author: Ryan Tenney

Two tracks: the CLI growing to cover everything Container Manager does, and
`exec` closing the gap with `docker exec`.

## Container Manager on the command line

The goal is that anything the Container Manager UI can do, synodock can do from a
terminal, through the same web API calls. Commands go in docker-style noun groups;
`ps`, `logs`, `top` and `exec` stay at the top level as shortcuts, as docker keeps
them.

```console
$ synodock project ls | config | create | update | rm | up | down | start | stop | restart | ps | logs
$ synodock container ls | inspect | start | stop | restart | kill | rm | logs | top | exec | stats | create | update | export
$ synodock image ls | inspect | rm | prune | pull | load | import | save | upgrade
$ synodock network ls | inspect | create | update | rm
$ synodock registry ls | add | update | rm | use | search | tags
$ synodock events ls | clear | export          # Container Manager's own log
```

### What the API has

Methods the Container Manager UI calls (from its scripts on DSM 7.3.2 / Container
Manager 24.0.2, `webman/3rdparty/ContainerManager/MainVue.js`). Each one's parameters
were read from the UI code, or from the browser's own requests, and confirmed with a
real call before a command used it.

| API | Methods | synodock (2026-10-28) |
|---|---|---|
| `SYNO.Docker.Project` | list, get, create, update, delete, log, get_share_info; `start/stop/restart/build/clean_stream` | all but get_share_info |
| `SYNO.Docker.Container` | list, get, create, set, start, stop, restart, signal, delete, export, stats, get_process | all but export (a container archive) |
| `SYNO.Docker.Container.Log` | get, export | get |
| `SYNO.Docker.Container.Profile` | export | export |
| `SYNO.Docker.Image` | list, get, delete, prune, import, upload, export, pull_start/pull_status, upgrade_start/upgrade_status | all but get |
| `SYNO.Docker.Network` | list, list_container, create, set, remove, update_bip | list, create, remove |
| `SYNO.Docker.Registry` | get, create, set, delete, search, tags, using | all but set |
| `SYNO.Docker.Log` | list, clear, export | all |
| `SYNO.Docker.Migrate` | status, import, import_status | none (not planned) |
| `Container.Resource`, `Container.PkgProfile`, `Utils` | not seen in the UI scripts | none |

### Plan

| Step | What | State |
|---|---|---|
| 0. Noun groups | A command table in `cli.go` (pflag, no cobra). `ps`, `logs`, `top`, `exec` are shortcuts for `container ls/logs/top/exec`; `synodock GROUP` lists a group's verbs | done |
| 1. Projects | `project ls`, `config`, `ps`, `logs`, `create`, `update`, `rm`, `up`/`down`/`start`/`stop`/`restart` (compose's output streamed, its exit code synodock's; `build`/`clean` are aliases) | done; every verb run on throwaway projects |
| 1a. Project guards | `up` refuses a running project; `update` refuses one that isn't stopped and backs up the old file locally and to `<share>/backups`; `rm` refuses a running project and asks for the name; `create` makes the project folder and nested bind-mount folders, and leaves file-like sources alone | done |
| 2. Containers | `ls`, `logs`, `top`, `exec`, `export` (settings JSON, written 0600), `create --profile FILE`, `update NAME --profile FILE` (name, CPU, memory, auto-restart only; it fails when a command change didn't take) | done; all run live |
| 3. Images | `ls`, `rm` (ids only), `import`, `load` (old tar kept as `.prev.tar`; every tag checked after), `pull` (checked against the registry's digest), `save`, `prune` (lists, then asks), `upgrade` (lists the containers it recreates, then asks) | done; all run live (`pull` through `deploy --pull`) |
| 4. Networks | `ls`, `inspect`, `create`, `rm`. Not done: `set` (replaces a network's whole membership), `update-bip` (the default bridge's subnet), and `network create`/`rm` through `serve` | partly done; optional |
| 5. Registries | `ls`, `add`, `rm`, `use`, `search`, `tags`; the registry for pulls can live in the profile (`registry`, `registry_pull`, `registry_user`, `registry_password_command`). Not done: `update` (the UI's set) | mostly done; `use` not run live (it changes what pulls default to) |
| 6. Events | `ls`, `export` (CSV or HTML), `clear` | done; all run live |
| 7. Project deploy | `project plan`/`deploy` with `--image`, `--pull`, `--compose`, `--service`, `--registry-pull`: everything that can fail happens while up; old images deleted once healthy, unless a container or tag still holds them. Tested end to end against a fake NAS (`deploy_e2e_test.go`); `--service`'s plan matched dsm.py's on live omnivault | done |
| 8. Migrate | `migrate status`, `import`: settings from the old Docker package | not planned: no NAS here to test it on |

Destructive verbs (`project rm`, `image rm`, `prune`, `upgrade`, `network rm`, `registry rm`,
`events clear`) ask first, with `-f` to skip, and refuse without a terminal. Anything
untested goes through a throwaway project first, never a real one.

### What testing found

Facts about Container Manager that the code depends on, each found by trying it:

- String parameters are JSON-encoded (`name="web"` with the quotes); `format=csv` unquoted fails with error 117.
- `project rm` removes the project and its containers; its folder (compose file, backups, bind-mounted data) stays.
- `memory_limit` is in bytes. An existing container's command can't be changed: its settings cover name, CPU, memory and auto-restart, and only while it's stopped.
- An image upgrade recreates every container on that image: running ones are stopped and started again, stopped ones stay stopped.
- The image list shows an untagged image with the tag `<none>`, which isn't a tag.
- Deleting by image id removes every tag on it; an image a container runs is refused.
- `network remove` takes the network objects as listed, not names.
- The log's level filter takes `error`, `warning`, `information`; the short forms silently match nothing.
- Exports and file downloads answer with a `Content-Disposition: attachment`; without it, the answer is an error envelope.
- `FileStation.CreateFolder` with `force_parent=true` makes parents, and an existing folder isn't an error.
- Uploads authenticate with the `id` cookie as well as `_sid`.

### Out of scope

Not Container Manager, so not synodock: FileStation beyond what projects need
(listing, uploads, downloads), `.env` editing, SMB mounts. These stay in whatever
wraps synodock (the homelab `dsm.py` for one).

## exec

`synodock exec` and `docker exec` through `synodock serve` run commands over
Container Manager's Terminal websocket (protocol and quirks in
`internal/terminal/session.go`).
The default mode is framed: in the container the command's output is cut into
chunks by `dd`, each gzipped and hex-encoded by `od`, and sent by `awk` as records
acked one at a time, with the exit status last. This is what's still missing
compared with `docker exec`, roughly in the order to build it.

### Status (2026-10-26)

| Feature | Feasible | How | Effort | State |
|---|---|---|---|---|
| `-d` (detach) | yes | the command starts in the background under `setsid` (where the image has it) with SIGHUP ignored, output discarded; returns in ~0.5 s. A missing binary fails with 127. `docker exec -d` too; inspect can't follow it afterwards | small | done |
| Faster output: gzip | yes | `gzip -1` per chunk when the image has gzip: 100k lines of `seq` 1.3 s instead of 4.8 s; 256 KB of random data ~0.2 s slower. On by default (`--no-gzip` or `SYNODOCK_GZIP=0` turns it off) | small | done |
| Live slow output | yes | streaming `od`, mawk and the 1 KiB records held slow output back (a line a second for 5 s arrived all at 5 s); `dd` chunks flushed through every stage fix it, gzip included | small | done |
| Records put back in order | yes | the relay was seen to split one record into more than 8 pty reads and reorder them; middle pieces are all hex, so a wrong order still looked like a record. Each data record now carries a Fletcher-16 checksum, and orderings are tried (nearest arrival first, once a record's start and end are in) until one matches it. 256 KB random: 40 of 40 byte-identical, against 4 failures in 6 before | small | done |
| Faster output: window | yes, with synodock-agent | the relay reads up to 4 KiB, merging writes and cutting anywhere, and reorders reads, so records can't be kept to one read. Instead synodock-agent (a static Go binary mounted at `/opt/synodock/agent`) sends self-contained records (base64, CRC-32, length) with 32 in flight, and the decoder puts cut and reordered pieces back together by length and CRC (`internal/wire`, stress-tested with random cuts and reordering). On the NAS: `seq 100000` 1.3 s instead of 2.4 s, 256 KB random 1.1 s instead of 2.7 s, byte-identical 15 of 15. Output is gzipped per stream (flushed after each read, so it stays live; `seq 100000` is 313 KB on the wire instead of 807 KB), negotiated in the go-ahead so an older synodock still gets plain records. Without the agent, the script as before | medium | done (prototype; not yet mounted into any homelab service) |
| stdout and stderr apart | yes, framed mode only | each stream through its own chunk loop, tagged into the same framer; each tagged line one `write()` under PIPE_BUF (built before printing: mawk -W interactive sends a multi-argument printf as several writes), `od -w512` to keep it cheap. `serve` sends stderr as docker's stderr stream. Tested in Debian and Alpine containers under load, and live | small | done |
| `docker top` | yes | `SYNO.Docker.Container get_process` (the UI's Processes view), no exec needed: pid, cpu %, memory bytes and %, start (HH:MM), command; no UID or PPID, so ps options get a 501. `synodock top` too | small | done |
| `-u user` | when the exec runs as root | wrapped in util-linux `setpriv` (busybox's has no `--reuid`, so it's asked first), `gosu`, `su-exec`, or `su -m` (names only: a uid is looked up in /etc/passwd; `user:group` needs one of the others; `--` so busybox su doesn't take the command's own `-c`). Works through `serve` too (`docker exec -u`). Tested in Debian and Alpine containers, and live | medium | done |
| `docker cp` out | done for `synodock cp` (CLI) | `synodock cp CONTAINER:PATH DEST`: `tar -cf -` through framed exec (binary-safe), or the file alone where the image has no tar; extracted here inside `os.Root`, so a container's tar can't write outside DEST. The Docker API's archive endpoint is still to do | medium | done (CLI) |
| Framed stdin (`-i` without `-t`, ordered, binary-safe) | done, through synodock-agent | the agent takes I/E records on its tty (same format as output, CRC-checked, only in order) and writes them to the command's stdin pipe; its K records say how far it's written (8 records in flight beyond that, so a slow reader paces synodock) and ask again after a gap or damage, and synodock also resends after 500 ms without news. Tested with input lost, swapped and damaged on the way. Without an agent the script stops before the command and synodock falls back to unframed `-i`. On the NAS: 3 MB in about 4.4 s after login (about 0.7 MB/s; the 8-record window is the knob) | large | done |
| `docker cp` in | done for `synodock cp` (CLI) | `synodock cp SRC CONTAINER:PATH`: a file as `cat`'s stdin (mode kept), a directory as a tar (needs tar in the image), through framed stdin. The Docker API's archive endpoint is still to do | medium | done (CLI) |
| `docker attach` | technically | the Terminal tab's first entry (id = container name) is PID 1's console. A stray `^C` stops the container, so keep refusing it, or require an explicit flag | small, risky | not planned |
| `--privileged`, exec in a stopped container | no | DSM's exec has no such option; docker can't exec in a stopped container either | | not possible |

## Audit (2026-10-28)

An independent review of everything added since the noun groups found 19 issues;
the fixes are in commits dbf38a3 through 3862535. The open items it left are closed too:
an unhealthy container now fails the health wait after two minutes rather than at the
timeout (2ab6bd4); start, stop and restart were run on a throwaway project and each
reports its exit code; uploads send the session as the `id` cookie instead of in the URL,
checked live with an image load; and a registry reachable at another address from the
NAS has `registry_pull` (68425bc).

`--service` and an end-to-end deploy test have since landed: `project deploy --service`
replaces one container while the rest keeps running, blocked by the same checks as dsm.py
(dependents, a shared image, other services' images and environment drift against compose
and `.env`, key names only). On live omnivault its plan matches dsm.py's exactly (1302
environment values compared, the same blocks). Deploy and `--service` run in tests against a
stateful fake NAS (`deploy_e2e_test.go`), which found the `<none>` bug fixed in b52b631.
