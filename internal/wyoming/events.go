package wyoming

// Event types and data fields as OHF-Voice/wyoming names them at 844d383:
// info.py:10-30,63-156,273-318; asr.py:9-58; audio.py:20-139;
// error.py:8-30; ping.py:8-53; tts.py:10,26-59,62-112,115-212.
const (
	typeDescribe   = "describe"
	typeInfo       = "info"
	typePing       = "ping"
	typePong       = "pong"
	typeError      = "error"
	typeTranscribe = "transcribe"
	typeTranscript = "transcript"
	typeAudioStart = "audio-start"
	typeAudioChunk = "audio-chunk"
	typeAudioStop  = "audio-stop"
	typeSynthesize = "synthesize"

	typeSynthesizeStart   = "synthesize-start"
	typeSynthesizeChunk   = "synthesize-chunk"
	typeSynthesizeStop    = "synthesize-stop"
	typeSynthesizeStopped = "synthesize-stopped"
)

type attribution struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// artefact holds the fields every program, model and voice shares. The
// reference reader fills an absent description or version with None.
type artefact struct {
	Name        string      `json:"name"`
	Attribution attribution `json:"attribution"`
	Installed   bool        `json:"installed"`
	Description string      `json:"description,omitempty"`
	Version     string      `json:"version,omitempty"`
}

type asrModel struct {
	artefact
	Languages []string `json:"languages"`
}

type asrProgram struct {
	artefact
	Models                      []asrModel `json:"models"`
	SupportsTranscriptStreaming bool       `json:"supports_transcript_streaming"`
}

type ttsVoice struct {
	artefact
	Languages []string `json:"languages"`
}

type ttsProgram struct {
	artefact
	Voices                      []ttsVoice `json:"voices"`
	SupportsSynthesizeStreaming bool       `json:"supports_synthesize_streaming"`
}

type info struct {
	ASR []asrProgram `json:"asr"`
	TTS []ttsProgram `json:"tts"`
}

// audioFormat is the data of audio-start and audio-chunk; width is bytes
// per sample. Their optional timestamp is not read.
type audioFormat struct {
	Rate     int `json:"rate"`
	Width    int `json:"width"`
	Channels int `json:"channels"`
}

type transcript struct {
	Text string `json:"text"`
}

type errorData struct {
	Text string `json:"text"`
	Code string `json:"code,omitempty"`
}

// synthesizeData is the data of synthesize and synthesize-start, which has
// no text. The voice's speaker and language, the text format and the context
// are not read.
type synthesizeData struct {
	Text  string           `json:"text"`
	Voice *synthesizeVoice `json:"voice"`
}

type synthesizeChunkData struct {
	Text string `json:"text"`
}

type synthesizeVoice struct {
	Name string `json:"name"`
}

// voiceName is the requested voice, or "" for the default.
func (d *synthesizeData) voiceName() string {
	if d.Voice == nil {
		return ""
	}

	return d.Voice.Name
}

// pingData is the data of both ping and pong; pong copies the text, null
// included.
type pingData struct {
	Text *string `json:"text"`
}
