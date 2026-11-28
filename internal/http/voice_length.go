package http

import (
	"encoding/binary"

	"finnri/internal/ai"
)

/*
Voice reads are priced by how long the clip is (P106, 2026-11-20).

They used to be priced by file size, which only worked while every app
recorded at the same bitrate. Spoken-day batches (P105) record at speech
quality, about a quarter of the bytes per second, so size alone would price a
90-second day as a short clip. The length is read from the audio file itself —
the MPEG-4 `mvhd` box both platforms write — never from anything the app says.
A file whose length can't be read falls back to the old size tiers.
*/

const (
	voiceShortMaxSeconds  = 30
	voiceMediumMaxSeconds = 120
	voiceShortMaxBytes    = 512 * 1024
	voiceMediumMaxBytes   = 1536 * 1024
)

// classifyVoiceParseAction picks the voice action (and so the price) for a
// clip, and reports a clip too long to read at all.
func classifyVoiceParseAction(audio []byte) (ai.ActionCode, bool) {
	size := int64(len(audio))
	if size > voiceMediumMaxBytes {
		return "", true
	}
	if seconds, ok := mp4DurationSeconds(audio); ok {
		switch {
		case seconds <= voiceShortMaxSeconds:
			return ai.ActionTransactionParseVoiceShort, false
		case seconds <= voiceMediumMaxSeconds:
			return ai.ActionTransactionParseVoiceMedium, false
		default:
			return "", true
		}
	}
	if size <= voiceShortMaxBytes {
		return ai.ActionTransactionParseVoiceShort, false
	}
	return ai.ActionTransactionParseVoiceMedium, false
}

// mp4DurationSeconds reads the movie length from an MPEG-4 / 3GP file's
// `moov/mvhd` box. Android's recorder may write `moov` after the audio, so the
// top level is walked box by box rather than assumed.
func mp4DurationSeconds(data []byte) (float64, bool) {
	moov, ok := findMP4Box(data, "moov")
	if !ok {
		return 0, false
	}
	mvhd, ok := findMP4Box(moov, "mvhd")
	if !ok || len(mvhd) < 20 {
		return 0, false
	}
	var timescale uint32
	var duration uint64
	switch mvhd[0] {
	case 0:
		timescale = binary.BigEndian.Uint32(mvhd[12:16])
		raw := binary.BigEndian.Uint32(mvhd[16:20])
		if raw == 0xFFFFFFFF {
			return 0, false
		}
		duration = uint64(raw)
	case 1:
		if len(mvhd) < 32 {
			return 0, false
		}
		timescale = binary.BigEndian.Uint32(mvhd[20:24])
		duration = binary.BigEndian.Uint64(mvhd[24:32])
		if duration == 0xFFFFFFFFFFFFFFFF {
			return 0, false
		}
	default:
		return 0, false
	}
	if timescale == 0 || duration == 0 {
		return 0, false
	}
	return float64(duration) / float64(timescale), true
}

// findMP4Box returns the body of the first box of the given type directly
// inside data.
func findMP4Box(data []byte, kind string) ([]byte, bool) {
	for offset := 0; offset+8 <= len(data); {
		size := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		boxType := string(data[offset+4 : offset+8])
		header := uint64(8)
		switch size {
		case 0:
			size = uint64(len(data) - offset)
		case 1:
			if offset+16 > len(data) {
				return nil, false
			}
			size = binary.BigEndian.Uint64(data[offset+8 : offset+16])
			header = 16
		}
		// Compared against what is left, so a large size cannot overflow.
		if size < header || size > uint64(len(data)-offset) {
			return nil, false
		}
		if boxType == kind {
			return data[uint64(offset)+header : uint64(offset)+size], true
		}
		offset += int(size)
	}
	return nil, false
}
