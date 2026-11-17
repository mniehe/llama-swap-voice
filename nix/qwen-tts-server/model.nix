{
  lib,
  fetchurl,
  linkFarm,
}: let
  repo = "Qwen/Qwen3-TTS-12Hz-1.7B-Base";
  rev = "fd4b254389122332181a7c3db7f27e918eec64e3";

  hashes = {
    "config.json" = "sha256-tPAXUtFaSIq94+GrRHI65PS55opANyV7CYs3N4k8wfk=";
    "generation_config.json" = "sha256-8bkLRRPzs0xihRBJ4kkte0xZQNrxJ2+JyCuO8EEn86o=";
    "merges.txt" = "sha256-WZurVAdQiHdLFzP96GXVvXR8vMelR8W8EmEOh04m9eM=";
    "model.safetensors" = "sha256-OPx/xRxed26EBBS2/UQ5YulBG5ZUiI/XkT5NpkPLhXw=";
    "preprocessor_config.json" = "sha256-793hAi6p12kov3qc1TE5E49bouRm6Dfwj2EFqxrxwRk=";
    "tokenizer_config.json" = "sha256-3Dwxw72u3VAWOCuzy+BzIwJnda1R9aT7VkUFmSrkpnA=";
    "vocab.json" = "sha256-yhDX6fs+0YV13R4neiV5wW0QjjLydDloSvoOELFECRA=";
    "speech_tokenizer/config.json" = "sha256-7mW7kByHZmSrhwfEhxV6oabuV8ZZabKPteydwhHmgWc=";
    "speech_tokenizer/configuration.json" = "sha256-a8JtZOtQJLTR2rWlI3GVi0KSVtbJ1ZeH8fUpSlTgzr0=";
    "speech_tokenizer/model.safetensors" = "sha256-g2t7NX9epD6ImTajcJr2jf43UYgazv5Ozw29MLpXElg=";
    "speech_tokenizer/preprocessor_config.json" = "sha256-/LOAXll+eG1AZ3BuYC9miFJGQPjTOWeQ4uCbWUL8vfs=";
  };

  fetch = file: hash:
    fetchurl {
      name = lib.replaceStrings ["/"] ["-"] file;
      url = "https://huggingface.co/${repo}/resolve/${rev}/${file}";
      inherit hash;
    };

  files = lib.mapAttrs fetch hashes;
in
  (linkFarm "qwen3-tts-12hz-1.7b-base" files).overrideAttrs {
    passthru = {inherit files repo rev;};
  }
