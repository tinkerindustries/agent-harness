#!/usr/bin/env python3
"""Local logger for agy model traffic.

usage: capture_server.py OUTDIR [UPSTREAM_URL]

Without UPSTREAM_URL every request is logged and answered with a 500. With one,
each request is forwarded and the exchange is logged. Headers are never logged.
Listens on 127.0.0.1:18765 and writes OUTDIR/exchanges.jsonl.
"""
import gzip, http.server, json, os, sys, time, urllib.error, urllib.request

OUT = os.path.join(sys.argv[1], 'exchanges.jsonl')
UP = sys.argv[2].rstrip('/') if len(sys.argv) > 2 else None
SKIP = {'host', 'content-length', 'accept-encoding', 'connection', 'transfer-encoding'}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def read_body(self):
        if 'chunked' in (self.headers.get('Transfer-Encoding') or '').lower():
            body = b''
            while True:
                size = int(self.rfile.readline().strip().split(b';')[0] or b'0', 16)
                if size == 0:
                    while self.rfile.readline().strip():
                        pass
                    return body
                body += self.rfile.read(size)
                self.rfile.readline()
        return self.rfile.read(int(self.headers.get('Content-Length') or 0))

    def handle_any(self):
        body = self.read_body()
        plain = gzip.decompress(body) if self.headers.get('Content-Encoding', '').lower() == 'gzip' else body
        if UP:
            headers = {k: v for k, v in self.headers.items() if k.lower() not in SKIP}
            headers['Accept-Encoding'] = 'identity'
            req = urllib.request.Request(UP + self.path, data=body or None, headers=headers, method=self.command)
            try:
                r = urllib.request.urlopen(req, timeout=120)
                code, resp_headers, resp = r.status, r.headers, r.read()
            except urllib.error.HTTPError as e:
                code, resp_headers, resp = e.code, e.headers, e.read()
        else:
            code, resp_headers = 500, {'Content-Type': 'application/json'}
            resp = b'{"error":{"code":500,"message":"capture","status":"INTERNAL"}}'
        with open(OUT, 'ab') as f:
            f.write(json.dumps({'t': time.time(), 'method': self.command, 'path': self.path, 'status': code,
                                'req': plain.decode('utf-8', 'replace'),
                                'resp': resp.decode('utf-8', 'replace') if UP else ''}).encode() + b'\n')
        self.send_response(code)
        for k, v in resp_headers.items():
            if k.lower() not in SKIP:
                self.send_header(k, v)
        self.send_header('Content-Length', str(len(resp)))
        self.end_headers()
        self.wfile.write(resp)

    do_GET = do_POST = do_PUT = do_DELETE = handle_any

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(('127.0.0.1', 18765), Handler).serve_forever()
