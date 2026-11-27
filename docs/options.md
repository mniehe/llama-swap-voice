# Module options

This page lists every option of the NixOS module, and explains what the
module writes into llama-swap. For a first setup, start with the
[README](../README.md#quick-start).

All options are under `services.llama-swap-voice`. Related pages:
[the HTTP API](api.md) and [the Qwen TTS server](qwen-tts-server.md).

## Conventions

- `models.{asr,tts}.ttl` means two options, `models.asr.ttl` and
  `models.tts.ttl`.
- A default of `null` leaves the setting to the service. The table then
  gives the service's default in brackets.
- A duration uses Go's syntax, such as `90s` or `1m30s`. It takes no leading
  `+` or `-`. Evaluation reads a duration exactly as the service does. It
  refuses a value above Go's maximum, which is about 292 years.
- Variable names such as `TTS_MAXINPUTCHARS` leave out the prefix
  `LLAMA_SWAP_VOICE_`. See [Environment variables](#environment-variables).

## The service

| Option | Type | Default | Effect |
|---|---|---|---|
| `enable` | boolean | `false` | Runs the service. |
| `defaultVoice` | string | none; you must set it | The voice for a request that names none. It must be a declared voice. |
| `tts.backend` | `"llamacpp"` or `"qwentts"` | `"llamacpp"` | Selects the TTS server. See [Choose a TTS backend](../README.md#choose-a-tts-backend). |
| `host` | string | `"127.0.0.1"` | The address the service listens on. For a non-loopback address, also list the clients in `ipAddressAllow`. |
| `port` | integer | `8920` | The port the service listens on. |
| `upstreamURL` | string | the address of your `services.llama-swap`; `"http://127.0.0.1:8080"` when `llamaSwap.enable` is off | The base URL of llama-swap. Set it when llama-swap runs on another machine. |
| `ipAddressAllow` | list of strings | `[]` | Addresses and prefixes, besides loopback, that the service can exchange packets with. The syntax is that of systemd's `IPAddressAllow=`. |

The sandbox lets the service exchange packets with loopback only. A remote
llama-swap, the DNS resolver that finds it, and each client of a non-loopback
`host` or `wyoming.host` must be in `ipAddressAllow`.

The module derives `upstreamURL` from `services.llama-swap`. A wildcard
listen address, such as `0.0.0.0` or `[::]`, becomes loopback. The URL uses
`https` when `services.llama-swap.tls.enable` is on. The certificate must
then cover the host in the derived URL, or you set `upstreamURL` yourself.

Evaluation checks only part of an `upstreamURL` that you set. The value must
start with `http://` or `https://`, have a host and contain no white space.
The service parses the URL fully at startup.

## Voices

| Option | Type | Default | Effect |
|---|---|---|---|
| `voices` | attribute set | `{}` | The voices, by name. A name starts with a letter, a digit or `_`. It can also contain `.` and `-`. |
| `voices.<name>.clip` | string | none; you must set it | The absolute path of the clip. Write it as a string, so that the clip stays out of the Nix store. |
| `voices.<name>.transcript` | string or `null` | `null` | The exact words of the clip. `null` selects embedding-only cloning. |
| `voices.<name>.instruct` | string or `null` | `null` | A style instruction, such as a mood. It needs a transcript. |
| `voices.<name>.temperature` | number, 0 or more | `0.7` | The sampling temperature. |
| `voices.<name>.topK` | integer, 1 or more | `50` | The top-k sampling limit. |
| `voices.<name>.topP` | number, 0 to 1 | `1.0` | The top-p sampling limit. |
| `voices.<name>.repetitionPenalty` | number above 0 | `1.05` | The repetition penalty. |

Only the `qwentts` backend uses the options after `clip`. The `llamacpp`
backend ignores them.

### The voices directory

The module makes one link for each declared voice,
`<stateDir>/voices/<name>.<extension>`. The clip stays where it is and never
enters the Nix store. The service reads the directory when it starts.

- Keep clips outside `/home`. Neither the service nor llama-swap can read it.
- A clip is a `.wav` or `.mp3` file of at most 10 MiB. A larger clip stops
  the service at startup.
- Every `.wav` or `.mp3` file in the voices directory is a voice, so a file
  that you copy there by hand counts too.
- A symlink in that directory is an alias. With `alloy.mp3 -> sample.wav`, a
  client that asks for OpenAI's `alloy` hears `sample`.
- Restart the service after you add a voice.
- The service follows symlinks in the voices directory wherever they point,
  so let only an administrator write there.
- Use only recordings that you have the right to clone.

The module never removes a link. Delete the old link by hand in three
cases:

- You removed a voice. The voice stays available until its link is gone.
- You changed the path of a clip. The service refuses to start until you
  delete the link and run `systemd-tmpfiles --create`.
- You changed the extension of a clip. The service refuses to start, with
  the message `voice "<name>" is provided by both`.

### Check that a clip has one constant gain

[Prepare a clip](../README.md#prepare-a-clip) normalises a clip with ffmpeg.
`loudnorm` changes to its dynamic mode without a warning when the gain would
push the true peak over the limit. Dynamic mode changes the character of the
voice. To check, scale the source by the gain that the second ffmpeg command
should have applied, then subtract the result from the clip.

```sh
# output_i: the input_i that the measuring command prints for narrator.wav
gain=$(echo "$output_i - ($input_i)" | bc)
ffmpeg -hide_banner -i source.wav -af volume=${gain}dB -ac 1 -ar 24000 -sample_fmt s16 scaled.wav
ffmpeg -hide_banner -nostats -i narrator.wav -i scaled.wav -filter_complex \
  '[0][1]amerge=inputs=2,pan=mono|c0=c0-c1,astats=measure_perchannel=none:measure_overall=RMS_level' -f null -
ffmpeg -hide_banner -nostats -i narrator.wav \
  -af astats=measure_perchannel=none:measure_overall=RMS_level -f null -
```

The first RMS level is the difference and the second is the clip. A
difference tens of dB below the clip means the gain was constant. A
difference within a few dB of the clip means the gain varied. In that case,
lower the peaks of the source or select a quieter target, and normalise
again.

## The models and llama-swap

| Option | Type | Default | Effect |
|---|---|---|---|
| `llamaCpp.backend` | `"cpu"`, `"cuda"`, `"vulkan"` or `"rocm"` | `"cpu"` | Selects the hardware for llama.cpp, and builds llama.cpp for it. |
| `llamaSwap.enable` | boolean | `true` | Adds the two entries to `services.llama-swap.settings` and derives `upstreamURL`. Turn it off when llama-swap runs on another machine. |
| `llamaSwap.group.enable` | boolean | `true` | Puts both entries in a group that keeps them loaded. See [When the models unload](#when-the-models-unload). |
| `llamaSwap.group.sharing` | `"pinned"` or `"takeTurns"` | `"pinned"` | Selects how the group shares the GPU with your other models. `"takeTurns"` lets a chat model and the voice models replace each other. See [When the models unload](#when-the-models-unload). |
| `models.{asr,tts}.name` | string | `"qwen3-asr"`, `"qwen3-tts"` | The name of the entry in llama-swap. The two names must differ. |
| `models.{asr,tts}.device` | string or `null` | `null` | The `--device` argument of `llama-server`, such as `"CUDA0"`. `null` lets llama.cpp select the device. |
| `models.{asr,tts}.gpuLayers` | integer, 0 or more | `99` | The number of layers on the GPU (`-ngl`). |
| `models.{asr,tts}.contextSize` | integer, 1 or more | `4096` | The context size (`-c`). |
| `models.{asr,tts}.parallel` | integer, 1 or more | `1` | The number of requests `llama-server` runs at a time (`-np`). |
| `models.{asr,tts}.ttl` | integer, 0 or more | `0` | The idle time, in seconds, after which llama-swap unloads the entry. `0` turns idle unloading off. |
| `models.{asr,tts}.preload` | boolean | `true` | Loads the entry when llama-swap starts. |

The `models.tts` options apply under the `llamacpp` backend only. Under
`qwentts`, the options in the next table describe the TTS entry.

With `llamaSwap.enable` off, the module leaves `services.llama-swap` alone.
It writes no entries and no configuration for the Qwen TTS server. Add the
models to the remote llama-swap yourself, as
[The same entries in YAML](#the-same-entries-in-yaml) shows.

## The Qwen TTS server

These options apply only when `tts.backend` is `"qwentts"`.

| Option | Type | Default | Effect |
|---|---|---|---|
| `tts.qwentts.language` | string | `"English"` | The language of every reply. |
| `tts.qwentts.cudaDevice` | string | `"0"` | The GPU the server uses (`CUDA_VISIBLE_DEVICES`). The index is in PCI bus order, as `nvidia-smi` shows it. |
| `tts.qwentts.queueSize` | integer, 1 or more | `8` | The number of requests the server holds besides the one it generates. |
| `tts.qwentts.queueWaitSeconds` | number above 0 | `60` | The longest time a request waits for its turn, in seconds. |
| `tts.qwentts.ttl` | integer, 0 or more | `0` | The idle time, in seconds, after which llama-swap unloads the server. `0` turns idle unloading off. |
| `tts.qwentts.preload` | boolean | `true` | Loads the server when llama-swap starts. |

The module writes the server's configuration file from these options and
the declared voices. [The Qwen TTS server](qwen-tts-server.md) describes the
file and the server.

## Limits and timeouts

[Concurrency](#concurrency) explains how the first five options relate.

| Option | Type | Default | Effect |
|---|---|---|---|
| `request.maxConcurrentTTS` | integer or `null` | `null` (8) | Speech requests the service holds at a time, including those that wait. More get a 503. |
| `upstream.ttsConcurrency` | integer, 1 or more | `1` | Speech requests the service sends to the TTS model at a time. |
| `request.maxConcurrent` | integer or `null` | `null` (4) | Transcriptions the service handles at a time. More get a 503. |
| `models.{asr,tts}.concurrencyLimit` | integer, 1 or more | ASR: follows `request.maxConcurrent` (4); TTS: `5` | Requests llama-swap passes to the entry at a time. llama-swap answers 429 to the rest. |
| `tts.qwentts.concurrencyLimit` | integer, 1 or more | the value of `queueSize` | The same limit for the Qwen TTS server's entry. |
| `tts.maxInputChars` | integer or `null` | `null` (4096) | The longest text one speech request can carry, in characters. |
| `request.timeout` | duration or `null` | `null` (`2m`) | The longest time a speech request or a transcription can take, streaming included. |
| `shutdown.grace` | duration or `null` | `null` (`0s`) | Step 1 of [shutdown](api.md#shutdown): how long the service keeps serving after it reports not ready. |
| `shutdown.total` | duration or `null` | `null` (`15s`) | Step 2: how long requests in progress get to finish. |
| `shutdown.hard` | duration or `null` | `null` (`3s`) | Step 3: how long cancelled requests get to stop. |

The module sets the unit's `TimeoutStopSec` to cover all three steps. The
value is the sum of the durations you set, plus 30 seconds, and at least 90
seconds.

## Wyoming

These options run the [Wyoming listener](api.md#wyoming), which Home
Assistant connects to. [Home Assistant](../README.md#home-assistant) in the
README shows a complete setup.

| Option | Type | Default | Effect |
|---|---|---|---|
| `wyoming.enable` | boolean | `false` | Serves the Wyoming protocol on a second TCP port. |
| `wyoming.host` | string | `"127.0.0.1"` | The address the listener binds. Write an IPv6 address without brackets, such as `"::1"`. |
| `wyoming.port` | integer, 1 to 65535 | `10300` | The port the listener binds. |
| `wyoming.languages` | non-empty list of strings | `["en"]` | The languages that `info` lists for the ASR model and for each voice. |
| `wyoming.speed` | number, 0.25 to 4 | `1.0` | The speed of every Wyoming synthesis. The service changes the tempo and keeps the pitch. At 1.0 it leaves the audio as the model made it. |
| `wyoming.streaming` | `"sentences"` or `"whole"` | `"sentences"` | How the service speaks a [streamed synthesis](api.md#streamed-synthesis). `"sentences"` speaks each sentence as it completes and advertises streaming in `info`. `"whole"` speaks the whole text after `synthesize-stop` and advertises no streaming. |
| `wyoming.streamingMaxChars` | integer, 1 or more | `300` | In `"sentences"` mode, the most characters of the sentences that one `synthesize-chunk` completes to join into one request. A longer sentence is spoken whole. |
| `wyoming.openFirewall` | boolean | `false` | Opens `wyoming.port`, and no other port, in the NixOS firewall. The rule follows `wyoming.port` and `wyoming.enable` only: an `extraEnvironment` override of `LLAMA_SWAP_VOICE_WYOMING_PORT` or `LLAMA_SWAP_VOICE_WYOMING_ENABLED` does not change it. |

Home Assistant asks for `info` every 30 seconds and replaces its speech
service when the answer changes. It reads whether the service streams from
that answer. A change to `wyoming.streaming` therefore reaches Home
Assistant within about 30 seconds, without a reload. Home Assistant sorts
the voices by description or name, so the order in `info` does not control
its list. The default voice is the one whose description ends in
`(default)`.

`wyoming.streamingMaxChars` must be at most `tts.maxInputChars`, 4096 by
default. `tts.maxInputChars` also bounds a stream: the text not yet spoken
plus an arriving `synthesize-chunk` may not exceed it.

A stream holds no speech slot. Each sentence takes one of the
`request.maxConcurrentTTS` slots, 8 by default, which HTTP speech requests
share. When every slot is held, a sentence fails with the error `busy`.
Raise `request.maxConcurrentTTS` if many satellites speak at once.

The sandbox applies one `ipAddressAllow` list to the whole unit, so it
cannot tell the two ports apart. An address that you allow for Wyoming can
also reach the HTTP port when `host` is an address that it can reach. Keep
`host` on loopback if only Home Assistant should come in from outside.

Evaluation fails, and names the option, in these cases:

- `wyoming.host` and `wyoming.port` are the same as `host` and `port`.
- `wyoming.host` contains `[` or `]`.
- `wyoming.streamingMaxChars` is above `tts.maxInputChars`. Both values
  are read after `extraEnvironment` is merged.
- `wyoming.languages` is empty, or one of its entries is empty, is only
  whitespace or contains a comma.
- `wyoming.speed` is outside 0.25 to 4, or `wyoming.streaming` is not
  `"sentences"` or `"whole"`.

Except for `wyoming.streamingMaxChars`, these checks read the options, not
`extraEnvironment`. The service checks an override of a Wyoming variable when
it starts, and refuses to start on a bad value. The host and port check
compares the options only: an override that gives both listeners one address
passes evaluation and fails at start, and an override that separates two equal
options does not clear the evaluation error.

## Recordings

This option keeps the audio of transcriptions, so that you can listen to
them and review the transcripts. It is off by default.

| Option | Type | Default | Effect |
|---|---|---|---|
| `recordings.enable` | boolean | `false` | Keeps the audio and transcript of every successful transcription, from the HTTP API and from the Wyoming listener, in `<stateDir>/recordings`. Serves the [recordings page](api.md#recordings) at `/recordings` on the HTTP port. |
| `recordings.allowedHosts` | list of strings | `[]` | Hosts that the recordings page answers besides `127.0.0.1`, `[::1]`, `localhost` and `host`. An entry without a port, such as `voice.example.org`, matches that host on every port; `voice.example.org:8443` matches that port only. An entry may not hold a comma, a slash or a space. |

**Turning this on stores household speech.** Each recording stays on disk
for 7 days. A recording that you mark kept on the page stays until you
release it. The page has no authentication: anyone who can reach the HTTP
port can listen to every recording, change its keep mark and its review, and
edit the speaker profiles.
Keep `host` on
loopback, or put a reverse proxy that checks who is asking in front of the
port.

The page answers only requests whose `Host` header names loopback, `host`
or an entry of `recordings.allowedHosts`, and refuses others with a 403.
This stops a web page in a browser on your network from reaching the
recordings by rebinding its own name to the service's address. Behind a
reverse proxy, make the proxy pass the public `Host` through unchanged and
list that host in `recordings.allowedHosts`; the service never reads
`X-Forwarded-Host`.

The service saves a recording in the background after the transcription
succeeds and it has answered. A failed transcription saves nothing. A save
that fails, for example because the disk is full, is logged as a warning;
the client already has its transcript. Up to 64 MiB may wait to be saved,
counting each recording's audio, its transcript and 512 bytes more; a
recording that would pass that is dropped with a warning. Both warnings
carry the request's `request_id`. On
shutdown the service saves what is waiting until the shutdown budget,
`shutdown.total` plus `shutdown.hard`, has passed, and drops the rest with
a warning.

Each recording is two files with the same name: `<id>.opus`, the audio that
was sent to the ASR model as Ogg Opus (16 kHz mono, 32 kbps, about 14 MB an
hour), and `<id>.json`:

```json
{"at":"2026-11-25T15:30:00.123456789Z","durationMs":2140,"source":"wyoming","transcript":"Turn of the kitten lights.","keep":true,"transcriptionError":true,"correctText":"Turn off the kitchen lights.","profiles":["3f9a1c0b7d2e4a65"]}
```

`source` is `openai` for the HTTP API and `wyoming` for the Wyoming
listener. `at` is the UTC time, to the nanosecond, at which the
transcription finished and the recording was queued, not when it was saved.
The id is that time to the second and a random suffix, such as
`20261125T153000Z-1a2b3c4d`.

`transcriptionError`, `correctText` and `profiles` hold the review you save
on the recording's page. `transcriptionError` is `true` when the transcript
is wrong; such a recording always has `keep` set, and the service treats a
`.json` with `transcriptionError` `true` as kept even when its `keep` says
`false`, so it never expires or evicts one. `correctText` is what was
said, at most 16 KiB with each line break stored as one LF, and is empty
when the flag is off. With the flag on,
an empty `correctText` means the recogniser wrote text when nobody spoke.
`profiles` holds the ids of the recording's speakers. `transcript` always
holds what the recogniser returned; a review never changes it. A `.json`
written before these fields existed lacks them and reads as not flagged,
with no correct text and no speakers.

The speaker profiles live in one more file in the directory,
`profiles.json`, which the service writes when you first add a profile:

```json
{"profiles":[{"id":"3f9a1c0b7d2e4a65","name":"Anna"}]}
```

The service picks each id, 16 random hex digits. A recording names its
speakers by id, so a rename shows on every recording at once. Deleting a
profile rewrites only `profiles.json`: a recording that named it no longer
shows it, and drops its id the next time the service writes that
recording's `.json`. At start the service refuses to run when
`profiles.json` is not a regular file (a symlink, a directory or a FIFO,
which it never opens), is over 32 KiB, is not valid UTF-8, or is not one
JSON object whose only key is a `profiles` list: a missing or `null` list,
another key, a profile with a key besides `id` and `name`, or anything after
the object refuses it. It also refuses a file that breaks a rule the
profiles page enforces: more than 64 profiles, an id that is not 16
lowercase hex digits, or a name that is empty, over 64 bytes, has spaces
around it, holds a character that is not graphic (a control or format
character, or a line or paragraph separator), or that another profile has,
ignoring case. `{"profiles":[]}` is valid. Mend or delete the file to start
it again; deleting it removes every profile from every recording.
`profiles.json` counts toward the cap.

Rolling back to a release without the review loses it: an older release
reads these files, but when it rewrites one, for a change of its keep mark,
it drops the review fields. A recording that was flagged then keeps only its
`keep`. An older release leaves `profiles.json` alone and does not count it
toward the cap.

The service deletes recordings that are not kept and are older than 7 days,
once when it starts and then every hour. The two files of all recordings
together may take at most 1 GiB. Before a save that would pass that cap, the
service deletes the oldest recordings that are not kept until the new one
fits. Kept recordings count toward the cap, and so do the recordings left
for you below, any file the service failed to delete, and `profiles.json`.
When those alone
leave no room, the service saves nothing and logs a warning. An edit never
deletes a recording to make room: a review, a keep change, a new profile or
a rename that makes its file larger is refused when the store would then
pass the cap, and the page answers 507. An edit that does not make its file
larger, and deleting a profile, are always allowed. A deletion that
fails is logged as a warning and the service goes on with the others. The
retention, the cap and the interval are fixed.

The recordings directory must belong to the service alone. At start and
every hour the service deletes, besides expired recordings, the files that
an interrupted save or delete leaves: a `.opus` or `.json` file without its
partner, and its own temporary files, `probe-<digits>.tmp`,
`<id>.opus.<digits>.tmp`, `<id>.json.<digits>.tmp` and
`profiles.json.<digits>.tmp`. It never deletes `profiles.json` and leaves
other files alone.

At start the service logs one warning for each recording whose `.json` does
not decode, has no `keep` of `true` or `false` or no recording time, is over
7 MiB, or cannot be read once open, and for each recording with a file whose
size it cannot read. The warning names its id and, for a `.json`, the
reason. It leaves both files of such a recording on disk for you to
delete: it does not list, expire or evict it, because it cannot read its
keep mark, but its files count toward the cap. A `.json` the service cannot
open at all, for example because of its permissions, stops the service from
starting.

With `recordings.enable` on, the module creates `<stateDir>/recordings`
with mode `0700`, owned by `user` and `group`, and makes that directory,
and only that directory, writable for the service. Turning the option off
again leaves the directory and its recordings on disk; delete them by hand.

## Packages, accounts, paths and model files

| Option | Type | Default | Effect |
|---|---|---|---|
| `package` | package | this flake's package | The build of the service to run. |
| `user` | string | `"llama-swap-voice"` | The user the service runs as. The module creates it only under the default name. |
| `group` | string | `"llama-swap-voice"` | The group the service runs as. The module creates it only under the default name. Under `qwentts`, llama-swap's user joins it, so do not share it with other services. |
| `stateDir` | string | `"/var/lib/llama-swap-voice"` | A directory for this module alone. The voices directory is `<stateDir>/voices`, and the [recordings](#recordings) directory is `<stateDir>/recordings`. Use an absolute path with no `%` and no slash at the end. |
| `extraEnvironment` | attribute set of strings | `{}` | Extra [environment variables](#environment-variables) for the service, by full name. They override the variables the module writes. Evaluation refuses a key that is not an environment variable name, that is, one that does not match `[A-Za-z_][A-Za-z0-9_]*`, such as a key with `=` or a non-ASCII letter. Of the values, it checks only the [variables the module reads back](#variables-the-module-reads-back). It refuses `RECORDINGS_ENABLE`, `RECORDINGS_DIR` and `RECORDINGS_ALLOWEDHOSTS` while `recordings.enable` is on, because the recordings directory and the sandbox follow the [recordings](#recordings) options. While `recordings.enable` is off it accepts them, as long as `RECORDINGS_ENABLE` is empty or false (`0`, `f`, `F`, `false`, `FALSE` or `False`), because the service then reads none of them. The service checks every other override, the Wyoming variables included, when it starts, and refuses to start on a bad value. |
| `voiceNames` | list of strings, read-only | the declared names | The names of the declared voices, sorted. Other modules can read it. |
| `llamaSwap.group.name` | string | `"voice"` | The name of the group. |
| `llamaSwap.group.location` | `"legacy"` or `"routing"` | `"legacy"` | Where the module writes the group: the top-level `groups` key, or `routing.router.settings.groups`. llama-swap refuses a configuration that uses both, so match the rest of your settings. |
| `llamaCpp.package` | package | `pkgs.llama-cpp`, built for `llamaCpp.backend` only | The llama.cpp that supplies `llama-server`, version 0.8.0 or later. A package you set must match `llamaCpp.backend`. |
| `models.{asr,tts}.artifacts.{model,mmproj}` | submodule | the pinned file | One model file: the model itself, or its multimodal projector (`mmproj`). Each has the three options below. |
| `models.{asr,tts}.artifacts.{model,mmproj}.url` | string | a pinned Hugging Face revision | Where Nix downloads the file from. |
| `models.{asr,tts}.artifacts.{model,mmproj}.hash` | string | the hash of that file | The SRI hash of the file. |
| `models.{asr,tts}.artifacts.{model,mmproj}.path` | path | `pkgs.fetchurl { inherit url hash; }` | The file on disk. Set it to a local file, as a string, to skip the download. llama-swap's user must be able to read it. |
| `models.{asr,tts}.extraArgs` | list of strings | `[]` | Extra arguments for `llama-server`, added to the end of the command. |
| `tts.qwentts.package` | package | this flake's `qwen-tts-server` | The build of the Qwen TTS server. |
| `tts.qwentts.model` | path | this flake's `qwen3-tts-model` | The directory of weights, in the Hugging Face layout. Give a local copy as a string. |
| `tts.qwentts.modelName` | string | `"qwen3-tts"` | The name of the server's entry in llama-swap. |
| `tts.qwentts.chunkSize` | integer, 1 or more | `12` | Codec steps in each streamed chunk. 12 steps are about one second of audio. |
| `tts.qwentts.maxSegmentChars` | integer, 1 or more | `400` | The longest segment, in characters, that the server splits text into. |
| `tts.qwentts.maxNewTokens` | integer, 1 or more | `600` | Codec steps allowed for one segment. |
| `tts.qwentts.extraArgs` | list of strings | `[]` | Extra arguments for `qwen-tts-server`, added to the end of the command. |

The default `llamaCpp.package` sets every backend flag itself. A global
`nixpkgs.config.cudaSupport` therefore cannot add CUDA to a `cpu` build. The
version check reads the version of the package, so a package whose version
is a build number passes unchecked.

### Variables the module reads back

The module reads seven [environment variables](#environment-variables)
after it merges `extraEnvironment`:

- `SHUTDOWN_GRACE`, `SHUTDOWN_TOTAL` and `SHUTDOWN_HARD` set the unit's
  `TimeoutStopSec`.
- `TTS_MAXINPUTCHARS`, `UPSTREAM_TTSCONCURRENCY` and `REQUEST_MAXCONCURRENT`
  set values in the llama-swap entries.
- `TTS_MAXINPUTCHARS` and `WYOMING_STREAMINGMAXCHARS` are compared with
  each other when `wyoming.enable` is on.

A value from `extraEnvironment` therefore replaces the option it overrides.
Evaluation fails, and names the variable, if the option would not accept the
value. A valid value is a duration, or a positive decimal integer with no
sign and no leading zero.

## What the module refuses

Evaluation fails, and names the option, when a setting cannot work. Two
cases are common. `services.llama-swap.enable` must be on. And
`services.llama-swap.settings.apiKeys` must be unset, because the service
cannot send an API key.

These cases always fail:

- `defaultVoice` is not set or is not a declared voice.
- A clip path is relative or does not end in `.wav` or `.mp3`.
- Under `qwentts`: a clip does not end in `.wav`, a transcript is empty, or a
  voice has an `instruct` but no transcript.
- With `wyoming.enable` on: a setting in the list under [Wyoming](#wyoming).

With `llamaSwap.enable` on, evaluation also fails in these cases:

- `services.llama-swap.enable` is off.
- `services.llama-swap.settings.apiKeys` is set. The service cannot send an
  API key.
- `services.llama-swap.listenAddress` is an IPv6 address without brackets,
  such as `::1`, or has a zone such as `%eth0`.
- `llamaCpp.package` is older than llama.cpp 0.8.0.
- The ASR entry and the TTS entry have the same name.
- `upstream.ttsConcurrency` is above the TTS entry's `concurrencyLimit`, or
  `request.maxConcurrent` is above `models.asr.concurrencyLimit`.
- The group does not fit the layout of your llama-swap settings. Set
  `llamaSwap.group.location` to match, or turn the group off.

## What the module adds to llama-swap

The module adds two entries to `services.llama-swap.settings` and leaves your
own entries alone.

| Entry | Default name | Command | Model |
|---|---|---|---|
| ASR | `qwen3-asr` | `llama-server` | Qwen3-ASR-1.7B, Q8_0 GGUF |
| TTS under `llamacpp` | `qwen3-tts` | `llama-server` | Qwen3-TTS-12Hz-1.7B-Base, Q8_0 GGUF |
| TTS under `qwentts` | `qwen3-tts` | `qwen-tts-server` | Qwen3-TTS-12Hz-1.7B-Base weights |

The default download for `llamacpp` is four GGUF files, about 4.8 GB: 2.17
and 0.36 GB for ASR, 1.85 and 0.45 GB for TTS. The module also adds each
preloaded entry to `hooks.on_startup.preload` and, by default, writes the
group. It sets no other llama-swap value.

A `llama-server` entry runs this command and uses `/health` as its health
check. The ASR entry adds `--temp 0`.

```
llama-server --port ${PORT} -m <model> --mmproj <mmproj> [--device <device>]
  -ngl 99 -c 4096 -np 1 --flash-attn on --no-webui
```

llama-swap waits `healthCheckTimeout`, 120 seconds by default, for an entry
to become healthy. Raise `services.llama-swap.settings.healthCheckTimeout`
if a model needs longer to load. The Qwen TTS server also speaks a warm-up
sentence in every voice before it is healthy.

### The same entries in YAML

Without the module, add the two models to your llama-swap configuration
yourself. This is what the module writes by default. It needs llama.cpp
0.8.0 or later. The `-hf` argument downloads the model files from Hugging
Face at the first start. Remove `-ngl 99` to run on the CPU.

```yaml
models:
  qwen3-asr:
    cmd: llama-server --port ${PORT} -hf ggml-org/Qwen3-ASR-1.7B-GGUF:Q8_0 -ngl 99 -c 4096 -np 1 --temp 0 --flash-attn on --no-webui
    checkEndpoint: /health
    concurrencyLimit: 4
    ttl: 0
  qwen3-tts:
    cmd: llama-server --port ${PORT} -hf ggml-org/Qwen3-TTS-12Hz-1.7B-Base-GGUF:Q8_0 -ngl 99 -c 4096 -np 1 --flash-attn on --no-webui
    checkEndpoint: /health
    concurrencyLimit: 5
    ttl: 0
groups:
  voice:
    swap: false
    exclusive: false
    persistent: true
    members: [qwen3-asr, qwen3-tts]
hooks:
  on_startup:
    preload: [qwen3-asr, qwen3-tts]
```

## When the models unload

Two things can unload a model in llama-swap: another model that needs its
place, and an idle timer (`ttl`). The module's group controls the first.
`llamaSwap.group.sharing` selects how the group behaves.

- **`"pinned"` (the default).** The module writes a group with
  `swap = false`, `exclusive = false` and `persistent = true`. The ASR model
  and the TTS model stay loaded together, and your other models never unload
  them. Only a `ttl` above 0 unloads them, when they are idle, or a call to
  llama-swap's `/unload`.
- **`"takeTurns"`.** The module writes a group with `swap = false`,
  `exclusive = true` and `persistent = false`. A model from llama-swap's
  default group unloads the ASR model and the TTS model. The next speech
  request or transcription then unloads that model and loads its own entry.
  It also unloads every running member of any other non-persistent group.
  Use this value on a single GPU that cannot hold a chat model and the voice
  models at the same time. Any client that can reach the service can then
  make llama-swap stop your other models, so keep the service on loopback or
  on a network you trust.
- **Group off** (`llamaSwap.group.enable = false`). Both entries join
  llama-swap's default group, where one model runs at a time. Loading one
  stops the other, whatever the `ttl`. A transcription followed by a speech
  request then reloads both models. llama-swap has a default group only
  under its group router, not under the matrix or capacity router.

## Concurrency

Each request passes several limits on its way to a model. The service
admits speech requests and transcriptions separately, so speech that waits
never blocks a transcription. Admitted speech requests then wait at the
speech gate, which sends them to the TTS model one at a time by default.

| Stage | Limit | Default | A request over the limit |
|---|---|---|---|
| Service, speech | `request.maxConcurrentTTS` | 8 | gets a 503 |
| Service, speech gate | `upstream.ttsConcurrency` | 1 | waits in arrival order, up to `request.timeout`, then gets a 504 |
| Service, transcriptions | `request.maxConcurrent` | 4 | gets a 503 |
| llama-swap, each entry | `concurrencyLimit` | ASR 4, TTS 5 (`qwentts`: 8) | gets a 429; llama-swap does not queue |
| `llama-server` | `parallel` (`-np`) | 1 | waits in order inside `llama-server` |
| Qwen TTS server | `tts.qwentts.queueSize` | 8, plus the one it generates | gets a 503 `queue full` |

Two rules follow from the 429:

- Keep `upstream.ttsConcurrency` at or below the TTS entry's
  `concurrencyLimit`.
- Keep `request.maxConcurrent` at or below `models.asr.concurrencyLimit`.

The module checks both. The speech gate keeps sentences in order. It lets
the next request through when the model finishes its reply, not when the
client finishes playback. The default of 1 therefore costs little, and a
higher value only moves the wait from the gate into the TTS server.

[Retries](api.md#retries) describes the one case where the service sends a
speech request again.

## The sandbox and the GPU

The service's unit can write only to its private `/tmp`, where large uploads
go, and, with `recordings.enable` on, to `<stateDir>/recordings`. It cannot
read home directories.

The llama-swap unit from nixpkgs runs as a dynamic user with a read-only
file system and no access to `/home`. The module changes that unit only when
a model needs an NVIDIA GPU:

- With `llamaCpp.backend = "cuda"` or `tts.backend = "qwentts"`, the module
  turns `MemoryDenyWriteExecute` off and allows the devices `char-nvidia`
  and `char-nvidia-uvm`. It also sets `CUDA_DEVICE_ORDER=PCI_BUS_ID`, so a
  device index means the same card as in `nvidia-smi`. The device list
  closes every other device, so the change affects every model that
  llama-swap runs.
- With `qwentts`, llama-swap's user also joins the service's group, to read
  the clips. A start step creates the server's cache directory.
- If you combine `qwentts` with `vulkan` or `rocm`, the module adds
  `char-drm` back, and `/dev/kfd` for `rocm`.

An older NVIDIA driver can name its device `nvidia-frontend` in
`/proc/devices`. Add that name to the unit's `DeviceAllow` yourself.

## Environment variables

The service reads all its configuration from environment variables. The
module writes them for you, so you need this table only when you run the
service by hand or use `extraEnvironment`. Each name starts with
`LLAMA_SWAP_VOICE_`; the table and the rest of this page leave the prefix
out. Only `VOICES_DEFAULT` is required. The service checks the `WYOMING_`
variables only when `WYOMING_ENABLED` is `true`, and `RECORDINGS_DIR` and
`RECORDINGS_ALLOWEDHOSTS` only when `RECORDINGS_ENABLE` is `true`.

| Variable (`LLAMA_SWAP_VOICE_…`) | Module option | Default | Effect |
|---|---|---|---|
| `HOST` | `host` | `127.0.0.1` | The address to listen on. |
| `PORT` | `port` | `8920` | The port to listen on. |
| `UPSTREAM_URL` | `upstreamURL` | `http://127.0.0.1:8080` | The base URL of llama-swap. |
| `UPSTREAM_TTSMODEL` | `models.tts.name` or `tts.qwentts.modelName` | `qwen3-tts` | The name of the TTS entry in llama-swap. |
| `UPSTREAM_ASRMODEL` | `models.asr.name` | `qwen3-asr` | The name of the ASR entry in llama-swap. |
| `UPSTREAM_TTSBACKEND` | `tts.backend` | `llamacpp` | `llamacpp` or `qwentts`. |
| `UPSTREAM_TTSCONCURRENCY` | `upstream.ttsConcurrency` | `1` | Speech requests sent to the TTS model at a time. |
| `VOICES_DIR` | `stateDir` | `/var/lib/llama-swap-voice/voices` | The directory of clips. The module sets it to `<stateDir>/voices`. |
| `VOICES_DEFAULT` | `defaultVoice` | none; required | The voice for a request that names none. |
| `TTS_LANG` | none | `en` | The language the `llamacpp` backend sends. Under `qwentts`, the server's `language` applies. |
| `TTS_MAXINPUTCHARS` | `tts.maxInputChars` | `4096` | The longest text for one speech request, in characters. |
| `ASR_PREFIXREGEX` | none | `^language \w+<asr_text>` | A pattern the service removes from the start of each transcript. |
| `REQUEST_TIMEOUT` | `request.timeout` | `2m` | The longest time for a speech request or a transcription. |
| `REQUEST_MAXCONCURRENT` | `request.maxConcurrent` | `4` | Transcriptions handled at a time. |
| `REQUEST_MAXCONCURRENTTTS` | `request.maxConcurrentTTS` | `8` | Speech requests held at a time. |
| `LOG_LEVEL` | none | `info` | `debug`, `info`, `warn` or `error`. |
| `LOG_FORMAT` | none | `json` | `json` or `console`. |
| `SHUTDOWN_GRACE` | `shutdown.grace` | `0s` | Step 1 of [shutdown](api.md#shutdown). |
| `SHUTDOWN_TOTAL` | `shutdown.total` | `15s` | Step 2 of shutdown. |
| `SHUTDOWN_HARD` | `shutdown.hard` | `3s` | Step 3 of shutdown. |
| `WYOMING_ENABLED` | `wyoming.enable` | `false` | `true` also serves the [Wyoming protocol](api.md#wyoming) on a second port. |
| `WYOMING_HOST` | `wyoming.host` | `127.0.0.1` | The address the Wyoming listener binds. Write an IPv6 address without brackets, such as `::1`. |
| `WYOMING_PORT` | `wyoming.port` | `10300` | The Wyoming port, 1 to 65535. The host and port together must differ from `HOST` and `PORT`. |
| `WYOMING_LANGUAGES` | `wyoming.languages` | `en` | The languages that `info` lists for the ASR model and each voice, separated by commas. |
| `WYOMING_SPEED` | `wyoming.speed` | `1.0` | The speech speed for every Wyoming synthesis, 0.25 to 4. |
| `WYOMING_STREAMING` | `wyoming.streaming` | `sentences` | How the service speaks a [streamed synthesis](api.md#streamed-synthesis): `sentences` speaks each sentence as it completes and advertises streaming in `info`; `whole` speaks the whole text after `synthesize-stop` and advertises no streaming. |
| `WYOMING_STREAMINGMAXCHARS` | `wyoming.streamingMaxChars` | `300` | 1 to `TTS_MAXINPUTCHARS`. In `sentences` mode, the most characters of the sentences that one `synthesize-chunk` completes to join into one request. A longer sentence is spoken whole. The text of a stream not yet spoken plus an arriving chunk may not exceed `TTS_MAXINPUTCHARS`. |
| `RECORDINGS_ENABLE` | `recordings.enable` | `false` | `true` keeps the audio and transcript of every successful transcription in `RECORDINGS_DIR` and serves the [recordings page](api.md#recordings). This stores household speech for 7 days, and kept recordings until they are released. |
| `RECORDINGS_DIR` | `stateDir` | none | The directory of recordings, an absolute path, for the service alone: it deletes the [leftover files](#recordings) it finds there. Required when `RECORDINGS_ENABLE` is `true`. The service creates it with mode `0700` if it is missing, and refuses to start if it cannot create it or write to it. The module sets it to `<stateDir>/recordings`. |
| `RECORDINGS_ALLOWEDHOSTS` | `recordings.allowedHosts` | empty | The hosts, separated by commas, that the recordings page answers besides `127.0.0.1`, `[::1]`, `localhost` and `HOST`. Each is a host, an IPv6 address in brackets, or either with a port; a host without a port matches every port. The service refuses to start on an entry with a scheme, a path or a space. |

Set a variable that has no option through `extraEnvironment`:

```nix
services.llama-swap-voice.extraEnvironment.LLAMA_SWAP_VOICE_LOG_LEVEL = "debug";
```

The module writes the two model names only when `llamaSwap.enable` is on,
the `WYOMING_` variables only when `wyoming.enable` is on, and the
`RECORDINGS_` variables only when `recordings.enable` is on. If
a value is wrong at startup, the service lists every problem and exits.
