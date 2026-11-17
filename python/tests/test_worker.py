import threading
import time

import pytest

from conftest import FakeEngine, SteppedEngine
from qwen_tts_server.app import RequestSlots
from qwen_tts_server.engine import RunawaySegment
from qwen_tts_server.worker import DONE, EXIT_FATAL, EXIT_LOAD_FAILED, Failure, Job, Worker, is_fatal

TIMEOUT = 5


def drain(job):
    items = []
    while (item := job.out.get(timeout=TIMEOUT)) is not DONE:
        items.append(item)
        if isinstance(item, Failure):
            break
    return items


def test_streams_first_chunk_then_rest():
    engine = FakeEngine(chunks=[b"a", b"b", b"c"])
    job = Job("robbie", ["Hello."])

    Worker(engine, 1).run_one(job)

    assert job.first.result() == b"a"
    assert drain(job) == [b"b", b"c"]
    assert engine.calls == [("robbie", ["Hello."])]


def test_cancel_before_turn_is_skipped():
    engine = FakeEngine()
    job = Job("robbie", ["Hello."])
    job.cancel.set()

    Worker(engine, 1).run_one(job)

    assert engine.calls == []
    assert job.out.empty()


def test_cancelled_future_is_skipped():
    engine = FakeEngine()
    job = Job("robbie", ["Hello."])
    job.first.cancel()

    Worker(engine, 1).run_one(job)

    assert engine.calls == []


def test_cancel_mid_stream_stops_within_one_chunk():
    engine = SteppedEngine()
    worker = Worker(engine, 1)
    worker.start()
    job = Job("robbie", ["Hello."])
    worker.submit(job)

    engine.gate.release()
    assert job.first.result(timeout=TIMEOUT) == b"\x01"
    engine.gate.release()
    assert job.out.get(timeout=TIMEOUT) == b"\x02"

    job.cancel.set()
    engine.gate.release()

    assert job.out.get(timeout=TIMEOUT) is DONE
    assert engine.closed.wait(TIMEOUT)
    engine.gate.release()
    engine.gate.release()
    assert engine.produced == 3


def test_error_before_first_chunk_fails_the_future():
    error = ValueError("bad prompt")
    job = Job("robbie", ["Hello."])

    Worker(FakeEngine(error=error, fail_at=0), 1, exit_hook=pytest.fail).run_one(job)

    assert job.first.exception() is error


def test_error_after_first_chunk_is_queued_as_failure():
    error = ValueError("decode failed")
    job = Job("robbie", ["Hello."])

    Worker(FakeEngine(chunks=[b"a", b"b"], error=error, fail_at=1), 1, exit_hook=pytest.fail).run_one(job)

    assert job.first.result() == b"a"
    assert drain(job) == [Failure(error)]


def test_runaway_segment_is_not_fatal():
    exits = []
    error = RunawaySegment("segment reached 600 steps")
    job = Job("robbie", ["Hello."])

    Worker(FakeEngine(error=error, fail_at=1), 1, exit_hook=exits.append).run_one(job)

    assert drain(job) == [Failure(error)]
    assert exits == []


def test_fatal_cuda_error_calls_exit_hook():
    exits = []
    error = RuntimeError("CUDA error: an illegal memory access was encountered")
    job = Job("robbie", ["Hello."])

    job.answered.set()
    Worker(FakeEngine(error=error, fail_at=0), 1, exit_hook=exits.append).run_one(job)

    assert exits == [EXIT_FATAL]
    assert job.first.exception() is error


def test_fatal_error_exits_after_the_grace_when_nobody_answers():
    exits = []
    grace = 0.05
    job = Job("robbie", ["Hello."])
    worker = Worker(FakeEngine(error=RuntimeError("CUDA error: launch failure"), fail_at=0), 1, exit_hook=exits.append, fatal_exit_grace=grace)

    started = time.monotonic()
    worker.run_one(job)

    assert exits == [EXIT_FATAL]
    assert time.monotonic() - started >= grace


def test_fatal_error_waits_for_the_answer_before_exiting():
    exits = []
    job = Job("robbie", ["Hello."])
    worker = Worker(FakeEngine(error=RuntimeError("CUDA error: launch failure"), fail_at=0), 1, exit_hook=exits.append)
    runner = threading.Thread(target=worker.run_one, args=(job,))

    runner.start()
    assert isinstance(job.first.exception(timeout=TIMEOUT), RuntimeError)
    time.sleep(0.1)
    assert exits == []

    job.answered.set()
    runner.join(TIMEOUT)
    assert exits == [EXIT_FATAL]


def test_jobs_run_first_in_first_out():
    engine = FakeEngine(chunks=[b"a"])
    worker = Worker(engine, 3)
    jobs = [Job(voice, [f"{voice}."]) for voice in ("robbie", "stefan", "james")]
    for job in jobs:
        worker.submit(job)

    worker.start()
    for job in jobs:
        job.first.result(timeout=TIMEOUT)

    assert [voice for voice, _ in engine.calls] == ["robbie", "stefan", "james"]


class BrokenEngine(FakeEngine):
    def load(self):
        raise OSError("no model")


def test_load_failure_calls_exit_hook():
    exits = []
    worker = Worker(BrokenEngine(), 1, exit_hook=exits.append)

    worker.start()
    worker._thread.join(TIMEOUT)

    assert exits == [EXIT_LOAD_FAILED]
    assert not worker.ready


@pytest.mark.parametrize(
    "error, fatal",
    [
        (type("OutOfMemoryError", (RuntimeError,), {})("CUDA out of memory"), True),
        (type("AcceleratorError", (RuntimeError,), {})("device-side assert"), True),
        (RuntimeError("CUDA error: unspecified launch failure"), True),
        (RuntimeError("shape mismatch"), False),
        (RunawaySegment("CUDA-free message"), False),
        (ValueError("CUDA mentioned in a value error"), False),
    ],
)
def test_is_fatal(error, fatal):
    assert is_fatal(error) is fatal


def test_worker_thread_loads_then_ready():
    engine = FakeEngine()
    engine.release_load.clear()
    worker = Worker(engine, 1)

    worker.start()
    assert not worker.ready

    engine.release_load.set()
    done = threading.Event()
    job = Job("robbie", ["Hello."])
    job.first.add_done_callback(lambda _: done.set())
    worker.submit(job)

    assert done.wait(TIMEOUT)
    assert worker.ready


def test_cancel_during_first_chunk_generation_produces_one_chunk():
    engine = SteppedEngine()
    worker = Worker(engine, 1)
    worker.start()
    job = Job("robbie", ["Hello."])
    worker.submit(job)
    assert engine.started.wait(TIMEOUT)

    job.cancel.set()
    engine.gate.release()

    assert job.first.result(timeout=TIMEOUT) == b"\x01"
    assert engine.closed.wait(TIMEOUT)
    assert engine.produced == 1
    assert job.out.get(timeout=TIMEOUT) is DONE


def test_releasing_a_slot_nobody_holds_raises():
    slots = RequestSlots(1)
    assert slots.acquire()
    slots.release()

    with pytest.raises(RuntimeError, match="no request slot is held"):
        slots.release()

    assert slots.in_use == 0
    assert slots.acquire()
    assert not slots.acquire()
