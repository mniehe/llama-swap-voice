{
  lib,
  pkgs,
  callPackage,
  python312,
  runCommand,
  writeText,
  writeShellApplication,
  addDriverRunpath,
  patchelf,
  sox,
  inputs,
}: let
  inherit (inputs) pyproject-nix uv2nix pyproject-build-systems;
  python = python312;
  root = ../../python;

  workspace = uv2nix.lib.workspace.loadWorkspace {
    workspaceRoot = lib.fileset.toSource {
      inherit root;
      fileset = lib.fileset.unions [(root + "/pyproject.toml") (root + "/uv.lock")];
    };
  };

  pythonSet = (callPackage pyproject-nix.build.packages {inherit python;}).overrideScope (
    lib.composeManyExtensions [
      pyproject-build-systems.overlays.wheel
      (workspace.mkPyprojectOverlay {sourcePreference = "wheel";})
      (import ./overrides.nix {inherit pkgs python;})
    ]
  );

  # No GPU in the sandbox, but cudnnGetVersion still goes through libcudnn.so.9's
  # dlopen of its sub-libraries, which is what fails when $ORIGIN is missing.
  env = (pythonSet.mkVirtualEnv "qwen-tts-server-env" {qwen-tts-server = ["engine"];}).overrideAttrs {
    doInstallCheck = true;
    nativeInstallCheckInputs = [patchelf];
    installCheckPhase = ''
      runHook preInstallCheck

      missing=0
      while IFS= read -r lib; do
        if [[ "$(patchelf --print-rpath "$lib")" != *'$ORIGIN'* ]]; then
          echo "RUNPATH lacks \$ORIGIN: $lib"
          missing=1
        fi
      done < <(find -L "$out/${python.sitePackages}/nvidia" -type f \( -name '*.so' -o -name '*.so.*' \))
      [ "$missing" = 0 ]

      HOME="$TMPDIR" "$out/bin/python" -c "import torch; version = torch.backends.cudnn.version(); assert version, version; print('cudnn', version)"

      runHook postInstallCheck
    '';
  };

  # The base dependencies and the dev group: no torch, so the checks stay small.
  testEnv = pythonSet.mkVirtualEnv "qwen-tts-server-test-env" {qwen-tts-server = ["dev"];};
  engineTestEnv = pythonSet.mkVirtualEnv "qwen-tts-server-engine-test-env" {qwen-tts-server = ["engine" "dev"];};
  model = callPackage ./model.nix {};

  src = lib.fileset.toSource {
    inherit root;
    fileset = root + "/qwen_tts_server";
  };

  testSrc = lib.fileset.toSource {
    inherit root;
    fileset = lib.fileset.unions [(root + "/qwen_tts_server") (root + "/tests")];
  };

  # The one test that needs torch: it builds the speech tokenizer's encoder.
  tokenizerTest = "tests/test_tokenizer.py";

  tests = runCommand "qwen-tts-server-tests" {} ''
    export HOME="$TMPDIR" HF_HUB_OFFLINE=1
    cd ${testSrc}
    ${testEnv}/bin/python -m pytest -q -p no:cacheprovider --ignore=${tokenizerTest} tests
    touch $out
  '';

  tokenizer-test =
    runCommand "qwen-tts-server-tokenizer-test" {
      QWEN_TTS_TOKENIZER_CONFIG = model.files."speech_tokenizer/config.json";
    } ''
      export HOME="$TMPDIR" HF_HUB_OFFLINE=1
      cd ${testSrc}
      ${engineTestEnv}/bin/python -m pytest -q -p no:cacheprovider ${tokenizerTest}
      touch $out
    '';

  # Files the loader reads from the model directory, in the Hugging Face layout.
  modelFiles = [
    "config.json"
    "generation_config.json"
    "merges.txt"
    "model.safetensors"
    "preprocessor_config.json"
    "speech_tokenizer/config.json"
    "speech_tokenizer/configuration.json"
    "speech_tokenizer/model.safetensors"
    "speech_tokenizer/preprocessor_config.json"
    "tokenizer_config.json"
    "vocab.json"
  ];

  # Checked at evaluation only: the output records strings, not the fetches,
  # so building it downloads no weights.
  model-pin = let
    inherit (model) repo rev files;
    url = file: "https://huggingface.co/${repo}/resolve/${rev}/${file}";
    fileProblems = file: fetch:
      lib.optional (fetch.urls != [(url file)]) "${file}: url ${toString fetch.urls} is not ${url file}"
      ++ lib.optional (builtins.match "sha256-[A-Za-z0-9+/]{43}=" fetch.outputHash == null) "${file}: ${fetch.outputHash} is not a sha256 SRI hash"
      ++ lib.optional (fetch.outputHash == lib.fakeHash) "${file}: hash is lib.fakeHash";
    problems =
      lib.optional (builtins.match "[0-9a-f]{40}" rev == null) "revision ${rev} is not a full commit hash"
      ++ lib.optional (lib.attrNames files != modelFiles) "files ${toString (lib.attrNames files)} are not ${toString modelFiles}"
      ++ lib.concatLists (lib.mapAttrsToList fileProblems files);
    manifest = lib.concatMapStrings (file: "${files.${file}.outputHash}  ${url file}\n") (lib.attrNames files);
  in
    assert lib.assertMsg (problems == []) (lib.concatStringsSep "\n" problems);
      writeText "qwen3-tts-model-pin" manifest;
in
  writeShellApplication {
    name = "qwen-tts-server";
    runtimeInputs = [sox];
    text = ''
      cache="''${XDG_CACHE_HOME:-/var/cache/llama-swap}/qwen-tts-server"
      export HOME="$cache" HF_HOME="$cache/hf" TRITON_CACHE_DIR="$cache/triton" TORCHINDUCTOR_CACHE_DIR="$cache/inductor"
      export HF_HUB_OFFLINE=1 LD_LIBRARY_PATH=${addDriverRunpath.driverLink}/lib
      export PYTHONPATH=${src}
      exec ${env}/bin/python -m qwen_tts_server "$@"
    '';
    passthru = {
      inherit env model testEnv tests tokenizer-test;
      checks = {
        qwen-tts-server-tests = tests;
        qwen3-tts-model-pin = model-pin;
      };
    };
  }
