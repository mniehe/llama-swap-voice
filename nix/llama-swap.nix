# The llama-swap side of services.llama-swap-voice: the Qwen3 ASR entry, one
# TTS entry chosen by tts.backend, the TTS server's configuration and what
# the llama-swap unit needs to run them.
{
  flakeSelf,
  linkName,
  ttsModelName,
  effectiveInteger,
}: {
  config,
  lib,
  pkgs,
  ...
}: let
  inherit (lib) mkOption types;

  cfg = config.services.llama-swap-voice;
  swap = config.services.llama-swap;
  opt = "services.llama-swap-voice";
  pinned = import ./models.nix;
  qwen = cfg.tts.qwentts;
  qwentts = cfg.tts.backend == "qwentts";
  system = pkgs.stdenv.hostPlatform.system;

  minLlamaCppVersion = "0.8.0";
  serverCache = "\${XDG_CACHE_HOME:-/var/cache/llama-swap}/qwen-tts-server";

  nvidiaDevices = ["char-nvidia rw" "char-nvidia-uvm rw"];
  # A DeviceAllow list closes every other device, so these must be named
  # again for a llama.cpp on another GPU.
  otherGPUDevices = {
    vulkan = ["char-drm rw"];
    rocm = ["char-drm rw" "/dev/kfd rw"];
  };

  # Every flag is set, so nixpkgs.config.cudaSupport or rocmSupport cannot
  # add a backend the unit's hardening does not expect.
  llamaCppFor = backend:
    pkgs.llama-cpp.override {
      cudaSupport = backend == "cuda";
      rocmSupport = backend == "rocm";
      vulkanSupport = backend == "vulkan";
    };

  flakePackage = name:
    flakeSelf.packages.${system}.${name}
    or (throw "${opt}: this flake builds ${name} only for x86_64-linux; set the option to your own for ${system}.");

  artifactOption = pin: what:
    mkOption {
      type = types.submodule ({config, ...}: {
        options = {
          url = mkOption {
            type = types.str;
            default = pin.url;
            description = "Where ${what} is downloaded from.";
          };
          hash = mkOption {
            type = types.str;
            default = pin.hash;
            description = "SRI hash of ${what}.";
          };
          path = mkOption {
            type = types.path;
            default = pkgs.fetchurl {inherit (config) url hash;};
            defaultText = lib.literalExpression "pkgs.fetchurl { inherit url hash; }";
            description = ''
              ${what} on disk. Set it to a local file to skip the download.
              llama-swap's user must be able to read it, and llama-swap's unit
              cannot see /home.
            '';
          };
        };
      });
      default = {};
      description = "${what}, downloaded from a pinned revision unless path is set.";
    };

  ttlDescription = what: ''
    Seconds idle before llama-swap unloads the ${what}; 0 disables idle
    unloading. Under the default llamaSwap.group.sharing, "pinned", loading
    another model never unloads it. Under "takeTurns", loading a model from
    an exclusive group, such as llama-swap's implicit default group of
    ungrouped models, unloads it whatever its ttl. With
    llamaSwap.group.enable false the ${what} is itself in that default group,
    so loading any model in it, the other voice entry included, unloads it
    too.
  '';

  # What each llamaSwap.group.sharing value writes into the voice group.
  sharingModes = {
    pinned = {
      exclusive = false;
      persistent = true;
    };
    takeTurns = {
      exclusive = true;
      persistent = false;
    };
  };

  entryOptions = {
    what,
    name,
    pins,
    concurrencyLimit,
  }: {
    name = mkOption {
      type = types.nonEmptyStr;
      default = name;
      description = "Model name of the ${what} entry in llama-swap.";
    };
    artifacts = {
      model = artifactOption pins.model "The ${what} GGUF";
      mmproj = artifactOption pins.mmproj "The ${what} multimodal projector GGUF";
    };
    device = mkOption {
      type = types.nullOr types.str;
      default = null;
      example = "CUDA0";
      description = "llama-server's --device. Null leaves it out, so llama.cpp chooses, which a CPU build needs.";
    };
    gpuLayers = mkOption {
      type = types.ints.unsigned;
      default = 99;
      description = "Layers offloaded to the GPU (-ngl).";
    };
    contextSize = mkOption {
      type = types.ints.positive;
      default = 4096;
      description = "Context size (-c).";
    };
    parallel = mkOption {
      type = types.ints.positive;
      default = 1;
      description = "Server slots (-np).";
    };
    concurrencyLimit = mkOption {
      type = types.ints.positive;
      inherit (concurrencyLimit) default;
      defaultText = concurrencyLimit.text or concurrencyLimit.default;
      description = "Requests llama-swap passes to the entry at once (concurrencyLimit). llama-swap answers 429 to requests beyond it; llama-server queues those beyond its slots (parallel).";
    };
    ttl = mkOption {
      type = types.ints.unsigned;
      default = 0;
      description = ttlDescription "entry";
    };
    preload = mkOption {
      type = types.bool;
      default = true;
      description = "Load the entry when llama-swap starts (hooks.on_startup.preload).";
    };
    extraArgs = mkOption {
      type = types.listOf types.str;
      default = [];
      description = "Extra llama-server arguments, appended to the command.";
    };
  };

  esc = lib.escapeShellArg;

  llamaServerCommand = entry: flags:
    lib.concatStringsSep " " (
      [
        (esc (lib.getExe' cfg.llamaCpp.package "llama-server"))
        "--port \${PORT}"
        "-m ${esc "${entry.artifacts.model.path}"}"
        "--mmproj ${esc "${entry.artifacts.mmproj.path}"}"
      ]
      ++ lib.optional (entry.device != null) "--device ${esc entry.device}"
      ++ [
        "-ngl ${toString entry.gpuLayers}"
        "-c ${toString entry.contextSize}"
        "-np ${toString entry.parallel}"
      ]
      ++ flags
      ++ ["--flash-attn on" "--no-webui"]
      ++ map esc entry.extraArgs
    );

  llamaServerEntry = entry: flags: {
    cmd = llamaServerCommand entry flags;
    checkEndpoint = "/health";
    inherit (entry) concurrencyLimit ttl;
  };

  serverConfig = pkgs.writeText "qwen-tts-server.json" (builtins.toJSON (import ./qwen-tts-config.nix {
    inherit lib cfg linkName;
    maxInputChars = effectiveInteger "TTS_MAXINPUTCHARS";
  }));

  qwenEntry = {
    cmd = lib.concatStringsSep " " (
      [
        (esc (lib.getExe' qwen.package "qwen-tts-server"))
        "--port \${PORT}"
        "--config ${serverConfig}"
      ]
      ++ map esc qwen.extraArgs
    );
    checkEndpoint = "/health";
    env = ["CUDA_VISIBLE_DEVICES=${qwen.cudaDevice}"];
    inherit (qwen) concurrencyLimit ttl;
  };

  asrName = cfg.models.asr.name;
  tts =
    if qwentts
    then {
      entry = qwenEntry;
      inherit (qwen) preload concurrencyLimit;
      option = "${opt}.tts.qwentts.concurrencyLimit";
    }
    else {
      entry = llamaServerEntry cfg.models.tts [];
      inherit (cfg.models.tts) preload concurrencyLimit;
      option = "${opt}.models.tts.concurrencyLimit";
    };

  preloaded = lib.optional cfg.models.asr.preload asrName ++ lib.optional tts.preload ttsModelName;

  group = cfg.llamaSwap.group;

  voiceGroup = {
    swap = false;
    inherit (sharingModes.${group.sharing}) exclusive persistent;
    members = [asrName ttsModelName];
  };
  groupOpt = "${opt}.llamaSwap.group.location";

  # What llama-swap's loader treats as each layout (internal/config/load.go).
  router = swap.settings.routing.router or {};
  routerSettings = router.settings or {};
  usesRouter =
    (router.use or "")
    != ""
    || (routerSettings.matrix or null) != null
    || (routerSettings.groups or {}) != {}
    || (routerSettings.capacity or null) != null;
  topLevelGroups = (swap.settings.groups or null) != null;
  topLevelMatrix = (swap.settings.matrix or null) != null;

  usesCuda = cfg.llamaCpp.backend == "cuda" || qwentts;

  cacheRoot = pkgs.writeShellScript "llama-swap-voice-qwen-tts-server-cache" ''
    ${pkgs.coreutils}/bin/mkdir -p -- "${serverCache}"
  '';

  version = lib.getVersion cfg.llamaCpp.package;

  ttsConcurrency = effectiveInteger "UPSTREAM_TTSCONCURRENCY";
  transcriptions = effectiveInteger "REQUEST_MAXCONCURRENT";

  # nixpkgs' unit passes --listen=<address>:<port>, which needs brackets
  # around IPv6; a zone (%) would not survive into the service's URL.
  listen = swap.listenAddress;
  listenUsable = !(lib.hasInfix ":" listen) || (lib.hasPrefix "[" listen && lib.hasSuffix "]" listen && !(lib.hasInfix "%" listen));
  layoutWayOut = "Set ${groupOpt} to match, or ${opt}.llamaSwap.group.enable = false.";
in {
  options.services.llama-swap-voice = {
    llamaSwap = {
      enable =
        lib.mkEnableOption ''
          the Qwen3 ASR and TTS entries in services.llama-swap.settings, and
          upstreamURL pointing at that llama-swap. Turn it off when llama-swap
          runs elsewhere''
        // {default = true;};

      group = {
        enable =
          lib.mkEnableOption ''
            a llama-swap group holding the ASR and TTS entries, so loading
            one never stops the other; group.sharing decides how they share
            llama-swap with other models. Without it they fall into
            llama-swap's implicit default group, where loading one stops the
            other and loading a model from any exclusive group stops both,
            whatever their ttl''
          // {default = true;};
        sharing = mkOption {
          type = types.enum (lib.attrNames sharingModes);
          default = "pinned";
          description = ''
            How the ASR and TTS entries share llama-swap with other models;
            it has effect only with group.enable. Either way the two entries
            never stop each other.

            "pinned" writes the group with exclusive = false and
            persistent = true: loading another model never stops the entries
            and loading them never stops another model. Only a ttl above 0,
            or an unload through llama-swap's API, stops them.

            "takeTurns" writes exclusive = true and persistent = false, for a
            GPU that cannot hold the voice models beside a large model.
            Loading a model from an exclusive group, which includes
            llama-swap's implicit default group of ungrouped models, stops
            both entries. Loading either entry stops every running model of
            every group that is not persistent, the default group and
            non-exclusive groups included. llama-swap preloads in order, so a
            later preload can stop an earlier one.
          '';
        };
        name = mkOption {
          type = types.str;
          default = "voice";
          description = "Name of the group.";
        };
        location = mkOption {
          type = types.enum ["legacy" "routing"];
          default = "legacy";
          description = ''
            Where the group goes: legacy writes the top-level groups key,
            routing writes routing.router.settings.groups. llama-swap refuses
            a configuration that uses both, so this must match the layout of
            the rest of services.llama-swap.settings.
          '';
        };
      };
    };

    models = {
      asr = entryOptions {
        what = "Qwen3 ASR";
        name = "qwen3-asr";
        pins = pinned.asr;
        concurrencyLimit = {
          default = transcriptions;
          text = lib.literalMD "the service's transcription limit: `request.maxConcurrent`, or `REQUEST_MAXCONCURRENT` from `extraEnvironment`, else 4";
        };
      };
      tts = entryOptions {
        what = "Qwen3 TTS (llamacpp backend only)";
        name = "qwen3-tts";
        pins = pinned.tts;
        concurrencyLimit.default = 5;
      };
    };

    llamaCpp = {
      backend = mkOption {
        type = types.enum ["cpu" "cuda" "vulkan" "rocm"];
        default = "cpu";
        description = ''
          GPU backend of the llama.cpp that runs the ASR entry, and the TTS
          entry under llamacpp. It picks the default package and, for cuda,
          relaxes llama-swap's unit: MemoryDenyWriteExecute off and only the
          NVIDIA devices allowed. That affects every model llama-swap runs.
        '';
      };
      package = mkOption {
        type = types.package;
        default = llamaCppFor cfg.llamaCpp.backend;
        defaultText = lib.literalMD "`pkgs.llama-cpp` with `cudaSupport`, `rocmSupport` and `vulkanSupport` each true only for the selected backend";
        description = "The llama.cpp providing llama-server; version ${minLlamaCppVersion} or newer. Its compiled backend must match llamaCpp.backend, which sets the unit's hardening.";
      };
    };

    tts.qwentts = {
      package = mkOption {
        type = types.package;
        default = flakePackage "qwen-tts-server";
        defaultText = lib.literalExpression "llama-swap-voice.packages.\${system}.qwen-tts-server";
        description = "The faster-qwen3-tts server.";
      };
      model = mkOption {
        type = types.path;
        default = flakePackage "qwen3-tts-model";
        defaultText = lib.literalExpression "llama-swap-voice.packages.\${system}.qwen3-tts-model";
        description = "Directory holding the Qwen3-TTS weights in the Hugging Face layout.";
      };
      modelName = mkOption {
        type = types.nonEmptyStr;
        default = "qwen3-tts";
        description = "Model name of the TTS entry in llama-swap.";
      };
      language = mkOption {
        type = types.str;
        default = "English";
        description = "Language of every reply.";
      };
      chunkSize = mkOption {
        type = types.ints.positive;
        default = 12;
        description = "Codec steps per streamed chunk; 12 is about one second of audio.";
      };
      queueSize = mkOption {
        type = types.ints.positive;
        default = 8;
        description = "Requests the server holds besides the one generating.";
      };
      queueWaitSeconds = mkOption {
        type = types.numbers.positive;
        default = 60;
        description = "Longest a request waits for its turn, in seconds.";
      };
      maxSegmentChars = mkOption {
        type = types.ints.positive;
        default = 400;
        description = "Longest segment the text is split into.";
      };
      maxNewTokens = mkOption {
        type = types.ints.positive;
        default = 600;
        description = "Codec steps allowed per segment.";
      };
      ttl = mkOption {
        type = types.ints.unsigned;
        default = 0;
        description = ttlDescription "server";
      };
      preload = mkOption {
        type = types.bool;
        default = true;
        description = "Load the server when llama-swap starts (hooks.on_startup.preload).";
      };
      concurrencyLimit = mkOption {
        type = types.ints.positive;
        default = qwen.queueSize;
        defaultText = lib.literalExpression "queueSize";
        description = "Requests llama-swap passes to the server at once (concurrencyLimit); llama-swap answers 429 to requests beyond it.";
      };
      cudaDevice = mkOption {
        type = types.str;
        default = "0";
        description = "CUDA_VISIBLE_DEVICES for the server, in PCI bus order.";
      };
      extraArgs = mkOption {
        type = types.listOf types.str;
        default = [];
        description = "Extra qwen-tts-server arguments, appended to the command.";
      };
    };
  };

  config = lib.mkIf (cfg.enable && cfg.llamaSwap.enable) {
    assertions = [
      {
        assertion = swap.enable;
        message = "${opt}.llamaSwap.enable is true, so set services.llama-swap.enable = true; or set ${opt}.llamaSwap.enable = false when llama-swap runs elsewhere.";
      }
      {
        assertion = lib.versionAtLeast version minLlamaCppVersion;
        message = "${opt}.llamaCpp.package is llama.cpp ${version}; the Qwen3 entries need ${minLlamaCppVersion} or newer.";
      }
      {
        assertion = tts.concurrencyLimit >= ttsConcurrency;
        message = "${tts.option} is ${toString tts.concurrencyLimit}, which must be at least the speech requests the service sends at once, ${opt}.upstream.ttsConcurrency (UPSTREAM_TTSCONCURRENCY = ${toString ttsConcurrency}); llama-swap answers 429 to the rest.";
      }
      {
        assertion = cfg.models.asr.concurrencyLimit >= transcriptions;
        message = "${opt}.models.asr.concurrencyLimit is ${toString cfg.models.asr.concurrencyLimit}, which must be at least the transcriptions the service admits, ${opt}.request.maxConcurrent (REQUEST_MAXCONCURRENT = ${toString transcriptions}); llama-swap answers 429 to the rest.";
      }
      {
        assertion = (swap.settings.apiKeys or []) == [];
        message = "${opt}.llamaSwap.enable is true, but services.llama-swap.settings.apiKeys is set, and the service cannot send a llama-swap API key yet, so every voice request would get 401. Remove apiKeys, or set ${opt}.llamaSwap.enable = false and point upstreamURL at a llama-swap without keys.";
      }
      {
        assertion = listenUsable;
        message = "${opt}.llamaSwap.enable is true, so services.llama-swap.listenAddress, \"${listen}\", must put an IPv6 address in brackets, such as \"[::1]\", without a zone (%).";
      }
      {
        assertion = asrName != ttsModelName;
        message = "${opt}.models.asr.name and the TTS entry's name are both \"${asrName}\"; llama-swap needs two entries.";
      }
      {
        assertion = group.enable && group.location == "legacy" -> !usesRouter;
        message = "${groupOpt} is \"legacy\", but services.llama-swap.settings uses the routing layout (routing.router); llama-swap refuses both together. ${layoutWayOut}";
      }
      {
        assertion = group.enable && group.location == "legacy" -> !topLevelMatrix;
        message = "${groupOpt} is \"legacy\", but services.llama-swap.settings has a top-level matrix; llama-swap refuses groups and a matrix together. Set ${opt}.llamaSwap.group.enable = false.";
      }
      {
        assertion = group.enable && group.location == "routing" -> !(topLevelGroups || topLevelMatrix);
        message = "${groupOpt} is \"routing\", but services.llama-swap.settings uses the legacy layout (top-level groups or matrix); llama-swap refuses both together. ${layoutWayOut}";
      }
      {
        assertion = group.enable && group.location == "routing" -> lib.elem (router.use or "") ["" "group"];
        message = "${groupOpt} is \"routing\", but services.llama-swap.settings.routing.router.use is \"${toString (router.use or "")}\"; the group takes effect only under the group router. Set ${opt}.llamaSwap.group.enable = false.";
      }
    ];

    services.llama-swap.settings = lib.mkMerge [
      # Two definitions, so equal names fail the assertion below rather than
      # aborting evaluation of the whole attribute set.
      {models.${asrName} = llamaServerEntry cfg.models.asr ["--temp 0"];}
      {models.${ttsModelName} = tts.entry;}
      (lib.mkIf (preloaded != []) {hooks.on_startup.preload = preloaded;})
      (lib.mkIf (group.enable && group.location == "legacy") {groups.${group.name} = voiceGroup;})
      (lib.mkIf (group.enable && group.location == "routing") {routing.router.settings.groups.${group.name} = voiceGroup;})
    ];

    systemd.services.llama-swap = lib.mkIf swap.enable (lib.mkMerge [
      (lib.mkIf usesCuda {
        # Identical cards otherwise enumerate in a different order in CUDA
        # than in nvidia-smi, so a device index would pick the wrong one.
        environment.CUDA_DEVICE_ORDER = "PCI_BUS_ID";
        serviceConfig = {
          MemoryDenyWriteExecute = lib.mkForce false;
          DeviceAllow = nvidiaDevices ++ otherGPUDevices.${cfg.llamaCpp.backend} or [];
        };
      })
      (lib.mkIf qwentts {
        serviceConfig = {
          # The server reads each clip through the voices directory.
          SupplementaryGroups = [cfg.group];
          ExecStartPre = [cacheRoot];
        };
      })
    ]);
  };
}
