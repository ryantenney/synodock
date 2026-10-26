"""synodock: docker exec and a Docker API for Synology NAS containers, without SSH.

Author: Ryan Tenney

    synodock login [--otp CODE]          check the login; with --otp, save a 2FA device token
    synodock ps [-a]                     containers
    synodock logs NAME [-n 100]          a container's log, oldest first
    synodock exec [-i] [-t] [-d] NAME -- COMMAND...
    synodock serve [--socket PATH]       Docker Engine API on a Unix socket, for the docker CLI

Settings (SYNODOCK_URL, SYNODOCK_USER, ...) are described in synodock/client.py
and the README.
"""

import argparse
import sys

from . import __version__
from .client import CACHE_DIR, Client, Config, DsmError


def parser():
    ap = argparse.ArgumentParser(prog="synodock", description="docker exec and a Docker API for "
                                 "Synology Container Manager, over DSM's web API (no SSH)")
    ap.add_argument("--version", action="version", version=f"synodock {__version__}")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("login", help="check that the login works; with --otp, save a 2FA device token")
    p.add_argument("--otp", metavar="CODE", help="a current code from the authenticator app")

    p = sub.add_parser("ps", help="list containers")
    p.add_argument("-a", "--all", action="store_true", help="include stopped containers")

    p = sub.add_parser("logs", help="a container's log, oldest first")
    p.add_argument("name")
    p.add_argument("-n", "--tail", type=int, default=100, metavar="N", help="last N lines (default 100)")
    p.add_argument("-k", "--keyword", default="", help="only lines containing this")

    p = sub.add_parser("exec", help="run a command in a running container",
                       description="Runs through Container Manager's Terminal: always on a TTY, as the "
                                   "container's default user, so stderr comes back with stdout.")
    p.add_argument("-i", "--interactive", action="store_true", help="send stdin to the command")
    p.add_argument("-t", "--tty", action="store_true", help="with -i on a terminal: raw mode, resize, ^C goes through")
    p.add_argument("-d", "--detach", action="store_true", help="start it in the background and return; output discarded")
    p.add_argument("--raw", action="store_true", help="don't wrap in sh (images without one); no exit status")
    p.add_argument("--no-gzip", dest="gzip", action="store_false", default=None,
                   help="don't compress output in the container (SYNODOCK_GZIP=0 does the same)")
    p.add_argument("name")
    p.add_argument("argv", nargs=argparse.REMAINDER, metavar="command")

    p = sub.add_parser("serve", help="serve a Docker Engine API (subset) on a Unix socket, for the docker CLI")
    p.add_argument("--socket", default=str(CACHE_DIR / "docker.sock"), help="default: %(default)s")
    p.add_argument("-v", "--verbose", action="store_true", help="log each request")
    return ap


def main(argv=None):
    sys.stdout.reconfigure(line_buffering=True)
    args = parser().parse_args(argv)
    try:
        client = Client(Config.load())
    except DsmError as e:
        print(f"synodock: {e}", file=sys.stderr)
        return 2
    try:
        client.login(otp=getattr(args, "otp", None))
        return run(client, args)
    except DsmError as e:
        print(f"synodock: {e}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130
    finally:
        client.logout()


def run(client, args):
    if args.cmd == "login":
        print(f"logged in to {client.url} as {client.config.user}")

    elif args.cmd == "ps":
        rows = sorted(client.containers(), key=lambda c: c["name"])
        if not args.all:
            rows = [c for c in rows if (c.get("State") or {}).get("Running")]
        for c in rows:
            print(f"{c['name']:40} {c.get('status', ''):10} {c.get('image', '')}")

    elif args.cmd == "logs":
        data = client.container_logs(args.name, args.tail, args.keyword)
        for entry in reversed(data.get("logs", [])):
            print(f"{entry.get('created', '')} {entry.get('stream', ''):6} {entry.get('text', '').rstrip()}")

    elif args.cmd == "exec":
        from . import terminal
        argv = args.argv[1:] if args.argv[:1] == ["--"] else args.argv
        if not argv:
            raise DsmError("exec: no command given")
        return terminal.run(client, args.name, argv, stdin=args.interactive, tty=args.interactive and args.tty,
                            raw=args.raw, detach=args.detach, gzip=args.gzip)

    elif args.cmd == "serve":
        from . import docker_api
        return docker_api.serve(client, args.socket, args.verbose)
    return 0


if __name__ == "__main__":
    sys.exit(main())
