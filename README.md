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
$ uv tool install git+https://github.com/ryantenney/synodock
$ # or run it without installing:
$ uvx --from git+https://github.com/ryantenney/synodock synodock --help
```

Python 3.12+.

## Setup

Use a DSM account that has Container Manager permission. A dedicated one is best: an
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
| Password | `SYNODOCK_PASSWORD` | | else the macOS keychain, else a prompt |
| Keychain service (macOS) | `SYNODOCK_KEYCHAIN` | `keychain` | default `synodock`; account = the user |

On macOS, store the password in the keychain once:

```console
$ security add-generic-password -s synodock -a docker -w
```

If the account uses two-factor authentication, log in once with a current code. That
saves a device token (in the keychain on macOS, otherwise in
`~/.config/synodock/`, mode 0600), and later logins need no code:

```console
$ synodock login --otp 123456
```

## Commands

```console
$ synodock ps [-a]                          # containers
$ synodock logs NAME [-n 100]               # a container's log
$ synodock exec NAME -- COMMAND...          # exits with the command's status
$ synodock exec -it NAME bash               # interactive
$ printf 'data' | synodock exec -i NAME -- cat
$ synodock exec -d NAME -- sleep 600        # in the background; returns at once
$ synodock serve                            # Docker API on ~/.cache/synodock/docker.sock
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

The socket is mode 0600, and nothing listens on the network. Supported: `ps`,
`inspect`, `start`, `stop`, `restart`, `kill`, `rm`, `logs` (`-f` by polling),
`stats`, `images`, `image inspect`, `rmi`, `load`, `pull` (Docker Hub),
`network ls`, `volume ls`, `events` (polled every 2 s), `exec` (`-i`, `-t`, `-e`, `-w`,
`-d`), `version`, `info`. Everything else answers 501. TUIs like
[lazydocker](https://github.com/jesseduffield/lazydocker) work against it
(`DOCKER_HOST=unix://$HOME/.cache/synodock/docker.sock lazydocker`).

`docker inspect` on a container returns its full config, environment secrets
included, as dockerd does.

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

`synodock/terminal.py` has the protocol in full; `docs/roadmap.md` has what's missing
compared with `docker exec` and how it could be done.

## License

MIT
