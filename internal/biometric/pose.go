package biometric

import (
	"errors"
	"fmt"
	"math"

	"gonum.org/v1/gonum/mat"
)

// ErrPoseUnavailable means the landmarks do not determine an orientation:
// they are collapsed, collinear, or otherwise degenerate.
var ErrPoseUnavailable = errors.New("biometric: head pose cannot be estimated from these landmarks")

// Pose is a head orientation in degrees.
//
// Sign conventions, all from the viewer's side of the camera:
//
//	Yaw   > 0  the nose swings towards the right of the image
//	Pitch > 0  the chin lifts, the face tips backwards
//	Roll  > 0  the head tilts so the top leans right
//
// Viewer-relative rather than subject-relative because that is what the pixels
// show. Whether "turn left" means this or its opposite is a question about the
// instruction shown to the user, and belongs with the challenge, not here.
type Pose struct {
	Yaw   float64 `json:"yaw"`
	Pitch float64 `json:"pitch"`
	Roll  float64 `json:"roll"`
}

// poseCorrespondence pairs a landmark index with its position on a canonical 3D
// face.
type poseCorrespondence struct {
	landmark int
	model    [3]float64
}

// poseModel is a canonical 3D face, in millimetres, in the image convention:
// x to the right, y downwards, z away from the camera.
//
// Depth is measured backwards from the nose, so the eye corners at 135 mm are
// the furthest into the head and the chin at 65 mm the shallowest. The widely
// copied version of this model writes those depths negative, which quietly
// means the opposite of what "z away from the camera" says and flips the sign
// of every recovered yaw.
//
// Only landmarks whose anatomy is certain are used. The dense model has 106
// points, but most sit on smooth curves where a small identification error
// slides the point a long way along the surface — and a wrong 3D
// correspondence tilts the whole estimate without ever failing.
//
// The nose tip is what makes the set solvable, and leaving it out was a real
// bug rather than a simplification. Without it six of the seven remaining
// points sit within 10 mm of one depth: a nearly planar set, and a weak
// perspective solve from nearly planar points is ill conditioned in the
// textbook way. On a real session that showed up as a pitch of 40 to 72 degrees
// for a subject sitting square to a laptop, and a yaw that never passed 22 even
// when they plainly turned their head.
//
// The tip is the only landmark on a face that protrudes. It carries the depth
// the other seven do not have, which is why the model this one derives from
// puts it at the origin with everything else behind it.
var poseModel = []poseCorrespondence{
	{NoseFirst + NoseTip, [3]float64{0, 0, 0}},
	{LeftEyeFirst + eyeCornerLeft, [3]float64{-225, -170, 135}},
	{LeftEyeFirst + eyeCornerRight, [3]float64{-75, -170, 125}},
	{RightEyeFirst + eyeCornerLeft, [3]float64{75, -170, 125}},
	{RightEyeFirst + eyeCornerRight, [3]float64{225, -170, 135}},
	{MouthFirst + mouthCornerLeft, [3]float64{-150, 150, 125}},
	{MouthFirst + mouthCornerRight, [3]float64{150, 150, 125}},
	{ContourFirst, [3]float64{0, 330, 65}},
}

// EstimatePose recovers the head orientation from dense landmarks.
//
// The projection model is scaled orthographic — weak perspective — rather than
// full perspective. That is a deliberate limitation: a perspective solution
// needs the camera's focal length, and an arbitrary webcam does not report one.
// Assuming a focal length is an approximation too, just a hidden one. At the
// distance a selfie is taken from, weak perspective costs a couple of degrees
// and asks for nothing the caller cannot supply.
func (l Landmarks106) EstimatePose() (Pose, error) {
	n := len(poseModel)

	// Centring both sets removes the translation, leaving only rotation and
	// scale to solve for.
	var meanImg Point
	var meanModel [3]float64
	for _, c := range poseModel {
		p := l[c.landmark]
		meanImg.X += p.X
		meanImg.Y += p.Y
		for k := 0; k < 3; k++ {
			meanModel[k] += c.model[k]
		}
	}
	meanImg.X /= float64(n)
	meanImg.Y /= float64(n)
	for k := 0; k < 3; k++ {
		meanModel[k] /= float64(n)
	}

	model := mat.NewDense(3, n, nil)
	image := mat.NewDense(2, n, nil)
	for i, c := range poseModel {
		p := l[c.landmark]
		image.Set(0, i, p.X-meanImg.X)
		image.Set(1, i, p.Y-meanImg.Y)
		for k := 0; k < 3; k++ {
			model.Set(k, i, c.model[k]-meanModel[k])
		}
	}

	// Under weak perspective the image is M·X for some 2x3 M whose rows are the
	// first two rows of the rotation, scaled. Least squares gives M directly.
	var gram mat.Dense
	gram.Mul(model, model.T())

	var inv mat.Dense
	if err := inv.Inverse(&gram); err != nil {
		return Pose{}, fmt.Errorf("%w: model points are degenerate: %v", ErrPoseUnavailable, err)
	}

	var cross mat.Dense
	cross.Mul(image, model.T())

	var m mat.Dense
	m.Mul(&cross, &inv)

	r1 := [3]float64{m.At(0, 0), m.At(0, 1), m.At(0, 2)}
	r2 := [3]float64{m.At(1, 0), m.At(1, 1), m.At(1, 2)}

	if norm(r1) < 1e-12 || norm(r2) < 1e-12 {
		return Pose{}, fmt.Errorf("%w: landmarks have collapsed", ErrPoseUnavailable)
	}

	rot, err := rotationFromRows(r1, r2)
	if err != nil {
		return Pose{}, err
	}
	return eulerFromRotation(rot), nil
}

// rotationFromRows builds the nearest rotation matrix to the two measured rows.
//
// The least-squares rows carry the scale and are not exactly orthonormal, since
// nothing in the fit required them to be. Normalising and taking a cross
// product gets close; projecting onto SO(3) with an SVD is what makes the
// result an actual rotation rather than something that merely resembles one.
func rotationFromRows(r1, r2 [3]float64) (*mat.Dense, error) {
	a := unit(r1)
	b := unit(r2)
	c := unit(crossProduct(a, b))

	if norm(c) < 1e-9 {
		return nil, fmt.Errorf("%w: the two measured axes are parallel", ErrPoseUnavailable)
	}

	approx := mat.NewDense(3, 3, []float64{
		a[0], a[1], a[2],
		b[0], b[1], b[2],
		c[0], c[1], c[2],
	})

	var svd mat.SVD
	if !svd.Factorize(approx, mat.SVDFull) {
		return nil, fmt.Errorf("%w: orthonormalisation failed", ErrPoseUnavailable)
	}

	var u, v mat.Dense
	svd.UTo(&u)
	svd.VTo(&v)

	var rot mat.Dense
	rot.Mul(&u, v.T())

	// A reflection has determinant -1 and is not a rotation. Flipping the sign
	// of the last singular direction gives the nearest proper rotation.
	if det3(&rot) < 0 {
		d := mat.NewDense(3, 3, []float64{1, 0, 0, 0, 1, 0, 0, 0, -1})
		var fixed mat.Dense
		fixed.Mul(&u, d)
		rot.Mul(&fixed, v.T())
	}
	return &rot, nil
}

// eulerFromRotation decomposes R = Ry(yaw)·Rx(pitch)·Rz(roll) into degrees.
//
// Yaw outermost because it is the angle the turn-your-head challenges measure,
// and this ordering keeps it well conditioned across the range they use.
func eulerFromRotation(r *mat.Dense) Pose {
	const gimbalEpsilon = 1e-6

	sinPitch := -r.At(1, 2)
	sinPitch = math.Max(-1, math.Min(1, sinPitch))
	pitch := math.Asin(sinPitch)

	cosPitch := math.Cos(pitch)

	var yaw, roll float64
	if math.Abs(cosPitch) < gimbalEpsilon {
		// Looking straight up or down: yaw and roll describe the same motion,
		// so the split between them is arbitrary. Attribute all of it to yaw.
		yaw = math.Atan2(-r.At(2, 0), r.At(0, 0))
		roll = 0
	} else {
		yaw = math.Atan2(r.At(0, 2), r.At(2, 2))
		roll = math.Atan2(r.At(1, 0), r.At(1, 1))
	}

	return Pose{
		Yaw:   degrees(yaw),
		Pitch: degrees(pitch),
		Roll:  degrees(roll),
	}
}

// rotationMatrix builds R = Ry(yaw)·Rx(pitch)·Rz(roll) from degrees.
//
// It is the exact inverse of eulerFromRotation, which is what lets the tests
// check a full round trip rather than a plausible-looking number.
func rotationMatrix(p Pose) *mat.Dense {
	sy, cy := math.Sincos(radians(p.Yaw))
	sp, cp := math.Sincos(radians(p.Pitch))
	sr, cr := math.Sincos(radians(p.Roll))

	return mat.NewDense(3, 3, []float64{
		cy*cr + sy*sp*sr, -cy*sr + sy*sp*cr, sy * cp,
		cp * sr, cp * cr, -sp,
		-sy*cr + cy*sp*sr, sy*sr + cy*sp*cr, cy * cp,
	})
}

func det3(m *mat.Dense) float64 {
	return m.At(0, 0)*(m.At(1, 1)*m.At(2, 2)-m.At(1, 2)*m.At(2, 1)) -
		m.At(0, 1)*(m.At(1, 0)*m.At(2, 2)-m.At(1, 2)*m.At(2, 0)) +
		m.At(0, 2)*(m.At(1, 0)*m.At(2, 1)-m.At(1, 1)*m.At(2, 0))
}

func norm(v [3]float64) float64 {
	return math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
}

func unit(v [3]float64) [3]float64 {
	n := norm(v)
	if n == 0 {
		return v
	}
	return [3]float64{v[0] / n, v[1] / n, v[2] / n}
}

func crossProduct(a, b [3]float64) [3]float64 {
	return [3]float64{
		a[1]*b[2] - a[2]*b[1],
		a[2]*b[0] - a[0]*b[2],
		a[0]*b[1] - a[1]*b[0],
	}
}

func degrees(rad float64) float64 { return rad * 180 / math.Pi }
func radians(deg float64) float64 { return deg * math.Pi / 180 }
