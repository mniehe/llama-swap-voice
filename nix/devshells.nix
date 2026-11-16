{
  pkgs,
  go,
}: {
  default = pkgs.mkShell {
    name = "llama-swap-voice";

    packages = with pkgs; [
      go
      gopls
      golangci-lint
      pkg-config

      # Test oracles: the tests skip their checks when these are missing.
      opus-tools
      ffmpeg-headless

      gomod2nix
      cocogitto
      alejandra
    ];

    buildInputs = [pkgs.libopus];

    CGO_ENABLED = "1";
  };
}
