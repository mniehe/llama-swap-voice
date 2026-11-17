"""A strict stand-in for llama-swap in front of one TTS backend and the ASR model.

It accepts only the requests the service's internal/upstream package sends:
speech for the chosen backend, answered with a fixed WAV, and transcriptions,
answered with a fixed transcript. Every request is appended to a JSON-lines
record; all but transcriptions keep their raw body for the test to inspect.

A body goes to its own file, named by its SHA-256, in the directory
`<record>.bodies`, and the record holds only the digest. The test driver
reads guest output over a console that drops bytes when the host is slow,
so a record carrying the clips would arrive damaged under load.
"""

import argparse
import email.parser
import email.policy
import hashlib
import io
import json
import os
import threading
import wave
import socket
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROUTES = {
    "llamacpp": "/upstream/{model}/tts",
    "qwentts": "/upstream/{model}/v1/tts",
}
ASR_ROUTE = "/upstream/{model}/v1/audio/transcriptions"
ASR_RATE = 16000
TRANSCRIPT = "language English<asr_text>The kitchen lights are off."


def multipart_parts(content_type, body):
    head = f"Content-Type: {content_type}\r\n\r\n".encode()
    message = email.parser.BytesParser(policy=email.policy.HTTP).parsebytes(head + body)
    if not message.is_multipart():
        return None
    parts = {}
    for part in message.iter_parts():
        name = part.get_param("name", header="content-disposition")
        parts[name] = (part.get_filename(), part.get_payload(decode=True))
    return parts


def llamacpp_problems(args, content_type, body):
    if not content_type.startswith("multipart/form-data"):
        return [f"want multipart/form-data, got {content_type!r}"]
    parts = multipart_parts(content_type, body)
    if parts is None:
        return ["body is not multipart"]
    want = {"lang": None, "response_format": b"wav", "stream": b"true"}
    problems = [f"field {name} is {parts.get(name)!r}" for name, value in want.items()
                if name not in parts or (value is not None and parts[name][1] != value)]
    if not parts.get("input", (None, b""))[1]:
        problems.append("field input is missing or empty")
    filename, clip = parts.get("speaker_ref", (None, None))
    if filename is None:
        problems.append("no speaker_ref file part")
    elif clip != args.clip_bytes:
        problems.append("speaker_ref is not the declared clip")
    return problems


def qwentts_problems(args, content_type, body):
    if content_type != "application/json":
        return [f"want application/json, got {content_type!r}"]
    try:
        request = json.loads(body)
    except ValueError as err:
        return [f"body is not JSON: {err}"]
    if not isinstance(request, dict) or set(request) != {"voice", "input", "response_format"}:
        return [f"want exactly voice, input and response_format, got {request!r}"]
    problems = []
    if request["voice"] != args.voice:
        problems.append(f"voice is {request['voice']!r}, want {args.voice!r}")
    if request["response_format"] != "wav":
        problems.append(f"response_format is {request['response_format']!r}")
    if not isinstance(request["input"], str) or not request["input"]:
        problems.append("input is missing or empty")
    return problems


def asr_problems(args, content_type, body):
    if not content_type.startswith("multipart/form-data"):
        return [f"want multipart/form-data, got {content_type!r}"]
    parts = multipart_parts(content_type, body)
    if parts is None:
        return ["body is not multipart"]
    problems = []
    if parts.get("model", (None, None))[1] != args.asr_model.encode():
        problems.append(f"field model is {parts.get('model')!r}")
    filename, audio = parts.get("file", (None, None))
    if filename is None:
        return problems + ["no file part"]
    try:
        with wave.open(io.BytesIO(audio), "rb") as clip:
            shape = (clip.getnchannels(), clip.getframerate(), clip.getsampwidth())
            frames = clip.getnframes()
    except (wave.Error, EOFError) as err:
        return problems + [f"file part is not WAV: {err}"]
    if shape != (1, ASR_RATE, 2):
        problems.append(f"file part is {shape} (channels, rate, width), want mono 16 kHz 16-bit")
    if frames == 0:
        problems.append("file part holds no audio")
    return problems


CHECKS = {"llamacpp": llamacpp_problems, "qwentts": qwentts_problems}


def keep_body(bodies, body):
    digest = hashlib.sha256(body).hexdigest()
    path = os.path.join(bodies, digest)
    partial = f"{path}.{threading.get_ident()}"
    with open(partial, "wb") as file:
        file.write(body)
    os.replace(partial, path)
    return digest


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.reply(200, "application/json", b'{"status":"ok"}')
            return
        self.error(404, f"no route {self.path}")

    def do_POST(self):
        args = self.server.args
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        content_type = self.headers.get("Content-Type", "")

        entry = {"path": self.path, "content_type": content_type}
        if self.path == ASR_ROUTE.format(model=args.asr_model):
            problems = asr_problems(args, content_type, body)
            reply = ("application/json", json.dumps({"text": TRANSCRIPT}).encode())
        elif self.path == ROUTES[args.backend].format(model=args.model):
            problems = CHECKS[args.backend](args, content_type, body)
            entry["body_sha256"] = keep_body(args.bodies, body)
            reply = ("audio/wav", args.reply_bytes)
        else:
            problems = [f"no route {self.path}"]
            entry["body_sha256"] = keep_body(args.bodies, body)
            reply = None

        entry["problems"] = problems
        with open(args.record, "a") as record:
            record.write(json.dumps(entry) + "\n")

        if problems:
            self.error(400 if reply else 404, "; ".join(problems))
            return
        self.reply(200, *reply)

    def error(self, code, message):
        body = json.dumps({"error": {"message": message}}).encode()
        self.reply(code, "application/json", body)

    def reply(self, code, content_type, body):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--backend", choices=sorted(ROUTES), required=True)
    parser.add_argument("--model", default="qwen3-tts")
    parser.add_argument("--asr-model", default="qwen3-asr")
    parser.add_argument("--voice", required=True)
    parser.add_argument("--clip", required=True)
    parser.add_argument("--reply", required=True)
    parser.add_argument("--record", required=True)
    parser.add_argument("--bind", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8080)
    args = parser.parse_args()

    with open(args.clip, "rb") as clip:
        args.clip_bytes = clip.read()
    with open(args.reply, "rb") as reply:
        args.reply_bytes = reply.read()
    args.bodies = f"{args.record}.bodies"
    os.makedirs(args.bodies, exist_ok=True)

    family = socket.AF_INET6 if ":" in args.bind else socket.AF_INET
    server_class = type("Server", (ThreadingHTTPServer,), {"address_family": family})
    server = server_class((args.bind, args.port), Handler)
    server.args = args
    server.serve_forever()


if __name__ == "__main__":
    main()
