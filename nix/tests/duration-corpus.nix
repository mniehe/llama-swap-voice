{
  pkgs,
  module,
  go,
}: let
  # The converter travels on the duration option's type, so the check reads
  # the module's own function rather than a copy.
  duration = (pkgs.nixos [module]).options.services.llama-swap-voice.shutdown.grace.type.nestedTypes.elemType;
  rows = map (input: {
    inherit input;
    nix = duration.toNanoseconds input;
  }) (import ./durations.nix);
in
  pkgs.runCommand "llama-swap-voice-duration-corpus" {
    nativeBuildInputs = [go];
    rows = builtins.toJSON rows;
    passAsFile = ["rows"];
  } ''
    export HOME=$TMPDIR GOCACHE=$TMPDIR/go-cache GOTOOLCHAIN=local GOFLAGS= GOPROXY=off
    set -o pipefail
    # Under testdata, the program stays outside the service module's ./...
    go run ${./testdata/parse-duration.go} $rowsPath | tee $out
  ''
