"""A small Wyoming client for the VM tests, on the Python standard library.

Each event is one JSON header line, then data_length bytes of JSON data, then
payload_length bytes of payload. The client sends the events Home Assistant
sends and prints a short JSON summary of the reply on stdout. Audio goes to
a file: the test driver reads guest output over a console that drops bytes
on large outputs.

Exit status: 0 on the expected reply, 1 on an error event or an unexpected
reply, 3 when the connection cannot be made (refused, unroutable or not
established within the timeout), 4 when the connection was made and then
failed, closed or gave no reply within the timeout.
"""

import argparse
import hashlib
import json
import socket
import sys
import time
import wave

EXIT_PROTOCOL = 1
EXIT_UNREACHABLE = 3
EXIT_NO_REPLY = 4

MAX_HEADER_BYTES = 64 << 10
AUDIO_CHUNK_BYTES = 4096
DEFAULT_PORT = 10300
DEFAULT_TIMEOUT_SECONDS = 10.0


class ProtocolError(Exception):
    pass


class Connection:
    def __init__(self, host, port, timeout):
        self.sock = socket.create_connection((host, port), timeout=timeout)
        self.reader = self.sock.makefile("rb")

    def send(self, event_type, data=None, payload=b""):
        header = {"type": event_type}
        body = b""
        if data is not None:
            body = json.dumps(data).encode()
            header["data_length"] = len(body)
        if payload:
            header["payload_length"] = len(payload)
        self.sock.sendall(json.dumps(header).encode() + b"\n" + body + payload)

    def receive(self):
        line = self.reader.readline(MAX_HEADER_BYTES)
        if not line.endswith(b"\n"):
            raise ProtocolError("connection closed before a complete event")
        header = json.loads(line)
        data = header.get("data") or {}
        data_length = header.get("data_length") or 0
        if data_length:
            data.update(json.loads(self.read_exactly(data_length)))
        payload = self.read_exactly(header.get("payload_length") or 0)
        return header["type"], data, payload

    def read_exactly(self, length):
        raw = self.reader.read(length)
        if len(raw) != length:
            raise ProtocolError(f"connection closed after {len(raw)} of {length} bytes")
        return raw

    def close(self):
        self.reader.close()
        self.sock.close()


def receive_until(conn, wanted):
    event_type, data, payload = conn.receive()
    if event_type == "error":
        raise ProtocolError(f"error event: {json.dumps(data)}")
    if event_type != wanted:
        raise ProtocolError(f"got {event_type}, want {wanted}")
    return data, payload


def describe(conn, _args):
    conn.send("describe")
    info, _ = receive_until(conn, "info")
    return {
        "asr_models": [model["name"] for program in info.get("asr", []) for model in program.get("models", [])],
        "tts_voices": [
            {"name": voice["name"], "description": voice.get("description")}
            for program in info.get("tts", [])
            for voice in program.get("voices", [])
        ],
        "supports_synthesize_streaming": [program.get("supports_synthesize_streaming") for program in info.get("tts", [])],
    }


def transcribe(conn, args):
    with wave.open(args.file, "rb") as clip:
        audio_format = {"rate": clip.getframerate(), "width": clip.getsampwidth(), "channels": clip.getnchannels()}
        frames = clip.readframes(clip.getnframes())
    conn.send("transcribe", {})
    conn.send("audio-start", audio_format)
    for start in range(0, len(frames), AUDIO_CHUNK_BYTES):
        conn.send("audio-chunk", audio_format, frames[start:start + AUDIO_CHUNK_BYTES])
    conn.send("audio-stop", {})
    transcript, _ = receive_until(conn, "transcript")
    return {"text": transcript.get("text")}


class StreamReply:
    def __init__(self):
        self.events = []
        self.audio_format = None
        self.audio = bytearray()

    def read_until(self, conn, last):
        while True:
            event_type, data, payload = conn.receive()
            if not self.events or self.events[-1] != event_type:
                self.events.append(event_type)
            if event_type == "error":
                raise ProtocolError(f"error event: {json.dumps(data)}; events {self.events}")
            if event_type == "audio-start":
                self.audio_format = data
            elif event_type == "audio-chunk":
                self.audio += payload
            if event_type == last:
                return


def synthesize_stream(conn, args):
    voice = {"name": args.voice}
    conn.send("synthesize-start", {"voice": voice})
    for text in args.text:
        conn.send("synthesize-chunk", {"text": text})
    # Home Assistant also sends the whole text for servers that do not stream.
    conn.send("synthesize", {"text": "".join(args.text), "voice": voice})

    # A sentence completed by the chunks is spoken before synthesize-stop.
    reply = StreamReply()
    reply.read_until(conn, "audio-chunk")
    audio_before_stop = len(reply.audio)
    conn.send("synthesize-stop")
    stop_sent_monotonic = time.monotonic()
    reply.read_until(conn, "synthesize-stopped")

    with open(args.out, "wb") as out:
        out.write(reply.audio)
    return {
        "events": reply.events,
        "audio_start": reply.audio_format,
        "audio_before_stop": audio_before_stop,
        "stop_sent_monotonic": stop_sent_monotonic,
        "bytes": len(reply.audio),
        "sha256": hashlib.sha256(reply.audio).hexdigest(),
    }


def parse_args():
    parser = argparse.ArgumentParser(prog="wyoming-client")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    parser.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT_SECONDS,
                        help="seconds for the connection and for each read")
    commands = parser.add_subparsers(dest="command", required=True)

    commands.add_parser("describe").set_defaults(run=describe)

    transcribe_parser = commands.add_parser("transcribe")
    transcribe_parser.add_argument("file")
    transcribe_parser.set_defaults(run=transcribe)

    stream_parser = commands.add_parser("synthesize-stream")
    stream_parser.add_argument("--out", required=True, help="file for the raw PCM")
    stream_parser.add_argument("voice")
    stream_parser.add_argument("text", nargs="+", help="one synthesize-chunk each")
    stream_parser.set_defaults(run=synthesize_stream)

    return parser.parse_args()


def report(status, key, err):
    print(json.dumps({key: f"{type(err).__name__}: {err}"}))
    return status


def main():
    args = parse_args()
    try:
        conn = Connection(args.host, args.port, args.timeout)
    except OSError as err:
        return report(EXIT_UNREACHABLE, "unreachable", err)
    try:
        result = args.run(conn, args)
    except (ProtocolError, ValueError, KeyError) as err:
        return report(EXIT_PROTOCOL, "error", err)
    except OSError as err:
        return report(EXIT_NO_REPLY, "no_reply", err)
    finally:
        conn.close()
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
