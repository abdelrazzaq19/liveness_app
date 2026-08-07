package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"testing"

	"github.com/ziad/liveness-verifier/internal/biometric"
)

// ---------------------------------------------------------------- helpers ---

func testLimits() Limits {
	return Limits{MaxBytes: 4 << 20, MaxPixels: 4_000_000}
}

// gradient builds a deterministic image with structure in both axes, so that
// blur and hashing have something to work with.
func gradient(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x * 255) / max(w-1, 1)),
				G: uint8((y * 255) / max(h-1, 1)),
				B: uint8(((x + y) * 255) / max(w+h-2, 1)),
				A: 255,
			})
		}
	}
	return img
}

// checkerboard has maximal high-frequency energy: the sharpest possible input.
func checkerboard(w, h, cell int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8(0)
			if ((x/cell)+(y/cell))%2 == 0 {
				v = 255
			}
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return img
}

// textured builds an image whose energy is spread across many spatial
// frequencies, the way a photograph's is.
//
// A smooth gradient is a poor perceptual-hash fixture: nearly all of its DCT
// energy sits in the first few coefficients, leaving the rest clustered around
// zero, where a hair of noise flips many bits at once. Values are also kept
// well inside [0,255] so that a brightness shift changes the level rather than
// clipping and changing the structure.
func textured(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		fy := float64(y) / float64(h)
		for x := 0; x < w; x++ {
			fx := float64(x) / float64(w)

			v := 0.5 +
				0.20*math.Sin(2*math.Pi*3*fx) +
				0.15*math.Sin(2*math.Pi*5*fy+1.1) +
				0.10*math.Sin(2*math.Pi*11*(fx+fy)) +
				0.05*math.Sin(2*math.Pi*23*fx*fy)

			g := uint8(30 + v*160)
			img.SetRGBA(x, y, color.RGBA{R: g, G: g, B: g, A: 255})
		}
	}
	return img
}

func solid(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// withEXIF splices an APP1 segment carrying one orientation tag into a JPEG,
// right after the SOI marker. Building it by hand keeps the test independent of
// whatever encoder produced the original bytes.
func withEXIF(t *testing.T, jpegBytes []byte, o Orientation) []byte {
	t.Helper()
	if len(jpegBytes) < 2 || jpegBytes[0] != 0xFF || jpegBytes[1] != 0xD8 {
		t.Fatal("input is not a JPEG")
	}

	tiff := make([]byte, 0, 26)
	tiff = append(tiff, 'I', 'I')                     // little endian
	tiff = binary.LittleEndian.AppendUint16(tiff, 42) // magic
	tiff = binary.LittleEndian.AppendUint32(tiff, 8)  // offset of IFD0
	tiff = binary.LittleEndian.AppendUint16(tiff, 1)  // one entry
	tiff = binary.LittleEndian.AppendUint16(tiff, exifTagOrientation)
	tiff = binary.LittleEndian.AppendUint16(tiff, 3) // type SHORT
	tiff = binary.LittleEndian.AppendUint32(tiff, 1) // count
	tiff = binary.LittleEndian.AppendUint16(tiff, uint16(o))
	tiff = append(tiff, 0, 0)                        // pad the 4-byte value field
	tiff = binary.LittleEndian.AppendUint32(tiff, 0) // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff...)

	segment := []byte{0xFF, 0xE1}
	segment = binary.BigEndian.AppendUint16(segment, uint16(len(payload)+2))
	segment = append(segment, payload...)

	out := make([]byte, 0, len(jpegBytes)+len(segment))
	out = append(out, jpegBytes[:2]...)
	out = append(out, segment...)
	out = append(out, jpegBytes[2:]...)
	return out
}

// ----------------------------------------------------------------- decode ---

func TestDecodeAcceptsJPEGAndPNG(t *testing.T) {
	src := gradient(64, 48)

	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"png", encodePNG(t, src)},
		{"jpeg", encodeJPEG(t, src)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode(bytes.NewReader(tt.data), testLimits())
			if err != nil {
				t.Fatalf("Decode() returned an unexpected error: %v", err)
			}
			if b := got.Bounds(); b.Dx() != 64 || b.Dy() != 48 {
				t.Errorf("decoded %dx%d, want 64x48", b.Dx(), b.Dy())
			}
		})
	}
}

func TestDecodeEnforcesByteLimit(t *testing.T) {
	data := encodePNG(t, gradient(200, 200))

	limits := testLimits()
	limits.MaxBytes = int64(len(data) - 1)

	_, err := Decode(bytes.NewReader(data), limits)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("Decode() error = %v, want ErrTooLarge", err)
	}
}

// A few kilobytes of PNG can declare an enormous canvas. The limit has to be
// enforced from the header, before any pixels are decoded.
func TestDecodeEnforcesPixelLimitFromHeader(t *testing.T) {
	data := encodePNG(t, solid(1000, 1000, color.RGBA{A: 255}))

	limits := testLimits()
	limits.MaxPixels = 999_999

	_, err := Decode(bytes.NewReader(data), limits)
	if !errors.Is(err, ErrTooManyPixels) {
		t.Errorf("Decode() error = %v, want ErrTooManyPixels", err)
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	valid := encodePNG(t, gradient(32, 32))

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrUnsupportedFormat},
		{"not an image", []byte("this is plain text, not an image at all"), ErrUnsupportedFormat},
		{"gif is not accepted", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), ErrUnsupportedFormat},
		{"truncated png", valid[:len(valid)/2], ErrCorrupt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Decode(bytes.NewReader(tt.data), testLimits())
			if !errors.Is(err, tt.want) {
				t.Errorf("Decode() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestDecodeRejectsUnsetLimits(t *testing.T) {
	data := encodePNG(t, gradient(16, 16))

	for _, tt := range []struct {
		name   string
		limits Limits
	}{
		{"no byte limit", Limits{MaxPixels: 1000}},
		{"no pixel limit", Limits{MaxBytes: 1000}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode(bytes.NewReader(data), tt.limits); err == nil {
				t.Error("Decode() accepted unset limits, want an error")
			}
		})
	}
}

// A malformed upload must be an error, never a panic: this decoder sits
// directly behind an unauthenticated-shaped request body.
func FuzzDecode(f *testing.F) {
	f.Add(encodePNG(f2t(f), gradient(8, 8)))
	f.Add([]byte("\xff\xd8\xff\xe0"))
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Only the absence of a panic matters; every outcome is acceptable.
		_, _ = Decode(bytes.NewReader(data), Limits{MaxBytes: 1 << 20, MaxPixels: 1 << 20})
	})
}

// f2t adapts *testing.F where a helper wants *testing.T.
func f2t(f *testing.F) *testing.T {
	f.Helper()
	return &testing.T{}
}

// ------------------------------------------------------------------- exif ---

func TestEXIFOrientationIsRead(t *testing.T) {
	base := encodeJPEG(t, gradient(32, 16))

	for _, o := range []Orientation{
		OrientationNormal, OrientationFlipH, OrientationRotate180, OrientationFlipV,
		OrientationTranspose, OrientationRotate90, OrientationTransverse, OrientationRotate270,
	} {
		if got := exifOrientation(withEXIF(t, base, o)); got != o {
			t.Errorf("exifOrientation() = %d, want %d", got, o)
		}
	}
}

// Anything unparseable must read as "upright", which is also the right answer
// for a file with no EXIF at all.
func TestEXIFOrientationDefaultsToNormal(t *testing.T) {
	base := encodeJPEG(t, gradient(16, 16))

	tests := []struct {
		name string
		data []byte
	}{
		{"no exif segment", base},
		{"not a jpeg", []byte("plain bytes")},
		{"empty", nil},
		{"truncated exif", withEXIF(t, base, OrientationRotate90)[:10]},
		{"out of range tag value", withEXIF(t, base, Orientation(99))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exifOrientation(tt.data); got != OrientationNormal {
				t.Errorf("exifOrientation() = %d, want %d", got, OrientationNormal)
			}
		})
	}
}

// Orientations 5 to 8 exchange width and height; the others must not.
func TestApplyOrientationDimensions(t *testing.T) {
	src := gradient(40, 20)

	tests := []struct {
		o    Orientation
		w, h int
	}{
		{OrientationNormal, 40, 20},
		{OrientationFlipH, 40, 20},
		{OrientationRotate180, 40, 20},
		{OrientationFlipV, 40, 20},
		{OrientationTranspose, 20, 40},
		{OrientationRotate90, 20, 40},
		{OrientationTransverse, 20, 40},
		{OrientationRotate270, 20, 40},
	}

	for _, tt := range tests {
		got := ApplyOrientation(src, tt.o).Bounds()
		if got.Dx() != tt.w || got.Dy() != tt.h {
			t.Errorf("orientation %d gave %dx%d, want %dx%d", tt.o, got.Dx(), got.Dy(), tt.w, tt.h)
		}
	}
}

// A marked corner is the clearest way to prove each transform moves pixels the
// way the EXIF spec says it should.
func TestApplyOrientationMovesTopLeftCorner(t *testing.T) {
	const w, h = 4, 2
	src := solid(w, h, color.RGBA{A: 255})
	marker := color.RGBA{R: 255, A: 255}
	src.SetRGBA(0, 0, marker)

	tests := []struct {
		o         Orientation
		wantX     int
		wantY     int
		swapsAxes bool
	}{
		{OrientationFlipH, w - 1, 0, false},
		{OrientationRotate180, w - 1, h - 1, false},
		{OrientationFlipV, 0, h - 1, false},
		{OrientationTranspose, 0, 0, true},
		{OrientationRotate90, h - 1, 0, true},
		{OrientationTransverse, h - 1, w - 1, true},
		{OrientationRotate270, 0, w - 1, true},
	}

	for _, tt := range tests {
		out := ApplyOrientation(src, tt.o)
		r, _, _, _ := out.At(tt.wantX, tt.wantY).RGBA()
		if r>>8 != 255 {
			t.Errorf("orientation %d: marker is not at (%d,%d)", tt.o, tt.wantX, tt.wantY)
		}
	}
}

func TestDecodeAppliesEXIFOrientation(t *testing.T) {
	// Landscape source; orientation 6 means the camera was held in portrait.
	base := encodeJPEG(t, gradient(64, 32))

	got, err := Decode(bytes.NewReader(withEXIF(t, base, OrientationRotate90)), testLimits())
	if err != nil {
		t.Fatalf("Decode() returned an unexpected error: %v", err)
	}
	if b := got.Bounds(); b.Dx() != 32 || b.Dy() != 64 {
		t.Errorf("decoded %dx%d, want the image rotated to 32x64", b.Dx(), b.Dy())
	}
}

// ---------------------------------------------------------------- quality ---

func TestMeasureBrightness(t *testing.T) {
	tests := []struct {
		name string
		img  image.Image
		want float64
	}{
		{"black", solid(16, 16, color.RGBA{A: 255}), 0},
		{"white", solid(16, 16, color.RGBA{R: 255, G: 255, B: 255, A: 255}), 255},
		{"mid grey", solid(16, 16, color.RGBA{R: 128, G: 128, B: 128, A: 255}), 128},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Measure(tt.img).Brightness
			if math.Abs(got-tt.want) > 1.5 {
				t.Errorf("Brightness = %.1f, want about %.1f", got, tt.want)
			}
		})
	}
}

// The blur metric only has to order images correctly; the absolute scale is
// what calibration is for.
func TestLaplacianVarianceOrdersImagesBySharpness(t *testing.T) {
	flat := Measure(solid(64, 64, color.RGBA{R: 128, G: 128, B: 128, A: 255})).LaplacianVariance
	smooth := Measure(gradient(64, 64)).LaplacianVariance
	sharp := Measure(checkerboard(64, 64, 4)).LaplacianVariance

	if !(flat <= smooth && smooth < sharp) {
		t.Errorf("expected flat <= gradient < checkerboard, got %.2f, %.2f, %.2f", flat, smooth, sharp)
	}
	if flat > 1e-6 {
		t.Errorf("a flat image has edge variance %.6f, want approximately 0", flat)
	}
}

func TestMeasureHandlesTinyImages(t *testing.T) {
	for _, size := range []int{1, 2} {
		m := Measure(solid(size, size, color.RGBA{R: 200, G: 200, B: 200, A: 255}))
		if m.LaplacianVariance != 0 {
			t.Errorf("%dx%d image gave edge variance %.2f, want 0", size, size, m.LaplacianVariance)
		}
		if m.Width != size || m.Height != size {
			t.Errorf("measured %dx%d, want %dx%d", m.Width, m.Height, size, size)
		}
	}
}

func TestGateRejectionReasons(t *testing.T) {
	gate := Gate{
		MinLaplacianVariance: 80,
		MinBrightness:        40,
		MaxBrightness:        215,
		MinFaceWidth:         112,
	}
	good := Metrics{Width: 640, Height: 480, Brightness: 120, LaplacianVariance: 500}

	tests := []struct {
		name      string
		metrics   Metrics
		faceWidth float64
		wantHint  string
	}{
		{"passes", good, 200, ""},
		{"face check skipped when width is zero", good, 0, ""},
		{"no pixels", Metrics{}, 0, "no pixels"},
		{"blurred", Metrics{Width: 640, Height: 480, Brightness: 120, LaplacianVariance: 10}, 200, "blurred"},
		{"too dark", Metrics{Width: 640, Height: 480, Brightness: 12, LaplacianVariance: 500}, 200, "too dark"},
		{"too bright", Metrics{Width: 640, Height: 480, Brightness: 240, LaplacianVariance: 500}, 200, "too bright"},
		{"face too small", good, 50, "face too small"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := gate.Check(tt.metrics, tt.faceWidth)

			if tt.wantHint == "" {
				if err != nil {
					t.Fatalf("Check() rejected a usable frame: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("Check() accepted a frame it should reject (%s)", tt.wantHint)
			}
			if !errors.Is(err, ErrLowQuality) {
				t.Errorf("Check() error = %v, want it to wrap ErrLowQuality", err)
			}
			if !bytes.Contains([]byte(err.Error()), []byte(tt.wantHint)) {
				t.Errorf("error does not explain the reason %q: %v", tt.wantHint, err)
			}
		})
	}
}

// ------------------------------------------------------------------ align ---

// keypoints builds a Keypoints value from five x,y pairs.
func keypoints(pts [5][2]float64) biometric.Keypoints {
	var k biometric.Keypoints
	for i, p := range pts {
		k[i] = biometric.Point{X: p[0], Y: p[1]}
	}
	return k
}

// A transform applied and then estimated must come back exactly; anything less
// means every aligned crop is subtly wrong.
func TestEstimateSimilarityRecoversAKnownTransform(t *testing.T) {
	src := keypoints([5][2]float64{{10, 10}, {40, 12}, {25, 30}, {14, 45}, {38, 44}})

	tests := []struct {
		name           string
		scale, degrees float64
		tx, ty         float64
	}{
		{"identity", 1, 0, 0, 0},
		{"translation only", 1, 0, 25, -17},
		{"uniform scale", 2.5, 0, 0, 0},
		{"rotation", 1, 30, 0, 0},
		{"scale rotation and translation", 1.7, -45, 100, 60},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rad := tt.degrees * math.Pi / 180
			want := Similarity{
				A:  tt.scale * math.Cos(rad),
				B:  tt.scale * math.Sin(rad),
				Tx: tt.tx,
				Ty: tt.ty,
			}

			var dst biometric.Keypoints
			for i := range src {
				dst[i] = want.Apply(src[i])
			}

			got, err := EstimateSimilarity(src, dst)
			if err != nil {
				t.Fatalf("EstimateSimilarity() returned an unexpected error: %v", err)
			}

			for _, c := range []struct {
				name      string
				got, want float64
			}{
				{"A", got.A, want.A},
				{"B", got.B, want.B},
				{"Tx", got.Tx, want.Tx},
				{"Ty", got.Ty, want.Ty},
			} {
				if math.Abs(c.got-c.want) > 1e-9 {
					t.Errorf("%s = %.12f, want %.12f", c.name, c.got, c.want)
				}
			}
			if math.Abs(got.Scale()-tt.scale) > 1e-9 {
				t.Errorf("Scale() = %.12f, want %.12f", got.Scale(), tt.scale)
			}
		})
	}
}

func TestSimilarityInvertRoundTrips(t *testing.T) {
	s := Similarity{A: 1.5 * math.Cos(0.4), B: 1.5 * math.Sin(0.4), Tx: 30, Ty: -12}

	inv, err := s.Invert()
	if err != nil {
		t.Fatalf("Invert() returned an unexpected error: %v", err)
	}

	for _, p := range []biometric.Point{{X: 0, Y: 0}, {X: 17, Y: -4}, {X: -100, Y: 250}} {
		back := inv.Apply(s.Apply(p))
		if math.Abs(back.X-p.X) > 1e-9 || math.Abs(back.Y-p.Y) > 1e-9 {
			t.Errorf("round trip of %+v gave %+v", p, back)
		}
	}
}

func TestEstimateSimilarityRejectsDegenerateLandmarks(t *testing.T) {
	// Every point identical: no scale or rotation can be recovered.
	same := keypoints([5][2]float64{{5, 5}, {5, 5}, {5, 5}, {5, 5}, {5, 5}})

	if _, err := EstimateSimilarity(same, arcFaceTemplate); !errors.Is(err, ErrDegenerateLandmarks) {
		t.Errorf("EstimateSimilarity() error = %v, want ErrDegenerateLandmarks", err)
	}

	if _, err := (Similarity{}).Invert(); !errors.Is(err, ErrDegenerateLandmarks) {
		t.Errorf("Invert() of a collapsed transform error = %v, want ErrDegenerateLandmarks", err)
	}
}

// The real proof: markers painted at the source keypoints must land on the
// template positions in the aligned crop.
func TestAlignFacePlacesLandmarksOnTheTemplate(t *testing.T) {
	const (
		srcW, srcH = 400, 400
		markerSize = 8
	)

	// An arbitrary face pose: rotated, off-centre, and at a different scale
	// from the template.
	src := keypoints([5][2]float64{
		{150, 170}, {250, 150}, {205, 215}, {165, 275}, {245, 258},
	})

	img := solid(srcW, srcH, color.RGBA{R: 30, G: 30, B: 30, A: 255})
	markers := [biometric.KeypointCount]color.RGBA{
		{R: 255, A: 255},
		{G: 255, A: 255},
		{B: 255, A: 255},
		{R: 255, G: 255, A: 255},
		{R: 255, B: 255, A: 255},
	}
	for i, p := range src {
		for dy := -markerSize; dy <= markerSize; dy++ {
			for dx := -markerSize; dx <= markerSize; dx++ {
				img.SetRGBA(int(p.X)+dx, int(p.Y)+dy, markers[i])
			}
		}
	}

	out, err := AlignFace(img, src, TemplateSize)
	if err != nil {
		t.Fatalf("AlignFace() returned an unexpected error: %v", err)
	}
	if b := out.Bounds(); b.Dx() != TemplateSize || b.Dy() != TemplateSize {
		t.Fatalf("aligned crop is %dx%d, want %dx%d", b.Dx(), b.Dy(), TemplateSize, TemplateSize)
	}

	for i, want := range arcFaceTemplate {
		x, y := int(math.Round(want.X)), int(math.Round(want.Y))
		got := out.RGBAAt(x, y)

		if nearest := nearestMarker(got, markers); nearest != i {
			t.Errorf("keypoint %d: template position (%d,%d) holds %v, which resembles marker %d (%v)",
				i, x, y, got, nearest, markers[nearest])
		}
	}
}

// nearestMarker returns the index of the marker colour c most resembles.
//
// Comparison is by direction rather than distance: bilinear resampling blends a
// marker towards the dark background, so the sample is a dimmer version of the
// right colour. Raw Euclidean distance would call a dim magenta "red", while
// the normalised direction still points at magenta.
func nearestMarker(c color.RGBA, markers [biometric.KeypointCount]color.RGBA) int {
	normalise := func(r, g, b float64) (float64, float64, float64) {
		n := math.Sqrt(r*r + g*g + b*b)
		if n == 0 {
			return 0, 0, 0
		}
		return r / n, g / n, b / n
	}

	cr, cg, cb := normalise(float64(c.R), float64(c.G), float64(c.B))

	best, bestDist := -1, math.MaxFloat64
	for i, m := range markers {
		mr, mg, mb := normalise(float64(m.R), float64(m.G), float64(m.B))
		d := (cr-mr)*(cr-mr) + (cg-mg)*(cg-mg) + (cb-mb)*(cb-mb)
		if d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
}

func TestAlignFaceScalesTheTemplate(t *testing.T) {
	src := keypoints([5][2]float64{{150, 170}, {250, 150}, {205, 215}, {165, 275}, {245, 258}})
	img := gradient(400, 400)

	for _, size := range []int{TemplateSize, 224} {
		out, err := AlignFace(img, src, size)
		if err != nil {
			t.Fatalf("AlignFace(size=%d) returned an unexpected error: %v", size, err)
		}
		if b := out.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Errorf("aligned crop is %dx%d, want %dx%d", b.Dx(), b.Dy(), size, size)
		}
	}
}

func TestAlignFaceRejectsBadArguments(t *testing.T) {
	src := keypoints([5][2]float64{{150, 170}, {250, 150}, {205, 215}, {165, 275}, {245, 258}})

	if _, err := AlignFace(nil, src, TemplateSize); err == nil {
		t.Error("AlignFace() accepted a nil image, want an error")
	}
	if _, err := AlignFace(gradient(64, 64), src, 0); err == nil {
		t.Error("AlignFace() accepted a zero size, want an error")
	}
}

// ------------------------------------------------------------------ phash ---

func TestPHashIsStableForTheSameImage(t *testing.T) {
	img := textured(320, 240)

	if a, b := PHash(img), PHash(img); a != b {
		t.Errorf("PHash is not deterministic: %016x then %016x", a, b)
	}
	if d := HammingDistance(PHash(img), PHash(img)); d != 0 {
		t.Errorf("distance to itself = %d, want 0", d)
	}
}

// The case this is built for: a replayed still image, where consecutive frames
// differ only by sensor noise and compression.
func TestPHashIsInsensitiveToNoiseAndBrightness(t *testing.T) {
	base := textured(320, 240)

	noisy := image.NewRGBA(base.Bounds())
	copy(noisy.Pix, base.Pix)
	for i := 0; i < len(noisy.Pix); i += 97 {
		if noisy.Pix[i] < 250 {
			noisy.Pix[i] += 3
		}
	}

	brighter := image.NewRGBA(base.Bounds())
	for i, v := range base.Pix {
		if i%4 == 3 {
			brighter.Pix[i] = v
			continue
		}
		brighter.Pix[i] = uint8(min(int(v)+20, 255))
	}

	for _, tt := range []struct {
		name string
		img  image.Image
	}{
		{"sensor noise", noisy},
		{"brightness shift", brighter},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := HammingDistance(PHash(base), PHash(tt.img))
			if d >= 5 {
				t.Errorf("distance = %d, want under 5 for a near-identical frame", d)
			}
		})
	}
}

// Two genuinely different scenes must be far apart, or the replay check would
// reject a live subject who simply moved.
func TestPHashSeparatesDifferentScenes(t *testing.T) {
	a := PHash(textured(320, 240))
	b := PHash(checkerboard(320, 240, 16))

	if d := HammingDistance(a, b); d < 10 {
		t.Errorf("distance between unrelated images = %d, want at least 10", d)
	}
}

// Scale invariance matters because frames arrive at whatever size the client's
// camera produced.
func TestPHashSurvivesRescaling(t *testing.T) {
	full := textured(320, 320)
	half := textured(160, 160)

	if d := HammingDistance(PHash(full), PHash(half)); d >= 8 {
		t.Errorf("distance across a 2x rescale = %d, want under 8", d)
	}
}

func TestHammingDistance(t *testing.T) {
	tests := []struct {
		a, b uint64
		want int
	}{
		{0, 0, 0},
		{0xFFFFFFFFFFFFFFFF, 0xFFFFFFFFFFFFFFFF, 0},
		{0, 1, 1},
		{0, 0xFFFFFFFFFFFFFFFF, 64},
		{0b1010, 0b0101, 4},
	}

	for _, tt := range tests {
		if got := HammingDistance(tt.a, tt.b); got != tt.want {
			t.Errorf("HammingDistance(%016x, %016x) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
