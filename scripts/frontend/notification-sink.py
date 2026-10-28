"""CI-only loopback SMTP capture of real customer verification notifications.

Launcher owns lifetime. MIME parsing uses stdlib; tokens stay in bounded memory,
never logs/files. A launcher capability protects recipient-specific proof reads.
"""
import email.policy
from email.parser import BytesParser
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import socketserver
import threading
from urllib.parse import parse_qs, urlsplit

if os.environ.get('GITHUB_ACTIONS') != 'true':
    raise RuntimeError('Notification sink requires CI')
CAPABILITY = json.loads(Path('.work/visual-real/runtime.json').read_text())['control']
MESSAGES = {}
LOCK = threading.Lock()


class SMTP(socketserver.StreamRequestHandler):
    def handle(self):
        self.connection.settimeout(10)
        self.wfile.write(b'220 visual notification sink\r\n')
        for _ in range(32):
            line = self.rfile.readline(4097)
            if not line or len(line) > 4096:
                return
            verb = line.split(b' ', 1)[0].strip().upper()
            if verb in (b'EHLO', b'HELO', b'MAIL', b'RCPT', b'RSET', b'NOOP'):
                self.wfile.write(b'250 OK\r\n')
            elif verb == b'DATA':
                self.wfile.write(b'354 End with dot\r\n')
                parts, size = [], 0
                while True:
                    part = self.rfile.readline(4097)
                    if part == b'.\r\n':
                        break
                    size += len(part)
                    if not part or len(part) > 4096 or size > 131072:
                        return
                    parts.append(part[1:] if part.startswith(b'..') else part)
                message = BytesParser(policy=email.policy.default).parsebytes(b''.join(parts))
                addresses = message['To'].addresses if message['To'] else []
                body = message.get_body(preferencelist=('plain',))
                proof = urlsplit(body.get_content().strip().split('\n')[-1]) if body else None
                fragment = parse_qs(proof.fragment) if proof else {}
                tokens = fragment.get('token', [])
                if (len(addresses) != 1 or not proof or proof.scheme != 'https'
                        or proof.netloc != 'store.visual.test' or proof.path != '/customer/verify'
                        or fragment.get('tenant') != ['c0900001-0000-4000-8000-000000000001']
                        or len(tokens) != 1 or not re.fullmatch(r'[A-Za-z0-9_-]{43}', tokens[0])):
                    self.wfile.write(b'550 Invalid notification\r\n')
                    continue
                with LOCK:
                    if len(MESSAGES) >= 128:
                        self.wfile.write(b'452 Sink full\r\n')
                        continue
                    MESSAGES[addresses[0].addr_spec] = tokens[0]
                self.wfile.write(b'250 Accepted\r\n')
            elif verb == b'QUIT':
                self.wfile.write(b'221 Bye\r\n')
                return
            else:
                self.wfile.write(b'502 Unsupported\r\n')


class HTTP(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def reply(self, status, data=None):
        self.send_response(status)
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        if data is not None:
            self.wfile.write(json.dumps(data).encode())

    def do_GET(self):
        self.reply(204 if self.path == '/health' else 404)

    def do_POST(self):
        if not hmac.compare_digest(self.headers.get('Authorization', ''), 'Bearer ' + CAPABILITY):
            self.reply(403)
            return
        try:
            size = int(self.headers.get('Content-Length', '0'))
            if self.path != '/proof' or not 0 < size <= 512:
                raise ValueError
            data = json.loads(self.rfile.read(size))
            address = data['email']
            if not isinstance(address, str) or len(address) > 254 or not address.endswith('.test'):
                raise ValueError
        except (ValueError, KeyError, TypeError):
            self.reply(400)
            return
        with LOCK:
            token = MESSAGES.get(address)
        self.reply(200, {'token': token}) if token else self.reply(404)


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


with Server(('127.0.0.1', 4183), SMTP) as smtp:
    threading.Thread(target=smtp.serve_forever, daemon=True).start()
    ThreadingHTTPServer(('127.0.0.1', 4182), HTTP).serve_forever()
