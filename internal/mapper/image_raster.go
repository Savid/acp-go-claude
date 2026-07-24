package mapper

import (
	"bytes"
	"encoding/binary"
)

// Raster inspection is structural and decode-free: format identification reads
// magic bytes, dimensions come from fixed header fields, and animation
// detection walks the container's block/chunk list without decoding any pixel
// data.

const (
	mimePNG  = "image/png"
	mimeJPEG = "image/jpeg"
	mimeGIF  = "image/gif"
	mimeWebP = "image/webp"
	mimeBMP  = "image/bmp"
)

var (
	magicPNG   = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	magicJPEG  = []byte{0xFF, 0xD8, 0xFF}
	magicGIF87 = []byte("GIF87a")
	magicGIF89 = []byte("GIF89a")
	magicBMP   = []byte("BM")
)

type rasterInfo struct {
	mime     string
	width    int
	height   int
	animated bool
}

// RasterInfo is structurally derived raster metadata.
type RasterInfo struct {
	MimeType string
	Width    int
	Height   int
	Animated bool
}

// InspectRaster returns structurally derived raster metadata without decoding pixels.
func InspectRaster(data []byte) (RasterInfo, bool) {
	info, ok := parseRaster(data)
	if !ok {
		return RasterInfo{}, false
	}

	return RasterInfo{
		MimeType: info.mime,
		Width:    info.width,
		Height:   info.height,
		Animated: info.animated,
	}, true
}

// sniffRasterMime identifies the container format from magic bytes alone. It
// returns "" for bytes matching no known raster container.
func sniffRasterMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, magicPNG):
		return mimePNG
	case bytes.HasPrefix(data, magicGIF87) || bytes.HasPrefix(data, magicGIF89):
		return mimeGIF
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return mimeWebP
	case bytes.HasPrefix(data, magicJPEG):
		return mimeJPEG
	case bytes.HasPrefix(data, magicBMP):
		return mimeBMP
	default:
		return ""
	}
}

// parseRaster reads format, dimensions, and animation markers from the
// container header. ok is false when the bytes match no known container or
// the header yields no valid dimensions. On the false path the returned info
// carries the sniffed mime, empty when no container matched, so callers can
// distinguish an unrecognized format from a recognized one with unreadable
// dimensions.
func parseRaster(data []byte) (rasterInfo, bool) {
	var (
		info   rasterInfo
		parsed bool
	)

	mime := sniffRasterMime(data)

	switch mime {
	case mimePNG:
		info, parsed = parsePNG(data)
	case mimeGIF:
		info, parsed = parseGIF(data)
	case mimeWebP:
		info, parsed = parseWebP(data)
	case mimeJPEG:
		info, parsed = parseJPEG(data)
	case mimeBMP:
		info, parsed = parseBMP(data)
	}

	if !parsed || info.width <= 0 || info.height <= 0 {
		return rasterInfo{mime: mime}, false
	}

	return info, true
}

func parsePNG(data []byte) (rasterInfo, bool) {
	// Signature (8) plus the mandatory leading IHDR chunk (8 header + 13 data + 4 CRC).
	const minLen = 33

	if len(data) < minLen || string(data[12:16]) != "IHDR" {
		return rasterInfo{}, false
	}

	return rasterInfo{
		mime:     mimePNG,
		width:    int(binary.BigEndian.Uint32(data[16:20])),
		height:   int(binary.BigEndian.Uint32(data[20:24])),
		animated: pngHasAnimationControl(data),
	}, true
}

// pngHasAnimationControl walks the chunk list for an acTL chunk. The APNG
// specification constrains acTL to precede the first IDAT, so the walk ends
// there.
func pngHasAnimationControl(data []byte) bool {
	offset := 8

	for offset+8 <= len(data) {
		// A chunk occupies 12 bytes of framing plus its declared data length.
		// The length is read into int64 so a value above 2GiB stays positive on
		// every build, and the walk stops once a chunk claims more than the
		// remaining buffer rather than advancing offset out of range.
		length := int64(binary.BigEndian.Uint32(data[offset : offset+4]))
		if length+12 > int64(len(data)-offset) {
			return false
		}

		switch string(data[offset+4 : offset+8]) {
		case "acTL":
			return true
		case "IDAT":
			return false
		}

		offset += 12 + int(length)
	}

	return false
}

func parseGIF(data []byte) (rasterInfo, bool) {
	// Header (6) plus logical screen descriptor (7).
	const minLen = 13

	if len(data) < minLen {
		return rasterInfo{}, false
	}

	return rasterInfo{
		mime:     mimeGIF,
		width:    int(binary.LittleEndian.Uint16(data[6:8])),
		height:   int(binary.LittleEndian.Uint16(data[8:10])),
		animated: gifImageDescriptorCount(data) > 1,
	}, true
}

// gifImageDescriptorCount walks the GIF block stream counting image
// descriptors. More than one frame means animation, so the walk stops at the
// second descriptor.
func gifImageDescriptorCount(data []byte) int {
	offset := 13
	if data[10]&0x80 != 0 {
		offset += 3 * (2 << (data[10] & 0x07))
	}

	count := 0

	for offset < len(data) {
		switch data[offset] {
		case 0x21: // Extension: introducer, label, then data sub-blocks.
			offset = skipGIFSubBlocks(data, offset+2)
		case 0x2C: // Image descriptor.
			count++
			if count > 1 {
				return count
			}

			offset = skipGIFImage(data, offset)
		default: // Trailer (0x3B) or a corrupt stream.
			return count
		}
	}

	return count
}

func skipGIFImage(data []byte, offset int) int {
	const descriptorLen = 10

	if offset+descriptorLen > len(data) {
		return len(data)
	}

	flags := data[offset+9]
	offset += descriptorLen

	if flags&0x80 != 0 {
		offset += 3 * (2 << (flags & 0x07))
	}

	// One LZW minimum-code-size byte precedes the image data sub-blocks.
	return skipGIFSubBlocks(data, offset+1)
}

func skipGIFSubBlocks(data []byte, offset int) int {
	for offset < len(data) {
		size := int(data[offset])
		offset++

		if size == 0 {
			return offset
		}

		offset += size
	}

	return offset
}

func parseWebP(data []byte) (rasterInfo, bool) {
	// RIFF header (12) plus first chunk header (8) plus the smallest payload
	// prefix any of the three chunk kinds needs for dimensions.
	const minLen = 30

	if len(data) < minLen {
		return rasterInfo{}, false
	}

	switch string(data[12:16]) {
	case "VP8X":
		return rasterInfo{
			mime:     mimeWebP,
			width:    1 + int(uint32(data[24])|uint32(data[25])<<8|uint32(data[26])<<16),
			height:   1 + int(uint32(data[27])|uint32(data[28])<<8|uint32(data[29])<<16),
			animated: data[20]&0x02 != 0,
		}, true
	case "VP8 ":
		if data[23] != 0x9D || data[24] != 0x01 || data[25] != 0x2A {
			return rasterInfo{}, false
		}

		return rasterInfo{
			mime:   mimeWebP,
			width:  int(binary.LittleEndian.Uint16(data[26:28]) & 0x3FFF),
			height: int(binary.LittleEndian.Uint16(data[28:30]) & 0x3FFF),
		}, true
	case "VP8L":
		if data[20] != 0x2F {
			return rasterInfo{}, false
		}

		bits := binary.LittleEndian.Uint32(data[21:25])

		return rasterInfo{
			mime:   mimeWebP,
			width:  int(bits&0x3FFF) + 1,
			height: int(bits>>14&0x3FFF) + 1,
		}, true
	default:
		return rasterInfo{}, false
	}
}

func parseJPEG(data []byte) (rasterInfo, bool) {
	offset := 2

	for offset+9 <= len(data) {
		if data[offset] != 0xFF {
			return rasterInfo{}, false
		}

		marker := data[offset+1] //nolint:gosec // The loop condition bounds offset+9 <= len(data).
		if marker == 0xFF {      // Fill byte before a marker.
			offset++

			continue
		}

		if marker >= 0xD0 && marker <= 0xD9 { // Standalone markers carry no length.
			offset += 2

			continue
		}

		if jpegFrameMarker(marker) {
			return rasterInfo{
				mime:   mimeJPEG,
				width:  int(binary.BigEndian.Uint16(data[offset+7 : offset+9])),
				height: int(binary.BigEndian.Uint16(data[offset+5 : offset+7])),
			}, true
		}

		if marker == 0xDA { // Scan data reached without a frame header.
			return rasterInfo{}, false
		}

		offset += 2 + int(binary.BigEndian.Uint16(data[offset+2:offset+4]))
	}

	return rasterInfo{}, false
}

// jpegFrameMarker reports whether marker is an SOF frame header: SOF0-SOF15
// excluding DHT, JPG, and DAC, which reuse the SOF numbering space.
func jpegFrameMarker(marker byte) bool {
	return marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC
}

func parseBMP(data []byte) (rasterInfo, bool) {
	// File header (14) plus the BITMAPINFOHEADER prefix through the height field.
	const minLen = 26

	if len(data) < minLen {
		return rasterInfo{}, false
	}

	height := int(int32(binary.LittleEndian.Uint32(data[22:26]))) //nolint:gosec // The header field is signed 32-bit by specification.
	if height < 0 {                                               // Top-down rows are stored as a negative height.
		height = -height
	}

	return rasterInfo{
		mime:   mimeBMP,
		width:  int(int32(binary.LittleEndian.Uint32(data[18:22]))), //nolint:gosec // The header field is signed 32-bit by specification.
		height: height,
	}, true
}
