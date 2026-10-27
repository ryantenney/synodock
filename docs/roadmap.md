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
Manager 24.0.2, `webman/3rdparty/ContainerManager/MainVue.js`). The names were
pattern-matched out of the bundle, so each one's parameters get read from the UI
code and confirmed with a real call before a command uses it.

| API | Methods | synodock now |
|---|---|---|
| `SYNO.Docker.Project` | list, get, create, update, delete, log, get_share_info; `start_stream`, `stop_stream`, `restart_stream`, `build_stream`, `clean_stream` | none |
| `SYNO.Docker.Container` | list, get, create, set, start, stop, restart, signal, delete, export, stats, get_process | list, start/stop/restart/signal, delete, stats, get_process (some through `serve` only) |
| `SYNO.Docker.Container.Log` | get, export | get |
| `SYNO.Docker.Container.Profile` | export | none |
| `SYNO.Docker.Image` | list, get, delete, prune, import, upload, export, pull_start/pull_status, upgrade_start/upgrade_status | list, delete, import, pull from Docker Hub (`serve` only) |
| `SYNO.Docker.Network` | list, list_container, create, set, remove, update_bip | list (`serve` only) |
| `SYNO.Docker.Registry` | get, create, set, delete, search, tags, using | none |
| `SYNO.Docker.Log` | list, clear, export | none |
| `SYNO.Docker.Migrate` | status, import, import_status | none |
| `Container.Resource`, `Container.PkgProfile`, `Utils` | not seen in the UI scripts | none |

### Plan

| Step | What | API | Effort | State |
|---|---|---|---|---|
| 0. Noun groups | A command table in `cli.go` (still pflag, no cobra): a group, a verb, its aliases and flags. Top-level `ps`, `logs`, `top`, `exec` become aliases of `container ls/logs/top/exec`; the help lists groups, and `synodock GROUP` its verbs. `service install\|uninstall\|status` became a group too | none | small | done |
| 1. Projects | `project ls`, `config` (the compose YAML, nothing else: `get` also returns `containers[]` as full inspect output, env secrets included, so `inspect` prints a filtered view), `ps`, `logs`; `up`/`down`/`start`/`stop`/`restart` streaming compose's output and exiting with its code (`up` = `build_stream`, `down` = `clean_stream`; `build`/`clean` as aliases, Container Manager's names); `create NAME FILE [--path]`, `update NAME FILE`, `rm` | Project.* | medium | |
| 1a. Project guards | Rules the homelab `dsm.py` learned: `up` on a running project recreates its containers in place and Container Manager then lists them as `<12hex>_<name>`, where `stop` and `down` silently do nothing; so `up` refuses a running project unless `--force`. `update` refuses a running project, as the UI does, though the API allows it; it backs up the old YAML first. `create` makes the project folder and the folders behind relative bind mounts, since a missing one fails the build. `rm`: find out what it deletes (the folder? volumes?) before shipping it | Project.*, FileStation | small | |
| 2. Containers | `container` group over what `serve` already does, plus `create` (from a JSON profile, the UI wizard's shape), `update` (`set`: restart policy, resource limits, name), `export` (settings as JSON, `Container.Profile export`; `Container export` to an archive, if that's what it is) | Container.*, Container.Profile | medium | |
| 3. Images | `image load TAR` (upload to a NAS folder, then import; keep the previous tar as `.prev.tar`), `import NAS_PATH`, `pull` from any configured registry with progress (`pull_start`/`pull_status`), `save` (`export`), `prune`, `upgrade` (`upgrade_start`/`status`: what it recreates needs checking). Old images left `<none>` after a tag moves are only deletable by 12-hex short id | Image.* | medium | |
| 4. Networks | `ls`, `inspect` (with `list_container`), `create`, `update` (`set`), `rm`; `update-bip` for the default bridge subnet. `serve` gets `network create`/`rm` too | Network.* | small | |
| 5. Registries | `ls`, `add`, `update`, `rm`, `use` (the UI's active registry), `search`, `tags`. Credentials from the keychain or a command, like the DSM password; never on the command line | Registry.* | small | |
| 6. Events | `events ls`, `clear`, `export`: Container Manager's own log (the UI's Log tab), not a container's | Log.* | small | |
| 7. Project deploy | Generic `project deploy NAME [--image TAR]... [--pull REF]... [--compose FILE] [--service NAME]`: load or pull images, update the compose, `down` + `up`, wait for healthy, delete the replaced images; `--dry-run` prints the plan. Ported from the homelab `dsm.py deploy`, minus what's homelab-specific | the above | large | |
| 8. Migrate | `migrate status`, `import`: settings from the old Docker package. Only on a NAS that had it | Migrate.* | small | maybe |

Destructive verbs (`project rm`, `image prune`, `events clear`, `container rm` of a
project's container) ask first, as `docker * prune` does, with `-f` to skip. Anything
untested goes through a throwaway project first, never a real one.

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
| Faster output: window | yes | a few records in flight, each small enough to stay one pty read, sorted by sequence number. Helps incompressible output, which gzip doesn't | medium | |
| stdout and stderr apart | yes, framed mode only | stderr through its own `od \| awk` tagger into the same framer; each tagged line is one `write()` under PIPE_BUF, so lines don't mix. Order between the streams is approximate, as with real docker. `-t` keeps them merged, as real docker does | small | |
| `docker top` | yes | `SYNO.Docker.Container get_process` (the UI's Processes view), no exec needed: pid, cpu %, memory bytes and %, start (HH:MM), command; no UID or PPID, so ps options get a 501. `synodock top` too | small | done |
| `-u user` | when the exec runs as root | wrap in whatever the image has: `setpriv`, `su-exec`, `gosu`, `chroot --userspec`, busybox `su`; a numeric uid with no passwd entry needs `setpriv`. Fail clearly otherwise | medium | |
| `docker cp` out | yes | `GET /containers/{id}/archive` = `tar -cf - -C dir name` through framed exec (binary-safe); the stat header from `stat`. Bound by exec throughput | medium | |
| Framed stdin (`-i` without `-t`, ordered, binary-safe) | yes, hardest | one `sh` loop is the only tty reader: ack lines go to the framer through a FIFO, stdin lines (octal escapes, under the tty's 4,095-character line limit) become bytes with `printf` into a FIFO feeding the command. Needs `mkfifo` and a writable `/tmp`. Risk: if the command stops reading stdin, the loop blocks and output acks stall, so stdin needs its own flow control | large | |
| `docker cp` in | after framed stdin | tar through framed stdin; for paths on a bind mount, FileStation upload is faster and already works | medium | |
| `docker attach` | technically | the Terminal tab's first entry (id = container name) is PID 1's console. A stray `^C` stops the container, so keep refusing it, or require an explicit flag | small, risky | not planned |
| `--privileged`, exec in a stopped container | no | DSM's exec has no such option; docker can't exec in a stopped container either | | not possible |
