import asyncio
import json
import logging
import threading
import time
from http import HTTPStatus

import pytest
from starlette.requests import ClientDisconnect, Request
from starlette.testclient import TestClient

from conftest import VOICE, FakeEngine, SteppedEngine
from qwen_tts_server import wav
from qwen_tts_server.app import Speech, StreamAborted, StreamState, _body, _wait_first, create_app
from qwen_tts_server.worker import EXIT_FATAL, Job, Worker

TIMEOUT = 5


def post(client, voice="robbie", text="The kitchen lights are now off.", fmt="wav"):
    return client.post("/v1/tts", json={"voice": voice, "input": text, "response_format": fmt})


def running(engine, cfg, **options):
    worker = Worker(engine, cfg.queue_size, **options)
    worker.start()
    wait_ready(worker)
    return worker


def idle(engine, cfg):
    """A ready worker that reports itself alive but never takes a job, so submitted jobs stay queued."""
    worker = Worker(engine, cfg.queue_size)
    worker.ready = True
    worker.alive = lambda: True
    return worker


def wait_ready(worker):
    deadline = time.monotonic() + TIMEOUT
    while not worker.ready:
        if time.monotonic() > deadline:
            pytest.fail(f"worker not ready within {TIMEOUT} s")
        time.sleep(0.01)


def wait_until(condition, what):
    deadline = time.monotonic() + TIMEOUT
    while not condition():
        if time.monotonic() > deadline:
            pytest.fail(f"{what} within {TIMEOUT} s")
        time.sleep(0.01)


def asgi_post(app, body, then_disconnect, sent=None):
    """Calls the app as uvicorn would: the body, then a disconnect or a client that keeps waiting.

    Returns the messages the app sent, appended to sent as they happen when it is given.
    """
    scope = {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": "POST",
        "scheme": "http",
        "path": "/v1/tts",
        "raw_path": b"/v1/tts",
        "query_string": b"",
        "root_path": "",
        "headers": [(b"content-type", b"application/json"), (b"content-length", str(len(body)).encode())],
        "client": ("127.0.0.1", 50000),
        "server": ("127.0.0.1", 8930),
    }
    sent = [] if sent is None else sent

    async def run():
        pending = [{"type": "http.request", "body": body, "more_body": False}]

        async def receive():
            if pending:
                return pending.pop()
            if then_disconnect:
                return {"type": "http.disconnect"}
            await asyncio.Event().wait()

        async def send(message):
            sent.append(message)

        await app(scope, receive, send)

    asyncio.run(run())
    return sent


def error_message(response):
    assert response.headers["content-type"] == "application/json"
    return response.json()["error"]["message"]


def test_wav_stream_uses_the_named_voice(make_config):
    cfg = make_config()
    engine = FakeEngine(chunks=[b"\x01\x00", b"\x02\x00"])
    client = TestClient(create_app(cfg, running(engine, cfg)))

    response = post(client, voice="stefan")

    assert response.status_code == 200
    assert response.headers["content-type"] == "audio/wav"
    assert response.content == wav.header() + b"\x01\x00\x02\x00"
    assert engine.calls == [("stefan", ["The kitchen lights are now off."])]


def test_the_go_client_body_is_accepted(make_config):
    cfg = make_config()
    engine = FakeEngine(chunks=[b"\x01\x00"])
    client = TestClient(create_app(cfg, running(engine, cfg)))

    # The exact bytes internal/upstream's qwentts backend sends.
    body = b'{"voice":"robbie","input":"hello","response_format":"wav"}'
    response = client.post("/v1/tts", content=body, headers={"content-type": "application/json"})

    assert response.status_code == 200
    assert response.content == wav.header() + b"\x01\x00"


def test_response_format_defaults_to_wav(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, running(FakeEngine(chunks=[b"\x01\x00"]), cfg)))

    response = client.post("/v1/tts", json={"voice": "robbie", "input": "Hello."})

    assert response.status_code == 200
    assert response.headers["content-type"] == "audio/wav"


def test_pcm_stream_has_no_header(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, running(FakeEngine(chunks=[b"\x01\x00"]), cfg)))

    response = post(client, fmt="pcm")

    assert response.status_code == 200
    assert response.headers["content-type"] == "audio/pcm;rate=24000"
    assert response.content == b"\x01\x00"


def test_long_input_is_segmented(make_config):
    cfg = make_config()
    engine = FakeEngine()
    client = TestClient(create_app(cfg, running(engine, cfg)))

    post(client, text="First sentence is here. Second sentence is here. Third.")

    assert engine.calls[0][1] == ["First sentence is here.", "Second sentence is here. Third."]


def test_unknown_voice_is_400_before_any_audio(make_config):
    cfg = make_config()
    worker = idle(FakeEngine(), cfg)
    client = TestClient(create_app(cfg, worker))

    response = post(client, voice="nobody")

    assert response.status_code == 400
    assert error_message(response) == "unknown voice 'nobody'"
    assert worker.jobs.empty()


def test_multipart_body_is_415_without_audio(make_config):
    cfg = make_config()
    worker = idle(FakeEngine(), cfg)
    client = TestClient(create_app(cfg, worker))

    response = client.post(
        "/v1/tts",
        data={"input": "Hello.", "response_format": "wav"},
        files={"speaker_ref": ("robbie.mp3", b"ID3 clip bytes", "audio/mpeg")},
    )

    assert response.status_code == 415
    assert error_message(response) == "request body must be application/json"
    assert worker.jobs.empty()


def test_the_old_multipart_route_is_gone(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = client.post("/tts", files={"speaker_ref": ("robbie.mp3", b"clip", "audio/mpeg")})

    assert response.status_code == 404


@pytest.mark.parametrize(
    "body, message",
    [
        (b"{not json", "request body is not valid JSON"),
        (b'["robbie", "Hello."]', "request body must be a JSON object"),
        (b'{"input": "Hello."}', "voice must be a string"),
        (b'{"voice": 7, "input": "Hello."}', "voice must be a string"),
        (b'{"voice": "robbie"}', "input must be a string"),
        (b'{"voice": "robbie", "input": "Hello.", "response_format": 1}', "unsupported response_format 1"),
        (b'{"voice": "robbie", "input": "Hello.", "response_format": []}', "unsupported response_format []"),
        (b'{"voice": "robbie", "input": "Hello.\xff"}', "request body is not valid JSON"),
        (b"[" * 100_000, "request body is not valid JSON"),
    ],
)
def test_malformed_body_is_400(make_config, body, message):
    cfg = make_config()
    worker = idle(FakeEngine(), cfg)
    client = TestClient(create_app(cfg, worker))

    response = client.post("/v1/tts", content=body, headers={"content-type": "application/json"})

    assert response.status_code == 400
    assert error_message(response) == message
    assert worker.jobs.empty()


def test_empty_input_is_400(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = post(client, text="   ")

    assert response.status_code == 400
    assert error_message(response) == "input is empty"


def test_unsupported_format_is_400(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = post(client, fmt="mp3")

    assert response.status_code == 400
    assert error_message(response) == "unsupported response_format 'mp3'"


def test_over_long_input_is_413(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = post(client, text="a" * (cfg.max_input_chars + 1))

    assert response.status_code == 413
    assert error_message(response) == f"input exceeds {cfg.max_input_chars} characters"


def test_input_at_the_limit_is_accepted(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, running(FakeEngine(), cfg)))

    response = post(client, text="a" * cfg.max_input_chars)

    assert response.status_code == 200


def test_not_ready_is_503_with_retry_after(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, Worker(FakeEngine(), cfg.queue_size)))

    response = post(client)

    assert response.status_code == 503
    assert error_message(response) == "model is loading"
    assert response.headers["retry-after"] == "5"


def test_queue_full_is_503(make_config):
    cfg = make_config(queue_size=1)
    worker = idle(FakeEngine(), cfg)
    worker.submit(Job("robbie", ["Waiting."]))
    client = TestClient(create_app(cfg, worker))

    response = post(client)

    assert response.status_code == 503
    assert error_message(response) == "queue full"
    assert response.headers["retry-after"] == "2"


def test_queue_wait_exceeded_is_503_and_job_is_skipped(make_config):
    cfg = make_config(queue_wait_seconds=0.05)
    engine = FakeEngine()
    worker = idle(engine, cfg)
    client = TestClient(create_app(cfg, worker))

    response = post(client)

    assert response.status_code == 503
    assert error_message(response) == "queue wait exceeded"
    job = worker.jobs.get_nowait()
    assert job.cancel.is_set()
    worker.run_one(job)
    assert engine.calls == []


def test_error_before_first_chunk_is_500_json_and_logs_the_cause(make_config, caplog):
    caplog.set_level(logging.INFO, logger="qwen_tts_server")
    cfg = make_config()
    engine = FakeEngine(error=ValueError("prompt failed"), fail_at=0)
    client = TestClient(create_app(cfg, running(engine, cfg)))

    response = post(client)

    assert response.status_code == 500
    assert response.json() == {"error": {"message": "generation failed"}}
    assert '"cause": "prompt failed"' in caplog.text


def test_unexpected_error_before_streaming_is_500_json(make_config, monkeypatch):
    def broken(text, max_chars):
        raise RuntimeError("segmenter bug")

    monkeypatch.setattr("qwen_tts_server.app.segment", broken)
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)), raise_server_exceptions=False)

    response = post(client)

    assert response.status_code == 500
    assert response.json() == {"error": {"message": "internal server error"}}


def test_dead_worker_is_503_and_queues_nothing(make_config):
    cfg = make_config()
    worker = Worker(FakeEngine(), cfg.queue_size)
    worker.ready = True
    client = TestClient(create_app(cfg, worker))

    response = post(client)

    assert response.status_code == 503
    assert error_message(response) == "worker is not running"
    assert response.headers["retry-after"] == "5"
    assert worker.jobs.empty()


def test_long_unknown_voice_is_echoed_short(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = post(client, voice="x" * 1000)

    assert response.status_code == 400
    assert error_message(response) == "unknown voice " + repr("x" * 64) + "…"


def test_long_unsupported_format_is_echoed_short(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = post(client, fmt=["y" * 1000])

    assert response.status_code == 400
    message = error_message(response)
    assert message.startswith("unsupported response_format ['yyy")
    assert len(message) == len("unsupported response_format ") + 64 + 1


def test_error_after_first_chunk_aborts_the_body(make_config):
    cfg = make_config()
    engine = FakeEngine(chunks=[b"\x01\x00", b"\x02\x00"], error=ValueError("decode failed"), fail_at=1)
    client = TestClient(create_app(cfg, running(engine, cfg)))

    with pytest.raises(StreamAborted, match="decode failed"):
        post(client)


def test_health_is_503_until_loaded_then_200_with_voices(make_config):
    cfg = make_config(voices={"robbie": dict(VOICE)})
    engine = FakeEngine()
    engine.release_load.clear()
    worker = Worker(engine, cfg.queue_size)
    worker.start()
    client = TestClient(create_app(cfg, worker))

    loading = client.get("/health")

    assert loading.status_code == 503
    assert loading.headers["retry-after"] == "5"
    assert loading.json() == {"status": "loading"}

    engine.release_load.set()
    wait_ready(worker)
    ready = client.get("/health")

    assert ready.status_code == 200
    assert ready.json() == {"status": "ready", "voices": ["robbie"]}


def test_health_is_503_when_worker_thread_is_dead(make_config):
    cfg = make_config()
    worker = Worker(FakeEngine(), cfg.queue_size)
    worker.ready = True
    client = TestClient(create_app(cfg, worker))

    response = client.get("/health")

    assert response.status_code == 503
    assert response.headers["retry-after"] == "5"


def test_embedding_only_voice_is_ready_and_speaks(make_config):
    cfg = make_config(voices={"stefan": dict(VOICE, transcript=None)})
    engine = FakeEngine(chunks=[b"\x01\x00"])
    worker = running(engine, cfg)
    client = TestClient(create_app(cfg, worker))

    health = client.get("/health")
    response = post(client, voice="stefan")

    assert health.status_code == 200
    assert health.json() == {"status": "ready", "voices": ["stefan"]}
    assert response.status_code == 200
    assert engine.calls == [("stefan", ["The kitchen lights are now off."])]


def test_voices_lists_the_configured_voices_before_ready(make_config):
    cfg = make_config()
    client = TestClient(create_app(cfg, Worker(FakeEngine(), cfg.queue_size)))

    response = client.get("/v1/voices")

    assert response.status_code == 200
    assert response.json() == {"voices": ["robbie", "stefan"]}


BODY_LIMIT = 1024


def oversized_body() -> bytes:
    return json.dumps({"voice": "robbie", "input": "a" * BODY_LIMIT}).encode()


def body_of(size: int) -> bytes:
    """A valid request naming an unknown voice, padded with JSON whitespace to exactly size bytes."""
    body = json.dumps({"voice": "nobody", "input": "Hello."}).encode()
    return body + b" " * (size - len(body))


@pytest.mark.parametrize("chunked", [False, True])
@pytest.mark.parametrize("size, status", [(BODY_LIMIT, 400), (BODY_LIMIT + 1, 413)])
def test_body_limit_is_inclusive(make_config, monkeypatch, chunked, size, status):
    monkeypatch.setattr("qwen_tts_server.app.MAX_BODY_BYTES", BODY_LIMIT)
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))
    body = body_of(size)
    content = iter([body[:100], body[100:]]) if chunked else body

    response = client.post("/v1/tts", content=content, headers={"content-type": "application/json"})

    assert response.status_code == status


def test_body_with_content_length_over_the_limit_is_413(make_config, monkeypatch):
    monkeypatch.setattr("qwen_tts_server.app.MAX_BODY_BYTES", BODY_LIMIT)
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = client.post("/v1/tts", content=oversized_body(), headers={"content-type": "application/json"})

    assert response.status_code == 413
    assert error_message(response) == f"request body exceeds {BODY_LIMIT} bytes"


def test_streamed_body_over_the_limit_is_413(make_config, monkeypatch):
    monkeypatch.setattr("qwen_tts_server.app.MAX_BODY_BYTES", BODY_LIMIT)
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))
    body = oversized_body()

    # A generator body is sent chunked, without Content-Length.
    response = client.post("/v1/tts", content=iter([body[:100], body[100:]]), headers={"content-type": "application/json"})

    assert response.status_code == 413
    assert error_message(response) == f"request body exceeds {BODY_LIMIT} bytes"


class BlockingEngine:
    """Holds the worker inside its first job until released, recording each voice it is asked for."""

    def __init__(self):
        self.calls = []
        self.release = threading.Event()

    def load(self):
        pass

    def stream(self, voice, segments):
        self.calls.append(voice)
        self.release.wait(TIMEOUT)
        yield b"\x01\x00"


def test_client_that_disconnects_while_queued_is_skipped(make_config):
    cfg = make_config()
    engine = BlockingEngine()
    worker = running(engine, cfg)
    worker.submit(Job("robbie", ["Busy."]))
    wait_until(lambda: engine.calls == ["robbie"], "the worker did not start the blocking job")
    app = create_app(cfg, worker)
    body = json.dumps({"voice": "stefan", "input": "Hello."}).encode()

    started = time.monotonic()
    sent = asgi_post(app, body, then_disconnect=True)
    elapsed = time.monotonic() - started

    assert elapsed < cfg.queue_wait_seconds / 2
    assert sent == []
    queued = worker.jobs.queue[0]
    assert queued.voice == "stefan"
    assert queued.cancel.is_set()
    assert queued.first.cancelled()

    engine.release.set()
    after = Job("robbie", ["After."])
    worker.submit(after)
    after.first.result(timeout=TIMEOUT)
    assert engine.calls == ["robbie", "robbie"]


def test_fatal_error_exits_only_after_the_500_is_sent(make_config):
    cfg = make_config()
    sent_at_exit = []
    exited = threading.Event()
    sent = []

    def exit_hook(code):
        sent_at_exit.append((code, list(sent)))
        exited.set()

    engine = FakeEngine(error=RuntimeError("CUDA error: an illegal memory access was encountered"), fail_at=0)
    worker = running(engine, cfg, exit_hook=exit_hook)
    app = create_app(cfg, worker)

    asgi_post(app, json.dumps({"voice": "robbie", "input": "Hello."}).encode(), then_disconnect=False, sent=sent)

    assert exited.wait(TIMEOUT)
    code, delivered = sent_at_exit[0]
    assert code == EXIT_FATAL
    assert delivered[0]["type"] == "http.response.start"
    assert delivered[0]["status"] == 500
    assert json.loads(delivered[1]["body"]) == {"error": {"message": "generation failed"}}


def test_closing_the_body_cancels_generation_within_a_chunk(make_config):
    engine = SteppedEngine()
    worker = Worker(engine, 1)
    worker.start()
    job = Job("robbie", ["Hello."])
    worker.submit(job)
    engine.gate.release()
    first = job.first.result(timeout=TIMEOUT)

    async def read_first_then_close():
        body = _body(job, first, Speech("robbie", "Hello.", "pcm"), StreamState())
        assert await anext(body) == first
        await body.aclose()

    asyncio.run(read_first_then_close())

    assert job.cancel.is_set()
    engine.gate.release()
    assert engine.closed.wait(TIMEOUT)


def test_cancelling_the_wait_for_the_first_chunk_cancels_the_job():
    job = Job("robbie", ["Hello."])

    async def cancel_while_waiting():
        async def receive():
            await asyncio.Event().wait()

        task = asyncio.create_task(_wait_first(job, TIMEOUT, receive))
        await asyncio.sleep(0.05)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(cancel_while_waiting())

    assert job.cancel.is_set()
    assert job.first.cancelled()
    assert job.answered.is_set()


def recording_jobs(worker):
    jobs = []
    submit = worker.submit

    def record(job):
        jobs.append(job)
        submit(job)

    worker.submit = record
    return jobs


def tts_events(caplog):
    events = (json.loads(record.getMessage()) for record in caplog.records if record.name == "qwen_tts_server")
    return [event for event in events if event["event"] == "tts"]


def tts_scope(body, spec_version):
    return {
        "type": "http",
        "asgi": {"version": "3.0", "spec_version": spec_version},
        "http_version": "1.1",
        "method": "POST",
        "scheme": "http",
        "path": "/v1/tts",
        "raw_path": b"/v1/tts",
        "query_string": b"",
        "root_path": "",
        "headers": [(b"content-type", b"application/json"), (b"content-length", str(len(body)).encode())],
        "client": ("127.0.0.1", 50000),
        "server": ("127.0.0.1", 8930),
    }


@pytest.mark.parametrize(
    "yielding_sends, outcome",
    [
        # uvicorn's send awaits, so the cancellation lands before _body has started.
        ({"http.response.start", "http.response.body"}, "disconnected"),
        # The cancellation lands in send after a chunk, leaving _body suspended at its yield.
        ({"http.response.body"}, "cancelled"),
        # No send awaits, so the cancellation lands inside _body while it waits for audio.
        (set(), "cancelled"),
    ],
)
def test_disconnect_at_stream_start_answers_the_job_and_logs_once(make_config, caplog, yielding_sends, outcome):
    caplog.set_level(logging.INFO, logger="qwen_tts_server")
    cfg = make_config()
    worker = running(FakeEngine(chunks=[b"\x01\x00", b"\x02\x00"]), cfg)
    jobs = recording_jobs(worker)
    tts = create_app(cfg, worker).routes[2].endpoint
    body = json.dumps({"voice": "robbie", "input": "Hello."}).encode()
    # uvicorn 0.54 advertises ASGI 2.3, under which starlette cancels the stream on disconnect.
    scope = tts_scope(body, "2.3")

    async def call_then_disconnect():
        pending = [{"type": "http.request", "body": body, "more_body": False}]

        async def receive_body():
            if pending:
                return pending.pop()
            await asyncio.Event().wait()

        response = await tts(Request(scope, receive_body))

        async def receive_disconnect():
            return {"type": "http.disconnect"}

        async def send(message):
            if message["type"] in yielding_sends:
                await asyncio.sleep(0)

        await response(scope, receive_disconnect, send)

        # Checked before the loop closes, so nothing relies on the generator being collected.
        job = jobs[0]
        assert job.cancel.is_set()
        assert job.answered.is_set()
        assert [event["outcome"] for event in tts_events(caplog)] == [outcome]

    asyncio.run(call_then_disconnect())


def test_a_fully_read_stream_logs_exactly_one_event(make_config, caplog):
    caplog.set_level(logging.INFO, logger="qwen_tts_server")
    cfg = make_config()
    worker = running(FakeEngine(chunks=[b"\x01\x00", b"\x02\x00"]), cfg)
    jobs = recording_jobs(worker)
    client = TestClient(create_app(cfg, worker))

    response = post(client)

    assert response.status_code == 200
    assert response.content == wav.header() + b"\x01\x00\x02\x00"
    assert [event["outcome"] for event in tts_events(caplog)] == ["done"]
    assert jobs[0].answered.is_set()


def test_unexpected_error_after_submit_cancels_the_job(make_config, monkeypatch):
    async def broken(job, timeout, receive):
        raise RuntimeError("wait bug")

    monkeypatch.setattr("qwen_tts_server.app._wait_first", broken)
    cfg = make_config()
    worker = idle(FakeEngine(), cfg)
    jobs = recording_jobs(worker)
    client = TestClient(create_app(cfg, worker), raise_server_exceptions=False)

    response = post(client)

    assert response.status_code == 500
    assert response.json() == {"error": {"message": "internal server error"}}
    assert jobs[0].cancel.is_set()


async def asgi_call(app, body, send, spec_version="2.0"):
    """Calls the app with the body, then a client that neither sends more nor leaves."""
    scope = tts_scope(body, spec_version)
    pending = [{"type": "http.request", "body": body, "more_body": False}]

    async def receive():
        if pending:
            return pending.pop()
        await asyncio.Event().wait()

    await app(scope, receive, send)


def hello(voice="robbie"):
    return json.dumps({"voice": voice, "input": "Hello."}).encode()


def test_unfinished_replies_are_bounded_by_the_queue(make_config):
    cfg = make_config(queue_size=1)
    worker = running(FakeEngine(chunks=[b"\x01\x00", b"\x02\x00"]), cfg)
    app = create_app(cfg, worker)
    parked = threading.Semaphore(0)
    releases = [threading.Event(), threading.Event()]

    def parked_request(index):
        first_body = True

        async def send(message):
            nonlocal first_body
            if message["type"] != "http.response.body":
                return
            if first_body:
                first_body = False
                parked.release()
            await asyncio.to_thread(releases[index].wait, TIMEOUT)

        asyncio.run(asgi_call(app, hello(), send))

    threads = [threading.Thread(target=parked_request, args=(index,)) for index in range(2)]
    for thread in threads:
        thread.start()
        assert parked.acquire(timeout=TIMEOUT)

    client = TestClient(app)
    third = post(client)

    assert third.status_code == 503
    assert error_message(third) == "queue full"
    assert third.headers["retry-after"] == "2"

    releases[0].set()
    threads[0].join(TIMEOUT)
    fourth = post(client)

    assert fourth.status_code == 200
    releases[1].set()
    threads[1].join(TIMEOUT)
    assert app.state.slots.in_use == 0


@pytest.mark.parametrize("spec_version", ["2.3", "2.4"])
@pytest.mark.parametrize("failing", ["http.response.start", "http.response.body"])
def test_a_send_failure_still_finalises_the_request(make_config, caplog, spec_version, failing):
    caplog.set_level(logging.INFO, logger="qwen_tts_server")
    cfg = make_config()
    worker = running(FakeEngine(chunks=[b"\x01\x00", b"\x02\x00"]), cfg)
    jobs = recording_jobs(worker)
    app = create_app(cfg, worker)

    async def send(message):
        if message["type"] == failing:
            raise OSError("connection reset by peer")

    async def call_with_failing_send():
        # Under ASGI 2.4 starlette reports the failed send as ClientDisconnect.
        with pytest.raises((OSError, ClientDisconnect)):
            await asgi_call(app, hello(), send, spec_version)

        # Checked before the loop closes, so nothing relies on the generator being collected.
        job = jobs[0]
        assert job.cancel.is_set()
        assert job.answered.is_set()
        assert len(tts_events(caplog)) == 1
        assert app.state.slots.in_use == 0

    asyncio.run(call_with_failing_send())


def test_cancellation_during_watcher_cleanup_abandons_the_job():
    job = Job("robbie", ["Hello."])

    async def cancel_during_cleanup():
        cleanup = asyncio.Event()

        async def receive():
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                cleanup.set()
                await asyncio.sleep(0.5)
                raise

        task = asyncio.create_task(_wait_first(job, 0.01, receive))
        await cleanup.wait()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(cancel_during_cleanup())

    assert job.cancel.is_set()
    assert job.answered.is_set()


def test_a_cancelled_wait_emits_one_cancelled_event_and_frees_its_slot(make_config, caplog):
    caplog.set_level(logging.INFO, logger="qwen_tts_server")
    cfg = make_config()
    worker = idle(FakeEngine(), cfg)
    jobs = recording_jobs(worker)
    app = create_app(cfg, worker)

    async def send(_message):
        pass

    async def cancel_while_queued():
        task = asyncio.create_task(asgi_call(app, hello(), send))
        while not jobs:
            await asyncio.sleep(0.01)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(cancel_while_queued())

    assert [event["outcome"] for event in tts_events(caplog)] == ["cancelled"]
    assert jobs[0].cancel.is_set()
    assert jobs[0].answered.is_set()
    assert app.state.slots.in_use == 0


@pytest.mark.parametrize(
    "method, path, status, allow",
    [("GET", "/missing", 404, None), ("GET", "/v1/tts", 405, {"POST"}), ("POST", "/health", 405, {"GET", "HEAD"})],
)
def test_routing_errors_use_the_json_envelope(make_config, method, path, status, allow):
    cfg = make_config()
    client = TestClient(create_app(cfg, idle(FakeEngine(), cfg)))

    response = client.request(method, path)

    assert response.status_code == status
    assert response.headers["content-type"] == "application/json"
    assert response.json() == {"error": {"message": HTTPStatus(status).phrase}}
    if allow:
        assert set(response.headers["allow"].split(", ")) == allow


class SlowFirstEngine:
    """Takes a while to produce its first chunk, as a real model does, and records each voice it starts."""

    def __init__(self, first_delay):
        self.first_delay = first_delay
        self.calls = []

    def load(self):
        pass

    def stream(self, voice, segments):
        self.calls.append(voice)
        time.sleep(self.first_delay)
        yield b"\x01\x00"
        yield b"\x02\x00"


def test_first_chunk_generation_does_not_count_as_queue_wait(make_config):
    cfg = make_config(queue_wait_seconds=0.2)
    client = TestClient(create_app(cfg, running(SlowFirstEngine(first_delay=0.5), cfg)))

    response = post(client)

    assert response.status_code == 200
    assert response.content == wav.header() + b"\x01\x00\x02\x00"


def test_a_request_still_waiting_for_its_turn_gets_503_and_is_skipped(make_config):
    cfg = make_config(queue_wait_seconds=0.2)
    engine = BlockingEngine()
    worker = running(engine, cfg)
    worker.submit(Job("robbie", ["Busy."]))
    wait_until(lambda: engine.calls == ["robbie"], "the worker did not start the blocking job")
    app = create_app(cfg, worker)

    response = TestClient(app).post("/v1/tts", json={"voice": "stefan", "input": "Hello."})

    assert response.status_code == 503
    assert error_message(response) == "queue wait exceeded"
    engine.release.set()
    after = Job("robbie", ["After."])
    worker.submit(after)
    after.first.result(timeout=TIMEOUT)
    assert engine.calls == ["robbie", "robbie"]
    assert app.state.slots.in_use == 0


def test_a_disconnect_after_the_job_started_is_finalised(make_config, caplog):
    caplog.set_level(logging.INFO, logger="qwen_tts_server")
    cfg = make_config()
    engine = BlockingEngine()
    worker = running(engine, cfg)
    jobs = recording_jobs(worker)
    app = create_app(cfg, worker)
    body = hello("stefan")
    sent = []

    async def disconnect_once_started():
        pending = [{"type": "http.request", "body": body, "more_body": False}]

        async def receive():
            if pending:
                return pending.pop()
            await asyncio.to_thread(wait_until, lambda: engine.calls == ["stefan"], "the job did not start")
            return {"type": "http.disconnect"}

        async def send(message):
            sent.append(message)

        await app(tts_scope(body, "2.3"), receive, send)

    asyncio.run(disconnect_once_started())

    assert sent == []
    assert jobs[0].cancel.is_set()
    assert [event["outcome"] for event in tts_events(caplog)] == ["disconnected"]
    assert app.state.slots.in_use == 0
    engine.release.set()


def test_a_disconnect_during_the_upload_is_a_disconnect(make_config, caplog):
    caplog.set_level(logging.INFO)
    cfg = make_config()
    app = create_app(cfg, idle(FakeEngine(), cfg))
    scope = tts_scope(b"x" * 100, "2.3")
    messages = [{"type": "http.request", "body": b'{"voice": "rob', "more_body": True}, {"type": "http.disconnect"}]
    sent = []

    async def upload_then_leave():
        async def receive():
            return messages.pop(0)

        async def send(message):
            sent.append(message)

        await app(scope, receive, send)

    asyncio.run(upload_then_leave())

    assert sent == []
    assert [record for record in caplog.records if record.levelno >= logging.ERROR] == []
    assert [event["outcome"] for event in tts_events(caplog)] == ["disconnected"]
    assert app.state.slots.in_use == 0


class EngineAbort(BaseException):
    pass


@pytest.mark.filterwarnings("ignore::pytest.PytestUnhandledThreadExceptionWarning")
@pytest.mark.parametrize("fail_at, status", [(0, 500), (1, 200)])
def test_an_engine_base_exception_still_ends_the_request(make_config, caplog, fail_at, status):
    caplog.set_level(logging.INFO, logger="qwen_tts_server")
    cfg = make_config()
    engine = FakeEngine(chunks=[b"\x01\x00", b"\x02\x00"], error=EngineAbort("interrupted"), fail_at=fail_at)
    app = create_app(cfg, running(engine, cfg))
    outcome = {}

    def request():
        client = TestClient(app, raise_server_exceptions=False)
        try:
            outcome["status"] = post(client).status_code
        except Exception as exc:
            outcome["error"] = exc

    thread = threading.Thread(target=request, daemon=True)
    thread.start()
    thread.join(TIMEOUT)

    assert not thread.is_alive(), "the request never ended"
    assert outcome.get("status", status) == status
    assert len(tts_events(caplog)) == 1
    assert app.state.slots.in_use == 0
