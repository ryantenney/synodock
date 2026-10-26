"""A client for the DSM Web API calls Container Manager's UI makes.

Author: Ryan Tenney

The SYNO.Docker.* endpoints are undocumented: this speaks to them the way the
Container Manager UI does, so everything stays in step with the UI.

Settings, each from the environment or ~/.config/synodock/config.toml:

    SYNODOCK_URL       url = "https://nas.example.com:5001"   (required)
    SYNODOCK_USER      user = "docker"                         (required)
    SYNODOCK_KEYCHAIN  keychain = "synodock"   macOS keychain service for the password
                                               and the 2FA device token (account = user)

The password comes from SYNODOCK_PASSWORD, then the macOS keychain, then a
prompt. With 2FA, `synodock login --otp <code>` mints a device token once
(kept in the keychain on macOS, else in ~/.config/synodock/, mode 0600), and
later logins need no code.
"""

import contextlib
import getpass
import io
import json
import os
import pathlib
import shutil
import ssl
import subprocess
import sys
import threading
import time
import tomllib
import urllib.error
import urllib.parse
import urllib.request
import uuid

CONFIG_DIR = pathlib.Path(os.environ.get("XDG_CONFIG_HOME") or pathlib.Path.home() / ".config") / "synodock"
CACHE_DIR = pathlib.Path(os.environ.get("XDG_CACHE_HOME") or pathlib.Path.home() / ".cache") / "synodock"
TRACE = os.environ.get("SYNODOCK_TRACE") == "1"  # log each DSM call with its queue wait and duration

try:
    # Verify with the OS trust store (what browsers use). uv's standalone Pythons
    # ship no CA bundle, so the default context can't verify anything under uv.
    import truststore
    SSL_CTX = truststore.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
except ImportError:
    # OpenSSL with the interpreter's own bundle. DSM's certificate chain can lack an
    # Authority Key Identifier, which 3.13+ rejects under VERIFY_X509_STRICT; chain
    # and hostname are still verified.
    SSL_CTX = ssl.create_default_context()
    SSL_CTX.verify_flags &= ~ssl.VERIFY_X509_STRICT


class DsmError(Exception):
    def __init__(self, message, code=None):
        super().__init__(message)
        self.code = code


# DSM's generic "network unstable or system busy" codes: the backend gave up on the
# request, which for a long operation doesn't mean the operation itself failed.
BUSY_CODES = {109, 110, 111, 117, 118}


def js(value):
    """SYNO.Docker.* APIs take string params JSON-encoded (quoted), as the UI sends them."""
    return json.dumps(value)


class Config:
    def __init__(self, url, user, keychain="synodock"):
        if not url:
            raise DsmError("no NAS address: set SYNODOCK_URL or url in "
                           f"{CONFIG_DIR / 'config.toml'} (e.g. https://nas.example.com:5001)")
        if not user:
            raise DsmError(f"no DSM account: set SYNODOCK_USER or user in {CONFIG_DIR / 'config.toml'}")
        self.url = url.rstrip("/")
        self.user = user
        self.keychain = keychain

    @classmethod
    def load(cls):
        path = CONFIG_DIR / "config.toml"
        file = tomllib.loads(path.read_text()) if path.exists() else {}
        return cls(url=os.environ.get("SYNODOCK_URL") or file.get("url"),
                   user=os.environ.get("SYNODOCK_USER") or file.get("user"),
                   keychain=os.environ.get("SYNODOCK_KEYCHAIN") or file.get("keychain") or "synodock")


class Secrets:
    """The password and the 2FA device token: macOS keychain where there is one."""

    def __init__(self, config):
        self.config = config
        self.has_keychain = sys.platform == "darwin" and shutil.which("security") is not None

    def _keychain_get(self, service):
        r = subprocess.run(["security", "find-generic-password", "-s", service, "-a", self.config.user, "-w"],
                           capture_output=True, text=True)
        return r.stdout.rstrip("\n") if r.returncode == 0 else None

    def password(self):
        if os.environ.get("SYNODOCK_PASSWORD"):
            return os.environ["SYNODOCK_PASSWORD"]
        if self.has_keychain:
            found = self._keychain_get(self.config.keychain)
            if found is not None:
                return found
        if sys.stdin.isatty():
            return getpass.getpass(f"DSM password for {self.config.user}: ")
        where = f" or keychain item service={self.config.keychain} account={self.config.user}" \
            if self.has_keychain else ""
        raise DsmError(f"no password: set SYNODOCK_PASSWORD{where}")

    def _device_file(self):
        host = urllib.parse.urlsplit(self.config.url).hostname
        return CONFIG_DIR / f"device-{self.config.user}@{host}"

    def device_token(self):
        if self.has_keychain:
            return self._keychain_get(f"{self.config.keychain}-device")
        path = self._device_file()
        return path.read_text().strip() if path.exists() else None

    def save_device_token(self, token):
        if self.has_keychain:
            subprocess.run(["security", "add-generic-password", "-U", "-s", f"{self.config.keychain}-device",
                            "-a", self.config.user, "-w", token], check=True, capture_output=True)
            return f"keychain ({self.config.keychain}-device)"
        CONFIG_DIR.mkdir(parents=True, exist_ok=True)
        path = self._device_file()
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(token)
        return str(path)


class Client:
    # DSM makes overlapping SYNO.Docker calls wait in ~5s steps: two concurrent
    # Container get take 5.1s each instead of 0.1s, and under load list, stats and
    # log calls slow the same way. `synodock serve` handles many requests at once,
    # so calls queue here, one at a time, where waiting costs nothing. Slow calls
    # (an image import runs for minutes) don't take the lock.
    _call_lock = threading.Lock()

    def __init__(self, config=None):
        self.config = config or Config.load()
        self.url = self.config.url
        self.secrets = Secrets(self.config)
        self.sid = None
        self.token = None

    def _one_at_a_time(self, slow):
        return contextlib.nullcontext() if slow else self._call_lock

    def request(self, params, data=None, content_type=None, stream=False, length=None):
        if self.sid:
            params = {**params, "_sid": self.sid}
        url = f"{self.url}/webapi/entry.cgi"
        if data is None:
            data = urllib.parse.urlencode(params).encode()
            content_type = "application/x-www-form-urlencoded"
        else:
            url += "?" + urllib.parse.urlencode(params)
        req = urllib.request.Request(url, data=data, headers={"Content-Type": content_type})
        if length is not None:
            req.add_header("Content-Length", str(length))
        if self.token:
            req.add_header("X-SYNO-TOKEN", self.token)
        return urllib.request.urlopen(req, timeout=None if stream else 60, context=SSL_CTX)

    def call(self, api, method, version=1, slow=False, **params):
        all_params = {"api": api, "method": method, "version": str(version), **params}
        queued = time.monotonic()
        with self._one_at_a_time(slow):
            started = time.monotonic()
            with self.request(all_params, stream=slow) as r:
                body = json.load(r)
        if TRACE:
            print(f"synodock: {api}.{method} waited {started - queued:.2f}s, took {time.monotonic() - started:.2f}s",
                  file=sys.stderr)
        # 119 = session not found (seen transiently right after login), 106 = session
        # timed out (a long-lived server idles past it): log in again, once.
        if not body.get("success") and (body.get("error") or {}).get("code") in (106, 119) and api != "SYNO.API.Auth":
            self.login()
            with self._one_at_a_time(slow), self.request(all_params, stream=slow) as r:
                body = json.load(r)
        if not body.get("success"):
            raise DsmError(f"{api}.{method} failed: {body.get('error')}", (body.get("error") or {}).get("code"))
        return body.get("data")

    def login(self, otp=None):
        params = {"api": "SYNO.API.Auth", "version": "6", "method": "login", "account": self.config.user,
                  "passwd": self.secrets.password(), "session": "ContainerManager", "format": "sid",
                  "enable_syno_token": "yes", "device_name": "synodock"}
        if otp:
            params.update({"otp_code": otp, "enable_device_token": "yes"})
        else:
            device_id = self.secrets.device_token()
            if device_id:
                params["device_id"] = device_id
        with self.request(params) as r:
            body = json.load(r)
        if not body.get("success"):
            code = (body.get("error") or {}).get("code")
            hint = {400: "wrong account or password", 403: "2FA code required (run: synodock login --otp <code>)",
                    404: "2FA code rejected", 406: "the account must enroll in 2FA first"}.get(code, "")
            raise DsmError(f"login failed: code {code} {hint}".rstrip())
        self.sid = body["data"]["sid"]
        self.token = body["data"].get("synotoken")
        if otp and body["data"].get("did"):
            where = self.secrets.save_device_token(body["data"]["did"])
            print(f"device token saved to {where}; later logins need no code", file=sys.stderr)

    def logout(self):
        if self.sid:
            try:
                self.call("SYNO.API.Auth", "logout", 6, session="ContainerManager")
            except Exception:
                pass

    # --- projects -------------------------------------------------------

    def projects(self):
        return list(self.call("SYNO.Docker.Project", "list").values())

    def project(self, name_or_id):
        for p in self.projects():
            if name_or_id in (p["name"], p["id"]):
                return p
        raise DsmError(f"no project named {name_or_id!r}")

    def project_get(self, project_id):
        """Careful: its containers[] is full docker inspect, env secrets included."""
        return self.call("SYNO.Docker.Project", "get", id=js(project_id))

    # --- containers -----------------------------------------------------

    def containers(self):
        return self.call("SYNO.Docker.Container", "list", limit="-1", offset="0", type=js("all"))["containers"]

    def container_action(self, name, action):
        return self.call("SYNO.Docker.Container", action, name=js(name))

    def container_logs(self, name, limit, keyword=""):
        return self.call("SYNO.Docker.Container.Log", "get", name=js(name), limit=str(limit),
                         offset="0", sort_dir=js("DESC"), keyword=js(keyword))

    # --- file station ---------------------------------------------------

    def fs_list(self, folder):
        return self.call("SYNO.FileStation.List", "list", 2, folder_path=folder,
                         additional=js(["size", "time"]))["files"]

    def fs_get(self, path):
        params = {"api": "SYNO.FileStation.Download", "version": "2", "method": "download",
                  "path": js([path]), "mode": "download"}
        with self.request(params) as r:
            body = r.read()
            if r.headers.get_content_type() == "application/json":
                err = json.loads(body)
                if not err.get("success", True):
                    raise DsmError(f"download failed: {err.get('error')}")
        return body

    def fs_names(self, folder):
        try:
            return {f["name"] for f in self.fs_list(folder)}
        except DsmError:  # folder doesn't exist yet
            return set()

    def fs_delete(self, path):
        self.call("SYNO.FileStation.Delete", "delete", 2, path=js([path]), recursive="false")

    def fs_rename(self, path, new_name):
        self.call("SYNO.FileStation.Rename", "rename", 2, path=js([path]), name=js([new_name]))

    def fs_put(self, local_path, folder, create_parents=False, overwrite=True):
        path = pathlib.Path(local_path)
        with path.open("rb") as f:
            self._upload(f, path.stat().st_size, path.name, folder, create_parents, overwrite)

    def fs_put_bytes(self, content, filename, folder, create_parents=False, overwrite=True):
        self._upload(io.BytesIO(content), len(content), filename, folder, create_parents, overwrite)

    def _upload(self, fileobj, size, filename, folder, create_parents, overwrite):
        # Streamed from disk: image tars run to hundreds of MB.
        boundary = uuid.uuid4().hex
        fields = {"path": folder, "create_parents": str(create_parents).lower(),
                  "overwrite": str(overwrite).lower()}
        head = b"".join(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
                        for k, v in fields.items())
        head += (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
                 f'Content-Type: application/octet-stream\r\n\r\n').encode()
        tail = f"\r\n--{boundary}--\r\n".encode()
        body = _ChainedReader([io.BytesIO(head), fileobj, io.BytesIO(tail)],
                              total=len(head) + size + len(tail), label=filename)
        params = {"api": "SYNO.FileStation.Upload", "version": "2", "method": "upload"}
        with self.request(params, data=body, content_type=f"multipart/form-data; boundary={boundary}",
                          stream=True, length=len(head) + size + len(tail)) as r:
            result = json.load(r)
        if not result.get("success"):
            raise DsmError(f"upload failed: {result.get('error')}")

    # --- images ---------------------------------------------------------

    def images(self):
        return self.call("SYNO.Docker.Image", "list", limit="-1", offset="0", show_dsm="false")["images"]

    def image_import(self, nas_path, expect=None, wait=1800):
        """Load an image tar already on the NAS (Container Manager > Image > Add > Add From File).

        `expect` is {repo:tag: short image id}. A multi-GB load can outlast DSM's proxy
        timeout while carrying on server-side, so if the request itself fails, poll the
        image list until every tag points at its expected id."""
        try:
            return self.call("SYNO.Docker.Image", "import", slow=True, path=js(nas_path))
        except (urllib.error.URLError, TimeoutError, ConnectionError, json.JSONDecodeError, DsmError) as e:
            if not expect or (isinstance(e, DsmError) and e.code not in BUSY_CODES):
                raise
            print(f"import request ended early ({e}); waiting for the load to finish on the NAS",
                  file=sys.stderr)
        deadline = time.monotonic() + wait
        while time.monotonic() < deadline:
            time.sleep(15)
            try:
                current = {tag: image_id.split(":")[-1][:12] for tag, image_id in self.image_ids_by_tag().items()}
            except (urllib.error.URLError, DsmError):
                continue
            if all(current.get(tag) == image_id for tag, image_id in expect.items()):
                return {}
        raise DsmError(f"{nas_path} was not loaded within {wait}s")

    def last_docker_error(self):
        logs = self.call("SYNO.Docker.Log", "list", action=js("load"), offset="0", limit="5", sort_by=js("time"),
                         sort_dir=js("DESC"), loglevel=js(""), filter_content=js(""), datefrom="0", dateto="0")
        errors = [e.get("event") for e in logs.get("logs", []) if e.get("level") == "err"]
        return errors[0] if errors else None

    def image_ids_by_tag(self):
        # An image replaced by a load or pull loses its tag (and drops out of this map) while
        # still holding disk space until deleted by id.
        return {f"{img['repository']}:{tag}": img["id"] for img in self.images() for tag in img.get("tags") or []}

    def image_delete_id(self, image_id):
        # Short (12 hex) ids are what's been verified to work; "sha256:..." is not.
        self.call("SYNO.Docker.Image", "delete", images=js([{"identity": image_id.split(":")[-1][:12]}]))


class _ChainedReader(io.RawIOBase):
    """File-like concatenation of readers, so http.client streams the multipart body."""

    def __init__(self, readers, total=0, label=""):
        self.readers = list(readers)
        self.total, self.label, self.sent, self.reported = total, label, 0, 0

    def readable(self):
        return True

    def read(self, n=-1):
        while self.readers:
            chunk = self.readers[0].read(n if n and n > 0 else 1 << 20)
            if chunk:
                self._progress(len(chunk))
                return chunk
            self.readers.pop(0)
        return b""

    def _progress(self, size):
        # Only worth reporting for big uploads (image tars), every 10%.
        self.sent += size
        if self.total < 256 << 20:
            return
        pct = self.sent * 100 // self.total
        if pct >= self.reported + 10:
            self.reported = pct - pct % 10
            print(f"  {self.label}: {self.reported}% of {self.total / 1e9:.1f} GB", file=sys.stderr, flush=True)
