import logging
import os
import queue
import threading
from concurrent.futures import Future
from dataclasses import dataclass, field

log = logging.getLogger(__name__)

EXIT_LOAD_FAILED = 1
EXIT_FATAL = 3
FATAL_ERROR_NAMES = {"OutOfMemoryError", "AcceleratorError"}
# How long a fatal error waits for its request's error response before the process exits.
FATAL_EXIT_GRACE_SECONDS = 2.0

DONE = object()


class WorkerStopped(Exception):
    pass


@dataclass(frozen=True)
class Failure:
    error: BaseException


@dataclass
class Job:
    voice: str
    segments: list[str]
    cancel: threading.Event = field(default_factory=threading.Event)
    # Resolved when the worker takes the job; the queue-wait deadline applies until then.
    started: Future = field(default_factory=Future)
    first: Future = field(default_factory=Future)
    out: queue.Queue = field(default_factory=queue.Queue)
    # Set by the app once the client has its answer, so a fatal exit does not cut it off.
    answered: threading.Event = field(default_factory=threading.Event)


def is_fatal(exc: BaseException) -> bool:
    if type(exc).__name__ in FATAL_ERROR_NAMES:
        return True

    return isinstance(exc, RuntimeError) and "CUDA" in str(exc)


class Worker:
    """Loads and runs the engine on one thread so CUDA state and captured graphs are never shared."""

    def __init__(self, engine, queue_size: int, exit_hook=os._exit, fatal_exit_grace: float = FATAL_EXIT_GRACE_SECONDS):
        self.engine = engine
        self.jobs: queue.Queue[Job] = queue.Queue(maxsize=queue_size)
        self.ready = False
        self._exit = exit_hook
        self._fatal_exit_grace = fatal_exit_grace
        self._thread = threading.Thread(target=self._run, name="tts-worker", daemon=True)

    def start(self) -> None:
        self._thread.start()

    def alive(self) -> bool:
        return self._thread.is_alive()

    def submit(self, job: Job) -> None:
        self.jobs.put_nowait(job)

    def run_one(self, job: Job) -> None:
        if job.cancel.is_set() or not job.first.set_running_or_notify_cancel():
            return
        if not job.started.set_running_or_notify_cancel():
            return
        job.started.set_result(None)

        try:
            self._produce(job)
        except Exception as exc:
            self._fail(job, exc)
        except BaseException as exc:
            # The thread is about to die; a reader waiting on this job must still see an end.
            self._deliver(job, WorkerStopped(f"worker stopped: {exc!r}"))
            raise

    def _run(self) -> None:
        if not self._load():
            return

        while True:
            self.run_one(self.jobs.get())

    def _load(self) -> bool:
        try:
            self.engine.load()
        except Exception:
            log.exception("model load failed")
            self._exit(EXIT_LOAD_FAILED)
            return False

        self.ready = True
        return True

    def _produce(self, job: Job) -> None:
        chunks = self.engine.stream(job.voice, job.segments)
        try:
            job.first.set_result(next(chunks, b""))
            # Checked before each next(), which starts generating a chunk, and after it,
            # so a chunk finished after the cancel is dropped.
            while not job.cancel.is_set():
                chunk = next(chunks, DONE)
                if chunk is DONE or job.cancel.is_set():
                    break
                job.out.put(chunk)
        finally:
            chunks.close()

        # Always terminate the stream so a reader blocked on out.get() wakes up.
        job.out.put(DONE)

    def _fail(self, job: Job, exc: Exception) -> None:
        self._deliver(job, exc)
        if not is_fatal(exc):
            return

        log.critical("fatal engine error, exiting: %s", exc)
        job.answered.wait(self._fatal_exit_grace)
        self._exit(EXIT_FATAL)

    @staticmethod
    def _deliver(job: Job, exc: Exception) -> None:
        if job.first.done():
            job.out.put(Failure(exc))
        else:
            job.first.set_exception(exc)
