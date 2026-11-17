flakeSelf: {
  config,
  lib,
  pkgs,
  ...
}: let
  inherit (lib) mkOption types;

  cfg = config.services.llama-swap-voice;
  opt = "services.llama-swap-voice";

  envPrefix = "LLAMA_SWAP_VOICE_";
  voicesDir = "${cfg.stateDir}/voices";
  clipExtensions = ["wav" "mp3"];

  defaultName = "llama-swap-voice";

  # Go's time.ParseDuration syntax without a sign: "0", or numbers each
  # followed by a unit.
  durationPattern = ''0|((([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(ns|us|µs|μs|ms|s|m|h))+'';
  termPattern = "([0-9]*)(\\.([0-9]*))?(ns|us|µs|μs|ms|s|m|h)";
  maxDuration = 9223372036854775807;
  unitNanoseconds = {
    ns = 1;
    us = 1000;
    "µs" = 1000;
    "μs" = 1000;
    ms = 1000000;
    s = 1000000000;
    m = 60000000000;
    h = 3600000000000;
  };

  # Every bound is checked before the multiplication or addition it guards,
  # because Nix integers stop at maxDuration.
  wholeNanoseconds = digits:
    lib.foldl' (
      value: char: let
        digit = lib.toInt char;
      in
        if value == null || value > (maxDuration - digit) / 10
        then null
        else value * 10 + digit
    )
    0 (lib.stringToCharacters digits);

  # Go's leadingFraction: digits past what fits are ignored, not refused.
  fractionDigits = digits:
    lib.foldl' (
      state: char: let
        digit = lib.toInt char;
      in
        if state.full || state.value > maxDuration / 10 || (state.value == maxDuration / 10 && digit > 7)
        then state // {full = true;}
        else {
          value = state.value * 10 + digit;
          scale = state.scale * 10.0;
          full = false;
        }
    ) {
      value = 0;
      scale = 1.0;
      full = false;
    } (lib.stringToCharacters digits);

  termNanoseconds = term: let
    whole = wholeNanoseconds (lib.elemAt term 0);
    unit = unitNanoseconds.${lib.elemAt term 3};
    fraction = fractionDigits (lib.defaultTo "" (lib.elemAt term 2));
    # Go computes the fraction in float64 and truncates it; so does this.
    fractionNanoseconds = builtins.floor (fraction.value * 1.0 * (unit * 1.0 / fraction.scale));
  in
    if whole == null || whole > maxDuration / unit || whole * unit > maxDuration - fractionNanoseconds
    then null
    else whole * unit + fractionNanoseconds;

  # Nanoseconds as Go's time.ParseDuration reads the string, or null where Go
  # refuses it or the string has a sign, which the module does not accept.
  durationNanoseconds = value:
    if builtins.match durationPattern value == null
    then null
    else
      lib.foldl' (
        sum: term: let
          nanoseconds = termNanoseconds term;
        in
          if sum == null || nanoseconds == null || sum > maxDuration - nanoseconds
          then null
          else sum + nanoseconds
      )
      0 (lib.filter builtins.isList (builtins.split termPattern value));

  duration =
    types.addCheck types.str (value: durationNanoseconds value != null)
    // {
      description = "Go duration";
      toNanoseconds = durationNanoseconds;
    };
  positiveDuration =
    types.addCheck duration (value: durationNanoseconds value > 0)
    // {description = "positive Go duration";};

  # Decimal without a sign or leading zero, at most Go's int64: the subset
  # that Nix's toInt and the service's integer parsing read alike.
  positiveInteger =
    types.addCheck types.str (value:
      builtins.match "[1-9][0-9]{0,18}" value
      != null
      && (lib.stringLength value < 19 || value <= "9223372036854775807"))
    // {description = "positive integer";};

  # Variables the module itself reads after extraEnvironment is merged, each
  # with the type an override must have.
  checkedVariables = {
    SHUTDOWN_GRACE = duration;
    SHUTDOWN_TOTAL = positiveDuration;
    SHUTDOWN_HARD = positiveDuration;
    TTS_MAXINPUTCHARS = positiveInteger;
    UPSTREAM_TTSCONCURRENCY = positiveInteger;
    REQUEST_MAXCONCURRENT = positiveInteger;
  };

  # The service's own defaults (cmd/llama-swap-voice/config.go), for the
  # checked integers left unset.
  serviceDefaults = {
    TTS_MAXINPUTCHARS = 4096;
    UPSTREAM_TTSCONCURRENCY = 1;
    REQUEST_MAXCONCURRENT = 4;
  };

  rawValue = name: environment."${envPrefix}${name}" or null;

  # A checked variable's value after extraEnvironment, or null when it is
  # unset or invalid; an invalid value fails an assertion instead.
  effective = name: let
    value = rawValue name;
  in
    if value != null && checkedVariables.${name}.check value
    then value
    else null;

  effectiveInteger = name: let
    value = effective name;
  in
    if value == null
    then serviceDefaults.${name}
    else lib.toInt value;

  # The service runs the shutdown steps one after another. The margin covers
  # the steps left unset, whose Go defaults total 18 s. Seconds and remainders
  # are summed apart, so three maximal durations cannot overflow.
  stopTimeout = let
    valid = map durationNanoseconds (lib.filter (value: value != null) (map (step: effective "SHUTDOWN_${step}") ["GRACE" "TOTAL" "HARD"]));
    seconds = lib.foldl' (sum: value: sum + value / 1000000000) 0 valid;
    remainder = lib.foldl' (sum: value: sum + lib.mod value 1000000000) 0 valid;
  in
    lib.max 90 (seconds + (remainder + 999999999) / 1000000000 + 30);

  checkedAssertions =
    lib.mapAttrsToList (name: type: let
      value = rawValue name;
    in {
      assertion = value == null || type.check value;
      message = "${opt}.extraEnvironment.${envPrefix}${name} is \"${toString value}\", which is not a ${type.description} the module accepts.";
    })
    checkedVariables;

  # Python's str.isspace() characters, which config.py strips before it
  # refuses an empty transcript; JSON spells them, as Nix strings cannot.
  pythonWhitespace = builtins.fromJSON ''
    ["\t", "\n", "\u000b", "\u000c", "\r", "\u001c", "\u001d", "\u001e", "\u001f", " ",
     "\u0085", "\u00a0", "\u1680", "\u2000", "\u2001", "\u2002", "\u2003", "\u2004",
     "\u2005", "\u2006", "\u2007", "\u2008", "\u2009", "\u200a", "\u2028", "\u2029",
     "\u202f", "\u205f", "\u3000"]
  '';
  isBlank = text: builtins.replaceStrings pythonWhitespace (map (_: "") pythonWhitespace) text == "";

  # systemd-tmpfiles reads % as a specifier in paths and arguments.
  tmpfilesEscape = lib.replaceStrings ["%"] ["%%"];

  limit = type: description:
    mkOption {
      type = types.nullOr type;
      default = null;
      description = "${description} Null leaves the variable unset, so the service's own default applies.";
    };

  durationLimit = type: description: limit type "${description} Written as a Go duration such as \"1m30s\"; a leading + or - is not accepted.";

  voiceModule = types.submodule {
    options = {
      clip = mkOption {
        type = types.str;
        example = "/srv/voices/narrator.wav";
        description = ''
          Absolute path to the reference recording, as a string so the clip
          never enters the Nix store. It must end in .wav or .mp3, and in .wav
          under the qwentts backend. The service reads it at startup through
          the symlink in the voices directory, so the service user must be
          able to read it.
        '';
      };
      transcript = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = ''
          What the clip says. Null selects embedding-only cloning, which keeps
          the voice but not its accent. Under the qwentts backend it goes into the faster-qwen3-tts server's configuration, which the module generates when llamaSwap.enable is true; the llamacpp backend ignores it.
        '';
      };
      instruct = mkOption {
        type = types.nullOr types.str;
        default = null;
        description = "Style instruction, which needs a transcript under qwentts. Under the qwentts backend it goes into the faster-qwen3-tts server's configuration, which the module generates when llamaSwap.enable is true; the llamacpp backend ignores it.";
      };
      temperature = mkOption {
        type = types.numbers.nonnegative;
        default = 0.7;
        description = "Sampling temperature. Under the qwentts backend it goes into the faster-qwen3-tts server's configuration, which the module generates when llamaSwap.enable is true; the llamacpp backend ignores it.";
      };
      topK = mkOption {
        type = types.ints.positive;
        default = 50;
        description = "Top-k sampling. Under the qwentts backend it goes into the faster-qwen3-tts server's configuration, which the module generates when llamaSwap.enable is true; the llamacpp backend ignores it.";
      };
      topP = mkOption {
        type = types.numbers.between 0 1;
        default = 1.0;
        description = "Top-p sampling. Under the qwentts backend it goes into the faster-qwen3-tts server's configuration, which the module generates when llamaSwap.enable is true; the llamacpp backend ignores it.";
      };
      repetitionPenalty = mkOption {
        type = types.numbers.positive;
        default = 1.05;
        description = "Repetition penalty. Under the qwentts backend it goes into the faster-qwen3-tts server's configuration, which the module generates when llamaSwap.enable is true; the llamacpp backend ignores it.";
      };
    };
  };

  # llama-swap answers on every address for a wildcard listen address; the
  # service reaches it over loopback, which its IPAddressAllow always allows.
  # An unbracketed IPv6 address fails an assertion in llama-swap.nix.
  loopbackFor = address:
    if address == "" || address == "0.0.0.0"
    then "127.0.0.1"
    else if address == "[::]"
    then "[::1]"
    else address;

  llamaSwapURL = let
    swap = config.services.llama-swap;
    scheme =
      if swap.tls.enable
      then "https"
      else "http";
  in "${scheme}://${loopbackFor swap.listenAddress}:${toString swap.port}";

  whenIntegrated = value:
    if cfg.llamaSwap.enable
    then value
    else null;

  ttsModelName =
    if cfg.tts.backend == "qwentts"
    then cfg.tts.qwentts.modelName
    else cfg.models.tts.name;

  extensionOf = path: lib.last (lib.splitString "." path);
  hasExtension = exts: path: lib.elem (lib.toLower (extensionOf path)) exts;
  linkName = name: voice: "${voicesDir}/${name}.${extensionOf voice.clip}";

  voiceAssertions = name: voice: let
    voiceOpt = "${opt}.voices.${name}";
    qwentts = cfg.tts.backend == "qwentts";
  in [
    {
      assertion = builtins.match "[A-Za-z0-9_][A-Za-z0-9_.-]*" name != null;
      message = "${voiceOpt}: the voice name becomes a file name in ${voicesDir}, so it must start with a letter, digit or underscore and hold only letters, digits, '.', '_' and '-'.";
    }
    {
      assertion = lib.hasPrefix "/" voice.clip;
      message = "${voiceOpt}.clip must be an absolute path, got \"${voice.clip}\".";
    }
    {
      assertion = hasExtension clipExtensions voice.clip;
      message = "${voiceOpt}.clip must end in .wav or .mp3, the formats the service reads, got \"${voice.clip}\".";
    }
    {
      assertion = qwentts -> hasExtension ["wav"] voice.clip;
      message = "${voiceOpt}.clip must end in .wav when ${opt}.tts.backend is qwentts, got \"${voice.clip}\".";
    }
    {
      assertion = qwentts -> voice.transcript == null || !isBlank voice.transcript;
      message = "${voiceOpt}.transcript must be null or hold more than whitespace when ${opt}.tts.backend is qwentts; null selects embedding-only cloning.";
    }
    {
      assertion = qwentts -> voice.instruct != null -> voice.transcript != null;
      message = "${voiceOpt}.instruct is set, so ${voiceOpt}.transcript must be set too when ${opt}.tts.backend is qwentts; the TTS server ignores an instruct without a transcript.";
    }
  ];

  generated = {
    HOST = cfg.host;
    PORT = toString cfg.port;
    UPSTREAM_URL = cfg.upstreamURL;
    UPSTREAM_TTSBACKEND = cfg.tts.backend;
    UPSTREAM_TTSCONCURRENCY = cfg.upstream.ttsConcurrency;
    UPSTREAM_TTSMODEL = whenIntegrated ttsModelName;
    UPSTREAM_ASRMODEL = whenIntegrated cfg.models.asr.name;
    VOICES_DIR = voicesDir;
    VOICES_DEFAULT = cfg.defaultVoice;
    TTS_MAXINPUTCHARS = cfg.tts.maxInputChars;
    REQUEST_TIMEOUT = cfg.request.timeout;
    REQUEST_MAXCONCURRENT = cfg.request.maxConcurrent;
    REQUEST_MAXCONCURRENTTTS = cfg.request.maxConcurrentTTS;
    SHUTDOWN_GRACE = cfg.shutdown.grace;
    SHUTDOWN_TOTAL = cfg.shutdown.total;
    SHUTDOWN_HARD = cfg.shutdown.hard;
  };

  environment =
    lib.mapAttrs' (name: value: lib.nameValuePair "${envPrefix}${name}" (toString value))
    (lib.filterAttrs (_: value: value != null) generated)
    // cfg.extraEnvironment;

  # tmpfiles' L never replaces an existing path, so a link left from an
  # earlier clip, or a hand-placed file under a declared name, would keep
  # serving the wrong audio silently. This turns that into a failed start.
  checkLinks = pkgs.writeShellScript "llama-swap-voice-check-links" ''
    status=0
    check() {
      if [ "$(${pkgs.coreutils}/bin/readlink -- "$1")" != "$2" ]; then
        echo "$1 is not a symlink to $2; remove it and run systemd-tmpfiles --create" >&2
        status=1
      fi
    }
    ${lib.concatStrings (lib.mapAttrsToList (name: voice: ''
        check ${lib.escapeShellArg (linkName name voice)} ${lib.escapeShellArg voice.clip}
      '')
      cfg.voices)}
    exit $status
  '';
in {
  imports = [(import ./llama-swap.nix {inherit flakeSelf linkName ttsModelName effectiveInteger;})];

  options.services.llama-swap-voice = {
    enable = lib.mkEnableOption "llama-swap-voice, the OpenAI audio API in front of llama-swap";

    package = mkOption {
      type = types.package;
      default = flakeSelf.packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalExpression "llama-swap-voice.packages.\${pkgs.stdenv.hostPlatform.system}.default";
      description = "The llama-swap-voice package to run.";
    };

    user = mkOption {
      type = types.str;
      default = defaultName;
      description = "System user the service runs as. The module creates it only under the default name; a custom user must already exist.";
    };

    group = mkOption {
      type = types.str;
      default = defaultName;
      description = ''
        System group the service runs as. The module creates it only under
        the default name; a custom group must already exist. Under the
        qwentts backend llama-swap's user joins this group, gaining all its
        access, so do not pick a group other services share.
      '';
    };

    stateDir = mkOption {
      type = types.strMatching "/[^%]*[^/%]";
      default = "/var/lib/llama-swap-voice";
      description = ''
        Directory the module owns, so it must belong to this module alone:
        tmpfiles sets its owner and mode. An absolute path without a
        trailing slash or a %, which systemd would read as a specifier in
        the unit's environment. The service reads its voices from the voices
        subdirectory, where each declared voice is a symlink and clips placed
        by hand also count.
      '';
    };

    defaultVoice = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Voice used when a request names none. Must be one of voiceNames.";
    };

    host = mkOption {
      type = types.str;
      default = "127.0.0.1";
      description = ''
        Address to listen on. A non-loopback address also needs the clients'
        addresses in ipAddressAllow.
      '';
    };

    port = mkOption {
      type = types.ints.between 1 65535;
      default = 8920;
      description = "Port to listen on.";
    };

    upstreamURL = mkOption {
      type = types.strMatching "https?://[^[:space:]/?#]+([/?#][^[:space:]]*)?";
      default =
        if cfg.llamaSwap.enable
        then llamaSwapURL
        else "http://127.0.0.1:8080";
      defaultText = lib.literalMD ''
        services.llama-swap's listen address and port when llamaSwap.enable
        is true, with a wildcard address replaced by loopback; otherwise
        `http://127.0.0.1:8080`
      '';
      description = ''
        llama-swap's base URL. Evaluation checks only a conservative subset,
        http:// or https:// followed by a non-empty host and no whitespace;
        the service parses the URL fully at startup. A non-loopback upstream
        also needs its address in ipAddressAllow.
      '';
    };

    ipAddressAllow = mkOption {
      type = types.listOf types.str;
      default = [];
      example = ["192.168.1.10" "10.0.0.0/8"];
      description = ''
        Addresses or prefixes the service may exchange packets with besides
        loopback, in systemd's IPAddressAllow= syntax. Everything else is
        denied, in both directions. Name resolution goes through the
        resolver's address, so a DNS upstream name needs that allowed too.
      '';
    };

    tts = {
      backend = mkOption {
        type = types.enum ["llamacpp" "qwentts"];
        default = "llamacpp";
        description = ''
          How speech requests reach the TTS model. llamacpp posts a multipart
          form with the voice's clip attached; qwentts posts JSON naming the
          voice and sends no clip.
        '';
      };

      maxInputChars = limit types.ints.positive "Longest text accepted per speech request (TTS_MAXINPUTCHARS).";
    };

    upstream.ttsConcurrency = mkOption {
      type = types.ints.positive;
      default = 1;
      description = ''
        Speech requests sent to the TTS model at once
        (UPSTREAM_TTSCONCURRENCY). The service's gate keeps sentence order,
        and it releases a request when generation ends, not playback, so 1
        costs nothing. The TTS entry's concurrencyLimit must be at least this.
      '';
    };

    request = {
      timeout = durationLimit positiveDuration "Longest a speech or transcription request may take (REQUEST_TIMEOUT).";
      maxConcurrent = limit types.ints.positive "Transcriptions handled at once (REQUEST_MAXCONCURRENT); models.asr.concurrencyLimit follows it.";
      maxConcurrentTTS = limit types.ints.positive "Speech requests handled at once, queued ones included (REQUEST_MAXCONCURRENTTTS).";
    };

    shutdown = {
      grace = durationLimit duration "How long readiness reports 503 before the listener closes (SHUTDOWN_GRACE).";
      total = durationLimit positiveDuration "How long in-flight requests get to finish (SHUTDOWN_TOTAL).";
      hard = durationLimit positiveDuration "How long cancelled requests get to wind down (SHUTDOWN_HARD).";
    };

    voices = mkOption {
      type = types.attrsOf voiceModule;
      default = {};
      example = lib.literalExpression ''
        {
          narrator = {
            clip = "/srv/voices/narrator.wav";
            transcript = "The quick brown fox jumps over the lazy dog.";
          };
        }
      '';
      description = ''
        The voice registry. Each voice becomes the symlink
        <stateDir>/voices/<name>.<extension of clip>. The service uses only
        the clip; the other settings go into the faster-qwen3-tts server's
        configuration under the qwentts backend. Under qwentts llama-swap's
        user reads each clip too, through the voices directory, so each clip
        must be readable by the module's group.
      '';
    };

    voiceNames = mkOption {
      type = types.listOf types.str;
      readOnly = true;
      default = lib.attrNames cfg.voices;
      defaultText = lib.literalExpression "lib.attrNames config.services.llama-swap-voice.voices";
      description = ''
        Names of the declared voices, sorted: a Nix attribute set keeps no
        declaration order. Voices placed in the directory by hand are not
        listed.
      '';
    };

    extraEnvironment = mkOption {
      type = types.attrsOf types.str;
      default = {};
      example = {LLAMA_SWAP_VOICE_LOG_LEVEL = "debug";};
      description = ''
        Extra environment variables for the service, by full name. They win
        over the variables the module generates. The module reads the
        shutdown durations, TTS_MAXINPUTCHARS, UPSTREAM_TTSCONCURRENCY and
        REQUEST_MAXCONCURRENT after this merge, and evaluation fails when an
        override is not a value the matching option accepts.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions =
      [
        {
          assertion = cfg.defaultVoice != null;
          message = "${opt}.defaultVoice must be set to one of the declared voices: ${lib.concatStringsSep ", " cfg.voiceNames}.";
        }
        {
          assertion = cfg.defaultVoice == null || lib.elem cfg.defaultVoice cfg.voiceNames;
          message = "${opt}.defaultVoice is \"${toString cfg.defaultVoice}\", which is not one of voiceNames: ${lib.concatStringsSep ", " cfg.voiceNames}.";
        }
      ]
      ++ lib.concatLists (lib.mapAttrsToList voiceAssertions cfg.voices)
      ++ checkedAssertions;

    users.users = lib.mkIf (cfg.user == defaultName) {
      ${defaultName} = {
        isSystemUser = true;
        inherit (cfg) group;
      };
    };
    users.groups = lib.mkIf (cfg.group == defaultName) {${defaultName} = {};};

    # d, never D: --remove would empty a D directory, and the voices
    # directory also holds clips placed by hand.
    systemd.tmpfiles.settings.llama-swap-voice =
      {
        ${tmpfilesEscape cfg.stateDir}.d = {
          inherit (cfg) user group;
          mode = "0750";
        };
        ${tmpfilesEscape voicesDir}.d = {
          inherit (cfg) user group;
          mode = "0750";
        };
      }
      // lib.mapAttrs' (name: voice: lib.nameValuePair (tmpfilesEscape (linkName name voice)) {L.argument = tmpfilesEscape voice.clip;}) cfg.voices;

    systemd.services.llama-swap-voice = {
      description = "llama-swap-voice";
      wantedBy = ["multi-user.target"];
      after = ["network.target" "systemd-tmpfiles-setup.service" "systemd-tmpfiles-resetup.service"] ++ lib.optional cfg.llamaSwap.enable "llama-swap.service";
      inherit environment;

      serviceConfig = {
        Type = "simple";
        User = cfg.user;
        Group = cfg.group;
        ExecStartPre = checkLinks;
        ExecStart = lib.getExe cfg.package;
        Restart = "on-failure";
        RestartSec = "5s";
        TimeoutStopSec = stopTimeout;
        UMask = "0077";

        # ProtectSystem=strict leaves every path, stateDir included,
        # read-only; the only writable place is the private /tmp, where
        # multipart uploads larger than the in-memory limit spool.
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        PrivateUsers = true;
        PrivateMounts = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectProc = "invisible";
        ProcSubset = "pid";
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        RestrictAddressFamilies = ["AF_INET" "AF_INET6" "AF_UNIX"];
        IPAddressDeny = "any";
        IPAddressAllow = ["localhost"] ++ cfg.ipAddressAllow;
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        RemoveIPC = true;
        DevicePolicy = "closed";
        SystemCallArchitectures = "native";
        SystemCallFilter = ["@system-service" "~@privileged" "~@resources"];
        SystemCallErrorNumber = "EPERM";
      };
    };
  };
}
