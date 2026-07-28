package osenv

import "encoding/binary"

// imageSniffBytes is how much of a file's head is inspected for an image
// signature. Matches Pi's IMAGE_TYPE_SNIFF_BYTES (utils/mime.ts).
const imageSniffBytes = 4100

var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// DetectImageMimeType identifies a supported image type from its magic bytes,
// returning "" for anything else. Port of Pi's detectSupportedImageMimeType:
// detection is content-based, never extension-based, and deliberately rejects
// formats the model cannot consume (progressive-arithmetic JPEG, animated PNG).
func DetectImageMimeType(buf []byte) string {
	switch {
	case startsWith(buf, []byte{0xff, 0xd8, 0xff}):
		// 0xf7 marks arithmetic-coded JPEG, which decoders widely reject.
		if len(buf) > 3 && buf[3] == 0xf7 {
			return ""
		}
		return "image/jpeg"
	case startsWith(buf, pngSignature):
		if isPNG(buf) && !isAnimatedPNG(buf) {
			return "image/png"
		}
		return ""
	case startsWithASCII(buf, 0, "GIF"):
		return "image/gif"
	case startsWithASCII(buf, 0, "RIFF") && startsWithASCII(buf, 8, "WEBP"):
		return "image/webp"
	case startsWithASCII(buf, 0, "BM") && isBMP(buf):
		return "image/bmp"
	}
	return ""
}

func startsWith(buf, prefix []byte) bool {
	if len(buf) < len(prefix) {
		return false
	}
	for i, b := range prefix {
		if buf[i] != b {
			return false
		}
	}
	return true
}

func startsWithASCII(buf []byte, offset int, s string) bool {
	if len(buf) < offset+len(s) {
		return false
	}
	return string(buf[offset:offset+len(s)]) == s
}

func isPNG(buf []byte) bool {
	return len(buf) >= 16 &&
		binary.BigEndian.Uint32(buf[len(pngSignature):]) == 13 &&
		startsWithASCII(buf, 12, "IHDR")
}

// isAnimatedPNG reports an APNG: an acTL chunk appearing before the first IDAT.
func isAnimatedPNG(buf []byte) bool {
	offset := len(pngSignature)
	for offset+8 <= len(buf) {
		chunkLength := int(binary.BigEndian.Uint32(buf[offset:]))
		typeOffset := offset + 4
		if startsWithASCII(buf, typeOffset, "acTL") {
			return true
		}
		if startsWithASCII(buf, typeOffset, "IDAT") {
			return false
		}
		next := offset + 8 + chunkLength + 4
		if next <= offset || next > len(buf) {
			return false
		}
		offset = next
	}
	return false
}

func isBMP(buf []byte) bool {
	if len(buf) < 26 {
		return false
	}
	declaredFileSize := binary.LittleEndian.Uint32(buf[2:])
	pixelOffset := binary.LittleEndian.Uint32(buf[10:])
	headerSize := binary.LittleEndian.Uint32(buf[14:])
	if declaredFileSize == 0 || pixelOffset < 26 {
		return false
	}
	switch headerSize {
	case 12, 40, 52, 56, 64, 108, 124:
		return true
	}
	return false
}
