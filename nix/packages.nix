{
  pkgs,
  version,
  inputs,
}:
{
  default = pkgs.buildGoApplication {
    pname = "llama-swap-voice";
    inherit version;
    src = ../.;
    pwd = ../.;

    subPackages = ["cmd/llama-swap-voice"];
    ldflags = ["-s" "-w" "-X main.Version=${version}"];

    # The Opus encoder links libopus through cgo.
    CGO_ENABLED = "1";
    nativeBuildInputs = [pkgs.pkg-config];
    buildInputs = [pkgs.libopus];

    # The tests run as the flake's tests check.
    doCheck = false;

    meta = {
      description = "OpenAI audio API (speech and transcriptions) in front of llama-swap";
      license = pkgs.lib.licenses.mit;
      mainProgram = "llama-swap-voice";
    };
  };
}
# The TTS server's lock resolves CUDA wheels for x86_64 Linux only.
// pkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") rec {
  qwen-tts-server = pkgs.callPackage ./qwen-tts-server {inherit inputs;};
  qwen3-tts-model = qwen-tts-server.model;
}
