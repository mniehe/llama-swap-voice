{
  pkgs,
  go,
  version,
  package,
}: let
  inherit (pkgs) lib;

  modules = (lib.importTOML ../gomod2nix.toml).mod;

  # A check built as a gomod2nix application gets every module from the
  # vendor set, so it runs offline in the sandbox.
  goCheck = {
    name,
    tools ? [],
    command,
  }:
    pkgs.buildGoApplication {
      pname = "llama-swap-voice-${name}";
      version = "check";
      src = ../.;
      pwd = ../.;

      CGO_ENABLED = "1";
      nativeBuildInputs = [pkgs.pkg-config] ++ tools;
      buildInputs = [pkgs.libopus];

      dontBuild = true;
      doCheck = true;
      checkPhase = ''
        runHook preCheck
        ${command}
        runHook postCheck
      '';
      installPhase = "touch $out";
    };

  sourceCheck = name: tools: command:
    pkgs.runCommand "llama-swap-voice-${name}" {nativeBuildInputs = tools;} ''
      cd ${../.}
      ${command}
      touch $out
    '';

  rfc6716 = pkgs.fetchurl {
    url = "https://www.rfc-editor.org/rfc/rfc6716.txt";
    sha256 = "41caac5240a4a22661efd0031d5b7aee48f3c0bde3b2cdcee8165932e485f98c";
  };

  rfc8251Patch = pkgs.fetchurl {
    url = "https://www.ietf.org/proceedings/98/slides/materials-98-codec-opus-update-00.patch";
    sha256 = "2b06d9a981b1feeb98348d981c1e326658f842fff9abc0cf6afbd62a916b1f02";
  };

  rfc6716Vectors = pkgs.fetchurl {
    url = "https://opus-codec.org/static/testvectors/opus_testvectors.tar.gz";
    sha256 = "94ac78ca4f74c4e43bc9fe4ec1ad0aa36f38ab90f45b0727c40dd1e96096e767";
  };

  rfc8251Vectors = pkgs.fetchurl {
    url = "https://opus-codec.org/static/testvectors/opus_testvectors-rfc8251.tar.gz";
    sha256 = "6b26a22f9ba87b2b836906a9bb7afec5f8e54d49553b1200382520ee6fedfa55";
  };

  # RFC 6716 Appendix A carries the reference decoder as base64 lines that
  # start with "###"; RFC 8251 patches it.
  opusReference = pkgs.runCommand "opus-rfc6716-reference" {} ''
    grep '^   ###' ${rfc6716} | sed -e 's/...###//' | base64 --decode > opus-rfc6716.tar.gz
    echo "2282d2aa4677562fce0602849e9be3df760edc14a08b39fa0cb7c861f8c7fd39  opus-rfc6716.tar.gz" | sha256sum -c
    mkdir $out
    tar -xzf opus-rfc6716.tar.gz -C $out --strip-components=1
    patch -d $out -p1 < ${rfc8251Patch}
  '';

  # The layout pion/opus's conformance test reads: <root>/rfc6716 and
  # <root>/rfc8251, each holding the flat testvector*.bit and *.dec files.
  opusVectors = pkgs.runCommand "opus-testvectors" {} ''
    extract() {
      archive=$1 dest=$2 want=$3
      unpacked=$(mktemp -d)
      tar -xzf "$archive" -C "$unpacked"
      mkdir -p "$dest"
      find "$unpacked" -type f \( -name 'testvector*.bit' -o -name 'testvector*.dec' \) -exec cp {} "$dest" \;
      found=$(find "$dest" -type f | wc -l)
      if [ "$found" != "$want" ]; then
        echo "expected $want vector files in $dest, found $found" >&2
        exit 1
      fi
    }
    extract ${rfc6716Vectors} $out/rfc6716 24
    extract ${rfc8251Vectors} $out/rfc8251 36
  '';
in {
  tests = goCheck {
    name = "tests";
    tools = [pkgs.opus-tools pkgs.ffmpeg-headless];
    command = ''
      # The tests read assets by path, which -trimpath would hide.
      export GOFLAGS=''${GOFLAGS//-trimpath/}
      set -o pipefail
      go test -v -race ./... 2>&1 | tee $TMPDIR/go-test.log

      # A test that cannot find its oracle skips, and then a broken encoder
      # passes; no skip is expected here.
      if grep -F -- '--- SKIP' $TMPDIR/go-test.log; then
        echo "a test skipped; every oracle must be present" >&2
        exit 1
      fi
    '';
  };

  lint = goCheck {
    name = "lint";
    tools = [pkgs.golangci-lint];
    command = ''
      export GOLANGCI_LINT_CACHE=$TMPDIR/golangci-lint-cache
      golangci-lint run ./...
    '';
  };

  gofmt = sourceCheck "gofmt" [go] ''
    unformatted=$(gofmt -l .)
    if [ -n "$unformatted" ]; then
      echo "gofmt would change:" >&2
      echo "$unformatted" >&2
      exit 1
    fi
  '';

  alejandra = sourceCheck "alejandra" [pkgs.alejandra] ''
    alejandra --check .
  '';

  # nix flake check only evaluates packages, so this builds the binary and
  # starts it to prove the build stamps .version into it.
  package = pkgs.runCommand "llama-swap-voice-package" {} ''
    mkdir voices
    cp ${../internal/audio/testdata/speech.wav} voices/sample.wav

    LLAMA_SWAP_VOICE_VOICES_DIR=$PWD/voices \
    LLAMA_SWAP_VOICE_VOICES_DEFAULT=sample \
    LLAMA_SWAP_VOICE_HOST=127.0.0.1 \
    LLAMA_SWAP_VOICE_PORT=18920 \
      timeout 30 ${lib.getExe package} > startup.log 2>&1 &
    pid=$!

    for _ in $(seq 100); do
      if grep -q '"msg":"server starting"' startup.log || ! kill -0 $pid 2> /dev/null; then
        break
      fi
      sleep 0.1
    done
    kill -TERM $pid 2> /dev/null || true
    wait $pid || true

    cat startup.log
    if ! grep -F '"msg":"server starting"' startup.log | grep -qF '"version":"${version}"'; then
      echo "the startup log does not carry version ${version}" >&2
      exit 1
    fi
    touch $out
  '';

  # gomod2nix builds from gomod2nix.toml and skips Go's vendor consistency
  # check, so a go.mod edit without `gomod2nix generate` would build and
  # test the old modules and stay green.
  gomod2nix-sync =
    pkgs.runCommand "llama-swap-voice-gomod2nix-sync" {
      nativeBuildInputs = [go pkgs.python3];
      src = lib.fileset.toSource {
        root = ../.;
        fileset = lib.fileset.unions [../go.mod ../gomod2nix.toml];
      };
    } ''
      export HOME=$TMPDIR GOTOOLCHAIN=local GOFLAGS= GOPROXY=off
      go mod edit -json $src/go.mod > go.mod.json
      python3 -I - go.mod.json $src/gomod2nix.toml <<'EOF'
      import json, sys, tomllib

      with open(sys.argv[1]) as f:
          gomod = json.load(f)
      with open(sys.argv[2], "rb") as f:
          toml = tomllib.load(f)

      required = {r["Path"]: r["Version"] for r in gomod.get("Require") or []}
      pinned = {path: mod["version"] for path, mod in toml.get("mod", {}).items()}

      replaces = [r["Old"]["Path"] for r in gomod.get("Replace") or []]
      differing = [p for p in sorted(required.keys() | pinned.keys()) if required.get(p) != pinned.get(p)]

      for path in replaces:
          print(f"go.mod replaces {path}; gomod2nix-sync does not support replace", file=sys.stderr)
      for path in differing:
          print(f"{path}: go.mod {required.get(path, 'absent')}, gomod2nix.toml {pinned.get(path, 'absent')}", file=sys.stderr)
      if differing:
          print("run: nix develop -c gomod2nix generate", file=sys.stderr)
      if replaces or differing:
          sys.exit(1)
      print(f"{len(required)} modules agree")
      EOF
      touch $out
    '';

  # Runs pion/opus's own RFC 6716 / RFC 8251 decoder conformance test against
  # the revision gomod2nix.toml pins, taken from the vendor set. The source is
  # only go.mod and go.sum, so the check reruns when a dependency moves and
  # not when the service's own code does.
  opus-conformance = pkgs.buildGoApplication rec {
    pname = "pion-opus-conformance";
    version = modules."github.com/pion/opus".version;
    # pwd ends up in the derivation's environment, so it must be the
    # filtered source too, or every repository edit reruns the check.
    pwd = lib.fileset.toSource {
      root = ../.;
      fileset = lib.fileset.unions [../go.mod ../go.sum ../gomod2nix.toml];
    };
    src = pwd;

    CGO_ENABLED = "0";

    dontBuild = true;
    doCheck = true;
    checkPhase = ''
      runHook preCheck

      # The test copies the reference tree and builds it with make, which
      # needs a writable copy rather than the read-only store path.
      cp -R ${opusReference} $TMPDIR/reference
      chmod -R u+w $TMPDIR/reference
      export OPUS_RFC6716_REFERENCE=$TMPDIR/reference
      export OPUS_RFC6716_TESTVECTORS=${opusVectors}

      mkdir -p $out
      export OPUS_CONFORMANCE_MARKDOWN=$out/matrix.md
      set -o pipefail
      go test -v -count=1 -vet=off -timeout 60m -tags conformance \
        -run '^TestRFC6716Conformance$' github.com/pion/opus 2>&1 |
        tee $TMPDIR/go-test.log

      # go test exits 0 when the test skips or nothing matches -run.
      if ! grep -q '^--- PASS: TestRFC6716Conformance ' $TMPDIR/go-test.log ||
        grep -F -e '--- SKIP' -e 'no tests to run' $TMPDIR/go-test.log; then
        echo "TestRFC6716Conformance did not run and pass" >&2
        exit 1
      fi

      runHook postCheck
    '';
    installPhase = "true";
  };
}
