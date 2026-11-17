# The GGUF artifacts the module's llama.cpp entries default to, pinned to a
# Hugging Face commit with a hash per file.
let
  artifact = repo: rev: file: hash: {
    url = "https://huggingface.co/${repo}/resolve/${rev}/${file}";
    inherit hash;
  };

  asr = artifact "ggml-org/Qwen3-ASR-1.7B-GGUF" "36a678687ba7d07a74ca70ccb0e36902e005fb80";
  tts = artifact "ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF" "ca27d74bc954b73dadab5b71ca265d87fc861a7c";
in {
  asr = {
    model = asr "Qwen3-ASR-1.7B-Q8_0.gguf" "sha256-WOItBTLU6srwNM+sF6b+0VnzfEE5DHEBhng75DnR/Fc=";
    mmproj = asr "mmproj-Qwen3-ASR-1.7B-Q8_0.gguf" "sha256-RsHVM68/NUzrN86FXbzv99p/p88ealI987E0QL0WTA0=";
  };
  tts = {
    model = tts "Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf" "sha256-rHkxrrLnqtGm7WYC01OlZ5ydCWsYzoIErHMKhAjVcuE=";
    mmproj = tts "mmproj-Qwen3-TTS-12Hz-1.7B-Base-Q8_0.gguf" "sha256-b9ZRiIObzW7MkbJ3rUceIqDt+tpGmaD+gvEWXBjPzOI=";
  };
}
