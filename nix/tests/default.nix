{
  pkgs,
  module,
  go,
}: {
  duration-corpus = import ./duration-corpus.nix {inherit pkgs module go;};
  module-eval = import ./eval.nix {inherit pkgs module;};
  module-vm = import ./vm.nix {inherit pkgs module;};
}
