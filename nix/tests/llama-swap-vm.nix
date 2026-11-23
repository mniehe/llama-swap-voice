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

  # A second voice with its own clip, so a recorded request shows which
  # voice the service chose. A quarter second keeps the recorded body small
  # enough for the driver's console.
  narratorClip = "/srv/clips/narrator.wav";
  narratorSource = pkgs.runCommand "narrator.wav" {nativeBuildInputs = [pkgs.python3];} ''
    python3 -I - ${fixture} $out <<'EOF'
    import sys, wave
    with wave.open(sys.argv[1], "rb") as src:
        params = src.getparams()
        frames = src.readframes(params.framerate // 4)
    with wave.open(sys.argv[2], "wb") as dst:
        dst.setparams(params)
        dst.writeframes(frames)
    EOF
  '';

  wyomingClient = pkgs.writeShellScriptBin "wyoming-client" ''
    exec ${pkgs.python3}/bin/python3 -I ${./wyoming-client.py} "$@"
  '';

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
      "C ${narratorClip} - - - - ${narratorSource}"
      "z ${narratorClip} 0640 root llama-swap-voice -"
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
      voices.narrator.clip = narratorClip;
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

  # The round trip stays on loopback; wyoming-vm.nix covers remote clients.
  wyomingNode = backend: {
    imports = [(node backend)];
    services.llama-swap-voice.wyoming = {
      enable = true;
      speed = 1.0;
    };
    environment.systemPackages = [wyomingClient];
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
      llamacpp = wyomingNode "llamacpp";
      qwentts = wyomingNode "qwentts";
      pinned = chatNode {};
      takeTurns = chatNode {llamaSwap.group.sharing = "takeTurns";};
    };

    testScript = {nodes, ...}: let
      models = node: node.services.llama-swap.settings.models;
    in ''
      import base64
      import email.parser
      import email.policy
      import hashlib
      import json
      import re
      import wave

      FIXTURE = "${fixture}"
      STATE_DIR = "${stateDir}"
      TRANSCRIPT = "${transcript}"
      LLAMA_RECORD = "/var/cache/llama-swap/fake-llama-server.jsonl"
      QWEN_RECORD = "/var/cache/llama-swap/qwen-tts-server/requests.jsonl"
      ASR_MODEL = "${fakes.artifacts.asr.model.path}"
      TTS_MODEL = "${fakes.artifacts.tts.model.path}"
      QWEN_CMD = "${(models nodes.qwentts).qwen3-tts.cmd}"
      SPEECH = "http://127.0.0.1:8920/v1/audio/speech"
      NARRATOR_CLIP = "${narratorClip}"


      # Complete lines only: the fake appends a response record after its reply
      # has gone, so a read can meet that line half-written.
      def records(machine, path):
          lines = machine.succeed(f"cat {path}").split("\n")[:-1]
          return [json.loads(line) for line in lines]


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
          narrator_sha256 = qwentts.succeed(f"sha256sum {NARRATOR_CLIP}").split()[0]
          assert start["clips"] == {"sample": clip_sha256, "narrator": narrator_sha256}, start["clips"]

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


      def wyoming(machine, args):
          return json.loads(machine.succeed(f"wyoming-client {args}"))


      def multipart_fields(content_type, raw):
          head = f"Content-Type: {content_type}\r\n\r\n".encode()
          message = email.parser.BytesParser(policy=email.policy.HTTP).parsebytes(head + raw)
          return {part.get_param("name", header="content-disposition"): part.get_payload(decode=True) for part in message.iter_parts()}


      with wave.open(FIXTURE, "rb") as reply_wav:
          REPLY_PCM_SHA256 = hashlib.sha256(reply_wav.readframes(reply_wav.getnframes())).hexdigest()

      # The Wyoming listener binds before the HTTP one, so the waits for port
      # 8920 above already cover port 10300.
      with subtest("wyoming: info names the declared voices, the default first"):
          for machine in (llamacpp, qwentts):
              info = wyoming(machine, "describe")
              assert info["tts_voices"] == [
                  {"name": "sample", "description": "sample (default)"},
                  {"name": "narrator", "description": "narrator"},
              ], info
              assert info["asr_models"] == ["qwen3-asr"], info
              assert info["supports_synthesize_streaming"] == [True], info

      with subtest("wyoming: a transcription of the fixture returns the fake model's text"):
          for machine in (llamacpp, qwentts):
              reply = wyoming(machine, f"transcribe {FIXTURE}")
              assert reply == {"text": TRANSCRIPT}, reply
              asr = [r for r in records(machine, LLAMA_RECORD) if r["event"] == "request" and r["path"] == "/v1/audio/transcriptions"]
              assert len(asr) == 2, asr

      # Waits up to 5 s for a response record per request, then summarises both,
      # so a broken reply shows how many bytes the fake wrote.
      def wire(machine, path):
          machine.execute(
              f"for _ in $(seq 50); do "
              f"[ $(grep -c '\"event\": \"response\"' {path}) -ge $(grep -c '\"event\": \"request\"' {path}) ] && break; "
              f"sleep 0.1; done"
          )
          entries = records(machine, path)
          return {
              "requests": [(r["path"], r["framing"]) for r in entries if r["event"] == "request"],
              "responses": [
                  (r["path"], r["status"], r["content_length"], r["headers_sent"], r["bytes_written"], r["error"])
                  for r in entries
                  if r["event"] == "response"
              ],
          }


      # The second chunk completes the sentence, so the service speaks it before
      # synthesize-stop, and the client reads that audio before sending stop.
      STREAM_CHUNKS = "'The kitchen ' 'lights are off. '"
      STOP_SENT = {}

      with subtest("wyoming: a streamed synthesis speaks the completed sentence before synthesize-stop"):
          for machine in (llamacpp, qwentts):
              status, output = machine.execute(f"wyoming-client synthesize-stream --out /tmp/wyoming.pcm narrator {STREAM_CHUNKS}")
              record = QWEN_RECORD if machine is qwentts else LLAMA_RECORD
              print(f"{machine.name}: wyoming-client status {status}, output {output.strip()}, wire {json.dumps(wire(machine, record))}")
              assert status == 0, output
              reply = json.loads(output)
              assert reply["events"] == ["audio-start", "audio-chunk", "audio-stop", "synthesize-stopped"], reply
              assert reply["audio_before_stop"] > 0, reply
              STOP_SENT[machine.name] = reply["stop_sent_monotonic"]
              assert reply["audio_start"] == {"rate": 24000, "width": 2, "channels": 1}, reply
              assert reply["sha256"] == REPLY_PCM_SHA256, (reply, REPLY_PCM_SHA256)
              on_disk = machine.succeed("sha256sum /tmp/wyoming.pcm").split()[0]
              assert on_disk == REPLY_PCM_SHA256, on_disk

      with subtest("llamacpp: the streamed synthesis sent the sentence with the narrator's clip in one request before stop"):
          tts = [r for r in records(llamacpp, LLAMA_RECORD) if r["event"] == "request" and r["path"] == "/tts"]
          assert len(tts) == 2, tts
          assert tts[-1]["monotonic"] < STOP_SENT["llamacpp"], (tts[-1]["monotonic"], STOP_SENT["llamacpp"])
          fields = multipart_fields(tts[-1]["content_type"], request_body(llamacpp, LLAMA_RECORD, tts[-1]))
          assert fields.get("input") == TRANSCRIPT.encode(), fields.get("input")
          narrator_sha256 = llamacpp.succeed(f"sha256sum {NARRATOR_CLIP}").split()[0]
          speaker_ref = hashlib.sha256(fields.get("speaker_ref", b"")).hexdigest()
          assert speaker_ref == narrator_sha256, (speaker_ref, narrator_sha256)

      with subtest("qwentts: the streamed synthesis named the narrator voice in one request before stop"):
          request = [r for r in records(qwentts, QWEN_RECORD) if r["event"] == "request"]
          assert len(request) == 2, request
          assert request[-1]["monotonic"] < STOP_SENT["qwentts"], (request[-1]["monotonic"], STOP_SENT["qwentts"])
          raw = request_body(qwentts, QWEN_RECORD, request[-1])
          assert json.loads(raw) == {"voice": "narrator", "input": TRANSCRIPT, "response_format": "wav"}, raw


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
