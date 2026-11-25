{
  pkgs,
  module,
}: let
  inherit (pkgs) lib;

  opt = "services.llama-swap-voice";
  prefix = "LLAMA_SWAP_VOICE_";
  clip = "/srv/clips/sample.wav";

  fakes = import ./fakes.nix {inherit pkgs;};

  # The cases above the llama-swap ones check the service alone.
  base = {
    enable = true;
    defaultVoice = "sample";
    voices.sample.clip = clip;
    llamaSwap.enable = false;
  };

  evaluateWith = system: settings:
    pkgs.nixos [
      module
      system
      {
        # Enough of a system for NixOS's own assertions to pass, so the only
        # failures left are the module's.
        boot.loader.grub.enable = false;
        fileSystems."/" = {
          device = "none";
          fsType = "tmpfs";
        };
        system.stateVersion = lib.trivial.release;
        services.llama-swap-voice = settings;
      }
    ];

  evaluate = evaluateWith {};

  moduleFailuresWith = system: settings:
    map (a: a.message) (lib.filter (a: !a.assertion && lib.hasPrefix opt a.message) (evaluateWith system settings).config.assertions);
  moduleFailures = moduleFailuresWith {};

  serviceEnv = settings: lib.filterAttrs (name: _: lib.hasPrefix prefix name) (evaluate settings).config.systemd.services.llama-swap-voice.environment;

  # Details name store paths; without their context the report does not make
  # the check depend on what those paths would build or fetch.
  show = value: builtins.unsafeDiscardStringContext (builtins.toJSON value);

  failsWithIn = system: name: settings: needles: let
    failures = moduleFailuresWith system settings;
    named = lib.any (message: lib.all (needle: lib.hasInfix needle message) needles) failures;
  in {
    inherit name;
    ok = named;
    detail = "no failed assertion names all of ${show needles}; failed: ${show failures}";
  };
  failsWith = failsWithIn {};

  # A value its option's type refuses throws while the assertions are read,
  # so that is caught too, and the case fails rather than the whole check.
  evaluates = name: settings: let
    messages = moduleFailures settings;
    failures = builtins.tryEval (builtins.deepSeq messages messages);
    built = builtins.tryEval (evaluate settings).config.system.build.toplevel.drvPath;
  in {
    inherit name;
    ok = failures.success && failures.value == [] && built.success;
    detail = "assertions evaluated: ${show failures.success}; failed: ${show (lib.optionals failures.success failures.value)}; toplevel evaluated: ${show built.success}";
  };

  holds = name: ok: detail: {inherit name ok detail;};

  # Evaluation also fails for unrelated reasons, so a refusal counts only
  # when the option's own type is what rejects the value.
  refuses = name: path: value: let
    evaluated = evaluate (lib.recursiveUpdate base (lib.setAttrByPath path value));
    option = lib.getAttrFromPath path evaluated.options.services.llama-swap-voice;
    forced = builtins.tryEval (builtins.deepSeq (lib.getAttrFromPath path evaluated.config.services.llama-swap-voice) true);
  in {
    inherit name;
    ok = !forced.success && !(option.type.check value);
    detail = "evaluation succeeded: ${show forced.success}; the option's type accepts ${show value}: ${show (option.type.check value)}";
  };

  # A list type checks only that the value is a list; its elements are
  # checked when the definitions merge, so the element type is asked here.
  refusesElement = name: path: element: let
    evaluated = evaluate (lib.recursiveUpdate base (lib.setAttrByPath path [element]));
    elementType = (lib.getAttrFromPath path evaluated.options.services.llama-swap-voice).type.nestedTypes.elemType;
    forced = builtins.tryEval (builtins.deepSeq (lib.getAttrFromPath path evaluated.config.services.llama-swap-voice) true);
  in {
    inherit name;
    ok = !forced.success && !(elementType.check element);
    detail = "evaluation succeeded: ${show forced.success}; the element type accepts ${show element}: ${show (elementType.check element)}";
  };

  acceptsElement = name: path: element: let
    evaluated = evaluate (lib.recursiveUpdate base (lib.setAttrByPath path [element]));
    elementType = (lib.getAttrFromPath path evaluated.options.services.llama-swap-voice).type.nestedTypes.elemType;
    forced = builtins.tryEval (builtins.deepSeq (lib.getAttrFromPath path evaluated.config.services.llama-swap-voice) true);
  in {
    inherit name;
    ok = forced.success && elementType.check element;
    detail = "evaluation succeeded: ${show forced.success}; the element type accepts ${show element}: ${show (elementType.check element)}";
  };

  serviceConfig = settings: (evaluate settings).config.systemd.services.llama-swap-voice.serviceConfig;

  stopTimeout = name: shutdown: want: let
    got = (serviceConfig (base // {inherit shutdown;})).TimeoutStopSec or null;
  in
    holds name (got == want) "TimeoutStopSec is ${show got}, want ${show want}";

  qwentts = settings: lib.recursiveUpdate base ({tts.backend = "qwentts";} // settings);

  limits = {
    tts.maxInputChars = 1000;
    upstream.ttsConcurrency = 2;
    request = {
      timeout = "90s";
      maxConcurrent = 3;
      maxConcurrentTTS = 5;
    };
    shutdown = {
      grace = "2s";
      total = "20s";
      hard = "1500ms";
    };
  };

  limitEnv = {
    "${prefix}TTS_MAXINPUTCHARS" = "1000";
    "${prefix}UPSTREAM_TTSCONCURRENCY" = "2";
    "${prefix}REQUEST_TIMEOUT" = "90s";
    "${prefix}REQUEST_MAXCONCURRENT" = "3";
    "${prefix}REQUEST_MAXCONCURRENTTTS" = "5";
    "${prefix}SHUTDOWN_GRACE" = "2s";
    "${prefix}SHUTDOWN_TOTAL" = "20s";
    "${prefix}SHUTDOWN_HARD" = "1500ms";
  };

  baseEnv = {
    "${prefix}HOST" = "127.0.0.1";
    "${prefix}PORT" = "8920";
    "${prefix}UPSTREAM_URL" = "http://127.0.0.1:8080";
    "${prefix}UPSTREAM_TTSBACKEND" = "llamacpp";
    "${prefix}UPSTREAM_TTSCONCURRENCY" = "1";
    "${prefix}VOICES_DIR" = "/var/lib/llama-swap-voice/voices";
    "${prefix}VOICES_DEFAULT" = "sample";
  };

  cases = [
    (failsWith "no defaultVoice fails naming defaultVoice" (removeAttrs base ["defaultVoice"]) ["${opt}.defaultVoice"])

    (failsWith "a defaultVoice that is not declared fails" (base // {defaultVoice = "nobody";}) ["${opt}.defaultVoice" "nobody"])

    (failsWith "qwentts: instruct without a transcript fails naming the voice, instruct and transcript"
      (qwentts {voices.sample.instruct = "Speak slowly.";})
      ["${opt}.voices.sample.instruct" "${opt}.voices.sample.transcript"])

    (evaluates "qwentts: a null transcript and no instruct evaluates" (qwentts {}))

    (evaluates "qwentts: instruct with a transcript evaluates" (qwentts {
      voices.sample = {
        transcript = "Hello there.";
        instruct = "Speak slowly.";
      };
    }))

    (failsWith "qwentts: an empty transcript fails" (qwentts {voices.sample.transcript = "";}) ["${opt}.voices.sample.transcript"])

    (failsWith "a relative clip fails naming the option" (base // {voices.sample.clip = "clips/sample.wav";}) ["${opt}.voices.sample.clip" "absolute"])

    (failsWith "qwentts: a non-.wav clip fails" (qwentts {voices.sample.clip = "/srv/clips/sample.mp3";}) ["${opt}.voices.sample.clip" ".wav" "qwentts"])

    (evaluates "llamacpp: an .mp3 clip evaluates" (base // {voices.sample.clip = "/srv/clips/sample.mp3";}))

    (failsWith "a clip the service cannot read fails" (base // {voices.sample.clip = "/srv/clips/sample.flac";}) ["${opt}.voices.sample.clip" ".wav or .mp3"])

    (failsWith "a voice name that is not a plain file name fails" (base
      // {
        voices."../escape".clip = clip;
      }) ["${opt}.voices.../escape"])

    (let
      evaluated = evaluate (base // {voices.sample.clip = ./fake-upstream.py;});
      clipOption = (evaluated.options.services.llama-swap-voice.voices.type.getSubOptions []).clip;
      forced = builtins.tryEval evaluated.config.services.llama-swap-voice.voices.sample.clip;
    in
      holds "a path literal clip is refused by the clip option's type, so no clip enters the store"
      (!forced.success && !(clipOption.type.check ./fake-upstream.py))
      "evaluation succeeded: ${show forced.success}; the clip type accepts a path: ${show (clipOption.type.check ./fake-upstream.py)}")

    (evaluates "llamacpp: an instruct without a transcript evaluates" (lib.recursiveUpdate base {voices.sample.instruct = "Speak slowly.";}))

    (evaluates "llamacpp: an empty transcript evaluates" (lib.recursiveUpdate base {voices.sample.transcript = "";}))

    (refuses "a duration without a unit is refused for request.timeout" ["request" "timeout"] "90")
    (refuses "a duration without a unit is refused for shutdown.grace" ["shutdown" "grace"] "90")
    (refuses "a negative duration is refused" ["shutdown" "total"] "-1s")
    (refuses "a zero request.timeout is refused" ["request" "timeout"] "0s")
    (refuses "a zero shutdown.total is refused" ["shutdown" "total"] "0s")
    (refuses "a zero shutdown.hard is refused" ["shutdown" "hard"] "0ms")
    (evaluates "a zero shutdown.grace evaluates" (base // {shutdown.grace = "0s";}))

    (refuses "stateDir / is refused" ["stateDir"] "/")
    (refuses "a stateDir holding % is refused" ["stateDir"] "/srv/l%sv")

    (refuses "port 0 is refused" ["port"] 0)
    (refuses "port 65536 is refused" ["port"] 65536)
    (refuses "an upstreamURL that is not a URL is refused" ["upstreamURL"] "not-a-url")
    (refuses "an upstreamURL with another scheme is refused" ["upstreamURL"] "ftp://192.0.2.10")
    (refuses "an upstreamURL with no host is refused" ["upstreamURL"] "http://")
    (refuses "an upstreamURL holding whitespace is refused" ["upstreamURL"] "http://llama swap:8080")
    (evaluates "an IPv6 upstreamURL with a path evaluates" (base // {upstreamURL = "https://[::1]:8081/llama";}))

    (stopTimeout "three maximal shutdown durations sum without overflow" {
        grace = "2562047h47m16.854775807s";
        total = "9223372036854775807ns";
        hard = "9223372036.854775807s";
      }
      27670116141)

    (let
      got = (serviceConfig (base // {extraEnvironment."${prefix}SHUTDOWN_GRACE" = "5m";})).TimeoutStopSec or null;
    in
      holds "a shutdown override in extraEnvironment sets TimeoutStopSec" (got == 330) "TimeoutStopSec is ${show got}, want 330")

    (let
      got =
        (serviceConfig (base
          // {
            shutdown.grace = "10s";
            extraEnvironment."${prefix}SHUTDOWN_GRACE" = "5m";
          })).TimeoutStopSec or null;
    in
      holds "TimeoutStopSec follows the override, not the option it replaces" (got == 330) "TimeoutStopSec is ${show got}, want 330")

    (failsWith "an invalid shutdown override fails naming the variable"
      (base // {extraEnvironment."${prefix}SHUTDOWN_GRACE" = "soon";})
      ["${opt}.extraEnvironment.${prefix}SHUTDOWN_GRACE" "soon"])

    (failsWith "a zero shutdown total override fails naming the variable"
      (base // {extraEnvironment."${prefix}SHUTDOWN_TOTAL" = "0s";})
      ["${opt}.extraEnvironment.${prefix}SHUTDOWN_TOTAL"])

    (let
      rules = (evaluate (base // {voices.sample.clip = "/srv/clips/speaker%n.wav";})).config.systemd.tmpfiles.settings.llama-swap-voice;
      link = rules."/var/lib/llama-swap-voice/voices/sample.wav".L.argument or null;
    in
      holds "a % in a clip path is escaped in the tmpfiles rule" (link == "/srv/clips/speaker%%n.wav") "the link argument is ${show link}")

    (let
      env = serviceEnv (base // {voices.sample.clip = "/srv/clips/speaker%n.wav";});
    in
      holds "a % in a clip path leaves the environment literal" (env == baseEnv) "environment is ${show env}")

    (stopTimeout "TimeoutStopSec is 90 with no shutdown option set" {} 90)
    (stopTimeout "TimeoutStopSec is the shutdown sum plus 30 s" {
        grace = "30s";
        total = "60s";
        hard = "10s";
      }
      130)
    (stopTimeout "compound and fractional durations convert to seconds" {
        grace = "1.25m";
        total = "1m30s";
        hard = "1500000µs";
      }
      197)
    (stopTimeout "hours and leading-dot fractions convert to seconds" {
        total = "2h";
        hard = ".5s";
      }
      7231)

    (let
      evaluated = (evaluate (base // {user = "voice";})).config;
      service = evaluated.systemd.services.llama-swap-voice.serviceConfig;
    in
      holds "a custom user is used but not created"
      (service.User == "voice" && !(evaluated.users.users ? voice) && !(evaluated.users.users ? llama-swap-voice))
      "User ${show service.User}; users.users has voice: ${show (evaluated.users.users ? voice)}")

    (let
      evaluated = (evaluate base).config;
    in
      holds "the default user and group are created"
      (evaluated.users.users.llama-swap-voice.isSystemUser && evaluated.users.groups ? llama-swap-voice)
      "users.users.llama-swap-voice or users.groups.llama-swap-voice is missing")

    (let
      voiceNames =
        (evaluate (base
          // {
            voices = {
              zulu.clip = clip;
              sample.clip = clip;
              alpha.clip = clip;
            };
          })).config.services.llama-swap-voice.voiceNames;
    in
      holds "voiceNames is a list of strings equal to attrNames voices"
      (builtins.isList voiceNames && lib.all builtins.isString voiceNames && voiceNames == ["alpha" "sample" "zulu"])
      "voiceNames is ${show voiceNames}")

    (let
      env = serviceEnv (base // limits);
    in
      holds "a limit set to a value appears as its variable"
      (lib.all (name: env.${name} or null == limitEnv.${name}) (lib.attrNames limitEnv))
      "environment is ${show env}")

    (let
      env = serviceEnv base;
    in
      holds "a limit left null leaves its variable unset"
      (env == baseEnv)
      "environment is ${show env}, want ${show baseEnv}")

    (let
      env = serviceEnv (qwentts {});
    in
      holds "tts.backend sets UPSTREAM_TTSBACKEND"
      (env."${prefix}UPSTREAM_TTSBACKEND" or null == "qwentts")
      "environment is ${show env}")

    (let
      env = serviceEnv (base
        // {
          request.maxConcurrent = 3;
          extraEnvironment = {
            "${prefix}REQUEST_MAXCONCURRENT" = "9";
            "${prefix}LOG_LEVEL" = "debug";
          };
        });
    in
      holds "extraEnvironment wins over generated values"
      (env."${prefix}REQUEST_MAXCONCURRENT" == "9" && env."${prefix}LOG_LEVEL" == "debug")
      "environment is ${show env}")

    (let
      rules = (evaluate (base // {stateDir = "/srv/lsv";})).config.systemd.tmpfiles.settings.llama-swap-voice;
      want = {
        "/srv/lsv" = ["d"];
        "/srv/lsv/voices" = ["d"];
        "/srv/lsv/voices/sample.wav" = ["L"];
      };
    in
      holds "tmpfiles creates stateDir/voices with d, never D, and links each voice"
      (lib.mapAttrs (_: lib.attrNames) rules == want && rules."/srv/lsv/voices/sample.wav".L.argument == clip)
      "rules are ${show (lib.mapAttrs (_: lib.attrNames) rules)}")

    (let
      service = (evaluate (base // {ipAddressAllow = ["192.0.2.10"];})).config.systemd.services.llama-swap-voice.serviceConfig;
    in
      holds "network is loopback only unless ipAddressAllow adds addresses"
      (service.IPAddressDeny == "any" && service.IPAddressAllow == ["localhost" "192.0.2.10"])
      "IPAddressDeny ${show service.IPAddressDeny}, IPAddressAllow ${show service.IPAddressAllow}")

    (let
      evaluated = (evaluate (base // {stateDir = "/srv/lsv";})).config;
      env = lib.filterAttrs (name: _: lib.hasPrefix "${prefix}RECORDINGS_" name) evaluated.systemd.services.llama-swap-voice.environment;
      service = evaluated.systemd.services.llama-swap-voice.serviceConfig;
      rules = evaluated.systemd.tmpfiles.settings.llama-swap-voice;
    in
      holds "recordings are off by default: no variables, no directory, nothing writable"
      (env == {} && !(rules ? "/srv/lsv/recordings") && !(service ? ReadWritePaths))
      "RECORDINGS variables ${show env}; tmpfiles rules ${show (lib.attrNames rules)}; ReadWritePaths ${show (service.ReadWritePaths or null)}")

    (let
      evaluated =
        (evaluate (base
          // {
            stateDir = "/srv/lsv";
            user = "voice";
            group = "voice";
            recordings.enable = true;
          })).config;
      env = evaluated.systemd.services.llama-swap-voice.environment;
      service = evaluated.systemd.services.llama-swap-voice.serviceConfig;
      rule = evaluated.systemd.tmpfiles.settings.llama-swap-voice."/srv/lsv/recordings" or {};
    in
      holds "recordings.enable sets both variables, a private d rule and the one writable path"
      (env."${prefix}RECORDINGS_ENABLE" or null
        == "true"
        && env."${prefix}RECORDINGS_DIR" or null == "/srv/lsv/recordings"
        && lib.attrNames rule == ["d"]
        && lib.getAttrs ["user" "group" "mode"] rule.d
        == {
          user = "voice";
          group = "voice";
          mode = "0700";
        }
        && service.ReadWritePaths == ["/srv/lsv/recordings"]
        && service.ProtectSystem == "strict")
      "RECORDINGS_ENABLE ${show (env."${prefix}RECORDINGS_ENABLE" or null)}; RECORDINGS_DIR ${show (env."${prefix}RECORDINGS_DIR" or null)}; rule ${show rule}; ReadWritePaths ${show (service.ReadWritePaths or null)}")

    (evaluates "recordings.enable evaluates to a system" (base // {recordings.enable = true;}))

    (let
      env = serviceEnv (base
        // {
          recordings = {
            enable = true;
            allowedHosts = ["voice.example" "proxy.example:8443"];
          };
        });
    in
      holds "recordings.allowedHosts sets RECORDINGS_ALLOWEDHOSTS as a comma-separated list"
      (env."${prefix}RECORDINGS_ALLOWEDHOSTS" or null == "voice.example,proxy.example:8443")
      "RECORDINGS_ALLOWEDHOSTS ${show (env."${prefix}RECORDINGS_ALLOWEDHOSTS" or null)}")

    (let
      env = serviceEnv (base // {recordings.allowedHosts = ["voice.example"];});
    in
      holds "recordings.allowedHosts writes nothing while recordings are off"
      (!(env ? "${prefix}RECORDINGS_ALLOWEDHOSTS"))
      "RECORDINGS_ALLOWEDHOSTS ${show (env."${prefix}RECORDINGS_ALLOWEDHOSTS" or null)}")

    (refusesElement "a recordings.allowedHosts entry with a comma is refused" ["recordings" "allowedHosts"] "a.example,b.example")
    (refusesElement "a recordings.allowedHosts entry with a space is refused" ["recordings" "allowedHosts"] "voice example")
    (refusesElement "a recordings.allowedHosts entry with a scheme is refused" ["recordings" "allowedHosts"] "https://voice.example")
    (acceptsElement "a recordings.allowedHosts entry with a port is accepted" ["recordings" "allowedHosts"] "voice.example:8443")

    (failsWith "RECORDINGS_ENABLE in extraEnvironment fails naming recordings.enable"
      (base // {extraEnvironment."${prefix}RECORDINGS_ENABLE" = "true";})
      ["${opt}.extraEnvironment.${prefix}RECORDINGS_ENABLE" "${opt}.recordings.enable"])
    (failsWith "RECORDINGS_ENABLE = \"1\" in extraEnvironment turns recordings on and fails"
      (base // {extraEnvironment."${prefix}RECORDINGS_ENABLE" = "1";})
      ["${opt}.extraEnvironment.${prefix}RECORDINGS_ENABLE" "${opt}.recordings.enable"])
    (failsWith "RECORDINGS_ENABLE = \"false\" in extraEnvironment fails while recordings are on"
      (base
        // {
          recordings.enable = true;
          extraEnvironment."${prefix}RECORDINGS_ENABLE" = "false";
        })
      ["${opt}.extraEnvironment.${prefix}RECORDINGS_ENABLE" "${opt}.recordings.enable"])
    (failsWith "RECORDINGS_DIR in extraEnvironment fails naming stateDir while recordings are on"
      (base
        // {
          recordings.enable = true;
          extraEnvironment."${prefix}RECORDINGS_DIR" = "/srv/elsewhere";
        })
      ["${opt}.extraEnvironment.${prefix}RECORDINGS_DIR" "${opt}.stateDir"])
    (evaluates "RECORDINGS_ENABLE = \"false\" in extraEnvironment is accepted while recordings are off"
      (base // {extraEnvironment."${prefix}RECORDINGS_ENABLE" = "false";}))
    (evaluates "RECORDINGS_ENABLE = \"0\" and inert RECORDINGS_DIR and RECORDINGS_ALLOWEDHOSTS are accepted while recordings are off"
      (base
        // {
          extraEnvironment = {
            "${prefix}RECORDINGS_ENABLE" = "0";
            "${prefix}RECORDINGS_DIR" = "/srv/elsewhere";
            "${prefix}RECORDINGS_ALLOWEDHOSTS" = "voice.example";
          };
        }))
    (failsWith "RECORDINGS_ALLOWEDHOSTS in extraEnvironment fails naming recordings.allowedHosts"
      (base
        // {
          recordings.enable = true;
          extraEnvironment."${prefix}RECORDINGS_ALLOWEDHOSTS" = "voice.example";
        })
      ["${opt}.extraEnvironment.${prefix}RECORDINGS_ALLOWEDHOSTS" "${opt}.recordings.allowedHosts"])
  ];

  # A consumer's own llama-swap configuration, in the legacy group layout.
  consumer = {
    healthCheckTimeout = 300;
    models.other = {
      cmd = "/srv/bin/other --port \${PORT}";
      ttl = 60;
    };
    groups.cpu = {
      swap = true;
      exclusive = false;
      members = ["other"];
    };
    hooks.on_startup.preload = ["other"];
  };

  withLlamaSwap = settings: {
    services.llama-swap = {
      enable = true;
      inherit settings;
    };
  };

  # The service with the llama-swap contribution on and every package and
  # artifact a fake, so evaluation names no download.
  swapped = settings:
    lib.recursiveUpdate (base
      // {
        llamaSwap.enable = true;
        llamaCpp.package = fakes.llamaServer;
        models = {
          asr.artifacts = fakes.artifacts.asr;
          tts.artifacts = fakes.artifacts.tts;
        };
        tts.qwentts = {
          package = fakes.qwenTtsServer;
          model = "/srv/models/qwen3-tts";
          modelName = "qwen3-tts-hq";
        };
      })
    settings;

  swapConfig = system: settings: (evaluateWith system (swapped settings)).config;
  swapSettings = system: settings: (swapConfig system settings).services.llama-swap.settings;
  swapUnit = system: settings: (swapConfig system settings).systemd.services.llama-swap;

  sorted = lib.sort lib.lessThan;
  llamaServer = "${fakes.llamaServer}/bin/llama-server";
  qwenTtsServer = "${fakes.qwenTtsServer}/bin/qwen-tts-server";
  ttsNames = {
    llamacpp = "qwen3-tts";
    qwentts = "qwen3-tts-hq";
  };
  ttsBinaries = {
    llamacpp = llamaServer;
    qwentts = qwenTtsServer;
  };

  voiceGroupWith = sharing: tts:
    {
      swap = false;
      members = ["qwen3-asr" tts];
    }
    // sharing;
  pinned = {
    exclusive = false;
    persistent = true;
  };
  takeTurns = {
    exclusive = true;
    persistent = false;
  };
  voiceGroup = voiceGroupWith pinned;

  mergesWith = backend: let
    settings = swapSettings (withLlamaSwap consumer) {
      tts.backend = backend;
      llamaSwap.group.enable = true;
    };
    tts = ttsNames.${backend};
  in
    holds "${backend}: the consumer's model, group and scalar stay, the asr entry and exactly one tts entry join them"
    (lib.attrNames settings.models
      == sorted ["other" "qwen3-asr" tts]
      && settings.models.qwen3-asr.ttl or null == 0
      && settings.models.${tts}.ttl or null == 0
      && lib.hasPrefix "${ttsBinaries.${backend}} " settings.models.${tts}.cmd
      && sorted settings.hooks.on_startup.preload == sorted ["other" "qwen3-asr" tts]
      && settings.models.other == consumer.models.other
      && settings.groups
      == {
        inherit (consumer.groups) cpu;
        voice = voiceGroup tts;
      }
      && settings.healthCheckTimeout == 300
      && !(settings ? routing))
    "settings are ${show settings}";

  routerGroups = {
    routing.router.settings.groups.cpu = {
      swap = true;
      members = ["other"];
    };
  };

  stock = (evaluateWith (withLlamaSwap consumer) (base // {enable = false;})).config.systemd.services.llama-swap;
  nvidiaDevices = ["char-nvidia rw" "char-nvidia-uvm rw"];
  withoutExec = serviceConfig: removeAttrs serviceConfig ["ExecStart"];

  relaxed = name: unit:
    holds name
    (unit.serviceConfig.MemoryDenyWriteExecute
      == false
      && unit.serviceConfig.DeviceAllow == nvidiaDevices
      && unit.environment.CUDA_DEVICE_ORDER or null == "PCI_BUS_ID")
    "MemoryDenyWriteExecute ${show unit.serviceConfig.MemoryDenyWriteExecute}; DeviceAllow ${show (unit.serviceConfig.DeviceAllow or null)}; CUDA_DEVICE_ORDER ${show (unit.environment.CUDA_DEVICE_ORDER or null)}";

  unicode = escaped: builtins.fromJSON "\"${escaped}\"";

  listenRefused = listenAddress:
    failsWithIn {
      services.llama-swap = {
        enable = true;
        inherit listenAddress;
      };
    } "listen address ${listenAddress} fails naming services.llama-swap.listenAddress" (swapped {}) ["services.llama-swap.listenAddress" listenAddress];

  layoutRefused = name: settings: location:
    failsWithIn (withLlamaSwap settings) name (swapped {llamaSwap.group.location = location;})
    ["${opt}.llamaSwap.group.location" "${opt}.llamaSwap.group.enable = false"];

  overrideRefused = variable: value:
    failsWithIn (withLlamaSwap {}) "an invalid ${variable} override fails naming the variable"
    (swapped {extraEnvironment."${prefix}${variable}" = value;})
    ["${opt}.extraEnvironment.${prefix}${variable}" value];

  cudaPkgs = import pkgs.path {
    inherit (pkgs.stdenv.hostPlatform) system;
    config.cudaSupport = true;
  };

  asrCommand = lib.concatStringsSep " " [
    llamaServer
    "--port \${PORT}"
    "-m ${fakes.artifacts.asr.model.path}"
    "--mmproj ${fakes.artifacts.asr.mmproj.path}"
    "-ngl 99 -c 4096 -np 1 --temp 0 --flash-attn on --no-webui"
  ];

  swapCases = [
    (mergesWith "llamacpp")
    (mergesWith "qwentts")

    (let
      models = (swapSettings (withLlamaSwap {}) {}).models;
    in
      holds "the asr and llamacpp tts entries run the proven llama-server commands"
      (models.qwen3-asr
        == {
          cmd = asrCommand;
          checkEndpoint = "/health";
          concurrencyLimit = 4;
          ttl = 0;
        }
        && models.qwen3-tts.cmd
        == lib.concatStringsSep " " [
          llamaServer
          "--port \${PORT}"
          "-m ${fakes.artifacts.tts.model.path}"
          "--mmproj ${fakes.artifacts.tts.mmproj.path}"
          "-ngl 99 -c 4096 -np 1 --flash-attn on --no-webui"
        ]
        && models.qwen3-tts.concurrencyLimit == 5)
      "models are ${show models}")

    (let
      asr =
        (swapSettings (withLlamaSwap {}) {
          models.asr = {
            device = "CUDA0";
            gpuLayers = 20;
            contextSize = 8192;
            parallel = 2;
            ttl = 300;
            preload = false;
            extraArgs = ["--threads" "4"];
          };
        }).models.qwen3-asr;
    in
      holds "asr device, layers, context, parallel, ttl and extraArgs reach the entry"
      (asr.cmd
        == "${llamaServer} --port \${PORT} -m ${fakes.artifacts.asr.model.path} --mmproj ${fakes.artifacts.asr.mmproj.path} --device CUDA0 -ngl 20 -c 8192 -np 2 --temp 0 --flash-attn on --no-webui --threads 4"
        && asr.ttl or null == 300)
      "asr is ${show asr}")

    (let
      settings = swapSettings (withLlamaSwap {}) {
        models.asr.preload = false;
        tts.backend = "qwentts";
      };
    in
      holds "preload = false leaves a model out of hooks.on_startup.preload"
      (settings.hooks.on_startup.preload == ["qwen3-tts-hq"])
      "preload is ${show (settings.hooks or null)}")

    (let
      entry = (swapSettings (withLlamaSwap {}) {tts.backend = "qwentts";}).models.qwen3-tts-hq;
    in
      holds "the qwentts entry runs the server with the generated config and only CUDA_VISIBLE_DEVICES"
      (builtins.match (builtins.unsafeDiscardStringContext "${lib.escapeRegex qwenTtsServer} --port [$][{]PORT[}] --config /nix/store/[a-z0-9]{32}-qwen-tts-server[.]json") (builtins.unsafeDiscardStringContext entry.cmd)
        != null
        && removeAttrs entry ["cmd"]
        == {
          checkEndpoint = "/health";
          concurrencyLimit = 8;
          env = ["CUDA_VISIBLE_DEVICES=0"];
          ttl = 0;
        })
      "entry is ${show entry}")

    (let
      settings = swapSettings (withLlamaSwap {}) {};
    in
      holds "the voice group is written by default, in the legacy layout"
      (settings.groups or null == {voice = voiceGroup "qwen3-tts";} && !(settings ? routing))
      "settings are ${show settings}")

    (let
      settings = swapSettings (withLlamaSwap routerGroups) {
        llamaSwap.group = {
          enable = true;
          name = "speech";
          location = "routing";
        };
      };
    in
      holds "group.location = routing writes the group under routing.router.settings.groups"
      (settings.routing.router.settings.groups
        == {
          inherit (routerGroups.routing.router.settings.groups) cpu;
          speech = voiceGroup "qwen3-tts";
        }
        && !(settings ? groups))
      "settings are ${show settings}")

    (holds "no group is written when llamaSwap.group.enable is false"
      (let
        settings = swapSettings (withLlamaSwap {}) {llamaSwap.group.enable = false;};
      in
        !(settings ? groups) && !(settings ? routing))
      "a group was written")

    (let
      legacy = swapSettings (withLlamaSwap {}) {llamaSwap.group.sharing = "takeTurns";};
      routing = swapSettings (withLlamaSwap {}) {
        llamaSwap.group = {
          sharing = "takeTurns";
          location = "routing";
        };
      };
      byDefault = swapSettings (withLlamaSwap {}) {llamaSwap.group.location = "routing";};
    in
      holds "group.sharing = takeTurns writes an exclusive, non-persistent group in either location; the default stays pinned"
      (legacy.groups or null
        == {voice = voiceGroupWith takeTurns "qwen3-tts";}
        && routing.routing.router.settings.groups or null == {voice = voiceGroupWith takeTurns "qwen3-tts";}
        && byDefault.routing.router.settings.groups or null == {voice = voiceGroupWith pinned "qwen3-tts";})
      "legacy ${show (legacy.groups or null)}; routing ${show (routing.routing or null)}; default routing ${show (byDefault.routing or null)}")

    (refuses "group.sharing refuses a value outside pinned and takeTurns" ["llamaSwap" "group" "sharing"] "evictable")

    (failsWithIn (withLlamaSwap routerGroups) "legacy group with a consumer routing.router block fails naming both layouts"
      (swapped {llamaSwap.group.enable = true;})
      ["${opt}.llamaSwap.group.location" "legacy" "routing.router"])

    (failsWithIn (withLlamaSwap {groups.cpu.members = ["other"];}) "routing group with consumer top-level groups fails naming both layouts"
      (swapped {
        llamaSwap.group = {
          enable = true;
          location = "routing";
        };
      })
      ["${opt}.llamaSwap.group.location" "routing" "groups"])

    (failsWithIn (withLlamaSwap {routing.router.use = "matrix";}) "routing group under another router fails"
      (swapped {
        llamaSwap.group = {
          enable = true;
          location = "routing";
        };
      })
      ["${opt}.llamaSwap.group.location" "routing.router.use"])

    (relaxed "llamaCpp.backend = cuda relaxes the llama-swap unit for CUDA" (swapUnit (withLlamaSwap {}) {llamaCpp.backend = "cuda";}))

    (relaxed "qwentts relaxes the llama-swap unit for CUDA under the cpu llama.cpp backend" (swapUnit (withLlamaSwap {}) {tts.backend = "qwentts";}))

    (let
      unit = swapUnit (withLlamaSwap consumer) {};
    in
      holds "cpu with llamacpp leaves the stock llama-swap unit"
      (withoutExec unit.serviceConfig == withoutExec stock.serviceConfig && unit.environment == stock.environment)
      "serviceConfig ${show (withoutExec unit.serviceConfig)}; stock ${show (withoutExec stock.serviceConfig)}")

    (let
      unit = swapUnit (withLlamaSwap {}) {tts.backend = "qwentts";};
      pre = unit.serviceConfig.ExecStartPre or [];
    in
      holds "qwentts adds llama-swap to the module's group and creates the server's cache root"
      (unit.serviceConfig.SupplementaryGroups
        or null
        == ["llama-swap-voice"]
        && builtins.length pre == 1
        && lib.hasInfix "qwen-tts-server-cache" (toString (lib.head pre)))
      "SupplementaryGroups ${show (unit.serviceConfig.SupplementaryGroups or null)}; ExecStartPre ${show pre}")

    (let
      unit = swapUnit (withLlamaSwap {}) {llamaCpp.backend = "cuda";};
    in
      holds "cuda without qwentts leaves llama-swap out of the module's group"
      (!(unit.serviceConfig ? SupplementaryGroups) && !(unit.serviceConfig ? ExecStartPre))
      "serviceConfig ${show unit.serviceConfig}")

    (let
      unit = swapUnit (withLlamaSwap {}) {
        tts.backend = "qwentts";
        llamaCpp.backend = "rocm";
      };
    in
      holds "rocm under qwentts keeps the AMD devices the NVIDIA allow-list would hide"
      (unit.serviceConfig.DeviceAllow == nvidiaDevices ++ ["char-drm rw" "/dev/kfd rw"])
      "DeviceAllow ${show unit.serviceConfig.DeviceAllow}")

    (let
      chosen = (evaluateWith (withLlamaSwap {}) (removeAttrs (swapped {}) ["llamaCpp"] // {llamaCpp.backend = "cuda";})).config.services.llama-swap-voice.llamaCpp.package;
    in
      holds "llamaCpp.backend = cuda selects nixpkgs' llama-cpp-cuda by default"
      (chosen.meta.description == pkgs.llama-cpp-cuda.meta.description && chosen.meta.description != pkgs.llama-cpp.meta.description)
      "package description ${show chosen.meta.description}")

    (failsWithIn (withLlamaSwap {}) "a llama.cpp older than 0.8.0 fails" (swapped {llamaCpp.package = fakes.oldLlamaServer;}) ["${opt}.llamaCpp.package" "0.8.0" "0.7.9"])

    (failsWithIn (withLlamaSwap {}) "llamacpp: a tts concurrencyLimit below upstream.ttsConcurrency fails"
      (swapped {
        upstream.ttsConcurrency = 3;
        models.tts.concurrencyLimit = 2;
      })
      ["${opt}.models.tts.concurrencyLimit" "${opt}.upstream.ttsConcurrency"])

    (failsWithIn (withLlamaSwap {}) "qwentts: a tts concurrencyLimit below upstream.ttsConcurrency fails"
      (swapped {
        tts.backend = "qwentts";
        upstream.ttsConcurrency = 3;
        tts.qwentts.concurrencyLimit = 2;
      })
      ["${opt}.tts.qwentts.concurrencyLimit" "${opt}.upstream.ttsConcurrency"])

    (failsWith "llama-swap disabled with llamaSwap.enable on fails telling the consumer to enable it"
      (swapped {})
      ["${opt}.llamaSwap.enable" "services.llama-swap.enable = true"])

    (let
      evaluated = (evaluateWith (withLlamaSwap consumer) (swapped {llamaSwap.enable = false;})).config;
    in
      holds "llamaSwap.enable = false contributes nothing to services.llama-swap"
      (evaluated.services.llama-swap.settings
        == consumer
        && withoutExec evaluated.systemd.services.llama-swap.serviceConfig == withoutExec stock.serviceConfig
        && evaluated.systemd.services.llama-swap.environment == stock.environment)
      "settings ${show evaluated.services.llama-swap.settings}")

    (let
      env = lib.filterAttrs (name: _: lib.hasPrefix prefix name) (swapConfig (withLlamaSwap {}) {tts.backend = "qwentts";}).systemd.services.llama-swap-voice.environment;
    in
      holds "the service reaches llama-swap at its listen address with the entries' model names"
      (env."${prefix}UPSTREAM_URL"
        == "http://localhost:8080"
        && env."${prefix}UPSTREAM_TTSMODEL" == "qwen3-tts-hq"
        && env."${prefix}UPSTREAM_ASRMODEL" == "qwen3-asr"
        && env."${prefix}UPSTREAM_TTSCONCURRENCY" == "1")
      "environment is ${show env}")

    (let
      url = listenAddress:
        (swapConfig {
          services.llama-swap = {
            enable = true;
            inherit listenAddress;
            port = 11343;
          };
        } {}).services.llama-swap-voice.upstreamURL;
      want = {
        "" = "http://127.0.0.1:11343";
        "0.0.0.0" = "http://127.0.0.1:11343";
        "[::]" = "http://[::1]:11343";
        "[::1]" = "http://[::1]:11343";
        "[2001:db8::1]" = "http://[2001:db8::1]:11343";
        "192.0.2.10" = "http://192.0.2.10:11343";
      };
      got = lib.mapAttrs (address: _: url address) want;
    in
      holds "a wildcard listen address becomes loopback in upstreamURL and any other address is kept"
      (got == want)
      "upstreamURL by listen address: ${show got}")

    (let
      evaluated =
        (swapConfig {
          services.llama-swap = {
            enable = true;
            tls = {
              enable = true;
              certFile = "/run/llama-swap/cert.pem";
              keyFile = "/run/llama-swap/key.pem";
            };
          };
        } {})
        .services.llama-swap-voice.upstreamURL;
    in
      holds "llama-swap's TLS makes upstreamURL https" (evaluated == "https://localhost:8080") "upstreamURL is ${evaluated}")

    (listenRefused "::")
    (listenRefused "::1")
    (listenRefused "[fe80::1%eth0]")

    (let
      env = lib.filterAttrs (name: _: lib.hasPrefix prefix name) (swapConfig (withLlamaSwap {}) {}).systemd.services.llama-swap-voice.environment;
    in
      holds "llamacpp: UPSTREAM_TTSMODEL is models.tts.name"
      (env."${prefix}UPSTREAM_TTSMODEL" == "qwen3-tts")
      "environment is ${show env}")

    (failsWithIn (withLlamaSwap {}) "qwentts: a blank transcript fails, since the server refuses it"
      (swapped {
        tts.backend = "qwentts";
        voices.sample.transcript = "  ";
      })
      ["${opt}.voices.sample.transcript"])

    (failsWith "qwentts: a transcript of only U+00A0 fails, as Python's str.strip removes it" (qwentts {voices.sample.transcript = unicode "\\u00a0";}) ["${opt}.voices.sample.transcript"])
    (failsWith "qwentts: a transcript of U+3000 and U+2003 fails" (qwentts {voices.sample.transcript = unicode "\\u3000\\u2003";}) ["${opt}.voices.sample.transcript"])
    (evaluates "qwentts: text between non-ASCII spaces evaluates" (qwentts {voices.sample.transcript = unicode "\\u00a0Hello there.\\u3000";}))

    (refuses "an empty models.asr.name is refused" ["models" "asr" "name"] "")
    (refuses "an empty models.tts.name is refused" ["models" "tts" "name"] "")
    (refuses "an empty tts.qwentts.modelName is refused" ["tts" "qwentts" "modelName"] "")

    (failsWithIn (withLlamaSwap {}) "the asr and tts entries may not share a name"
      (swapped {models.asr.name = "qwen3-tts";})
      ["${opt}.models.asr.name"])

    (failsWithIn (withLlamaSwap {apiKeys = ["secret"];}) "llama-swap with apiKeys fails, since the service sends no key"
      (swapped {})
      ["apiKeys" "${opt}.llamaSwap.enable = false"])

    (let
      after = enable: (swapConfig (withLlamaSwap {}) {llamaSwap.enable = enable;}).systemd.services.llama-swap-voice.after;
    in
      holds "the service starts after llama-swap only when integrated"
      (lib.elem "llama-swap.service" (after true) && !(lib.elem "llama-swap.service" (after false)))
      "after is ${show (after true)} integrated, ${show (after false)} not")

    (layoutRefused "legacy group with a consumer top-level matrix fails" {matrix.vars.a = "other";} "legacy")
    (layoutRefused "legacy group with only routing.router.use = group fails" {routing.router.use = "group";} "legacy")
    (layoutRefused "legacy group with routing.router.settings.capacity fails" {routing.router.settings.capacity.gpu = 1;} "legacy")
    (layoutRefused "routing group with a consumer top-level matrix fails" {matrix.vars.a = "other";} "routing")

    (let
      limit = settings: (swapSettings (withLlamaSwap {}) settings).models.qwen3-asr.concurrencyLimit;
    in
      holds "the asr concurrencyLimit defaults to the transcriptions the service admits"
      (limit {} == 4 && limit {request.maxConcurrent = 6;} == 6 && limit {extraEnvironment."${prefix}REQUEST_MAXCONCURRENT" = "7";} == 7)
      "default ${show (limit {})}, maxConcurrent 6 gives ${show (limit {request.maxConcurrent = 6;})}")

    (failsWithIn (withLlamaSwap {}) "an asr concurrencyLimit below the service's transcription limit fails"
      (swapped {models.asr.concurrencyLimit = 2;})
      ["${opt}.models.asr.concurrencyLimit" "REQUEST_MAXCONCURRENT"])

    (failsWithIn (withLlamaSwap {}) "an asr concurrencyLimit below a raised maxConcurrent fails"
      (swapped {
        request.maxConcurrent = 8;
        models.asr.concurrencyLimit = 6;
      })
      ["${opt}.models.asr.concurrencyLimit" "REQUEST_MAXCONCURRENT"])

    (failsWithIn (withLlamaSwap {}) "an UPSTREAM_TTSCONCURRENCY override above the tts concurrencyLimit fails"
      (swapped {extraEnvironment."${prefix}UPSTREAM_TTSCONCURRENCY" = "6";})
      ["${opt}.models.tts.concurrencyLimit" "UPSTREAM_TTSCONCURRENCY"])

    (failsWithIn (withLlamaSwap {}) "a REQUEST_MAXCONCURRENT override above the asr concurrencyLimit fails"
      (swapped {
        models.asr.concurrencyLimit = 4;
        extraEnvironment."${prefix}REQUEST_MAXCONCURRENT" = "5";
      })
      ["${opt}.models.asr.concurrencyLimit" "REQUEST_MAXCONCURRENT"])

    (overrideRefused "TTS_MAXINPUTCHARS" "lots")
    (overrideRefused "UPSTREAM_TTSCONCURRENCY" "0")
    (overrideRefused "REQUEST_MAXCONCURRENT" "9223372036854775808")

    (let
      chosen = backend:
        (cudaPkgs.nixos [
          module
          (withLlamaSwap {})
          {
            boot.loader.grub.enable = false;
            fileSystems."/" = {
              device = "none";
              fsType = "tmpfs";
            };
            system.stateVersion = lib.trivial.release;
            services.llama-swap-voice =
              base
              // {
                llamaSwap.enable = true;
                llamaCpp.backend = backend;
              };
          }
        ])
        .config
        .services
        .llama-swap-voice
        .llamaCpp
        .package
        .meta
        .description;
      hasCuda = backend: lib.hasInfix "CUDA" (chosen backend);
    in
      holds "with nixpkgs.config.cudaSupport the cpu and vulkan defaults carry no CUDA, the cuda default does"
      (!(hasCuda "cpu") && !(hasCuda "vulkan") && hasCuda "cuda")
      "cpu: ${chosen "cpu"}; vulkan: ${chosen "vulkan"}; cuda: ${chosen "cuda"}")
  ];

  wyomingPrefix = "${prefix}WYOMING_";
  wyomingEnv = settings: lib.filterAttrs (name: _: lib.hasPrefix wyomingPrefix name) (serviceEnv settings);
  wyomingOn = settings: lib.recursiveUpdate base {wyoming = {enable = true;} // settings;};
  firewallPorts = settings: (evaluate settings).config.networking.firewall.allowedTCPPorts;
  # Nix strings have no \u escape; JSON's gives the character from its code point.
  character = code: builtins.fromJSON ''"\u${code}"'';
  otherWyomingPort = 10301;

  wyomingCases = [
    (let
      env = wyomingEnv base;
    in
      holds "wyoming.enable = false sets no WYOMING_ variable" (env == {}) "WYOMING_ variables are ${show env}")

    (let
      env = wyomingEnv (wyomingOn {
        host = "0.0.0.0";
        port = 10400;
        languages = ["en" "de"];
        speed = 1.5;
        streaming = "whole";
        streamingMaxChars = 200;
      });
      want = {
        "${wyomingPrefix}ENABLED" = "true";
        "${wyomingPrefix}HOST" = "0.0.0.0";
        "${wyomingPrefix}PORT" = "10400";
        "${wyomingPrefix}LANGUAGES" = "en,de";
        "${wyomingPrefix}SPEED" = "1.5";
        "${wyomingPrefix}STREAMING" = "whole";
        "${wyomingPrefix}STREAMINGMAXCHARS" = "200";
      };
    in
      holds "wyoming.enable = true sets all seven WYOMING_ variables from the options, with the languages comma-joined"
      (env == want)
      "WYOMING_ variables are ${show env}, want ${show want}")

    (let
      env = wyomingEnv (wyomingOn {});
      want = {
        "${wyomingPrefix}ENABLED" = "true";
        "${wyomingPrefix}HOST" = "127.0.0.1";
        "${wyomingPrefix}PORT" = "10300";
        "${wyomingPrefix}LANGUAGES" = "en";
        "${wyomingPrefix}SPEED" = "1.0";
        "${wyomingPrefix}STREAMING" = "sentences";
        "${wyomingPrefix}STREAMINGMAXCHARS" = "300";
      };
    in
      holds "the wyoming defaults match the service's defaults" (env == want) "WYOMING_ variables are ${show env}, want ${show want}")

    (refuses "wyoming.streaming refuses a value outside sentences and whole" ["wyoming" "streaming"] "other")
    (refuses "wyoming.speed 5 is refused" ["wyoming" "speed"] 5)
    (refuses "wyoming.speed below 0.25 is refused" ["wyoming" "speed"] 0.2)
    (refusesElement "an empty wyoming language is refused" ["wyoming" "languages"] "")
    (refuses "an empty wyoming.languages list is refused" ["wyoming" "languages"] [])
    (refusesElement "a wyoming language holding a comma is refused" ["wyoming" "languages"] "en,")
    (refusesElement "a wyoming language of only a carriage return is refused" ["wyoming" "languages"] "\r")
    (refusesElement "a wyoming language of only a no-break space is refused" ["wyoming" "languages"] (character "00a0"))
    (acceptsElement "the wyoming language en is accepted" ["wyoming" "languages"] "en")
    (refuses "wyoming.port 0 is refused" ["wyoming" "port"] 0)
    (refuses "wyoming.streamingMaxChars 0 is refused" ["wyoming" "streamingMaxChars"] 0)

    (failsWith "a wyoming.port equal to port on the same host fails naming both"
      (wyomingOn {port = 8920;})
      ["${opt}.wyoming.port" "${opt}.port"])

    (evaluates "a wyoming.port equal to port on another host evaluates" (wyomingOn {
      host = "::1";
      port = 8920;
    }))

    (evaluates "wyoming.speed 0.25 evaluates" (wyomingOn {speed = 0.25;}))
    (evaluates "wyoming.speed 4.0 evaluates" (wyomingOn {speed = 4.0;}))

    (evaluates "wyoming.enable = false skips the Wyoming assertions, so tts.maxInputChars below wyoming.streamingMaxChars evaluates"
      (lib.recursiveUpdate base {tts.maxInputChars = 200;}))

    (evaluates "wyoming.streamingMaxChars equal to the default TTS_MAXINPUTCHARS evaluates"
      (wyomingOn {streamingMaxChars = 4096;}))

    (failsWith "wyoming.streamingMaxChars above tts.maxInputChars fails"
      (lib.recursiveUpdate (wyomingOn {}) {tts.maxInputChars = 200;})
      ["${opt}.wyoming.streamingMaxChars" "${opt}.tts.maxInputChars" "TTS_MAXINPUTCHARS"])

    (failsWith "wyoming.streamingMaxChars above the default TTS_MAXINPUTCHARS fails"
      (wyomingOn {streamingMaxChars = 5000;})
      ["${opt}.wyoming.streamingMaxChars" "TTS_MAXINPUTCHARS"])

    (failsWith "an extraEnvironment TTS_MAXINPUTCHARS below wyoming.streamingMaxChars fails"
      (lib.recursiveUpdate (wyomingOn {}) {extraEnvironment."${prefix}TTS_MAXINPUTCHARS" = "100";})
      ["${opt}.wyoming.streamingMaxChars" "TTS_MAXINPUTCHARS"])

    (evaluates "an extraEnvironment TTS_MAXINPUTCHARS that raises the limit allows a larger wyoming.streamingMaxChars"
      (lib.recursiveUpdate (wyomingOn {streamingMaxChars = 5000;}) {extraEnvironment."${prefix}TTS_MAXINPUTCHARS" = "8000";}))

    (failsWith "an extraEnvironment WYOMING_STREAMINGMAXCHARS above the effective TTS_MAXINPUTCHARS fails"
      (lib.recursiveUpdate (wyomingOn {}) {extraEnvironment."${wyomingPrefix}STREAMINGMAXCHARS" = "5000";})
      ["${opt}.wyoming.streamingMaxChars" "TTS_MAXINPUTCHARS"])

    (failsWith "an invalid WYOMING_STREAMINGMAXCHARS override fails naming the variable"
      (base // {extraEnvironment."${wyomingPrefix}STREAMINGMAXCHARS" = "lots";})
      ["${opt}.extraEnvironment.${wyomingPrefix}STREAMINGMAXCHARS" "lots"])

    (evaluates "wyoming.host ::1 evaluates" (wyomingOn {host = "::1";}))

    (failsWith "a bracketed wyoming.host fails naming the option"
      (wyomingOn {host = "[::1]";})
      ["${opt}.wyoming.host" "[::1]"])

    (let
      ports = firewallPorts (wyomingOn {
        port = otherWyomingPort;
        openFirewall = true;
      });
    in
      holds "wyoming.openFirewall = true opens wyoming.port and nothing else" (ports == [otherWyomingPort]) "allowedTCPPorts is ${show ports}, want ${show [otherWyomingPort]}")

    (let
      ports = firewallPorts (wyomingOn {});
    in
      holds "wyoming.openFirewall = false opens no port" (ports == []) "allowedTCPPorts is ${show ports}")

    (let
      ports = firewallPorts (lib.recursiveUpdate base {wyoming.openFirewall = true;});
    in
      holds "wyoming.openFirewall without wyoming.enable opens no port" (ports == []) "allowedTCPPorts is ${show ports}")
  ];

  # Go's strconv.ParseBool spellings, written out here so that a changed
  # list in the module fails a case.
  falseSpellings = ["" "0" "f" "F" "false" "FALSE" "False"];
  trueSpellings = ["1" "t" "T" "TRUE" "true" "True"];

  recordingsSpellingCases =
    map (value:
      evaluates "RECORDINGS_ENABLE = ${show value} in extraEnvironment is accepted while recordings are off"
      (base // {extraEnvironment."${prefix}RECORDINGS_ENABLE" = value;}))
    falseSpellings
    ++ map (value:
      failsWith "RECORDINGS_ENABLE = ${show value} in extraEnvironment turns recordings on and fails"
      (base // {extraEnvironment."${prefix}RECORDINGS_ENABLE" = value;})
      ["${opt}.extraEnvironment.${prefix}RECORDINGS_ENABLE" "${opt}.recordings.enable"])
    trueSpellings
    ++ [
      (failsWith "a mixed-case RECORDINGS_ENABLE in extraEnvironment is refused, naming the canonical key"
        (base // {extraEnvironment."${prefix}Recordings_Enable" = "true";})
        ["${opt}.extraEnvironment.${prefix}Recordings_Enable" "${prefix}RECORDINGS_ENABLE"])
      (failsWith "a lower-case RECORDINGS_ENABLE set to false is refused too"
        (base // {extraEnvironment."${prefix}recordings_enable" = "false";})
        ["${opt}.extraEnvironment.${prefix}recordings_enable" "${prefix}RECORDINGS_ENABLE"])
      (failsWith "RECORDINGS_DIR spelt with a dot is refused, naming the canonical key"
        (base // {extraEnvironment."${prefix}RECORDINGS.DIR" = "/srv/elsewhere";})
        ["${opt}.extraEnvironment.${prefix}RECORDINGS.DIR" "${prefix}RECORDINGS_DIR"])
      (failsWith "a mixed-case RECORDINGS_ALLOWEDHOSTS is refused while recordings are on"
        (base
          // {
            recordings.enable = true;
            extraEnvironment."${prefix}Recordings_AllowedHosts" = "voice.example";
          })
        ["${opt}.extraEnvironment.${prefix}Recordings_AllowedHosts" "${prefix}RECORDINGS_ALLOWEDHOSTS"])
      (evaluates "a key whose prefix is not the service's exact prefix is not a recordings variable"
        (base // {extraEnvironment."llama_swap_voice_RECORDINGS_ENABLE" = "true";}))
    ];

  allCases = cases ++ recordingsSpellingCases ++ swapCases ++ wyomingCases;

  report = lib.concatMapStrings (c:
    if c.ok
    then "PASS ${c.name}\n"
    else "FAIL ${c.name}: ${c.detail}\n")
  allCases;

  failed = lib.count (c: !c.ok) allCases;
in
  pkgs.runCommand "llama-swap-voice-module-eval" {
    inherit report;
    passAsFile = ["report"];
  } ''
    cat $reportPath
    if [ ${toString failed} -ne 0 ]; then
      echo "${toString failed} module evaluation case(s) failed" >&2
      exit 1
    fi
    cp $reportPath $out
  ''
