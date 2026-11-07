package audio

import "bytes"

const (
	riffMagic = "RIFF"
	waveMagic = "WAVE"
	oggMagic  = "OggS"
	opusMagic = "OpusHead"
	id3Magic  = "ID3"

	waveOffset          = 8
	oggSegmentCountByte = 26
	oggHeaderBytes      = 27

	mp3SyncByte     = 0xFF
	mp3SyncMask     = 0xE0
	mp3HeaderBytes  = 3
	mp3VersionByte  = 1
	mp3BitrateByte  = 2
	mp3VersionShift = 3
	mp3LayerShift   = 1
	mp3BitrateShift = 4
	mp3RateShift    = 2
	mp3FieldMask    = 0x03
	mp3Version25    = 0x00
	mp3ReservedVer  = 0x01
	mp3LayerIII     = 0x01
	mp3BadBitrate   = 0x0F
	mp3ReservedRate = 0x03
)

func sniff(data []byte) (decodeFunc, error) {
	switch {
	case isWAV(data):
		return decodeWAV, nil
	case isOggOpus(data):
		return decodeOpus, nil
	case bytes.HasPrefix(data, []byte(id3Magic)), isMP3Frame(data):
		return decodeMP3, nil
	}

	return nil, ErrUnsupported
}

func isWAV(data []byte) bool {
	return bytes.HasPrefix(data, []byte(riffMagic)) &&
		bytes.HasPrefix(data[min(waveOffset, len(data)):], []byte(waveMagic))
}

// Ogg also carries Vorbis, FLAC and Speex; only a first packet of OpusHead
// marks a stream this package can decode.
func isOggOpus(data []byte) bool {
	if !bytes.HasPrefix(data, []byte(oggMagic)) || len(data) < oggHeaderBytes {
		return false
	}

	body := oggHeaderBytes + int(data[oggSegmentCountByte])
	if body > len(data) {
		return false
	}

	return bytes.HasPrefix(data[body:], []byte(opusMagic))
}

type mp3Header struct {
	version byte
	layer   byte
	bitrate byte
	rate    byte
}

func readMP3Header(data []byte) (mp3Header, bool) {
	if len(data) < mp3HeaderBytes || data[0] != mp3SyncByte || data[mp3VersionByte]&mp3SyncMask != mp3SyncMask {
		return mp3Header{}, false
	}

	header := data[mp3VersionByte]
	return mp3Header{
		version: (header >> mp3VersionShift) & mp3FieldMask,
		layer:   (header >> mp3LayerShift) & mp3FieldMask,
		bitrate: data[mp3BitrateByte] >> mp3BitrateShift,
		rate:    (data[mp3BitrateByte] >> mp3RateShift) & mp3FieldMask,
	}, true
}

// AAC in ADTS shares the 12-bit sync word, so the layer bits must say
// Layer III before the bytes count as MP3.
func (h mp3Header) layerIII() bool {
	return h.version != mp3ReservedVer && h.layer == mp3LayerIII && h.bitrate != mp3BadBitrate && h.rate != mp3ReservedRate
}

// go-mp3 decodes MPEG-1 and MPEG-2 but not MPEG-2.5, the 8 to 12 kHz
// extension, so those frames are recognised only to be turned away.
func isMP3Frame(data []byte) bool {
	header, ok := readMP3Header(data)
	return ok && header.layerIII() && header.version != mp3Version25
}

func isMPEG25Frame(data []byte) bool {
	header, ok := readMP3Header(data)
	return ok && header.layerIII() && header.version == mp3Version25
}
