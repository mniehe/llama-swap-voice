# Fake model servers and artifacts, so no test fetches a GGUF or the weights.
{pkgs}: let
  reply = ../../internal/audio/testdata/speech.wav;
  script = ./fake-model-server.py;
  configModule = ../../python/qwen_tts_server/config.py;

  fakeServer = {
    name,
    version,
    args,
  }:
    pkgs.runCommand "fake-${name}-${version}" {
      inherit version;
      meta.mainProgram = name;
    } ''
      mkdir -p $out/bin
      cat > $out/bin/${name} <<EOF
      #!${pkgs.runtimeShell}
      exec ${pkgs.python3}/bin/python3 -I ${script} ${args} "\$@"
      EOF
      chmod +x $out/bin/${name}
    '';

  artifact = name: pkgs.writeText name "fake ${name}\n";
in {
  llamaServer = fakeServer {
    name = "llama-server";
    version = "1.0.0";
    args = "llama-server ${reply}";
  };

  # Starts but is too old for the module's version assertion.
  oldLlamaServer = fakeServer {
    name = "llama-server";
    version = "0.7.9";
    args = "llama-server ${reply}";
  };

  qwenTtsServer = fakeServer {
    name = "qwen-tts-server";
    version = "0";
    args = "qwen-tts-server ${reply} ${configModule}";
  };

  artifacts = {
    asr = {
      model.path = artifact "asr-model.gguf";
      mmproj.path = artifact "asr-mmproj.gguf";
    };
    tts = {
      model.path = artifact "tts-model.gguf";
      mmproj.path = artifact "tts-mmproj.gguf";
    };
    # A consumer's own model, outside the module's entries.
    chat = {
      model.path = artifact "chat-model.gguf";
      mmproj.path = artifact "chat-mmproj.gguf";
    };
  };
}
