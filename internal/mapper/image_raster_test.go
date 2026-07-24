package mapper

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInspectRasterEdges(t *testing.T) {
	t.Parallel()

	_, ok := InspectRaster(nil)
	require.False(t, ok)

	bmp := make([]byte, 26)
	copy(bmp, "BM")
	binary.LittleEndian.PutUint32(bmp[18:22], 2)
	negativeHeight := int32(-3)
	binary.LittleEndian.PutUint32(bmp[22:26], uint32(negativeHeight))
	info, ok := InspectRaster(bmp)
	require.True(t, ok)
	require.Equal(t, mimeBMP, info.MimeType)
	require.Equal(t, 2, info.Width)
	require.Equal(t, 3, info.Height)
	require.False(t, info.Animated)

	require.Equal(t, "", sniffRasterMime([]byte("not an image")))
	require.Equal(t, mimeBMP, sniffRasterMime(bmp))
	_, ok = parseBMP(nil)
	require.False(t, ok)
}

func TestRasterContainerWalkEdges(t *testing.T) {
	t.Parallel()

	png := append([]byte(nil), magicPNG...)
	png = append(png, 0, 0, 0, 0, 't', 'E', 'S', 'T', 0, 0, 0, 0)
	require.False(t, pngHasAnimationControl(png))

	// A chunk declaring a length past the remaining buffer stops the walk
	// instead of advancing offset out of range.
	oversized := append([]byte(nil), magicPNG...)
	oversized = append(oversized, 0xFF, 0xFF, 0xFF, 0xFF, 't', 'E', 'S', 'T', 0, 0, 0, 0)
	require.False(t, pngHasAnimationControl(oversized))

	_, ok := parseGIF(nil)
	require.False(t, ok)
	gif := append([]byte("GIF89a"), make([]byte, 7)...)
	binary.LittleEndian.PutUint16(gif[6:8], 1)
	binary.LittleEndian.PutUint16(gif[8:10], 1)
	require.Zero(t, gifImageDescriptorCount(gif))
	require.Equal(t, len(gif), skipGIFImage(gif, len(gif)))

	gifWithTable := append([]byte(nil), gif...)
	gifWithTable[10] = 0x80
	gifWithTable = append(gifWithTable, make([]byte, 3)...)
	gifWithTable = append(gifWithTable, 0x2C)
	descriptor := make([]byte, 9)
	descriptor[8] = 0x80
	gifWithTable = append(gifWithTable, descriptor...)
	gifWithTable = append(gifWithTable, make([]byte, 3)...)
	gifWithTable = append(gifWithTable, 2, 1)
	require.Greater(t, skipGIFImage(gifWithTable, 16), len(gifWithTable))
	require.Equal(t, 3, skipGIFSubBlocks([]byte{2, 1}, 0))
}

func TestWebPAndJPEGParserEdges(t *testing.T) {
	t.Parallel()

	_, ok := parseWebP(nil)
	require.False(t, ok)

	webp := make([]byte, 30)
	copy(webp, "RIFF")
	copy(webp[8:12], "WEBP")
	copy(webp[12:16], "VP8 ")
	_, ok = parseWebP(webp)
	require.False(t, ok)
	copy(webp[23:26], []byte{0x9D, 0x01, 0x2A})
	binary.LittleEndian.PutUint16(webp[26:28], 2)
	binary.LittleEndian.PutUint16(webp[28:30], 3)
	info, ok := parseWebP(webp)
	require.True(t, ok)
	require.Equal(t, 2, info.width)
	require.Equal(t, 3, info.height)

	copy(webp[12:16], "VP8L")
	webp[20] = 0
	_, ok = parseWebP(webp)
	require.False(t, ok)
	webp[20] = 0x2F
	binary.LittleEndian.PutUint32(webp[21:25], 1|(2<<14))
	info, ok = parseWebP(webp)
	require.True(t, ok)
	require.Equal(t, 2, info.width)
	require.Equal(t, 3, info.height)

	copy(webp[12:16], "NOPE")
	_, ok = parseWebP(webp)
	require.False(t, ok)

	_, ok = parseJPEG([]byte{0xFF, 0xD8, 0xFF})
	require.False(t, ok)
	_, ok = parseJPEG(append([]byte{0xFF, 0xD8}, make([]byte, 9)...))
	require.False(t, ok)

	fill := []byte{0xFF, 0xD8, 0xFF, 0xFF, 0xFF, 0xDA, 0, 0, 0, 0, 0}
	_, ok = parseJPEG(fill)
	require.False(t, ok)

	standalone := []byte{0xFF, 0xD8, 0xFF, 0xD9, 0xFF, 0xDA, 0, 0, 0, 0, 0}
	_, ok = parseJPEG(standalone)
	require.False(t, ok)

	scanBeforeFrame := append([]byte{0xFF, 0xD8, 0xFF, 0xDA}, make([]byte, 8)...)
	_, ok = parseJPEG(scanBeforeFrame)
	require.False(t, ok)
	require.False(t, jpegFrameMarker(0xC4))
}
