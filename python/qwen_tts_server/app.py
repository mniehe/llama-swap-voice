import asyncio
import json
import logging
import queue
import threading
import time
from dataclasses import dataclass
from functools import partial
from http import HTTPStatus

from starlette.applications import Starlette
from starlette.exceptions import HTTPException
from starlette.requests import ClientDisconnect, Request
from starlette.responses import JSONResponse, StreamingResponse
from starlette.routing import Route

from . import wav
from .config import Config
from .events import emit
from .text import segment
from .worker import DONE, Failure, Job, Worker

CONTENT_TYPES = {"wav": "audio/wav", "pcm": f"audio/pcm;rate={wav.SAMPLE_RATE}"}
DEFAULT_FORMAT = "wav"
JSON_CONTENT_TYPE = "application/json"
# The body is read whole before it is parsed; this stays far above max_input_chars
# of text even when every character arrives as a \u escape.
MAX_BODY_BYTES = 1024 * 1024
LOADING_RETRY_SECONDS = 5
QUEUE_FULL_RETRY_SECONDS = 2
# Client strings echoed in error messages are cut to this many characters.
MAX_ECHO_CHARS = 64
GENERATION_FAILED = "generation failed"
INTERNAL_ERROR = "internal server error"

log = logging.getLogger(__name__)


class RequestError(Exception):
    def __init__(self, status: HTTPStatus, message: str, retry_after: int | None = None):
        super().__init__(message)
        self.status = status
        self.message = message
        self.retry_after = retry_after


class StreamAborted(Exception):
    pass


class BodyTooLarge(Exception):
    pass


class ClientGone(Exception):
    pass


@dataclass(frozen=True)
class Speech:
    voice: str
    text: str
    fmt: str


class RequestSlots:
    """Counts requests from submission until their response is finalised.

    A reply generated for a slow reader waits in memory until it is sent, so
    bounding unfinished requests, not just queued ones, bounds the audio held.
    """

    def __init__(self, limit: int):
        self._limit = limit
        self._in_use = 0
        self._lock = threading.Lock()

    @property
    def in_use(self) -> int:
        return self._in_use

    def acquire(self) -> bool:
        with self._lock:
            if self._in_use >= self._limit:
                return False
            self._in_use += 1
            return True

    def release(self) -> None:
        with self._lock:
            # A double release would quietly raise the cap; fail loudly instead.
            if self._in_use == 0:
                raise RuntimeError("no request slot is held")
            self._in_use -= 1


class Submitted:
    """A request that holds a slot and a job; it is finished exactly once, however it ends."""

    def __init__(self, job: Job, speech: Speech, started: float, slots: RequestSlots):
        self.job = job
        self.speech = speech
        self._started = started
        self._slots = slots
        self._finished = False

    def finish(self, **fields) -> None:
        if self._finished:
            return
        self._finished = True
        self.job.cancel.set()
        # A fatal engine error waits for this before the process exits.
        self.job.answered.set()
        self._slots.release()
        emit("tts", voice=self.speech.voice, seconds=round(time.monotonic() - self._started, 2), **fields)


@dataclass
class StreamState:
    started: bool = False
    outcome: str = "cancelled"
    audio_bytes: int = 0


class Finalising:
    """Runs a response, then finalises its request on every exit: completion, a send error or cancellation."""

    def __init__(self, response, finish):
        self._response = response
        self._finish = finish

    async def __call__(self, scope, receive, send) -> None:
        try:
            await self._response(scope, receive, send)
        finally:
            await self._finish()


def create_app(cfg: Config, worker: Worker) -> Starlette:
    voices = list(cfg.voices)
    # The one request generating plus queue_size more.
    slots = RequestSlots(cfg.queue_size + 1)

    async def health(_request: Request) -> JSONResponse:
        # The worker sets ready only after every voice prompt is built and warmed.
        if worker.ready and worker.alive():
            return JSONResponse({"status": "ready", "voices": voices})
        headers = {"Retry-After": str(LOADING_RETRY_SECONDS)}
        return JSONResponse({"status": "loading"}, status_code=HTTPStatus.SERVICE_UNAVAILABLE, headers=headers)

    async def list_voices(_request: Request) -> JSONResponse:
        return JSONResponse({"voices": voices})

    async def tts(request: Request):
        started = time.monotonic()
        submitted = None
        try:
            speech = await _read_speech(request, cfg)
            submitted = _admit(worker, cfg, speech, slots, started)
            first = await _wait_first(submitted.job, cfg.queue_wait_seconds, request.receive)
        except RequestError as exc:
            return _error_response(exc, submitted)
        except ClientGone:
            submitted.finish(outcome="disconnected")
            return _no_response
        except ClientDisconnect:
            # The client left while sending its body, before anything was admitted.
            emit("tts", outcome="disconnected", seconds=round(time.monotonic() - started, 2))
            return _no_response
        except asyncio.CancelledError:
            if submitted:
                submitted.finish(outcome="cancelled")
            raise
        except Exception:
            log.exception("tts request failed before streaming")
            return _error_response(RequestError(HTTPStatus.INTERNAL_SERVER_ERROR, INTERNAL_ERROR), submitted)

        state = StreamState()
        body = _body(submitted.job, first, speech, state)
        response = StreamingResponse(body, media_type=CONTENT_TYPES[speech.fmt])
        return Finalising(response, partial(_finish_stream, body, state, submitted))

    app = Starlette(
        routes=[
            Route("/health", health),
            Route("/v1/voices", list_voices),
            Route("/v1/tts", tts, methods=["POST"]),
        ],
        exception_handlers={HTTPException: _http_error},
    )
    app.state.slots = slots
    return app


async def _http_error(_request: Request, exc: HTTPException) -> JSONResponse:
    """Starlette's own 404 and 405 answer the same JSON envelope as every other error, Allow kept."""
    return JSONResponse({"error": {"message": exc.detail}}, status_code=exc.status_code, headers=exc.headers)


async def _read_speech(request: Request, cfg: Config) -> Speech:
    body = await _read_json(request)
    if not isinstance(body, dict):
        raise RequestError(HTTPStatus.BAD_REQUEST, "request body must be a JSON object")

    voice = body.get("voice")
    text = body.get("input")
    fmt = body.get("response_format", DEFAULT_FORMAT)
    if not isinstance(voice, str):
        raise RequestError(HTTPStatus.BAD_REQUEST, "voice must be a string")
    if not isinstance(text, str):
        raise RequestError(HTTPStatus.BAD_REQUEST, "input must be a string")

    text = text.strip()
    if voice not in cfg.voices:
        raise RequestError(HTTPStatus.BAD_REQUEST, f"unknown voice {_echo(voice)}")
    if not text:
        raise RequestError(HTTPStatus.BAD_REQUEST, "input is empty")
    if len(text) > cfg.max_input_chars:
        raise RequestError(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, f"input exceeds {cfg.max_input_chars} characters")
    if not isinstance(fmt, str) or fmt not in CONTENT_TYPES:
        raise RequestError(HTTPStatus.BAD_REQUEST, f"unsupported response_format {_echo(fmt)}")

    return Speech(voice, text, fmt)


def _echo(value) -> str:
    if isinstance(value, str):
        shown = repr(value[:MAX_ECHO_CHARS])
        return shown + "…" if len(value) > MAX_ECHO_CHARS else shown

    shown = repr(value)
    return shown[:MAX_ECHO_CHARS] + "…" if len(shown) > MAX_ECHO_CHARS else shown


async def _read_json(request: Request):
    media_type = request.headers.get("content-type", "").partition(";")[0].strip().lower()
    if media_type != JSON_CONTENT_TYPE:
        raise RequestError(HTTPStatus.UNSUPPORTED_MEDIA_TYPE, f"request body must be {JSON_CONTENT_TYPE}")

    try:
        raw = await _limited(request).body()
    except BodyTooLarge:
        raise RequestError(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, f"request body exceeds {MAX_BODY_BYTES} bytes") from None

    try:
        return json.loads(raw)
    except (ValueError, RecursionError):
        raise RequestError(HTTPStatus.BAD_REQUEST, "request body is not valid JSON") from None


def _limited(request: Request) -> Request:
    """Counts body bytes as they arrive, so a chunked upload is capped as well as a declared one."""
    received = 0

    async def receive():
        nonlocal received
        message = await request.receive()
        received += len(message.get("body", b""))
        if received > MAX_BODY_BYTES:
            raise BodyTooLarge()
        return message

    return Request(request.scope, receive)


def _admit(worker: Worker, cfg: Config, speech: Speech, slots: RequestSlots, started: float) -> Submitted:
    if not worker.ready:
        raise RequestError(HTTPStatus.SERVICE_UNAVAILABLE, "model is loading", LOADING_RETRY_SECONDS)
    if not worker.alive():
        raise RequestError(HTTPStatus.SERVICE_UNAVAILABLE, "worker is not running", LOADING_RETRY_SECONDS)
    if not slots.acquire():
        raise RequestError(HTTPStatus.SERVICE_UNAVAILABLE, "queue full", QUEUE_FULL_RETRY_SECONDS)

    try:
        job = Job(speech.voice, segment(speech.text, cfg.max_segment_chars))
        worker.submit(job)
    except queue.Full:
        slots.release()
        raise RequestError(HTTPStatus.SERVICE_UNAVAILABLE, "queue full", QUEUE_FULL_RETRY_SECONDS) from None
    except BaseException:
        slots.release()
        raise

    return Submitted(job, speech, started, slots)


async def _wait_first(job: Job, timeout: float, receive) -> bytes:
    """Waits for the job's turn, then its first chunk, unless the client leaves first.

    The queue-wait deadline covers only the wait for a turn; first-chunk
    generation is not queueing. Neither starlette nor uvicorn cancels a handler
    whose client disconnects, so a watcher reads receive() for the disconnect.
    """
    started = asyncio.wrap_future(job.started)
    first = asyncio.wrap_future(job.first)
    watcher = asyncio.ensure_future(_disconnected(receive))
    try:
        try:
            done, _ = await asyncio.wait({started, watcher}, timeout=timeout, return_when=asyncio.FIRST_COMPLETED)
            if started in done and watcher not in done:
                done, _ = await asyncio.wait({first, watcher}, return_when=asyncio.FIRST_COMPLETED)
        finally:
            # A StreamingResponse reads receive() itself, so the watcher must be gone first.
            watcher.cancel()
            await asyncio.wait({watcher})
    except asyncio.CancelledError:
        # Covers a cancellation during the watcher's cleanup as well as during the waits.
        _abandon(job, first, started)
        job.answered.set()
        raise

    if first in done:
        try:
            return first.result()
        except Exception as exc:
            job.cancel.set()
            raise RequestError(HTTPStatus.INTERNAL_SERVER_ERROR, GENERATION_FAILED) from exc

    _abandon(job, first, started)
    if watcher in done:
        raise ClientGone()
    raise RequestError(HTTPStatus.SERVICE_UNAVAILABLE, "queue wait exceeded")


async def _disconnected(receive) -> None:
    while (await receive())["type"] != "http.disconnect":
        pass


def _abandon(job: Job, *waits: asyncio.Future) -> None:
    job.cancel.set()
    job.first.cancel()
    # Cancelled so a result the worker sets later is not delivered to a loop that may be closed.
    for wait in waits:
        wait.cancel()


async def _no_response(_scope, _receive, _send) -> None:
    """Sends nothing: the client has gone, and uvicorn drops a disconnected request without an error."""


async def _body(job: Job, first: bytes, speech: Speech, state: StreamState):
    state.started = True
    try:
        prefix = wav.header() if speech.fmt == "wav" else b""
        state.audio_bytes = len(first)
        yield prefix + first

        while (item := await asyncio.to_thread(job.out.get)) is not DONE:
            if isinstance(item, Failure):
                state.outcome = "failed"
                # The status line is already sent; dropping the connection is the only signal left.
                raise StreamAborted(str(item.error)) from item.error
            state.audio_bytes += len(item)
            yield item

        state.outcome = "done"
    finally:
        # Stops generation as soon as the stream ends, before the request is finalised.
        job.cancel.set()


async def _finish_stream(body, state: StreamState, submitted: Submitted) -> None:
    """Finalises a streamed request; a disconnect can cancel the response before _body starts or at a yield."""
    try:
        await body.aclose()
    finally:
        speech = submitted.speech
        submitted.finish(
            status=int(HTTPStatus.OK),
            chars=len(speech.text),
            segments=len(submitted.job.segments),
            format=speech.fmt,
            outcome=state.outcome if state.started else "disconnected",
            audio_bytes=state.audio_bytes,
        )


def _error_response(exc: RequestError, submitted: Submitted | None):
    headers = {"Retry-After": str(exc.retry_after)} if exc.retry_after else None
    response = JSONResponse({"error": {"message": exc.message}}, status_code=exc.status, headers=headers)
    fields = {"status": int(exc.status), "error": exc.message}
    if exc.__cause__:
        fields["cause"] = str(exc.__cause__)

    if submitted is None:
        emit("tts", **fields)
        return response

    async def finish() -> None:
        submitted.finish(**fields)

    return Finalising(response, finish)
