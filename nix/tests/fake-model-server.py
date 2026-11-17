"""Stand-ins for llama-server and qwen-tts-server, started by a real llama-swap.

Each takes the command line the module writes into llama-swap's settings,
checks that the files it names can be read from inside llama-swap's sandbox,
and appends every start and request to a JSON-lines record for the test.

llama-server answers /tts with a fixed WAV and /v1/audio/transcriptions with
a fixed transcript. qwen-tts-server loads its configuration with the real
server's config.py, reads every voice clip as the real engine does, and
answers JSON /v1/tts with a fixed WAV. Its record goes in the cache root the
module must create; it never creates that directory itself.
"""

import argparse
import base64
import hashlib
import importlib.util
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TRANSCRIPT = "language English<asr_text>The kitchen lights are off."
DEFAULT_CACHE = "/var/cache/llama-swap"
EXIT_CONFIG = 2


def cache_home():
    return os.environ.get("XDG_CACHE_HOME") or DEFAULT_CACHE


def readable(path):
    with open(path, "rb") as file:
        return hashlib.sha256(file.read()).hexdigest()


class Recorder:
    def __init__(self, path, model):
        self.path = path
        self.model = model

    def write(self, entry):
        entry["model"] = self.model
        with open(self.path, "a") as record:
            record.write(json.dumps(entry) + "\n")


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            self.reply(200, "application/json", json.dumps(self.server.health).encode())
            return
        self.reply(404, "application/json", b'{"error":{"message":"not found"}}')

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        content_type = self.headers.get("Content-Type", "")
        self.server.recorder.write({
            "event": "request",
            "path": self.path,
            "content_type": content_type,
            "body": base64.b64encode(body).decode(),
        })
        route = self.server.routes.get(self.path)
        if route is None:
            self.reply(404, "application/json", b'{"error":{"message":"no route"}}')
            return
        self.reply(*route(content_type, body))

    def reply(self, code, content_type, body):
        self.send_response(code)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


def serve(port, recorder, routes, health):
    server = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    server.recorder = recorder
    server.routes = routes
    server.health = health
    server.serve_forever()


def llama_server(reply, argv):
    parser = argparse.ArgumentParser(prog="llama-server")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("-m", dest="model", required=True)
    parser.add_argument("--mmproj", required=True)
    args, rest = parser.parse_known_args(argv)

    recorder = Recorder(os.path.join(cache_home(), "fake-llama-server.jsonl"), args.model)
    recorder.write({
        "event": "start",
        "argv": argv,
        "model_sha256": readable(args.model),
        "mmproj_sha256": readable(args.mmproj),
        "rest": rest,
    })

    def wav(_content_type, _body):
        return 200, "audio/wav", reply

    def transcript(_content_type, _body):
        return 200, "application/json", json.dumps({"text": TRANSCRIPT}).encode()

    routes = {"/tts": wav, "/v1/audio/transcriptions": transcript}
    serve(args.port, recorder, routes, {"status": "ok"})


def load_config(config_module, path):
    spec = importlib.util.spec_from_file_location("qwen_tts_server_config", config_module)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.load(path)


def qwen_tts_server(reply, config_module, argv):
    parser = argparse.ArgumentParser(prog="qwen-tts-server")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--config", required=True)
    args = parser.parse_args(argv)

    # Not created here: the module must have made it writable for this user.
    root = os.path.join(cache_home(), "qwen-tts-server")
    recorder = Recorder(os.path.join(root, "requests.jsonl"), None)

    try:
        cfg = load_config(config_module, args.config)
        clips = {name: readable(voice.clip) for name, voice in cfg.voices.items()}
    except (OSError, ValueError) as exc:
        print(f"qwen-tts-server: config {args.config}: {exc}", file=sys.stderr)
        sys.exit(EXIT_CONFIG)

    with open(args.config, encoding="utf-8") as file:
        config = json.load(file)
    recorder.write({
        "event": "start",
        "argv": argv,
        "config_path": args.config,
        "config": config,
        "clips": clips,
        "cuda_visible_devices": os.environ.get("CUDA_VISIBLE_DEVICES"),
        "xdg_cache_home": os.environ.get("XDG_CACHE_HOME"),
    })

    def tts(content_type, body):
        try:
            request = json.loads(body)
        except ValueError:
            return 400, "application/json", b'{"error":{"message":"not JSON"}}'
        if content_type != "application/json" or request.get("voice") not in cfg.voices:
            return 400, "application/json", b'{"error":{"message":"bad request"}}'
        return 200, "audio/wav", reply

    health = {"status": "ready", "voices": sorted(cfg.voices)}
    serve(args.port, recorder, {"/v1/tts": tts}, health)


def main():
    role, reply_path = sys.argv[1], sys.argv[2]
    with open(reply_path, "rb") as file:
        reply = file.read()
    if role == "llama-server":
        llama_server(reply, sys.argv[3:])
    else:
        qwen_tts_server(reply, sys.argv[3], sys.argv[4:])


if __name__ == "__main__":
    main()
