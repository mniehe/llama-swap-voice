{
  pkgs,
  module,
}: let
  inherit (pkgs) lib;

  opt = "services.llama-swap-voice";
  prefix = "LLAMA_SWAP_VOICE_";
  clip = "/srv/clips/sample.wav";

  base = {
    enable = true;
    defaultVoice = "sample";
    voices.sample.clip = clip;
  };

  evaluate = settings:
    pkgs.nixos [
      module
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

  moduleFailures = settings:
    map (a: a.message) (lib.filter (a: !a.assertion && lib.hasPrefix opt a.message) (evaluate settings).config.assertions);

  serviceEnv = settings: lib.filterAttrs (name: _: lib.hasPrefix prefix name) (evaluate settings).config.systemd.services.llama-swap-voice.environment;

  show = value: builtins.toJSON value;

  failsWith = name: settings: needles: let
    failures = moduleFailures settings;
    named = lib.any (message: lib.all (needle: lib.hasInfix needle message) needles) failures;
  in {
    inherit name;
    ok = named;
    detail = "no failed assertion names all of ${show needles}; failed: ${show failures}";
  };

  evaluates = name: settings: let
    failures = moduleFailures settings;
    built = builtins.tryEval (evaluate settings).config.system.build.toplevel.drvPath;
  in {
    inherit name;
    ok = failures == [] && built.success;
    detail = "failed: ${show failures}; toplevel evaluated: ${show built.success}";
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
  ];

  report = lib.concatMapStrings (c:
    if c.ok
    then "PASS ${c.name}\n"
    else "FAIL ${c.name}: ${c.detail}\n")
  cases;

  failed = lib.count (c: !c.ok) cases;
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
