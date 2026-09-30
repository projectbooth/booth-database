"""A stand-in for booth-core's credential broker route, for the end-to-end test only.

It does exactly what core's real ``POST /api/credentials`` does to a request (booth-core
internal/credentialbroker/service.go + internal/api/credentialbroker.go, ADR 0088), minus OIDC:
resolve (subject, workspace, role) from the caller — here from a fixed token table instead of a
verified JWT — check ``X-Workspace`` against it, apply the one authorization rule (readwrite needs
editor/owner), clamp the TTL to the 5-minute ceiling, forward core's providerRequest shape to the
provider's fixed path with the provider credential, and relay the provider's answer (refusals
verbatim). Real core can't be used here: its broker routing needs the BoothModule CRD controller on a
real cluster (docs/decisions/0001 "How this was verified").
"""

from __future__ import annotations

import json
import threading
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MAX_TTL = 300
RANK = {"viewer": 1, "editor": 2, "owner": 3}


class FakeCore:
    def __init__(self, provider_url: str, provider_credential: str, tokens: dict[str, tuple[str, str, str]]) -> None:
        self.provider_url = provider_url
        self.provider_credential = provider_credential
        self.tokens = tokens  # bearer -> (subject, workspace, role)
        self.forwarded: list[dict] = []
        core = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):  # keep test output quiet
                pass

            def do_POST(self):
                if self.path != "/api/credentials":
                    return self._send(404, b"not found\n")
                ident = core.tokens.get(self.headers.get("Authorization", "").removeprefix("Bearer "))
                if not ident:
                    return self._send(401, b"invalid token\n")
                subject, workspace, role = ident
                if self.headers.get("X-Workspace") != workspace:
                    return self._send(403, b"not a member of that workspace\n")
                req = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if req.get("access") == "readwrite" and RANK[role] < RANK["editor"]:
                    return self._send(403, b"caller's role does not permit the requested access\n")
                ttl = req.get("ttlSeconds") or MAX_TTL
                fwd = {
                    "kind": req["kind"],
                    "ttlSeconds": min(ttl, MAX_TTL),
                    "access": req["access"],
                    "scope": req["scope"],
                    "requester": {"subject": subject, "workspace": workspace, "role": role},
                }
                if "options" in req:
                    fwd["options"] = req["options"]
                core.forwarded.append(fwd)
                out = urllib.request.Request(core.provider_url + "/internal/credentials", data=json.dumps(fwd).encode(), method="POST")
                out.add_header("Authorization", "Bearer " + core.provider_credential)
                out.add_header("Content-Type", "application/json")
                try:
                    with urllib.request.urlopen(out, timeout=10) as resp:
                        return self._send(201, resp.read(), "application/json")
                except urllib.error.HTTPError as e:
                    return self._send(e.code, e.read(), "application/json")

            def _send(self, code, body, ctype="text/plain"):
                self.send_response(code)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self._server.server_address[1]}"
        threading.Thread(target=self._server.serve_forever, daemon=True).start()

    def close(self) -> None:
        self._server.shutdown()
