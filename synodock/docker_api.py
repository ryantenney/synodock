"""A Docker Engine API endpoint backed by Synology Container Manager's DSM Web API.

Author: Ryan Tenney

`synodock serve` serves this on a Unix socket only your user can open, so the
stock `docker` CLI works against the NAS with nothing listening on the network
and no SSH:

    docker context create synology --docker host=unix://$HOME/.cache/synodock/docker.sock
    docker --context synology ps -a

Every call is translated to the SYNO.Docker.* / FileStation calls Container
Manager's UI makes, so it covers what Container Manager's API can do:

    ps, inspect, start, stop, restart, kill, rm, logs (incl. -f, by polling),
    stats, images, image inspect, rmi, load, pull, network ls, exec,
    events (polled every 2s), volume ls (only volumes some container mounts)

exec goes through Container Manager's Terminal websocket (terminal.py): always a
TTY inside, so stderr comes back on stdout; no -u/--user. Plain `docker exec`
(no -i/-t) is framed, so output is in order and binary-safe, gzipped in the
container unless the server runs with SYNODOCK_GZIP=0; with -i or -t it's passed
through as it comes and can be out of order (see terminal.py). -d starts the
command in the background; inspect can't follow it after that.

Anything else (run/create, attach, cp, build, volume create/rm, compose) gets a
501 with a message instead of a guess.
"""

import datetime
import json
import os
import pathlib
import re
import secrets
import select
import signal
import socket
import socketserver
import struct
import sys
import tarfile
import tempfile
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler

from . import __version__, terminal
from .client import CACHE_DIR, DsmError, js

API_VERSION = "1.47"
MIN_API_VERSION = "1.24"
LOAD_FOLDER = "/docker/.synodock"  # where `docker load` tars are staged on the NAS
STATS_EVERY = 4  # seconds between DSM stats calls (each ~1.3s, all containers at once)
SIGNALS = {"SIGHUP": 1, "SIGINT": 2, "SIGQUIT": 3, "SIGKILL": 9, "SIGUSR1": 10, "SIGUSR2": 12, "SIGTERM": 15}


def tar_manifest(path):
    """(RepoTags, short image id) from a `docker save` tar; the id is the config blob's digest."""
    with tarfile.open(path) as t:
        entries = json.load(t.extractfile("manifest.json"))
    tags = [tag for entry in entries for tag in entry.get("RepoTags") or []]
    config = entries[0].get("Config", "") if entries else ""
    return tags, config.split("/")[-1].removesuffix(".json")[:12]


class ApiError(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status


def _ts(value):
    """DSM/Docker ISO timestamps (nanosecond precision) -> aware datetime, or None."""
    if not value or value.startswith("0001-"):
        return None
    m = re.match(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(\.\d+)?(Z|[+-]\d\d:\d\d)?", value)
    if not m:
        return None
    frac = (m.group(2) or ".0")[:7]
    tz = m.group(3) or "Z"
    return datetime.datetime.fromisoformat(m.group(1) + frac + ("+00:00" if tz == "Z" else tz))


def _ago(when):
    secs = int((datetime.datetime.now(datetime.timezone.utc) - when).total_seconds())
    for unit, size in (("days", 86400), ("hours", 3600), ("minutes", 60)):
        if secs >= 2 * size:
            return f"{secs // size} {unit}"
    if secs >= 3600:
        return "About an hour"
    if secs >= 60:
        return "About a minute"
    return f"{max(secs, 1)} seconds"


def _status_text(state):
    status = state.get("Status", "")
    if status == "running":
        started = _ts(state.get("StartedAt"))
        health = (state.get("Health") or {}).get("Status")
        text = f"Up {_ago(started)}" if started else "Up"
        return text + (f" ({health})" if health else "")
    if status == "exited":
        finished = _ts(state.get("FinishedAt"))
        return f"Exited ({state.get('ExitCode', 0)}) {_ago(finished)} ago" if finished else "Exited"
    return status.capitalize()


class Adapter:
    """Translates Engine API calls into DSM calls. One DSM session, shared."""

    def __init__(self, dsm):
        self.dsm = dsm
        self.execs = {}
        self.exec_lock = threading.Lock()
        self._cache = {}
        self._cache_lock = threading.Lock()
        self._fetch_locks = {}
        self._stats_pair = None
        self._volumes = None
        self._volumes_refresh = threading.Lock()

    def _cached(self, key, ttl, fetch):
        """Share one DSM answer among concurrent requests for `ttl` seconds: a
        dashboard like lazydocker streams stats for every container at once, and
        each sample would otherwise be two DSM calls. One fetch per key at a time;
        the others wait for it (a stats call takes ~1.3s, and 20 at once slowed
        every DSM call to ~5s)."""
        with self._cache_lock:
            fetch_lock = self._fetch_locks.setdefault(key, threading.Lock())
        with fetch_lock:
            with self._cache_lock:
                hit = self._cache.get(key)
                if hit and time.monotonic() - hit[0] < ttl:
                    return hit[1]
            value = fetch()
            with self._cache_lock:
                self._cache[key] = (time.monotonic(), value)
            return value

    def invalidate(self):
        with self._cache_lock:
            self._cache.clear()

    # --- lookups -----------------------------------------------------------

    def containers(self):
        return self._cached("containers", 1, self.dsm.containers)

    def container(self, ref):
        ref = ref.lstrip("/")
        found = [c for c in self.containers()
                 if ref in (c["name"], c["id"]) or (len(ref) >= 3 and c["id"].startswith(ref))]
        if len(found) != 1:
            raise ApiError(404 if not found else 400,
                           f"No such container: {ref}" if not found else f"Multiple containers match {ref}")
        return found[0]

    def images(self):
        return self._cached("images", 1, self.dsm.images)

    def image(self, ref):
        bare = ref.split(":", 1)[0] if ":" in ref and not ref.startswith("sha256:") else ref
        tag = ref.split(":", 1)[1] if bare != ref else "latest"
        short = ref.removeprefix("sha256:")
        for img in self.images():
            image_id = img["id"].removeprefix("sha256:")
            if (img["repository"] == bare and tag in (img.get("tags") or [])) or \
                    (len(short) >= 3 and image_id.startswith(short)):
                return img
        raise ApiError(404, f"No such image: {ref}")

    # --- containers --------------------------------------------------------

    def list_containers(self, query):
        show_all = query.get("all", ["0"])[0] in ("1", "true", "True")
        filters = json.loads(query.get("filters", ["{}"])[0] or "{}")
        out = []
        for c in self.containers():
            state = c.get("State") or {}
            if not show_all and not state.get("Running"):
                continue
            if not _matches(c, filters):
                continue
            networks = (c.get("NetworkSettings") or {}).get("Networks") or {}
            out.append({
                "Id": c["id"],
                "Names": ["/" + c["name"]],
                "Image": c.get("image") or c.get("Image", ""),
                "ImageID": c.get("ImageID", ""),
                "Command": c.get("cmd", ""),
                "Created": c.get("created", 0),
                "Ports": [],
                "Labels": c.get("Labels") or {},
                "State": state.get("Status", c.get("status", "")),
                "Status": _status_text(state),
                "HostConfig": {"NetworkMode": next(iter(networks), "default")},
                "NetworkSettings": {"Networks": networks},
                "Mounts": [],
            })
        return out

    def inspect_container(self, ref):
        return self.details(self.container(ref)["name"])

    def details(self, name):
        # lazydocker re-inspects every container a few times a second.
        return self._cached(("details", name), 2, lambda: self.dsm.call(
            "SYNO.Docker.Container", "get", name=js(name))["details"])

    def container_action(self, ref, action, query):
        c = self.container(ref)
        running = (c.get("State") or {}).get("Running")
        if action == "start" and running or action == "stop" and not running:
            return 304
        if action == "kill":
            sig = query.get("signal", ["SIGKILL"])[0].upper()
            num = int(sig) if sig.isdigit() else SIGNALS.get(sig if sig.startswith("SIG") else "SIG" + sig)
            if num is None:
                raise ApiError(400, f"unsupported signal {sig}")
            self.dsm.call("SYNO.Docker.Container", "signal", name=js(c["name"]), signal=str(num))
        else:
            self.dsm.container_action(c["name"], action)
        self.invalidate()
        return 204

    def remove_container(self, ref, query):
        c = self.container(ref)
        force = query.get("force", ["0"])[0] in ("1", "true", "True")
        if (c.get("State") or {}).get("Running") and not force:
            raise ApiError(409, f"You cannot remove a running container {c['id']}. Stop it first or use -f")
        self.dsm.call("SYNO.Docker.Container", "delete", name=js(c["name"]),
                      force=str(force).lower(), preserve_profile="false")
        self.invalidate()
        return 204

    def stats(self, ref):
        """One stats sample. CPU % is a delta and DSM's precpu_stats is always empty,
        so the previous all-containers sample stands in for it."""
        c = self.container(ref)
        current, previous = self._all_stats()
        s = dict(current.get(c["id"]) or {})
        s.setdefault("name", "/" + c["name"])
        s.setdefault("id", c["id"])
        s["read"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
        s["precpu_stats"] = (previous.get(c["id"]) or {}).get("cpu_stats", {})
        s.setdefault("pids_stats", {"current": s.get("num_procs", 0)})
        return s

    def _all_stats(self):
        """(latest, the one before) of DSM's stats, which come for every container in
        one call of ~1.3s. Fetched at most every STATS_EVERY seconds, so a dashboard
        streaming stats for 20 containers costs one call per interval."""
        def fetch():
            latest = self.dsm.call("SYNO.Docker.Container", "stats") or {}
            self._stats_pair = ((self._stats_pair or ({}, {}))[1], latest)
            return latest
        self._cached("stats", STATS_EVERY, fetch)
        if not self._stats_pair[0]:  # first ever: a delta needs a second sample
            time.sleep(1)
            with self._cache_lock:
                self._cache.pop("stats", None)
            self._cached("stats", STATS_EVERY, fetch)
        previous, latest = self._stats_pair
        return latest, previous

    def log_entries(self, name, tail, since):
        """Oldest-first log entries: the last `tail` (None = all), at or after `since`."""
        if tail is not None:
            page = self.dsm.call("SYNO.Docker.Container.Log", "get", name=js(name), limit=str(tail),
                                 offset="0", sort_dir=js("DESC"), keyword=js(""))
            entries = list(reversed(page.get("logs") or []))
        else:
            entries, offset = [], 0
            while True:
                page = self.dsm.call("SYNO.Docker.Container.Log", "get", name=js(name), limit="1000",
                                     offset=str(offset), sort_dir=js("ASC"), keyword=js(""))
                batch = page.get("logs") or []
                entries += batch
                offset += len(batch)
                if not batch or offset >= page.get("total", 0):
                    break
        if since:
            entries = [e for e in entries if (_ts(e["created"]) or since) >= since]
        return entries

    # --- images ------------------------------------------------------------

    def list_images(self):
        return [{
            "Id": img["id"],
            "ParentId": "",
            "RepoTags": [f"{img['repository']}:{t}" for t in img.get("tags") or []] or ["<none>:<none>"],
            "RepoDigests": [img["digest"]] if img.get("digest") else [],
            "Created": img.get("created", 0),
            "Size": img.get("size", 0),
            "VirtualSize": img.get("virtual_size", img.get("size", 0)),
            "SharedSize": -1,
            "Labels": {},
            "Containers": -1,
        } for img in self.images()]

    def inspect_image(self, ref):
        img = self.image(ref)
        return {
            "Id": img["id"],
            "RepoTags": [f"{img['repository']}:{t}" for t in img.get("tags") or []],
            "RepoDigests": [img["digest"]] if img.get("digest") else [],
            "Created": datetime.datetime.fromtimestamp(img.get("created", 0), datetime.timezone.utc).isoformat(),
            "Size": img.get("size", 0),
            "VirtualSize": img.get("virtual_size", img.get("size", 0)),
            "Os": "linux",
            "Architecture": "amd64",
            "Config": {},
            "RootFS": {"Type": "layers", "Layers": []},
            "Comment": img.get("description", ""),
        }

    def remove_image(self, ref):
        img = self.image(ref)
        if ref.removeprefix("sha256:") and img["id"].removeprefix("sha256:").startswith(ref.removeprefix("sha256:")):
            self.dsm.image_delete_id(img["id"])
            self.invalidate()
            return [{"Deleted": img["id"]}]
        repo = ref.split(":", 1)[0]
        tag = ref.split(":", 1)[1] if ":" in ref else "latest"
        self.dsm.call("SYNO.Docker.Image", "delete", images=js([{"repository": repo, "tags": [tag]}]))
        self.invalidate()
        return [{"Untagged": f"{repo}:{tag}"}]

    def load(self, tar_path, emit):
        tags, image_id = tar_manifest(tar_path)
        name = f"load-{os.getpid()}-{int(time.time())}.tar"
        staged = pathlib.Path(tar_path).with_name(name)
        os.replace(tar_path, staged)
        try:
            self.dsm.fs_put(staged, LOAD_FOLDER, create_parents=True)
        finally:
            staged.unlink(missing_ok=True)
        try:
            self.dsm.image_import(f"{LOAD_FOLDER}/{name}", expect={t: image_id for t in tags} if tags else None)
        finally:
            try:
                self.dsm.fs_delete(f"{LOAD_FOLDER}/{name}")
            except DsmError:
                pass
        self.invalidate()
        for tag in tags or [f"sha256:{image_id}"]:
            emit({"stream": f"Loaded image: {tag}\n"})

    def pull(self, from_image, tag, emit):
        # The CLI sends Docker Hub images fully qualified (docker.io/library/redis).
        from_image = re.sub(r"^(docker\.io|index\.docker\.io|registry-1\.docker\.io)/", "", from_image)
        if "@" in from_image or re.match(r"[^/]+[.:][^/]*/", from_image):
            raise ApiError(501, "the DSM adapter only pulls from Docker Hub by repository:tag")
        if ":" in from_image and not tag:
            from_image, tag = from_image.rsplit(":", 1)
        tag = tag or "latest"
        repo = from_image.removeprefix("library/")
        task = self.dsm.call("SYNO.Docker.Image", "pull_start", repository=js(repo), tag=js(tag),
                             registry=js(""), username=js(""), password=js(""))
        emit({"status": f"Pulling from {repo}", "id": tag})
        deadline = time.monotonic() + 3600
        while time.monotonic() < deadline:
            time.sleep(2)
            status = self.dsm.call("SYNO.Docker.Image", "pull_status", task_id=js(task["task_id"])) or {}
            if status.get("error") or status.get("errors"):
                raise ApiError(500, f"pull failed: {status.get('error') or status.get('errors')}")
            if status.get("finished"):
                self.invalidate()
                emit({"status": f"Status: Image is up to date for {repo}:{tag}"})
                return
            if status.get("downloaded"):
                emit({"status": "Downloading", "progressDetail": {"current": status["downloaded"]}, "id": tag})
        raise ApiError(504, "pull did not finish within an hour")

    # --- networks / system -------------------------------------------------

    def networks(self):
        return [{
            "Name": n["name"], "Id": n["id"], "Driver": n.get("driver", ""), "Scope": "local",
            "EnableIPv6": n.get("enable_ipv6", False), "Internal": False, "Attachable": False,
            "IPAM": {"Driver": "default", "Config": [{"Subnet": n.get("subnet", ""), "Gateway": n.get("gateway", "")}]},
            "Containers": {}, "Options": {}, "Labels": {},
        } for n in self.dsm.call("SYNO.Docker.Network", "list").get("network", [])]

    def volumes(self):
        """Named volumes, gathered from the containers that mount them: DSM has no
        volume list, so a volume no container uses doesn't show. That's one DSM call
        per container, ~50 queued behind everything else, so serve() gathers them at
        startup and a request gets the last list at once, refreshed in the background
        when it's over 30s old."""
        hit = self._volumes
        if hit is None:
            return self._gather_volumes()
        if time.monotonic() - hit[0] > 30 and self._volumes_refresh.acquire(blocking=False):
            def refresh():
                try:
                    self._gather_volumes()
                finally:
                    self._volumes_refresh.release()
            threading.Thread(target=refresh, daemon=True).start()
        return hit[1]

    def _gather_volumes(self):
        def gather():
            found = {}
            for c in self.containers():
                for m in self.details(c["name"]).get("Mounts") or []:
                    if m.get("Type") == "volume" and m.get("Name"):
                        found.setdefault(m["Name"], {
                            "Name": m["Name"], "Driver": m.get("Driver") or "local", "Mountpoint": m.get("Source", ""),
                            "Scope": "local", "Labels": {}, "Options": {}, "CreatedAt": ""})
            return sorted(found.values(), key=lambda v: v["Name"])
        value = gather()
        self._volumes = (time.monotonic(), value)  # kept apart from the cache invalidate() clears
        return value

    def snapshot(self):
        """What /events diffs: container id -> (name, image, status, started at),
        image id -> tags. StartedAt catches a restart quicker than the poll."""
        containers = {c["id"]: (c["name"], c.get("image") or c.get("Image", ""),
                                (c.get("State") or {}).get("Status") or c.get("status", ""),
                                (c.get("State") or {}).get("StartedAt", ""))
                      for c in self.containers()}
        images = {i["id"]: tuple(f"{i.get('repository', '')}:{t}" for t in (i.get("tags") or []))
                  for i in self.images()}
        return containers, images

    @staticmethod
    def events_between(old, new):
        """Docker-style events for what changed between two snapshots."""
        (old_c, old_i), (new_c, new_i) = old, new
        out = []

        def container(action, cid, name, image):
            out.append(("container", action, cid, {"name": name, "image": image}, image))

        for cid, (name, image, status, started) in new_c.items():
            before = old_c.get(cid)
            if before is None:
                container("create", cid, name, image)
                if status == "running":
                    container("start", cid, name, image)
            elif before[2] == status == "running" and before[3] != started:
                for action in ("die", "start", "restart"):
                    container(action, cid, name, image)
            elif before[2] != status:
                if status == "running":
                    container("unpause" if before[2] == "paused" else "start", cid, name, image)
                elif status == "paused":
                    container("pause", cid, name, image)
                elif before[2] in ("running", "paused"):
                    container("die", cid, name, image)
                    container("stop", cid, name, image)
        for cid, (name, image, *_) in old_c.items():
            if cid not in new_c:
                container("destroy", cid, name, image)
        for iid, tags in new_i.items():
            for tag in set(tags) - set(old_i.get(iid, ())):
                out.append(("image", "tag" if iid in old_i else "pull", iid, {"name": tag}, tag))
        for iid, tags in old_i.items():
            if iid not in new_i:
                out.append(("image", "delete", iid, {"name": tags[0] if tags else iid}, iid))
            else:
                for tag in set(tags) - set(new_i[iid]):
                    out.append(("image", "untag", iid, {"name": tag}, tag))
        now = time.time_ns()
        return [{"Type": kind, "Action": action, "Actor": {"ID": actor, "Attributes": attrs}, "scope": "local",
                 "time": now // 10**9, "timeNano": now,
                 **({"status": action, "id": actor, "from": frm} if kind == "container" else {})}
                for kind, action, actor, attrs, frm in out]

    def info(self):
        cs = self.containers()
        running = sum(1 for c in cs if (c.get("State") or {}).get("Running"))
        return {
            "ID": "synodock", "Name": urllib.parse.urlparse(self.dsm.url).hostname,
            "Containers": len(cs), "ContainersRunning": running, "ContainersPaused": 0,
            "ContainersStopped": len(cs) - running, "Images": len(self.images()),
            "ServerVersion": "Container Manager (via DSM Web API)", "OperatingSystem": "Synology DSM",
            "OSType": "linux", "Architecture": "x86_64", "Driver": "", "Plugins": {}, "Labels": [],
            "Warnings": ["Served by synodock: only a subset of the Engine API is supported"],
        }


class Exec:
    """An exec between create and inspect: what the CLI asked for, then its result."""

    def __init__(self, container, config):
        self.id = secrets.token_hex(32)
        self.container = container
        self.config = config
        self.running = False
        self.exit_code = None
        self.finished = None
        self.session = None
        self.size = None

    def argv(self):
        argv = list(self.config["Cmd"])
        if self.config.get("Env"):
            argv = ["env", *self.config["Env"], *argv]
        if self.config.get("WorkingDir"):
            argv = ["sh", "-c", 'cd "$1" || exit 126; shift; exec "$@"', "sh", self.config["WorkingDir"], *argv]
        return argv

    def resize(self, rows, cols):
        self.size = (rows, cols)
        session = self.session
        if session is not None and session.term is not None:
            session.resize(rows, cols)

    def inspect(self):
        cmd = self.config["Cmd"]
        return {"ID": self.id, "Running": self.running, "ExitCode": self.exit_code, "Pid": 0,
                "ContainerID": self.container["id"], "CanRemove": False, "DetachKeys": "",
                "OpenStdin": bool(self.config.get("AttachStdin")), "OpenStdout": True, "OpenStderr": True,
                "ProcessConfig": {"entrypoint": cmd[0], "arguments": cmd[1:], "tty": bool(self.config.get("Tty")),
                                  "privileged": False, "user": ""}}


def _matches(c, filters):
    if not filters:
        return True
    labels = c.get("Labels") or {}
    for key, wanted in filters.items():
        values = list(wanted) if isinstance(wanted, (list, dict)) else [wanted]
        if key == "name" and not any(v.lstrip("/") in c["name"] for v in values):
            return False
        if key == "id" and not any(c["id"].startswith(v) for v in values):
            return False
        if key == "status" and (c.get("State") or {}).get("Status") not in values:
            return False
        if key == "label":
            for v in values:
                k, _, val = v.partition("=")
                if k not in labels or (val and labels[k] != val):
                    return False
    return True


ROUTES = [
    ("GET", r"/_ping", "ping"), ("HEAD", r"/_ping", "ping"),
    ("GET", r"/version", "version"), ("GET", r"/info", "info"),
    ("GET", r"/containers/json", "containers_json"),
    ("GET", r"/containers/(?P<id>[^/]+)/json", "container_json"),
    ("GET", r"/containers/(?P<id>[^/]+)/logs", "container_logs"),
    ("GET", r"/containers/(?P<id>[^/]+)/stats", "container_stats"),
    ("POST", r"/containers/(?P<id>[^/]+)/(?P<action>start|stop|restart|kill)", "container_action"),
    ("DELETE", r"/containers/(?P<id>[^/]+)", "container_delete"),
    ("GET", r"/images/json", "images_json"),
    ("POST", r"/images/load", "images_load"),
    ("POST", r"/images/create", "images_create"),
    ("GET", r"/images/(?P<name>.+)/json", "image_json"),
    ("DELETE", r"/images/(?P<name>.+)", "image_delete"),
    ("GET", r"/networks", "networks_json"),
    ("GET", r"/volumes", "volumes_json"),
    ("GET", r"/events", "events"),
    ("POST", r"/containers/(?P<id>[^/]+)/exec", "exec_create"),
    ("POST", r"/exec/(?P<id>[0-9a-f]{64})/start", "exec_start"),
    ("POST", r"/exec/(?P<id>[0-9a-f]{64})/resize", "exec_resize"),
    ("GET", r"/exec/(?P<id>[0-9a-f]{64})/json", "exec_json"),
]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "synodock"
    adapter = None  # set by serve()

    def log_message(self, fmt, *args):
        if self.server.verbose:
            sys.stderr.write(f"{self.command} {self.path} -> {fmt % args}\n")

    def _dispatch(self):
        url = urllib.parse.urlsplit(self.path)
        path = re.sub(r"^/v1\.\d+", "", url.path)
        self.query = urllib.parse.parse_qs(url.query)
        for method, pattern, name in ROUTES:
            m = re.fullmatch(pattern, path)
            if m and method == self.command:
                try:
                    getattr(self, "do_" + name)(**m.groupdict())
                except ApiError as e:
                    self._json(e.status, {"message": str(e)})
                except DsmError as e:
                    self._json(502, {"message": f"DSM: {e}"})
                except (BrokenPipeError, ConnectionResetError):
                    pass
                return
        self._discard_body()
        self._json(501, {"message": f"{self.command} {path} is not supported by the DSM adapter "
                                    "(see synodock/docker_api.py for what is)"})

    do_GET = do_POST = do_DELETE = do_HEAD = do_PUT = _dispatch

    # --- responses ---------------------------------------------------------

    def _headers(self, status, content_type, length=None, chunked=False):
        self.send_response(status)
        self.send_header("Api-Version", API_VERSION)
        self.send_header("Docker-Experimental", "false")
        self.send_header("Ostype", "linux")
        if content_type:
            self.send_header("Content-Type", content_type)
        if chunked:
            self.send_header("Transfer-Encoding", "chunked")
        else:
            self.send_header("Content-Length", str(length or 0))
        self.end_headers()

    def _json(self, status, body):
        data = json.dumps(body).encode()
        self._headers(status, "application/json", len(data))
        if self.command != "HEAD":
            self.wfile.write(data)

    def _empty(self, status):
        self._headers(status, None, 0)

    def _chunk(self, data):
        if data:
            self.wfile.write(f"{len(data):x}\r\n".encode() + data + b"\r\n")
            self.wfile.flush()

    def _end_chunks(self):
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

    def _client_gone(self):
        """A follow stream with nothing to send never writes, so a disconnect
        would otherwise go unnoticed and the poll loop would run forever."""
        readable, _, _ = select.select([self.connection], [], [], 0)
        if not readable:
            return False
        try:
            return self.connection.recv(1, socket.MSG_PEEK) == b""
        except OSError:
            return True

    def _discard_body(self):
        for _ in self._body_chunks():
            pass

    def _body_chunks(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            while True:
                size = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
                if size == 0:
                    self.rfile.readline()
                    return
                remaining = size
                while remaining:
                    piece = self.rfile.read(min(remaining, 1 << 20))
                    remaining -= len(piece)
                    yield piece
                self.rfile.readline()
        else:
            remaining = int(self.headers.get("Content-Length") or 0)
            while remaining:
                piece = self.rfile.read(min(remaining, 1 << 20))
                if not piece:
                    return
                remaining -= len(piece)
                yield piece

    # --- endpoints ---------------------------------------------------------

    def do_ping(self):
        self._headers(200, "text/plain", 2)
        if self.command != "HEAD":
            self.wfile.write(b"OK")

    def do_version(self):
        self._json(200, {"Version": __version__, "ApiVersion": API_VERSION, "MinAPIVersion": MIN_API_VERSION,
                         "Os": "linux", "Arch": "amd64", "KernelVersion": "", "GitCommit": "",
                         "GoVersion": "", "BuildTime": "",
                         "Components": [{"Name": "Engine", "Version": __version__,
                                         "Details": {"ApiVersion": API_VERSION}}]})

    def do_info(self):
        self._json(200, self.adapter.info())

    def do_containers_json(self):
        self._json(200, self.adapter.list_containers(self.query))

    def do_container_json(self, id):
        self._json(200, self.adapter.inspect_container(id))

    def do_container_action(self, id, action):
        self._discard_body()
        self._empty(self.adapter.container_action(id, action, self.query))

    def do_container_delete(self, id):
        self._empty(self.adapter.remove_container(id, self.query))

    def do_container_stats(self, id):
        if self.query.get("stream", ["1"])[0] in ("0", "false", "False"):
            self._json(200, self.adapter.stats(id))
            return
        self._headers(200, "application/json", chunked=True)
        while not self._client_gone():
            self._chunk(json.dumps(self.adapter.stats(id)).encode() + b"\n")
            time.sleep(STATS_EVERY)

    def do_container_logs(self, id):
        q = self.query
        want = {"stdout": q.get("stdout", ["0"])[0] in ("1", "true"),
                "stderr": q.get("stderr", ["0"])[0] in ("1", "true")}
        tail = q.get("tail", ["all"])[0]
        tail = None if tail in ("all", "") else int(tail)
        since = q.get("since", ["0"])[0]
        since = datetime.datetime.fromtimestamp(float(since), datetime.timezone.utc) if float(since or 0) else None
        timestamps = q.get("timestamps", ["0"])[0] in ("1", "true")
        follow = q.get("follow", ["0"])[0] in ("1", "true")

        c = self.adapter.container(id)
        tty = bool(((self.adapter.inspect_container(c["id"]) or {}).get("Config") or {}).get("Tty"))
        self._headers(200, "application/vnd.docker.raw-stream" if tty else "application/vnd.docker.multiplexed-stream",
                      chunked=True)

        def emit(entries):
            out = b""
            for e in entries:
                stream = e.get("stream", "stdout")
                if not want.get(stream, False):
                    continue
                text = e.get("text", "")
                if not text.endswith("\n"):
                    text += "\n"
                if timestamps:
                    text = f"{e['created']} {text}"
                payload = text.encode()
                out += payload if tty else struct.pack(">BxxxI", 2 if stream == "stderr" else 1, len(payload)) + payload
            self._chunk(out)

        entries = self.adapter.log_entries(c["name"], tail, since)
        emit(entries)
        last = max((e["created"] for e in entries), default="")
        while follow and not self._client_gone():
            # Container Manager's log API is a paged store, not a stream: poll it.
            time.sleep(1)
            page = self.adapter.dsm.call("SYNO.Docker.Container.Log", "get", name=js(c["name"]), limit="500",
                                         offset="0", sort_dir=js("DESC"), keyword=js(""))
            fresh = [e for e in reversed(page.get("logs") or []) if e["created"] > last]
            if fresh:
                emit(fresh)
                last = fresh[-1]["created"]
        self._end_chunks()

    def do_images_json(self):
        self._json(200, self.adapter.list_images())

    def do_image_json(self, name):
        self._json(200, self.adapter.inspect_image(urllib.parse.unquote(name)))

    def do_image_delete(self, name):
        self._json(200, self.adapter.remove_image(urllib.parse.unquote(name)))

    def do_images_load(self):
        tmp_dir = CACHE_DIR / "tmp"
        tmp_dir.mkdir(parents=True, exist_ok=True)
        fd, tmp = tempfile.mkstemp(suffix=".tar", dir=tmp_dir)
        try:
            with os.fdopen(fd, "wb") as f:
                for piece in self._body_chunks():
                    f.write(piece)
            self._headers(200, "application/json", chunked=True)
            self.adapter.load(tmp, lambda msg: self._chunk(json.dumps(msg).encode() + b"\n"))
        except (ApiError, DsmError) as e:
            self._chunk(json.dumps({"errorDetail": {"message": str(e)}, "error": str(e)}).encode() + b"\n")
        finally:
            pathlib.Path(tmp).unlink(missing_ok=True)
        self._end_chunks()

    def do_images_create(self):
        self._discard_body()
        if "fromSrc" in self.query:
            raise ApiError(501, "docker import is not supported by the DSM adapter")
        from_image = self.query.get("fromImage", [""])[0]
        tag = self.query.get("tag", [""])[0]
        self._headers(200, "application/json", chunked=True)
        try:
            self.adapter.pull(from_image, tag, lambda msg: self._chunk(json.dumps(msg).encode() + b"\n"))
        except (ApiError, DsmError) as e:
            self._chunk(json.dumps({"errorDetail": {"message": str(e)}, "error": str(e)}).encode() + b"\n")
        self._end_chunks()

    def do_networks_json(self):
        self._json(200, self.adapter.networks())

    def do_volumes_json(self):
        self._json(200, {"Volumes": self.adapter.volumes(),
                         "Warnings": ["only volumes some container mounts are listed (DSM has no volume list)"]})

    def do_events(self):
        """Container Manager has no event stream: poll it every 2s and report what
        changed, from now on (no replay of --since)."""
        filters = json.loads(self.query.get("filters", ["{}"])[0] or "{}")
        until = self.query.get("until", [""])[0]
        deadline = float(until) if until else None

        def wanted(e):
            for key, values in filters.items():
                values = list(values) if isinstance(values, (list, dict)) else [values]
                attrs = e["Actor"]["Attributes"]
                if key == "type" and e["Type"] not in values:
                    return False
                if key == "event" and e["Action"] not in values:
                    return False
                if key == "container" and (e["Type"] != "container" or not any(
                        v in (attrs.get("name"), e["Actor"]["ID"]) or e["Actor"]["ID"].startswith(v) for v in values)):
                    return False
                if key == "image" and not any(v in (attrs.get("image"), attrs.get("name")) for v in values):
                    return False
            return True

        self._headers(200, "application/json", chunked=True)
        last = self.adapter.snapshot()
        while not self._client_gone() and (deadline is None or time.time() < deadline):
            time.sleep(2)
            current = self.adapter.snapshot()
            for event in self.adapter.events_between(last, current):
                if wanted(event):
                    self._chunk(json.dumps(event).encode() + b"\n")
            last = current
        self._end_chunks()

    # --- exec --------------------------------------------------------------

    def _exec(self, id):
        with self.adapter.exec_lock:
            ex = self.adapter.execs.get(id)
        if ex is None:
            raise ApiError(404, f"No such exec instance: {id}")
        return ex

    def do_exec_create(self, id):
        config = json.loads(b"".join(self._body_chunks()) or b"{}")
        if not config.get("Cmd"):
            raise ApiError(400, "No exec command specified")
        if config.get("User"):
            raise ApiError(501, "exec --user is not supported by the DSM adapter (Container Manager's "
                                "terminal runs as the container's user)")
        if config.get("Privileged"):
            raise ApiError(501, "exec --privileged is not supported by the DSM adapter")
        c = self.adapter.container(id)
        if c.get("status") != "running":
            raise ApiError(409, f"Container {c['name']} is not running")
        ex = Exec(c, config)
        with self.adapter.exec_lock:
            cutoff = time.time() - 600
            for old in [k for k, v in self.adapter.execs.items() if v.finished and v.finished < cutoff]:
                del self.adapter.execs[old]
            self.adapter.execs[ex.id] = ex
        self._json(201, {"Id": ex.id})

    def do_exec_resize(self, id):
        self._discard_body()
        ex = self._exec(id)
        rows, cols = int(self.query.get("h", ["0"])[0]), int(self.query.get("w", ["0"])[0])
        if rows and cols:
            ex.resize(rows, cols)
        self._empty(200)

    def do_exec_json(self, id):
        self._json(200, self._exec(id).inspect())

    def do_exec_start(self, id):
        body = json.loads(b"".join(self._body_chunks()) or b"{}")
        ex = self._exec(id)
        with self.adapter.exec_lock:
            if ex.running or ex.finished:
                raise ApiError(409, f"exec {id} has already been started")
            ex.running = True
        if body.get("Detach"):
            return self._exec_detached(ex)
        tty = bool(ex.config.get("Tty"))
        stdin = bool(ex.config.get("AttachStdin"))
        if body.get("ConsoleSize"):
            ex.size = tuple(body["ConsoleSize"])

        # Hijack the connection, as dockerd does: from here on it's a raw stream.
        self.close_connection = True
        upgrade = self.headers.get("Upgrade", "").lower() == "tcp"
        self.send_response(101 if upgrade else 200, "UPGRADED" if upgrade else None)
        self.send_header("Content-Type", "application/vnd.docker.raw-stream" if tty
                         else "application/vnd.docker.multiplexed-stream")
        if upgrade:
            self.send_header("Connection", "Upgrade")
            self.send_header("Upgrade", "tcp")
        self.end_headers()
        self.wfile.flush()

        def write(data, stream=1):
            if isinstance(data, str):
                data = data.encode()
            if data:
                self.wfile.write(data if tty else struct.pack(">BxxxI", stream, len(data)) + data)
                self.wfile.flush()

        def read():  # stdin from the CLI; b"" once it half-closes
            try:
                return self.rfile.read1(4096)
            except OSError:
                return b""

        code = 126
        try:
            framed = not tty and not stdin
            rows, cols = ex.size or ((24, 80) if tty else (50, 200))
            command = terminal.command_for(ex.argv(), framed=framed, interactive=tty, stdin=stdin,
                                           gzip=terminal.gzip_default())
            if not framed and not tty:
                write("synodock exec: unframed mode (-i), output may arrive out of order\n", stream=2)
            with terminal.Session(self.adapter.dsm, ex.container["name"], command, rows, cols) as session:
                ex.session = session
                if ex.size and ex.size != (rows, cols):
                    session.resize(*ex.size)
                if framed:
                    code = terminal.pump_framed(session, write)
                else:
                    chunks = terminal._text_chunks(read) if stdin else None
                    result = terminal.pump_unframed(session, write, chunks, wait_ready=stdin and not tty,
                                                    send_eof=not tty)
                    code = 255 if result is None else result
        except DsmError as e:
            write(f"synodock exec: {e}\n", stream=2)
        finally:
            ex.session = None
            ex.exit_code = code
            ex.running = False
            ex.finished = time.time()

    def _exec_detached(self, ex):
        """docker exec -d: start it in the background and answer at once, no stream.
        Nothing can see it after that, so inspect reports it as not running with no
        exit code; a command that doesn't exist fails here, as with dockerd."""
        out = []
        try:
            command = terminal.command_for(ex.argv(), detach=True)
            with terminal.Session(self.adapter.dsm, ex.container["name"], command) as session:
                code = terminal.pump_framed(session, out.append)
        finally:
            ex.running = False
            ex.finished = time.time()
        if code:
            ex.exit_code = code
            raise ApiError(500, (b"".join(out).decode(errors="replace").strip()
                                 or f"exec failed with status {code}"))
        self._empty(200)


class UnixServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    verbose = False

    def get_request(self):
        request, _ = super().get_request()
        return request, ("local", 0)  # BaseHTTPRequestHandler wants a (host, port)


def serve(dsm, socket_path, verbose=False):
    socket_path = pathlib.Path(socket_path).expanduser()
    socket_path.parent.mkdir(parents=True, exist_ok=True)
    socket_path.unlink(missing_ok=True)
    Handler.adapter = Adapter(dsm)
    # Gather volumes before any client asks: it's ~50 DSM calls (see Adapter.volumes).
    threading.Thread(target=lambda: Handler.adapter.volumes(), daemon=True).start()
    old_umask = os.umask(0o177)  # socket is 0600: only this user can talk to it
    try:
        server = UnixServer(str(socket_path), Handler)
    finally:
        os.umask(old_umask)
    server.verbose = verbose
    # SIGTERM (kill, launchd) should clean up the socket like Ctrl-C does. Ignore any
    # repeat (uv run forwards the signal it also received) so cleanup isn't cut short.
    def on_term(*_):
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, on_term)
    print(f"Docker API for {dsm.url} on unix://{socket_path}", file=sys.stderr)
    print(f"  docker context create synology --docker host=unix://{socket_path}", file=sys.stderr)
    print("  docker --context synology ps -a", file=sys.stderr)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        socket_path.unlink(missing_ok=True)
    return 0
