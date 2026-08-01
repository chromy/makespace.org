package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
)

// Uploads are re-encoded rather than stored as sent. That strips EXIF — which
// on a phone photo carries the GPS coordinates of whoever took it, and this
// bucket is public — and it proves the bytes really are an image before they
// are given a public URL.
//
// Orientation is the catch. Phones store the sensor image unrotated and record
// how to turn it in EXIF, so stripping the tag naively lays every portrait
// photo on its side. applyOrientation bakes the rotation into the pixels first.

const (
	maxPhotoBytes = 20 << 20 // per photo
	maxPhotos     = 8
	jpegQuality   = 88
)

// maxPixels caps the decoded size, which the byte limit does not: a 20 MB JPEG
// can hold 50 megapixels, and decoding that costs ~200 MB of RGBA — twice over
// while rotating. The machine has 512 MB. A variable so tests can lower it.
var maxPixels = 30_000_000

type photo struct {
	data        []byte
	ext         string
	contentType string
}

func normalisePhoto(raw []byte) (photo, error) {
	if len(raw) == 0 {
		return photo{}, fmt.Errorf("empty file")
	}
	// Check the dimensions from the header before decoding any pixels.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return photo{}, fmt.Errorf("not a readable image: %w", err)
	}
	if pixels := cfg.Width * cfg.Height; pixels > maxPixels {
		return photo{}, fmt.Errorf("it is %d megapixels, and the limit is %d",
			pixels/1_000_000, maxPixels/1_000_000)
	}

	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return photo{}, fmt.Errorf("not a readable image: %w", err)
	}

	switch format {
	case "jpeg":
		img = applyOrientation(img, jpegOrientation(raw))
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return photo{}, fmt.Errorf("re-encoding jpeg: %w", err)
		}
		return photo{data: buf.Bytes(), ext: ".jpg", contentType: "image/jpeg"}, nil
	case "png":
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return photo{}, fmt.Errorf("re-encoding png: %w", err)
		}
		return photo{data: buf.Bytes(), ext: ".png", contentType: "image/png"}, nil
	default:
		return photo{}, fmt.Errorf("unsupported image format %q — send a JPEG or PNG", format)
	}
}

// applyOrientation rewrites the pixels for EXIF orientations 2-8 so the image
// reads correctly with no metadata attached.
func applyOrientation(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 { // the four transposed cases swap the axes
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch orientation {
			case 2: // flip horizontal
				nx, ny = w-1-x, y
			case 3: // rotate 180
				nx, ny = w-1-x, h-1-y
			case 4: // flip vertical
				nx, ny = x, h-1-y
			case 5: // transpose
				nx, ny = y, x
			case 6: // rotate 90 clockwise
				nx, ny = h-1-y, x
			case 7: // transverse
				nx, ny = h-1-y, w-1-x
			case 8: // rotate 90 anticlockwise
				nx, ny = y, w-1-x
			}
			dst.Set(nx, ny, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// jpegOrientation digs the EXIF orientation tag out of a JPEG, returning 1
// (meaning "as-is") whenever it is absent or the metadata is malformed. It
// walks JPEG segments to the APP1/Exif block, then the TIFF header and IFD0
// inside it; anything unexpected ends the walk rather than erroring, since a
// missing tag is entirely normal.
func jpegOrientation(raw []byte) int {
	const orientationTag = 0x0112

	exif := exifSegment(raw)
	if len(exif) < 8 {
		return 1
	}

	var order binary.ByteOrder
	switch {
	case bytes.HasPrefix(exif, []byte("II")):
		order = binary.LittleEndian
	case bytes.HasPrefix(exif, []byte("MM")):
		order = binary.BigEndian
	default:
		return 1
	}

	ifdOffset := order.Uint32(exif[4:8])
	if int(ifdOffset)+2 > len(exif) {
		return 1
	}
	entries := int(order.Uint16(exif[ifdOffset : ifdOffset+2]))
	for i := range entries {
		// Each IFD entry is 12 bytes: tag, type, count, then value or offset.
		at := int(ifdOffset) + 2 + i*12
		if at+12 > len(exif) {
			return 1
		}
		if order.Uint16(exif[at:at+2]) != orientationTag {
			continue
		}
		v := int(order.Uint16(exif[at+8 : at+10]))
		if v < 1 || v > 8 {
			return 1
		}
		return v
	}
	return 1
}

// exifSegment returns the TIFF block inside the APP1 Exif marker, or nil.
func exifSegment(raw []byte) []byte {
	if !bytes.HasPrefix(raw, []byte{0xFF, 0xD8}) { // SOI
		return nil
	}
	for i := 2; i+4 <= len(raw); {
		if raw[i] != 0xFF {
			return nil
		}
		marker := raw[i+1]
		if marker == 0xDA || marker == 0xD9 { // start of scan, end of image
			return nil
		}
		size := int(binary.BigEndian.Uint16(raw[i+2 : i+4]))
		if size < 2 || i+2+size > len(raw) {
			return nil
		}
		if marker == 0xE1 { // APP1
			body := raw[i+4 : i+2+size]
			if header := []byte("Exif\x00\x00"); bytes.HasPrefix(body, header) {
				return body[len(header):]
			}
		}
		i += 2 + size
	}
	return nil
}
