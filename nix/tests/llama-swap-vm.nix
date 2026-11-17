# The module's llama-swap contribution under nixpkgs' own llama-swap, with a
# fake llama-server and a fake qwen-tts-server in place of the models.
{
  pkgs,
  module,
}: let
  fakes = import ./fakes.nix {inherit pkgs;};
  fixture = ../../internal/audio/testdata/speech.wav;
  clip = "/srv/clips/sample.wav";
  stateDir = "/var/lib/llama-swap-voice";
  transcript = "The kitchen lights are off.";

  # Not llama-swap's default port, so an upstreamURL that ignores
  # services.llama-swap misses it.
  llamaSwapPort = 8090;

  node = backend: {
    imports = [module];

    systemd.tmpfiles.rules = [
      "d /srv/clips 0755 root root -"
      "C ${clip} - - - - ${fixture}"
      # Readable by the module's group and nobody else, as the docs ask.
      "z ${clip} 0640 root llama-swap-voice -"
    ];

    services.llama-swap = {
      enable = true;
      port = llamaSwapPort;
    };

    services.llama-swap-voice = {
      enable = true;
      tts.backend = backend;
      defaultVoice = "sample";
      voices.sample = {
        inherit clip transcript;
      };
      llamaCpp.package = fakes.llamaServer;
      models = {
        asr.artifacts = fakes.artifacts.asr;
        tts.artifacts = fakes.artifacts.tts;
      };
      tts.qwentts = {
        package = fakes.qwenTtsServer;
        model = "/srv/models/qwen3-tts";
      };
    };

    environment.systemPackages = [pkgs.curl pkgs.jq];
    virtualisation.memorySize = 1024;
  };

  # The consumer's own model in no group, so llama-swap puts it in its
  # implicit default group, which is exclusive.
  chatNode = voice: {
    imports = [(node "llamacpp")];
    services.llama-swap.settings.models.chat = {
      cmd = "${fakes.llamaServer}/bin/llama-server --port \${PORT} -m ${fakes.artifacts.chat.model.path} --mmproj ${fakes.artifacts.chat.mmproj.path}";
      checkEndpoint = "/health";
    };
    services.llama-swap-voice = voice;
  };
in
  pkgs.testers.runNixOSTest {
    name = "llama-swap-voice-llama-swap";

    nodes = {
      llamacpp = node "llamacpp";
      qwentts = node "qwentts";
      pinned = chatNode {};
      takeTurns = chatNode {llamaSwap.group.sharing = "takeTurns";};
    };

    testScript = {nodes, ...}: let
      models = node: node.services.llama-swap.settings.models;
    in ''
      import base64
      import hashlib
      import json
      import re

      FIXTURE = "${fixture}"
      STATE_DIR = "${stateDir}"
      TRANSCRIPT = "${transcript}"
      LLAMA_RECORD = "/var/cache/llama-swap/fake-llama-server.jsonl"
      QWEN_RECORD = "/var/cache/llama-swap/qwen-tts-server/requests.jsonl"
      ASR_MODEL = "${fakes.artifacts.asr.model.path}"
      TTS_MODEL = "${fakes.artifacts.tts.model.path}"
      QWEN_CMD = "${(models nodes.qwentts).qwen3-tts.cmd}"
      SPEECH = "http://127.0.0.1:8920/v1/audio/speech"


      def records(machine, path):
          return [json.loads(line) for line in machine.succeed(f"cat {path}").splitlines()]


      def request_body(machine, path, request):
          digest = request["body_sha256"]
          raw = base64.b64decode(machine.succeed(f"base64 -w 0 {path}.bodies/{digest}"))
          assert hashlib.sha256(raw).hexdigest() == digest, f"body {digest} arrived damaged: {len(raw)} bytes"
          return raw


      def speech(machine, out):
          body = json.dumps({"input": TRANSCRIPT, "voice": "sample", "response_format": "wav"})
          return machine.succeed(
              f"curl -sS -o {out} -w '%{{http_code}} %{{content_type}}' "
              f"-H 'Content-Type: application/json' -d '{body}' {SPEECH}"
          ).strip()


      # Both entries ready at once: in llama-swap's implicit default group they
      # would stop each other.
      BOTH_READY = (
          "curl -sSf http://localhost:${toString llamaSwapPort}/running | "
          "jq -e '[.running[] | select(.state == \"ready\") | .model] | sort == [\"qwen3-asr\", \"qwen3-tts\"]'"
      )


      def starts(machine, path, model):
          return [r for r in records(machine, path) if r["event"] == "start" and r["model"] == model]


      def started(machine, path, model):
          machine.wait_until_succeeds(f"grep -F '\"event\": \"start\"' {path} | grep -qF '\"model\": {json.dumps(model)}'", timeout=60)


      start_all()
      for machine in (llamacpp, qwentts):
          machine.wait_for_unit("llama-swap.service")
          machine.wait_for_open_port(${toString llamaSwapPort})
          machine.wait_for_unit("llama-swap-voice.service")
          machine.wait_for_open_port(8920)

      with subtest("preload starts the asr and tts entries without a request"):
          started(llamacpp, LLAMA_RECORD, ASR_MODEL)
          started(llamacpp, LLAMA_RECORD, TTS_MODEL)
          started(qwentts, LLAMA_RECORD, ASR_MODEL)
          started(qwentts, QWEN_RECORD, None)

      with subtest("after preload both entries are loaded"):
          for machine in (llamacpp, qwentts):
              machine.wait_until_succeeds(BOTH_READY, timeout=30)

      with subtest("llamacpp: speech goes through llama-swap to llama-server's /tts"):
          status = speech(llamacpp, "/tmp/out.wav")
          assert status == "200 audio/wav", status
          llamacpp.succeed("head -c 4 /tmp/out.wav | grep -q RIFF")
          tts = [r for r in records(llamacpp, LLAMA_RECORD) if r["event"] == "request" and r["path"] == "/tts"]
          assert len(tts) == 1, tts
          assert tts[0]["model"] == TTS_MODEL, tts[0]["model"]
          assert tts[0]["content_type"].startswith("multipart/form-data"), tts[0]["content_type"]

      with subtest("a transcription reaches the asr entry"):
          for machine in (llamacpp, qwentts):
              reply = machine.succeed(
                  f"curl -sS -w '\\n%{{http_code}}' -F file=@{FIXTURE} http://127.0.0.1:8920/v1/audio/transcriptions"
              )
              body, status = reply.rsplit("\n", 1)
              assert status == "200", reply
              assert json.loads(body) == {"text": TRANSCRIPT}, body
              asr = [r for r in records(machine, LLAMA_RECORD) if r["event"] == "request" and r["path"] == "/v1/audio/transcriptions"]
              assert len(asr) == 1, asr
              assert asr[0]["model"] == ASR_MODEL, asr[0]["model"]

      with subtest("qwentts: the server was started with the module's config"):
          found = re.fullmatch(r".* --config (\S+)", QWEN_CMD)
          assert found is not None, QWEN_CMD
          config_path = found.group(1)
          start = [r for r in records(qwentts, QWEN_RECORD) if r["event"] == "start"]
          assert len(start) == 1, start
          start = start[0]
          assert start["config_path"] == config_path, (start["config_path"], config_path)
          assert start["cuda_visible_devices"] == "0", start["cuda_visible_devices"]
          # The server's launcher finds its cache root through this.
          assert start["xdg_cache_home"] == "/var/cache/llama-swap", start["xdg_cache_home"]
          voice = start["config"]["voices"]["sample"]
          assert voice["clip"] == f"{STATE_DIR}/voices/sample.wav", voice
          assert voice["transcript"] == TRANSCRIPT, voice
          # The fake read the clip through the voices directory as llama-swap's user.
          clip_sha256 = qwentts.succeed(f"sha256sum {FIXTURE}").split()[0]
          assert start["clips"] == {"sample": clip_sha256}, start["clips"]

      with subtest("qwentts: speech posts JSON naming the voice, with no clip bytes"):
          status = speech(qwentts, "/tmp/out.wav")
          request = [r for r in records(qwentts, QWEN_RECORD) if r["event"] == "request"]
          assert len(request) == 1, request
          raw = request_body(qwentts, QWEN_RECORD, request[0])
          assert b"RIFF" not in raw and b"Content-Disposition" not in raw, raw[:300]
          assert json.loads(raw) == {"voice": "sample", "input": TRANSCRIPT, "response_format": "wav"}, raw
          assert request[0]["path"] == "/v1/tts", request[0]["path"]
          assert request[0]["content_type"] == "application/json", request[0]["content_type"]
          assert status == "200 audio/wav", status

      with subtest("qwentts: no llama.cpp tts entry exists"):
          tts_starts = [r for r in records(qwentts, LLAMA_RECORD) if r["event"] == "start" and r["model"] == TTS_MODEL]
          assert tts_starts == [], tts_starts

      with subtest("after speech and a transcription both entries are still loaded, each started once"):
          for machine in (llamacpp, qwentts):
              machine.succeed(BOTH_READY)
          assert len(starts(llamacpp, LLAMA_RECORD, ASR_MODEL)) == 1
          assert len(starts(llamacpp, LLAMA_RECORD, TTS_MODEL)) == 1
          assert len(starts(qwentts, LLAMA_RECORD, ASR_MODEL)) == 1
          assert len(starts(qwentts, QWEN_RECORD, None)) == 1


      # Every curl below is bounded, so a hung load fails the subtest rather
      # than the driver's global timeout.
      CURL = "curl -sS --max-time 30"
      LLAMA_SWAP = "http://localhost:${toString llamaSwapPort}"
      CHAT_BODY = json.dumps({"model": "chat", "messages": [{"role": "user", "content": "hi"}]})
      SPEECH_BODY = json.dumps({"input": TRANSCRIPT, "voice": "sample", "response_format": "wav"})
      VOICE_READY = (
          f"{CURL} -f {LLAMA_SWAP}/running | "
          "jq -e '[.running[] | select(.state == \"ready\") | .model] | sort == [\"qwen3-asr\", \"qwen3-tts\"]'"
      )


      def running(machine):
          reply = json.loads(machine.succeed(f"{CURL} -f {LLAMA_SWAP}/running"))
          return sorted((r["model"], r["state"]) for r in reply["running"])


      # llama-swap stops the evicted models before it starts the target, and
      # replies only once the target is ready, so /running is settled when
      # each of these returns.
      def chat(machine):
          machine.succeed(f"{CURL} -f -H 'Content-Type: application/json' -d '{CHAT_BODY}' {LLAMA_SWAP}/v1/chat/completions")


      def transcribe(machine):
          return machine.succeed(
              f"{CURL} -o /dev/null -w '%{{http_code}}' -F file=@{FIXTURE} http://127.0.0.1:8920/v1/audio/transcriptions"
          ).strip()


      def speak(machine):
          return machine.succeed(
              f"{CURL} -o /dev/null -w '%{{http_code}}' -H 'Content-Type: application/json' -d '{SPEECH_BODY}' {SPEECH}"
          ).strip()


      for machine in (pinned, takeTurns):
          machine.wait_for_unit("llama-swap.service")
          machine.wait_for_open_port(${toString llamaSwapPort})
          machine.wait_for_unit("llama-swap-voice.service")
          machine.wait_for_open_port(8920)
          machine.wait_until_succeeds(VOICE_READY, timeout=30)

      with subtest("pinned: a default-group model loads beside the voice entries without stopping them"):
          chat(pinned)
          state = running(pinned)
          assert state == [("chat", "ready"), ("qwen3-asr", "ready"), ("qwen3-tts", "ready")], state
          assert len(starts(pinned, LLAMA_RECORD, ASR_MODEL)) == 1
          assert len(starts(pinned, LLAMA_RECORD, TTS_MODEL)) == 1

      with subtest("takeTurns: a default-group model stops the voice entries"):
          chat(takeTurns)
          state = running(takeTurns)
          assert state == [("chat", "ready")], state

      with subtest("takeTurns: a transcription through the service stops the default-group model"):
          status = transcribe(takeTurns)
          assert status == "200", status
          state = running(takeTurns)
          assert state == [("qwen3-asr", "ready")], state

      with subtest("takeTurns: speech then loads beside the asr entry"):
          status = speak(takeTurns)
          assert status == "200", status
          state = running(takeTurns)
          assert state == [("qwen3-asr", "ready"), ("qwen3-tts", "ready")], state
          assert len(starts(takeTurns, LLAMA_RECORD, ASR_MODEL)) == 2
          assert len(starts(takeTurns, LLAMA_RECORD, TTS_MODEL)) == 2
    '';
  }
