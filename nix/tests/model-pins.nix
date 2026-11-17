# Checked at evaluation only: the output records strings without their
# context, so building it fetches no GGUF.
{
  pkgs,
  module,
}: let
  inherit (pkgs) lib;
  pinned = import ../models.nix;

  evaluated = pkgs.nixos [
    module
    {
      boot.loader.grub.enable = false;
      fileSystems."/" = {
        device = "none";
        fsType = "tmpfs";
      };
      system.stateVersion = lib.trivial.release;
      services.llama-swap.enable = true;
      services.llama-swap-voice = {
        enable = true;
        defaultVoice = "sample";
        voices.sample.clip = "/srv/clips/sample.wav";
      };
    }
  ];

  inherit (evaluated.config.services.llama-swap-voice) models;
  inherit (evaluated.config.services.llama-swap.settings.models) qwen3-asr qwen3-tts;
  commands = {
    asr = qwen3-asr.cmd;
    tts = qwen3-tts.cmd;
  };

  plain = builtins.unsafeDiscardStringContext;
  urlPattern = "https://huggingface[.]co/[^/]+/[^/]+/resolve/[0-9a-f]{40}/[^/]+[.]gguf";
  hashPattern = "sha256-[A-Za-z0-9+/]{43}=";

  artifactProblems = entry: kind: let
    name = "models.${entry}.artifacts.${kind}";
    artifact = models.${entry}.artifacts.${kind};
    want = pinned.${entry}.${kind};
    fetch = artifact.path;
  in
    lib.optional (builtins.match urlPattern artifact.url == null) "${name}.url ${artifact.url} is not a Hugging Face GGUF at a full commit"
    ++ lib.optional (artifact.url != want.url) "${name}.url ${artifact.url} is not the pinned ${want.url}"
    ++ lib.optional (builtins.match hashPattern artifact.hash == null) "${name}.hash ${artifact.hash} is not a sha256 SRI hash"
    ++ lib.optional (artifact.hash != want.hash) "${name}.hash ${artifact.hash} is not the pinned ${want.hash}"
    ++ lib.optional (artifact.hash == lib.fakeHash) "${name}.hash is lib.fakeHash"
    ++ lib.optional (fetch.urls or null != [artifact.url]) "${name}.path does not fetch ${artifact.url}"
    ++ lib.optional (fetch.outputHash or null != artifact.hash) "${name}.path does not fetch with ${artifact.hash}"
    ++ lib.optional (!lib.hasInfix (plain fetch.outPath) (plain commands.${entry})) "the ${entry} entry's cmd does not name ${name}.path";

  artifacts = lib.cartesianProduct {
    entry = ["asr" "tts"];
    kind = ["model" "mmproj"];
  };
  hashes = map ({
    entry,
    kind,
  }:
    models.${entry}.artifacts.${kind}.hash)
  artifacts;

  problems =
    lib.concatMap ({
      entry,
      kind,
    }:
      artifactProblems entry kind)
    artifacts
    ++ lib.optional (lib.length (lib.unique hashes) != lib.length hashes) "two artifacts share a hash";

  manifest = lib.concatMapStrings ({
    entry,
    kind,
  }: let
    artifact = models.${entry}.artifacts.${kind};
  in "${artifact.hash}  ${artifact.url}\n")
  artifacts;
in
  assert lib.assertMsg (problems == []) (lib.concatStringsSep "\n" problems);
    pkgs.writeText "llama-swap-voice-model-pins" (plain manifest)
