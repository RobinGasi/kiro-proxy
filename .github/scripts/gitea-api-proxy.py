#!/usr/bin/env python3
"""Forward Gitea API calls with Git's Cloudflare-approved user agent."""

from __future__ import annotations

import argparse
import http.server
import urllib.error
import urllib.request

_HOP_BY_HOP_HEADERS = {
    "connection",
    "keep-alive",
    "proxy-authenticate",
    "proxy-authorization",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}


class ProxyHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    upstream = ""
    user_agent = ""

    def do_DELETE(self) -> None:
        self._forward()

    def do_GET(self) -> None:
        self._forward()

    def do_PATCH(self) -> None:
        self._forward()

    def do_POST(self) -> None:
        self._forward()

    def do_PUT(self) -> None:
        self._forward()

    def _forward(self) -> None:
        content_length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(content_length) if content_length else None
        headers = {
            name: value
            for name, value in self.headers.items()
            if name.lower() not in _HOP_BY_HOP_HEADERS
            and name.lower() not in {"host", "user-agent"}
        }
        headers["User-Agent"] = self.user_agent
        request = urllib.request.Request(
            f"{self.upstream}{self.path}",
            data=body,
            headers=headers,
            method=self.command,
        )

        try:
            response = urllib.request.urlopen(request)
        except urllib.error.HTTPError as error:
            response = error
        except Exception as error:
            message = str(error).encode()
            self.send_response(http.HTTPStatus.BAD_GATEWAY)
            self.send_header("Content-Type", "text/plain; charset=utf-8")
            self.send_header("Content-Length", str(len(message)))
            self.end_headers()
            self.wfile.write(message)
            return

        with response:
            response_body = response.read()
            self.send_response(response.status)
            for name, value in response.headers.items():
                if name.lower() not in _HOP_BY_HOP_HEADERS and name.lower() != "content-length":
                    self.send_header(name, value)
            self.send_header("Content-Length", str(len(response_body)))
            self.end_headers()
            self.wfile.write(response_body)

    def log_message(self, format: str, *args: object) -> None:
        print(f"gitea-api-proxy: {format % args}", flush=True)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=18080)
    parser.add_argument("--upstream", default="https://git.gasi.dev")
    parser.add_argument("--user-agent", default="git/2.54.0")
    args = parser.parse_args()

    ProxyHandler.upstream = args.upstream.rstrip("/")
    ProxyHandler.user_agent = args.user_agent
    server = http.server.ThreadingHTTPServer(("127.0.0.1", args.port), ProxyHandler)
    server.serve_forever()


if __name__ == "__main__":
    main()
