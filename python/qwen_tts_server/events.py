import json
import logging

_log = logging.getLogger("qwen_tts_server")


def emit(event: str, **fields) -> None:
    _log.info(json.dumps({"event": event, **fields}))
