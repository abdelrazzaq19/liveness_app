package imaging

import (
	"encoding/binary"
	"image"
)

// Orientation is the EXIF orientation tag.
//
// A camera records how the sensor was held rather than rotating the pixels, so
// a portrait selfie arrives as a landscape image plus a tag saying which way is
// up. Ignoring it feeds the detector a sideways face.
type Orientation uint16

const (
	OrientationNormal     Orientation = 1
	OrientationFlipH      Orientation = 2
	OrientationRotate180  Orientation = 3
	OrientationFlipV      Orientation = 4
	OrientationTranspose  Orientation = 5
	OrientationRotate90   Orientation = 6
	OrientationTransverse Orientation = 7
	OrientationRotate270  Orientation = 8
)

// swapsAxes reports whether the orientation exchanges width and height.
func (o Orientation) swapsAxes() bool {
	return o >= OrientationTranspose && o <= OrientationRotate270
}

// exifTagOrientation is the TIFF tag number for orientation.
const exifTagOrientation = 0x0112

// exifOrientation reads the orientation tag out of a JPEG.
//
// This is a deliberately narrow parser: it walks to IFD0, finds one tag, and
// gives up on anything it does not recognise. A full EXIF library would be a
// dependency and a much larger attack surface for one 16-bit value.
//
// Any malformed input yields OrientationNormal, which is also the correct
// answer for a file with no EXIF at all.
func exifOrientation(raw []byte) Orientation {
	app1, ok := findEXIFSegment(raw)
	if !ok {
		return OrientationNormal
	}
	return orientationFromTIFF(app1)
}

// findEXIFSegment walks the JPEG marker chain and returns the TIFF payload of
// the APP1 segment.
func findEXIFSegment(raw []byte) ([]byte, bool) {
	// SOI.
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return nil, false
	}

	for i := 2; i+4 <= len(raw); {
		if raw[i] != 0xFF {
			return nil, false
		}
		marker := raw[i+1]

		// Padding fill bytes; skip them one at a time.
		if marker == 0xFF {
			i++
			continue
		}
		// Start of scan: pixel data follows, and EXIF never does.
		if marker == 0xDA {
			return nil, false
		}
		// Standalone markers carry no length.
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD9) {
			i += 2
			continue
		}

		length := int(binary.BigEndian.Uint16(raw[i+2 : i+4]))
		if length < 2 || i+2+length > len(raw) {
			return nil, false
		}
		payload := raw[i+4 : i+2+length]

		const exifHeader = "Exif\x00\x00"
		if marker == 0xE1 && len(payload) > len(exifHeader) &&
			string(payload[:len(exifHeader)]) == exifHeader {
			return payload[len(exifHeader):], true
		}

		i += 2 + length
	}
	return nil, false
}

// orientationFromTIFF reads tag 0x0112 out of IFD0.
func orientationFromTIFF(tiff []byte) Orientation {
	// TIFF header: byte order, magic 42, offset to IFD0.
	if len(tiff) < 8 {
		return OrientationNormal
	}

	var order binary.ByteOrder
	switch {
	case tiff[0] == 'I' && tiff[1] == 'I':
		order = binary.LittleEndian
	case tiff[0] == 'M' && tiff[1] == 'M':
		order = binary.BigEndian
	default:
		return OrientationNormal
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return OrientationNormal
	}

	offset := int(order.Uint32(tiff[4:8]))
	if offset < 8 || offset+2 > len(tiff) {
		return OrientationNormal
	}

	count := int(order.Uint16(tiff[offset : offset+2]))
	entries := tiff[offset+2:]

	const entrySize = 12
	if count <= 0 || count*entrySize > len(entries) {
		return OrientationNormal
	}

	for i := 0; i < count; i++ {
		e := entries[i*entrySize : (i+1)*entrySize]
		if order.Uint16(e[0:2]) != exifTagOrientation {
			continue
		}
		// Type 3 is SHORT; the value sits inline in the first two bytes of the
		// value field. Anything else is not an orientation we understand.
		if order.Uint16(e[2:4]) != 3 {
			return OrientationNormal
		}

		o := Orientation(order.Uint16(e[8:10]))
		if o < OrientationNormal || o > OrientationRotate270 {
			return OrientationNormal
		}
		return o
	}
	return OrientationNormal
}

// ApplyOrientation returns the image rotated and flipped so that it is upright.
//
// The identity case returns the original image untouched, so the common path
// costs nothing.
func ApplyOrientation(img image.Image, o Orientation) image.Image {
	if o <= OrientationNormal || o > OrientationRotate270 {
		return img
	}

	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	dstW, dstH := w, h
	if o.swapsAxes() {
		dstW, dstH = h, w
	}

	out := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		for x := 0; x < dstW; x++ {
			sx, sy := sourcePixel(o, x, y, w, h)
			out.Set(x, y, img.At(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return out
}

// sourcePixel maps a destination coordinate back to the source it comes from.
func sourcePixel(o Orientation, x, y, w, h int) (int, int) {
	switch o {
	case OrientationFlipH:
		return w - 1 - x, y
	case OrientationRotate180:
		return w - 1 - x, h - 1 - y
	case OrientationFlipV:
		return x, h - 1 - y
	case OrientationTranspose:
		return y, x
	case OrientationRotate90:
		return y, h - 1 - x
	case OrientationTransverse:
		return w - 1 - y, h - 1 - x
	case OrientationRotate270:
		return w - 1 - y, x
	default:
		return x, y
	}
}
