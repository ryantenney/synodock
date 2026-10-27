# synodock

`docker exec`, and a Docker Engine API for the stock `docker` CLI, for containers on a
Synology NAS, with SSH left off.

Synology's Container Manager has a web API, but no exec in it, and the usual answer
is to turn on SSH (which on DSM needs an administrator account) or expose the Docker
daemon. Container Manager's own Terminal tab runs shells in containers over a
websocket, though, with nothing but a DSM login. synodock speaks that websocket, and
the same web API calls the Container Manager UI makes.

```console
$ synodock exec mycontainer -- uname -a
Linux mycontainer 4.4.302+ #86009 SMP ... x86_64 GNU/Linux
$ synodock exec -it mycontainer bash
root@mycontainer:/#
```

> **Unofficial.** Not affiliated with or endorsed by Synology. It uses undocumented
> interfaces of DSM and Container Manager, which an update can change. Tested on
> DSM 7.3.2 with Container Manager 24.0.2.

## Install

```console
$ go install github.com/ryantenney/synodock@latest
```

Go 1.26+. One static binary, no runtime to install; macOS and Linux.

## Setup

Use a DSM account that has Container Manager permission. A dedicated one is best: you
can audit, rotate or disable it on its own. Don't mistake it for a low-privilege
account, though: whoever can drive Container Manager can start a privileged container
with the NAS's `/` mounted, so treat its password and tokens like root's SSH key. An
exec runs as each container's default user, often root, the same as the Terminal tab.

Set the address and account in the environment or in `~/.config/synodock/config.toml`:

```toml
url = "https://nas.example.com:5001"
user = "docker"
```

| Setting | Environment | Config file | |
|---|---|---|---|
| DSM address | `SYNODOCK_URL` | `url` | required |
| Account | `SYNODOCK_USER` | `user` | required |
| Password | `SYNODOCK_PASSWORD` | | else `password_command`, else the macOS keychain, else a prompt |
| Password command | `SYNODOCK_PASSWORD_COMMAND` | `password_command` | prints the password |
| 2FA code command | `SYNODOCK_OTP_COMMAND` | `otp_command` | prints a current code, when DSM asks for one |
| Idle exit | `SYNODOCK_IDLE_EXIT` | `idle_exit` | how long `serve` waits idle before exiting, e.g. `"30m"`; `"0"` never; default 15m when started on demand |
| Keychain service (macOS) | `SYNODOCK_KEYCHAIN` | `keychain` | default `synodock`; account = `user@host`, else the user |

On macOS, store the password in the keychain once (`-a docker@nas.example.com`
instead keeps the same account name on several NAS apart):

```console
$ security add-generic-password -s synodock -a docker -w
```

Anywhere else, or to keep it in another store, point `password_command` at it. The
commands run under `sh` and the first line they print is used:

```toml
password_command = "secret-tool lookup service synodock user docker"   # GNOME/KDE
# password_command = "pass show nas/docker"
# password_command = "op read op://Private/NAS/password"                # 1Password
```

### Two-factor authentication

The first login with a code saves a device token (in the keychain on macOS,
otherwise in `~/.config/synodock/`, mode 0600), and later logins need no code. DSM
lists it under the account's trusted devices, where you can revoke it. When DSM
asks for a code, synodock takes it from `otp_command` if set, else asks on the
terminal, else fails. Or give one directly:

```console
$ synodock login --otp 123456
```

`otp_command` lets a machine with no one at the keyboard log in the first time, or
again after the device token is revoked. The command prints the code, so the TOTP
secret stays in your store; but a store holding both the password and the secret is
a full login on its own, from anywhere, until 2FA is re-enrolled:

```toml
otp_command = "op read 'op://Private/NAS/one-time password?attribute=otp'"
# otp_command = "oathtool --totp -b \"$(security find-generic-password -s synodock-totp -w)\""
```

### Several NAS

Name each NAS in a `[nas.NAME]` section, with the same settings as the top level:

```toml
default = "home"   # what `synodock use home` writes

[nas.home]
url = "https://nas.example.com:5001"
user = "docker"

[nas.office]
url = "https://office-nas.example.com:5001"
user = "svc-docker"
password_command = "op read op://Work/office-nas/password"
```

synodock picks the NAS the way the docker CLI picks its engine, first match wins:

| synodock | like docker's |
|---|---|
| `-H https://user@nas:5001` | `-H` |
| `--nas NAME` | `--context NAME` |
| `SYNODOCK_URL` (with `SYNODOCK_USER`) | `DOCKER_HOST` |
| `SYNODOCK_NAS` | `DOCKER_CONTEXT` |
| `default =`, set by `synodock use NAME` | `docker context use` |
| `url` and `user` at the top of `config.toml` | the default socket |

A `[nas.NAME]` profile is self-contained, as a docker context is: `SYNODOCK_PASSWORD`
and the other `SYNODOCK_*` settings only apply to a NAS given by `-H` or
`SYNODOCK_URL`, or to the one at the top. `synodock use` lists them and marks the
one in use.

## Commands

Commands come in docker-style groups, `synodock container ls`, with `ps`, `logs`,
`top` and `exec` as top-level shortcuts, as docker has them. `synodock GROUP` lists a
group's commands; more groups (projects, images, networks, registries) are coming,
see `docs/roadmap.md`.

```console
$ synodock ps [-a]                          # containers
$ synodock logs NAME [-n 100]               # a container's log
$ synodock top NAME                          # its processes (Container Manager's Processes view)
$ synodock exec NAME -- COMMAND...          # exits with the command's status
$ synodock exec -it NAME bash               # interactive
$ printf 'data' | synodock exec -i NAME -- cat
$ synodock exec -d NAME -- sleep 600        # in the background; returns at once
$ synodock serve                            # Docker API on ~/.cache/synodock/docker.sock
$ synodock --nas office serve --docker-context   # office.sock, and docker context synology-office
$ synodock project ls                       # Container Manager projects
$ synodock project config NAME [-o FILE]    # a project's compose file
$ synodock project up|down|start|stop|restart NAME   # compose build/clean/start/stop/restart, streamed
$ synodock project create NAME FILE [--path DIR]     # the folders are made too
$ synodock project update NAME FILE         # stopped projects only; the old file is backed up
$ synodock use [NAME]                       # list the NAS, or pick the default
$ synodock --nas home service install       # serve started on demand by launchd/systemd
```

### exec

- It always runs on a TTY, as the container's default user: stderr comes back with
  stdout, and there's no `--user`.
- By default the output is framed: in the container, `dd` cuts it into chunks, each
  is gzipped and hex-encoded by `od`, and `awk` sends them as records acknowledged
  one at a time. The output arrives complete, in order and binary-safe, slow output
  stays live, and the exit status is exact. That needs `sh`, `dd`, `od` and `awk` in
  the image (tested on Debian and busybox-based images); gzip is used when
  present (`--no-gzip` or `SYNODOCK_GZIP=0` turns it off).
- `-i`, `-it` and `--raw` pass output through as it arrives, and it **can arrive out
  of order**: the websocket relay on the NAS reorders output that's produced
  quickly. `--raw` runs the command without `sh` and can't report an exit status.

### serve: the docker CLI against the NAS

```console
$ synodock serve &
$ docker context create synology --docker host=unix://$HOME/.cache/synodock/docker.sock
$ docker --context synology ps
$ docker --context synology exec -it mycontainer sh
```

`serve` for a `[nas.NAME]` listens on `~/.cache/synodock/NAME.sock`, so each NAS
gets its own; `--docker-context` creates or updates a docker context for it
(`synology`, or `synology-NAME`; `--docker-context=other` names it).

The socket is mode 0600, and nothing listens on the network. Supported: `ps`,
`inspect`, `start`, `stop`, `restart`, `kill`, `rm`, `logs` (`-f` by polling),
`stats`, `top` (PID, %CPU, %MEM, RSS, STIME and CMD; no ps options), `images`,
`image inspect`, `rmi`, `load`, `pull` (Docker Hub), `network ls`, `volume ls`,
`events` (polled every 2 s), `exec` (`-i`, `-t`, `-e`, `-w`, `-d`), `version`, `info`.
Everything else answers 501. TUIs like
[lazydocker](https://github.com/jesseduffield/lazydocker) work against it, its Stats
and Top tabs included (`DOCKER_HOST=unix://$HOME/.cache/synodock/docker.sock
lazydocker`).

`docker inspect` on a container returns its full config, environment secrets
included, as dockerd does.

### Started on demand

`synodock service install` hands the socket to launchd (macOS) or systemd (Linux),
which listens on it and starts `synodock serve` when something first connects; serve
exits again after 15 idle minutes (`idle_exit` in the profile changes it) and comes
back on the next connection. No terminal has to stay open, and nothing logs in to DSM until it's used.

```console
$ synodock --nas home service install   # logs in here first (a 2FA code if needed), then
                                        # installs, creates docker context synology-home,
                                        # and checks serve answers
$ docker --context synology-home ps     # starts serve
$ synodock --nas home service status
$ synodock --nas home service uninstall
```

- The service runs unattended, so it logs in with the device token: install logs in
  first, where a 2FA code can still be typed, and saves one. The password comes from
  the keychain or `password_command` (the service gets this shell's `PATH`, so tools
  like `op` are found).
- It has to be a NAS in `config.toml`, not one given by `-H` or `SYNODOCK_URL`: the
  service doesn't see this shell's environment.
- macOS: a LaunchAgent `com.github.ryantenney.synodock.NAME` in
  `~/Library/LaunchAgents`, logging to `~/Library/Logs/synodock-NAME.log`. No cgo:
  synodock calls `launch_activate_socket` directly.
- macOS keeps processes launchd starts off the local network until you allow them,
  and asks once. `service install` waits while you click Allow (or turn synodock on
  in System Settings > Privacy & Security > Local Network). The permission only
  reaches processes started after it, so a serve that was refused exits once its
  requests are done and the next connection starts one that has it. A NAS reached
  through a public address or a VPN isn't affected.
- Linux: `synodock-NAME.socket` and `.service` in `~/.config/systemd/user`, logging to
  the journal. User services stop at logout unless `loginctl enable-linger` is on.
- The service runs `synodock --nas NAME serve`, so the profile in `config.toml`
  (address, account, credentials, `idle_exit`) is read at each start: edit it, no
  reinstall. Run `service install` again only if the synodock binary moves, since the
  service names it by its path. (`service install --idle-exit` fixes the timeout in
  the service instead.)

## How it works

The Terminal tab's websocket, `wss://<nas>/docker/ws`, wants the DSM session as the
`id` cookie and a matching `Origin`. Messages are JSON arrays: one socket creates
and lists sessions in a container, another attaches to one and carries the bytes.
Each session is a real Docker exec, started when it's attached.

Three quirks shape the rest:

1. **The relay reorders output.** When several pty reads are in flight, they can
   arrive in any order (the Terminal tab paints them wrong too). Hence the framing
   with one record in flight.
2. **The command is cut at the first unquoted `=`.** `echo a=b c` runs `echo a`, so
   every argument is single-quoted.
3. **Overlapping web API calls wait in ~5 s steps.** Two concurrent container
   lookups take 5.1 s each instead of 0.1 s, so synodock sends one call at a time and
   caches briefly. `SYNODOCK_TRACE=1` logs every call's wait and duration.

`internal/terminal/session.go` has the protocol in full; `docs/roadmap.md` has the plan
for covering the rest of Container Manager (projects, images, networks, registries) on
the command line, and what's missing compared with `docker exec`.

## Development

```console
$ go test ./...                                  # execs run under dash on a local pty
$ SYNODOCK_TEST_IMAGE=alpine go test ./internal/terminal/   # or in a throwaway container
```

The exec tests stand in for Container Manager's Terminal websocket and run the real
framing scripts; the serve test drives the stock `docker` CLI against a fake DSM.

## License

MIT
