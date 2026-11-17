# The faster-qwen3-tts server's configuration, in the shape
# python/qwen_tts_server/config.py reads: every key present, transcript and
# instruct null when unset.
{
  lib,
  cfg,
  linkName,
  maxInputChars,
}: let
  qwen = cfg.tts.qwentts;

  voice = name: voice: {
    clip = linkName name voice;
    inherit (voice) transcript instruct temperature;
    top_k = voice.topK;
    top_p = voice.topP;
    repetition_penalty = voice.repetitionPenalty;
  };
in {
  model_dir = "${qwen.model}";
  inherit (qwen) language;
  chunk_size = qwen.chunkSize;
  queue_size = qwen.queueSize;
  queue_wait_seconds = qwen.queueWaitSeconds;
  max_input_chars = maxInputChars;
  max_segment_chars = qwen.maxSegmentChars;
  max_new_tokens = qwen.maxNewTokens;
  voices = lib.mapAttrs voice cfg.voices;
}
