# Renders the qwen-tts-server configuration through the module and loads it
# with the server's own config.py, so the Nix writer and the Python reader
# cannot drift apart.
{
  pkgs,
  module,
}: let
  inherit (pkgs) lib;
  # Text between non-ASCII spaces, which config.py must accept unchanged.
  spaced = builtins.fromJSON ''"\u00a0Hej d\u00e5, sa hon.\u2003"'';
  fakes = import ./fakes.nix {inherit pkgs;};

  evaluate = settings:
    pkgs.nixos [
      module
      {
        boot.loader.grub.enable = false;
        fileSystems."/" = {
          device = "none";
          fsType = "tmpfs";
        };
        system.stateVersion = lib.trivial.release;
        services.llama-swap.enable = true;
        services.llama-swap-voice = settings;
      }
    ];

  common = {
    enable = true;
    tts.backend = "qwentts";
    defaultVoice = "robbie";
    llamaCpp.package = fakes.llamaServer;
    models.asr.artifacts = fakes.artifacts.asr;
    tts.qwentts = {
      package = fakes.qwenTtsServer;
      model = "/srv/models/qwen3-tts";
    };
    voices = {
      robbie = {
        clip = "/srv/clips/robbie.wav";
        transcript = "I sat me down and stared at the house of Shaws.";
        instruct = "Speak warmly.";
      };
      ingrid = {
        clip = "/srv/clips/ingrid.wav";
        transcript = spaced;
      };
      stefan = {
        clip = "/srv/clips/stefan.WAV";
        temperature = 0.9;
        topK = 20;
        topP = 0.8;
        repetitionPenalty = 1.1;
      };
    };
  };

  voices = stateDir: {
    robbie = {
      clip = "${stateDir}/voices/robbie.wav";
      transcript = "I sat me down and stared at the house of Shaws.";
      instruct = "Speak warmly.";
      temperature = 0.7;
      top_k = 50;
      top_p = 1.0;
      repetition_penalty = 1.05;
    };
    ingrid = {
      clip = "${stateDir}/voices/ingrid.wav";
      transcript = spaced;
      instruct = null;
      temperature = 0.7;
      top_k = 50;
      top_p = 1.0;
      repetition_penalty = 1.05;
    };
    stefan = {
      clip = "${stateDir}/voices/stefan.WAV";
      transcript = null;
      instruct = null;
      temperature = 0.9;
      top_k = 20;
      top_p = 0.8;
      repetition_penalty = 1.1;
    };
  };

  cases = {
    defaults = {
      settings = common;
      expected = {
        model_dir = "/srv/models/qwen3-tts";
        language = "English";
        chunk_size = 12;
        queue_size = 8;
        queue_wait_seconds = 60;
        # The Go service's own TTS_MAXINPUTCHARS default.
        max_input_chars = 4096;
        max_segment_chars = 400;
        max_new_tokens = 600;
        voices = voices "/var/lib/llama-swap-voice";
      };
    };
    limits = {
      settings = lib.recursiveUpdate common {
        stateDir = "/srv/lsv";
        tts.maxInputChars = 1000;
        tts.qwentts = {
          language = "German";
          chunkSize = 6;
          queueSize = 3;
          queueWaitSeconds = 2.5;
          maxSegmentChars = 200;
          maxNewTokens = 300;
        };
      };
      expected = {
        model_dir = "/srv/models/qwen3-tts";
        language = "German";
        chunk_size = 6;
        queue_size = 3;
        queue_wait_seconds = 2.5;
        max_input_chars = 1000;
        max_segment_chars = 200;
        max_new_tokens = 300;
        voices = voices "/srv/lsv";
      };
    };
    # The service's effective limit, after extraEnvironment, is the one the
    # server must share.
    override = {
      settings = lib.recursiveUpdate common {extraEnvironment.LLAMA_SWAP_VOICE_TTS_MAXINPUTCHARS = "8192";};
      expected = cases.defaults.expected // {max_input_chars = 8192;};
    };
  };

  rendered =
    lib.mapAttrs (_: case: {
      inherit (case) expected;
      cmd = (evaluate case.settings).config.services.llama-swap.settings.models.qwen3-tts.cmd;
    })
    cases;

  # The server's GPU-free environment, from the package the option defaults to.
  inherit ((evaluate common).options.services.llama-swap-voice.tts.qwentts.package.default) testEnv;

  src = lib.fileset.toSource {
    root = ../../python;
    fileset = ../../python/qwen_tts_server;
  };
in
  pkgs.runCommand "llama-swap-voice-qwen-tts-config" {
    cases = builtins.toJSON rendered;
    passAsFile = ["cases"];
  } ''
    export HOME=$TMPDIR PYTHONPATH=${src}
    ${testEnv}/bin/python - $casesPath <<'EOF'
    import json
    import re
    import sys

    from qwen_tts_server import config

    with open(sys.argv[1]) as file:
        cases = json.load(file)

    for name, case in sorted(cases.items()):
        found = re.fullmatch(r".* --config (/nix/store/\S+)", case["cmd"])
        assert found, f"{name}: no --config in {case['cmd']!r}"
        path = found.group(1)
        with open(path, encoding="utf-8") as file:
            rendered = json.load(file)
        assert rendered == case["expected"], f"{name}: rendered {rendered!r}, want {case['expected']!r}"

        loaded = config.load(path)
        assert sorted(loaded.voices) == sorted(case["expected"]["voices"]), loaded.voices
        assert loaded.voices["stefan"].transcript is None, loaded.voices["stefan"]
        ingrid = case["expected"]["voices"]["ingrid"]["transcript"]
        assert loaded.voices["ingrid"].transcript == ingrid, loaded.voices["ingrid"]
        assert loaded.max_input_chars == case["expected"]["max_input_chars"], loaded
        print(f"{name}: {path} accepted by config.py with voices {sorted(loaded.voices)}")
    EOF
    touch $out
  ''
