package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// jpegWithOrientation builds a real JPEG and splices an EXIF APP1 segment
// carrying the given orientation in after the SOI marker, which is how a phone
// camera writes one.
func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()

	var plain bytes.Buffer
	if err := jpeg.Encode(&plain, img, nil); err != nil {
		t.Fatalf("encoding jpeg: %v", err)
	}

	// TIFF header, one IFD entry: tag 0x0112, type SHORT, count 1, value.
	var tiff bytes.Buffer
	tiff.WriteString("MM")                                // big endian
	binary.Write(&tiff, binary.BigEndian, uint16(42))     // magic
	binary.Write(&tiff, binary.BigEndian, uint32(8))      // offset of IFD0
	binary.Write(&tiff, binary.BigEndian, uint16(1))      // entry count
	binary.Write(&tiff, binary.BigEndian, uint16(0x0112)) // Orientation
	binary.Write(&tiff, binary.BigEndian, uint16(3))      // SHORT
	binary.Write(&tiff, binary.BigEndian, uint32(1))      // count
	binary.Write(&tiff, binary.BigEndian, orientation)    // value, left-aligned
	binary.Write(&tiff, binary.BigEndian, uint16(0))      // padding of the 4-byte value
	binary.Write(&tiff, binary.BigEndian, uint32(0))      // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	segment := []byte{0xFF, 0xE1}
	segment = binary.BigEndian.AppendUint16(segment, uint16(len(payload)+2))
	segment = append(segment, payload...)

	out := append([]byte{0xFF, 0xD8}, segment...)
	return append(out, plain.Bytes()[2:]...) // everything after the original SOI
}

// landscape is 8 wide and 4 high, with a marked top-left pixel so a rotation is
// detectable after the metadata has been stripped.
func landscape() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	for y := range 4 {
		for x := range 8 {
			img.Set(x, y, color.RGBA{200, 200, 200, 255})
		}
	}
	img.Set(0, 0, color.RGBA{0, 0, 0, 255})
	return img
}

func TestJPEGOrientationRead(t *testing.T) {
	for _, want := range []uint16{1, 3, 6, 8} {
		raw := jpegWithOrientation(t, landscape(), want)
		if got := jpegOrientation(raw); got != int(want) {
			t.Errorf("jpegOrientation = %d, want %d", got, want)
		}
	}
}

func TestJPEGOrientationMissing(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, landscape(), nil); err != nil {
		t.Fatal(err)
	}
	if got := jpegOrientation(buf.Bytes()); got != 1 {
		t.Errorf("orientation of a plain jpeg = %d, want 1", got)
	}
	if got := jpegOrientation([]byte("not a jpeg at all")); got != 1 {
		t.Errorf("orientation of junk = %d, want 1", got)
	}
}

// Orientation 6 means "rotate 90° clockwise to display", so an 8x4 image has to
// come back as 4x8 with the pixels actually turned — otherwise stripping the
// tag would leave every portrait photo on its side.
func TestNormaliseAppliesOrientation(t *testing.T) {
	raw := jpegWithOrientation(t, landscape(), 6)

	p, err := normalisePhoto(raw)
	if err != nil {
		t.Fatalf("normalisePhoto: %v", err)
	}
	if p.ext != ".jpg" || p.contentType != "image/jpeg" {
		t.Errorf("got ext %q type %q, want .jpg/image/jpeg", p.ext, p.contentType)
	}

	out, err := jpeg.Decode(bytes.NewReader(p.data))
	if err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if w, h := out.Bounds().Dx(), out.Bounds().Dy(); w != 4 || h != 8 {
		t.Errorf("result is %dx%d, want 4x8 (rotated)", w, h)
	}
	// The marked corner started top-left; a 90° clockwise turn puts it top-right.
	if !darkAt(out, 3, 0) {
		t.Error("marked pixel is not in the top-right corner after rotation")
	}
}

func TestNormaliseStripsEXIF(t *testing.T) {
	raw := jpegWithOrientation(t, landscape(), 6)
	if exifSegment(raw) == nil {
		t.Fatal("test fixture has no EXIF to strip")
	}

	p, err := normalisePhoto(raw)
	if err != nil {
		t.Fatalf("normalisePhoto: %v", err)
	}
	if exifSegment(p.data) != nil {
		t.Error("EXIF survived normalisation — GPS coordinates would be published")
	}
	if jpegOrientation(p.data) != 1 {
		t.Error("orientation tag survived, so the rotation would be applied twice")
	}
}

func TestNormalisePNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, landscape()); err != nil {
		t.Fatal(err)
	}
	p, err := normalisePhoto(buf.Bytes())
	if err != nil {
		t.Fatalf("normalisePhoto: %v", err)
	}
	if p.ext != ".png" || p.contentType != "image/png" {
		t.Errorf("got ext %q type %q, want .png/image/png", p.ext, p.contentType)
	}
}

func TestNormaliseRejectsNonImages(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"text", []byte("this is not an image")},
		{"truncated jpeg", []byte{0xFF, 0xD8, 0xFF}},
	} {
		if _, err := normalisePhoto(tc.data); err == nil {
			t.Errorf("%s: normalisePhoto succeeded, want an error", tc.name)
		}
	}
}

func darkAt(img image.Image, x, y int) bool {
	r, g, b, _ := img.At(x, y).RGBA()
	return r>>8 < 100 && g>>8 < 100 && b>>8 < 100
}

// A byte limit does not bound the decoded size: a small file can claim enormous
// dimensions, and decoding it is what exhausts the machine.
func TestNormaliseRejectsHugeImages(t *testing.T) {
	original := maxPixels
	maxPixels = 8*4 - 1 // just under the fixture
	defer func() { maxPixels = original }()

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, landscape(), nil); err != nil {
		t.Fatal(err)
	}
	_, err := normalisePhoto(buf.Bytes())
	if err == nil {
		t.Fatal("normalisePhoto accepted an over-sized image")
	}
	if !strings.Contains(err.Error(), "megapixels") {
		t.Errorf("error = %v, want it to mention megapixels", err)
	}
}
