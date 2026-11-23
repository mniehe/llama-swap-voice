{
  pkgs,
  module,
  go,
}:
{
  duration-corpus = import ./duration-corpus.nix {inherit pkgs module go;};
  module-eval = import ./eval.nix {inherit pkgs module;};
  module-vm = import ./vm.nix {inherit pkgs module;};
  module-llama-swap-vm = import ./llama-swap-vm.nix {inherit pkgs module;};
  module-wyoming-vm = import ./wyoming-vm.nix {inherit pkgs module;};
  module-model-pins = import ./model-pins.nix {inherit pkgs module;};
}
# The server's test environment, which this check borrows, exists only where
# the server builds.
// pkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") {
  module-qwen-tts-config = import ./qwen-tts-config.nix {inherit pkgs module;};
}
