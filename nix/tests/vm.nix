{
  pkgs,
  module,
}: let
  fixture = ../../internal/audio/testdata/speech.wav;
  clip = "/srv/clips/sample.wav";
  stateDir = "/var/lib/llama-swap-voice";
  record = "/var/lib/fake-upstream/requests.jsonl";
  recordingsDir = "${stateDir}/recordings";
  percentClip = "/srv/clips/speaker%n.wav";

  # A second address on the llamacpp node stands in for an upstream on
  # another host: it is not loopback, so the default policy denies it.
  remoteAddress = "192.0.2.10";
  remoteURL = "http://${remoteAddress}:8082";

  # Larger than the service's 8 MiB in-memory multipart limit, so the upload
  # spools to the unit's private /tmp before it is decoded.
  longUpload = pkgs.runCommand "speech-long.wav" {nativeBuildInputs = [pkgs.python3];} ''
    python3 -I - ${fixture} $out <<'EOF'
    import os, sys, wave
    with wave.open(sys.argv[1], "rb") as src:
        params = src.getparams()
        frames = src.readframes(params.nframes)
    with wave.open(sys.argv[2], "wb") as dst:
        dst.setparams(params)
        for _ in range(100):
            dst.writeframes(frames)
    assert os.path.getsize(sys.argv[2]) > 8 << 20
    EOF
  '';

  fakeUpstream = {
    name,
    backend,
    bind ? "127.0.0.1",
    port ? 8080,
  }: {
    wantedBy = ["multi-user.target"];
    after = ["network.target"];
    before = ["llama-swap-voice.service"];
    serviceConfig = {
      DynamicUser = true;
      StateDirectory = name;
      Restart = "on-failure";
      ExecStart = "${pkgs.python3}/bin/python3 -I ${./fake-upstream.py} --backend ${backend} --voice sample --clip ${clip} --reply ${fixture} --record /var/lib/${name}/requests.jsonl --bind ${bind} --port ${toString port}";
    };
  };

  node = backend: {
    imports = [module];

    # The clip is copied onto the VM's disk at boot, so the module only ever
    # sees the runtime path as a string.
    systemd.tmpfiles.rules = [
      "d /srv/clips 0755 root root -"
      "C ${clip} - - - - ${fixture}"
    ];

    # The fake upstream stands in for llama-swap, so the module contributes
    # nothing to one.
    services.llama-swap-voice = {
      enable = true;
      llamaSwap.enable = false;
      tts.backend = backend;
      defaultVoice = "sample";
      voices.sample.clip = clip;
    };

    systemd.services.fake-upstream = fakeUpstream {
      name = "fake-upstream";
      inherit backend;
    };

    environment.systemPackages = [pkgs.curl pkgs.opus-tools];
    virtualisation.memorySize = 1024;
  };

  # The llamacpp node also switches to specialisations that change the
  # module's options at runtime, so each case runs the module's own output.
  llamacppExtras = {
    systemd.tmpfiles.rules = ["C ${builtins.replaceStrings ["%"] ["%%"] percentClip} - - - - ${fixture}"];

    networking.interfaces.lo.ipv4.addresses = [
      {
        address = remoteAddress;
        prefixLength = 32;
      }
    ];

    systemd.services.fake-upstream-remote = fakeUpstream {
      name = "fake-upstream-remote";
      backend = "llamacpp";
      bind = remoteAddress;
      port = 8082;
    };
    systemd.services.fake-upstream-v6 = fakeUpstream {
      name = "fake-upstream-v6";
      backend = "llamacpp";
      bind = "::1";
      port = 8081;
    };

    specialisation = {
      percent.configuration.services.llama-swap-voice.voices.percent.clip = percentClip;
      remoteDenied.configuration.services.llama-swap-voice = {
        upstreamURL = remoteURL;
        request.timeout = "3s";
      };
      remoteAllowed.configuration.services.llama-swap-voice = {
        upstreamURL = remoteURL;
        ipAddressAllow = [remoteAddress];
      };
      ipv6.configuration.services.llama-swap-voice.upstreamURL = "http://[::1]:8081";
    };
  };

  # Only the qwentts node keeps recordings, so the llamacpp node shows that
  # the default writes nothing.
  qwenttsExtras = {
    services.llama-swap-voice.recordings.enable = true;
  };
in
  pkgs.testers.runNixOSTest {
    name = "llama-swap-voice-module";

    nodes = {
      llamacpp = {imports = [(node "llamacpp") llamacppExtras];};
      qwentts = {imports = [(node "qwentts") qwenttsExtras];};
    };

    testScript = ''
      import base64
      import hashlib
      import json
      import re

      STATE_DIR = "${stateDir}"
      CLIP = "${clip}"
      PERCENT_CLIP = "${percentClip}"
      REMOTE_URL = "${remoteURL}"
      LONG_UPLOAD = "${longUpload}"
      RECORD = "${record}"
      RECORDINGS = "${recordingsDir}"
      FIXTURE = "${fixture}"
      SPEECH = "http://127.0.0.1:8920/v1/audio/speech"


      def speech(machine, fmt, out, voice="sample"):
          body = json.dumps({"input": "The kitchen lights are off.", "voice": voice, "response_format": fmt})
          return machine.succeed(
              f"curl -sS -o {out} -w '%{{http_code}} %{{content_type}}' "
              f"-H 'Content-Type: application/json' -d '{body}' {SPEECH}"
          ).strip()


      def requests(machine):
          lines = machine.succeed(f"cat {RECORD}").splitlines()
          return [json.loads(line) for line in lines]


      def request_body(machine, request):
          digest = request["body_sha256"]
          raw = base64.b64decode(machine.succeed(f"base64 -w 0 {RECORD}.bodies/{digest}"))
          assert hashlib.sha256(raw).hexdigest() == digest, f"body {digest} arrived damaged: {len(raw)} bytes"
          return raw


      def voices(machine):
          return json.loads(machine.succeed("curl -sSf http://127.0.0.1:8920/v1/audio/voices"))["voices"]


      def switch(machine, name):
          machine.succeed(f"/run/booted-system/specialisation/{name}/bin/switch-to-configuration test")
          machine.wait_for_open_port(8920)


      start_all()
      for machine in (llamacpp, qwentts):
          machine.wait_for_unit("fake-upstream.service")
          machine.wait_for_open_port(8080)
          machine.wait_for_unit("llama-swap-voice.service")
          machine.wait_for_open_port(8920)

      with subtest("each declared voice is a symlink to its clip"):
          for machine in (llamacpp, qwentts):
              machine.succeed(f"test -L {STATE_DIR}/voices/sample.wav")
              target = machine.succeed(f"readlink {STATE_DIR}/voices/sample.wav").strip()
              assert target == CLIP, f"sample.wav points at {target!r}, want {CLIP!r}"

      with subtest("llamacpp: wav and opus replies, clip uploaded as speaker_ref"):
          status = speech(llamacpp, "wav", "/tmp/out.wav")
          assert status == "200 audio/wav", status
          llamacpp.succeed("head -c 4 /tmp/out.wav | grep -q RIFF")

          status = speech(llamacpp, "opus", "/tmp/out.opus")
          assert status == "200 audio/ogg", status
          print(llamacpp.succeed("opusinfo /tmp/out.opus"))

          seen = requests(llamacpp)
          assert len(seen) == 2, seen
          for request in seen:
              assert request["problems"] == [], request["problems"]
              assert request["content_type"].startswith("multipart/form-data"), request["content_type"]

      with subtest("qwentts: wav and opus replies, JSON naming the voice and no file part"):
          for fmt, content_type in (("wav", "audio/wav"), ("opus", "audio/ogg")):
              status = speech(qwentts, fmt, f"/tmp/out.{fmt}")

              # The recorded body is checked before the status, so a
              # multipart upload fails on what was sent, not on the reply.
              request = requests(qwentts)[-1]
              raw = request_body(qwentts, request)
              assert b"Content-Disposition" not in raw and b"RIFF" not in raw, f"file part sent: {raw[:300]!r}"
              assert json.loads(raw) == {
                  "voice": "sample",
                  "input": "The kitchen lights are off.",
                  "response_format": "wav",
              }, raw
              assert request["content_type"] == "application/json", request["content_type"]
              assert request["path"] == "/upstream/qwen3-tts/v1/tts", request["path"]
              assert request["problems"] == [], request["problems"]

              assert status == f"200 {content_type}", status

          qwentts.succeed("head -c 4 /tmp/out.wav | grep -q RIFF")
          print(qwentts.succeed("opusinfo /tmp/out.opus"))
          assert len(requests(qwentts)) == 2

      with subtest("recordings: a transcription is saved as Opus with its metadata and listed"):
          owner = qwentts.succeed(f"stat -c '%a %U %G' {RECORDINGS}").strip()
          assert owner == "700 llama-swap-voice llama-swap-voice", owner

          reply = qwentts.succeed(
              f"curl -sS -w '\\n%{{http_code}}' -F file=@{FIXTURE} http://127.0.0.1:8920/v1/audio/transcriptions"
          )
          body, status = reply.rsplit("\n", 1)
          assert status == "200", reply
          transcript = json.loads(body)["text"]

          # The service saves in the background, after it has answered.
          qwentts.wait_until_succeeds(f"test $(ls -1 {RECORDINGS} | wc -l) -eq 2", timeout=30)
          names = sorted(qwentts.succeed(f"ls -1 {RECORDINGS}").split())
          assert len(names) == 2, names
          recording_id = names[0].removesuffix(".json")
          assert names == [f"{recording_id}.json", f"{recording_id}.opus"], names
          meta = json.loads(qwentts.succeed(f"cat {RECORDINGS}/{recording_id}.json"))
          assert meta["source"] == "openai", meta
          assert meta["transcript"] == transcript, meta
          assert meta["keep"] is False, meta
          assert meta["durationMs"] > 0, meta
          print(qwentts.succeed(f"opusinfo {RECORDINGS}/{recording_id}.opus"))

          status = qwentts.succeed(
              "curl -sS -o /dev/null -w '%{http_code}' -H 'Host: rebound.example' http://127.0.0.1:8920/recordings"
          ).strip()
          assert status == "403", f"a foreign Host got {status}"

          page = qwentts.succeed("curl -sSf http://127.0.0.1:8920/recordings")
          assert transcript in page, page
          assert f"/recordings/{recording_id}/audio" in page, page

          status = qwentts.succeed(
              f"curl -sS -o /tmp/recording.opus -w '%{{http_code}} %{{content_type}}' "
              f"http://127.0.0.1:8920/recordings/{recording_id}/audio"
          ).strip()
          assert status == "200 audio/ogg", status
          qwentts.succeed(f"cmp /tmp/recording.opus {RECORDINGS}/{recording_id}.opus")

          status = qwentts.succeed(
              f"curl -sS -o /dev/null -w '%{{http_code}} %{{redirect_url}}' -d keep=true "
              f"http://127.0.0.1:8920/recordings/{recording_id}/keep"
          ).strip()
          assert status == "303 http://127.0.0.1:8920/recordings", status
          meta = json.loads(qwentts.succeed(f"cat {RECORDINGS}/{recording_id}.json"))
          assert meta["keep"] is True, meta

      with subtest("recordings are off by default: no directory and no page"):
          llamacpp.fail(f"test -e {RECORDINGS}")
          status = llamacpp.succeed("curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:8920/recordings").strip()
          assert status == "404", status

      with subtest("a clip copied in by hand is listed and survives tmpfiles"):
          for machine in (llamacpp, qwentts):
              machine.succeed(f"install -m 0644 {CLIP} {STATE_DIR}/voices/handmade.wav")
              machine.succeed("systemctl restart systemd-tmpfiles-resetup.service")
              machine.succeed(f"test -f {STATE_DIR}/voices/handmade.wav")
              machine.succeed("systemctl restart llama-swap-voice.service")
              machine.wait_for_open_port(8920)
              listed = voices(machine)
              assert listed == ["handmade", "sample"], listed

      with subtest("a link that no longer points at the declared clip stops the start"):
          llamacpp.succeed(f"ln -sfn {STATE_DIR}/voices/handmade.wav {STATE_DIR}/voices/sample.wav")
          llamacpp.fail("systemctl restart llama-swap-voice.service")
          llamacpp.succeed(f"journalctl -u llama-swap-voice.service | grep -F 'is not a symlink to {CLIP}'")
          llamacpp.succeed(f"rm {STATE_DIR}/voices/sample.wav")

          # A regular file under a declared name must stop the start too.
          # Stopping first ends the restart loop, so the cursor separates the
          # messages of this attempt from the ones above.
          llamacpp.succeed("systemctl stop llama-swap-voice.service")
          llamacpp.succeed("systemctl reset-failed llama-swap-voice.service")
          cursor = llamacpp.succeed("journalctl -n0 --show-cursor | sed -n 's/^-- cursor: //p'").strip()
          llamacpp.succeed(f"install -m 0644 {CLIP} {STATE_DIR}/voices/sample.wav")
          llamacpp.fail("systemctl restart llama-swap-voice.service")
          llamacpp.succeed(
              f"journalctl -u llama-swap-voice.service --after-cursor='{cursor}' | grep -F 'is not a symlink to {CLIP}'"
          )
          llamacpp.succeed(f"rm {STATE_DIR}/voices/sample.wav")
          llamacpp.succeed("systemctl restart systemd-tmpfiles-resetup.service")
          llamacpp.succeed("systemctl reset-failed llama-swap-voice.service")
          llamacpp.succeed("systemctl restart llama-swap-voice.service")
          llamacpp.wait_for_open_port(8920)

      with subtest("a transcription upload spooled to the private /tmp is decoded and sent upstream"):
          reply = llamacpp.succeed(
              f"curl -sS -w '\\n%{{http_code}}' -F file=@{LONG_UPLOAD} http://127.0.0.1:8920/v1/audio/transcriptions"
          )
          body, status = reply.rsplit("\n", 1)
          assert status == "200", reply
          assert json.loads(body) == {"text": "The kitchen lights are off."}, body
          asr = [r for r in requests(llamacpp) if r["path"].endswith("/v1/audio/transcriptions")]
          assert len(asr) == 1, asr
          assert asr[0]["problems"] == [], asr[0]["problems"]

      with subtest("a clip path holding % is linked and served"):
          switch(llamacpp, "percent")
          target = llamacpp.succeed(f"readlink {STATE_DIR}/voices/percent.wav").strip()
          assert target == PERCENT_CLIP, f"percent.wav points at {target!r}, want {PERCENT_CLIP!r}"
          status = speech(llamacpp, "wav", "/tmp/percent.wav", voice="percent")
          assert status == "200 audio/wav", status

      with subtest("a non-loopback upstream is denied until ipAddressAllow names it"):
          llamacpp.wait_for_open_port(8082, "${remoteAddress}")
          llamacpp.succeed(f"curl -sSf {REMOTE_URL}/health")
          switch(llamacpp, "remoteDenied")
          status = speech(llamacpp, "wav", "/tmp/denied.wav")
          assert not status.startswith("200"), status
          llamacpp.fail("test -s /var/lib/fake-upstream-remote/requests.jsonl")

          switch(llamacpp, "remoteAllowed")
          status = speech(llamacpp, "wav", "/tmp/allowed.wav")
          assert status == "200 audio/wav", status
          llamacpp.succeed("test -s /var/lib/fake-upstream-remote/requests.jsonl")

      with subtest("an IPv6 loopback upstream is reachable"):
          llamacpp.succeed("curl -sSf 'http://[::1]:8081/health'")
          switch(llamacpp, "ipv6")
          status = speech(llamacpp, "wav", "/tmp/ipv6.wav")
          assert status == "200 audio/wav", status
          llamacpp.succeed("test -s /var/lib/fake-upstream-v6/requests.jsonl")

      with subtest("the unit is sandboxed"):
          for machine in (llamacpp, qwentts):
              shown = machine.succeed(
                  "systemctl show llama-swap-voice.service "
                  "-p User -p ProtectSystem -p IPAddressDeny -p IPAddressAllow -p MemoryDenyWriteExecute"
              )
              print(shown)
              assert "User=llama-swap-voice" in shown, shown
              assert "ProtectSystem=strict" in shown, shown
              assert "MemoryDenyWriteExecute=yes" in shown, shown
              pid = machine.succeed("systemctl show -p MainPID --value llama-swap-voice.service").strip()
              for path in ("/", "/var", STATE_DIR):
                  options = machine.succeed(
                      f"findmnt --task {pid} --noheadings --output OPTIONS --target {path}"
                  ).strip()
                  assert options.split(",")[0] == "ro", f"{path} is mounted {options} for the service"
          pid = qwentts.succeed("systemctl show -p MainPID --value llama-swap-voice.service").strip()
          options = qwentts.succeed(
              f"findmnt --task {pid} --noheadings --output OPTIONS --target {RECORDINGS}"
          ).strip()
          assert options.split(",")[0] == "rw", f"{RECORDINGS} is mounted {options} for the service"
          report = llamacpp.succeed("systemd-analyze security --no-pager llama-swap-voice.service")
          print(report)
          found = re.search(r"Overall exposure level for llama-swap-voice.service: ([0-9.]+)", report)
          assert found is not None, "systemd-analyze printed no exposure level"
          exposure = float(found.group(1))
          print(f"exposure {exposure}")
          assert exposure <= 2.0, f"exposure {exposure} is above 2.0"
    '';
  }
