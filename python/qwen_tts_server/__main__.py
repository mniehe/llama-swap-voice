import argparse
import logging
import sys

import uvicorn

from .app import create_app
from .config import ConfigError, load
from .engine import Engine
from .worker import Worker

DEFAULT_HOST = "127.0.0.1"
EXIT_CONFIG = 2


def main() -> None:
    args = _parse_args()
    logging.basicConfig(level=logging.INFO, stream=sys.stderr, format="%(message)s")

    try:
        cfg = load(args.config)
    except (ConfigError, OSError, ValueError) as exc:
        print(f"qwen-tts-server: config {args.config}: {exc}", file=sys.stderr)
        sys.exit(EXIT_CONFIG)

    worker = Worker(Engine(cfg), cfg.queue_size)
    worker.start()

    uvicorn.run(create_app(cfg, worker), host=args.host, port=args.port, access_log=False, log_level="warning")


def _parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(prog="qwen-tts-server")
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--host", default=DEFAULT_HOST)
    parser.add_argument("--config", required=True)
    return parser.parse_args()


if __name__ == "__main__":
    main()
