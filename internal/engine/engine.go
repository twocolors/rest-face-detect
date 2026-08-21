package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"sync"
	"unsafe"

	_ "image/jpeg"

	"github.com/ebitengine/purego"
	_ "golang.org/x/image/bmp"
)

type EmbeddingResult struct {
	Embedding []float32
	Preview   []byte
}

type Detection struct {
	Score     float32       `json:"score"`
	Box       [4]float32    `json:"box"`
	Landmarks [5][2]float32 `json:"landmarks"`
}

type detectResponse struct {
	Faces []Detection `json:"faces"`
}

type Engine struct {
	mu sync.Mutex

	ctx uintptr

	abiVersion func() int32
	load       func(string) uintptr
	free       func(uintptr)
	lastError  func(uintptr) string
	freeString func(uintptr)
	freeVec    func(uintptr)

	embedRGB    func(uintptr, *byte, int32, int32, *uintptr, *int32) int32
	detectJSON  func(uintptr, string) uintptr
	analyzeJSON func(uintptr, string) uintptr
}

const minEmbedSide = 192

func Load(libPath, modelPath string) (*Engine, error) {
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("model not found: %w", err)
	}
	if _, err := os.Stat(libPath); err != nil {
		return nil, fmt.Errorf("libfacedetect not found: %w", err)
	}

	lib, err := purego.Dlopen(libPath, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("dlopen %s: %w", libPath, err)
	}

	e := &Engine{}

	purego.RegisterLibFunc(&e.abiVersion, lib, "facedetect_capi_abi_version")
	purego.RegisterLibFunc(&e.load, lib, "facedetect_capi_load")
	purego.RegisterLibFunc(&e.free, lib, "facedetect_capi_free")
	purego.RegisterLibFunc(&e.lastError, lib, "facedetect_capi_last_error")
	purego.RegisterLibFunc(&e.freeString, lib, "facedetect_capi_free_string")
	purego.RegisterLibFunc(&e.freeVec, lib, "facedetect_capi_free_vec")
	purego.RegisterLibFunc(&e.embedRGB, lib, "facedetect_capi_embed_rgb")
	purego.RegisterLibFunc(&e.detectJSON, lib, "facedetect_capi_detect_path_json")
	purego.RegisterLibFunc(&e.analyzeJSON, lib, "facedetect_capi_analyze_path_json")

	abi := e.abiVersion()
	if abi < 1 {
		return nil, fmt.Errorf("unsupported face-detect ABI version: %d", abi)
	}

	e.ctx = e.load(modelPath)
	if e.ctx == 0 {
		return nil, fmt.Errorf("facedetect_capi_load failed for %q", modelPath)
	}

	return e, nil
}

func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ctx != 0 {
		e.free(e.ctx)
		e.ctx = 0
	}
}

func (e *Engine) Embeddings(data []byte) ([][]float32, error) {
	items, err := e.EmbeddingsWithPreview(data)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(items))
	for _, item := range items {
		out = append(out, item.Embedding)
	}
	return out, nil
}

func (e *Engine) EmbeddingsWithPreview(data []byte) ([]EmbeddingResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.ctx == 0 {
		return nil, errors.New("model not loaded")
	}
	img, err := decodeImage(data)
	if err != nil {
		return nil, fmt.Errorf("invalid image: %w", err)
	}
	dets, err := e.detectLocked(data)
	if err != nil {
		return nil, err
	}
	if len(dets) == 0 {
		return []EmbeddingResult{}, nil
	}

	out := make([]EmbeddingResult, 0, len(dets))
	for _, det := range dets {
		crop, err := cropFace(img, det)
		if err != nil {
			continue
		}
		rgb := imageToRGB(crop)
		embedding, err := e.embedRGBLocked(rgb, crop.Bounds().Dx(), crop.Bounds().Dy())
		if err != nil {
			continue
		}
		preview, err := encodePreview(crop)
		if err != nil {
			continue
		}
		out = append(out, EmbeddingResult{Embedding: embedding, Preview: preview})
	}
	return out, nil
}

type Analysis struct {
	Score  float32    `json:"score"`
	Box    [4]float32 `json:"box"`
	Age    float32    `json:"age"`
	Gender string     `json:"gender"`
}
type analyzeResponse struct {
	Faces []Analysis `json:"faces"`
}

func (e *Engine) Analyze(data []byte) ([]map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx == 0 {
		return nil, errors.New("model not loaded")
	}
	path, err := writeTempImage(data)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)
	ptr := e.analyzeJSON(e.ctx, path)
	if ptr == 0 {
		return nil, e.cError("analyze")
	}
	defer e.freeString(ptr)
	var doc analyzeResponse
	if err := json.Unmarshal([]byte(cString(ptr)), &doc); err != nil {
		return nil, errors.New("invalid analyze response")
	}
	out := make([]map[string]any, 0, len(doc.Faces))
	for i, f := range doc.Faces {
		out = append(out, map[string]any{
			"face": i + 1, "box": f.Box, "score": f.Score,
			"age": f.Age, "gender": f.Gender,
		})
	}
	return out, nil
}

func (e *Engine) detectLocked(data []byte) ([]Detection, error) {
	path, err := writeTempImage(data)
	if err != nil {
		return nil, err
	}
	defer os.Remove(path)

	ptr := e.detectJSON(e.ctx, path)
	if ptr == 0 {
		return nil, e.cError("detect")
	}
	defer e.freeString(ptr)

	doc := cString(ptr)
	var result detectResponse
	if err := json.Unmarshal([]byte(doc), &result); err != nil {
		return nil, fmt.Errorf("invalid detection JSON: %w", err)
	}

	return result.Faces, nil
}

func (e *Engine) embedRGBLocked(rgb []byte, width, height int) ([]float32, error) {
	if len(rgb) == 0 {
		return nil, errors.New("empty RGB image")
	}
	var vec uintptr
	var dim int32

	rc := e.embedRGB(
		e.ctx,
		&rgb[0],
		int32(width),
		int32(height),
		&vec,
		&dim,
	)
	if rc != 0 || vec == 0 || dim <= 0 {
		return nil, e.cError("embed")
	}
	defer e.freeVec(vec)

	src := unsafe.Slice((*float32)(unsafe.Pointer(vec)), int(dim))
	out := make([]float32, int(dim))
	copy(out, src)
	return out, nil
}

func (e *Engine) cError(op string) error {
	msg := ""
	if e.ctx != 0 {
		msg = e.lastError(e.ctx)
	}
	if msg == "" {
		msg = "unknown native error"
	}
	return fmt.Errorf("%s failed: %s", op, msg)
}

func decodeImage(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	return img, err
}

func cropFace(img image.Image, d Detection) (image.Image, error) {
	b := img.Bounds()

	x1 := int(math.Floor(float64(d.Box[0])))
	y1 := int(math.Floor(float64(d.Box[1])))
	x2 := int(math.Ceil(float64(d.Box[2])))
	y2 := int(math.Ceil(float64(d.Box[3])))

	w := x2 - x1
	h := y2 - y1
	if w <= 0 || h <= 0 {
		return nil, errors.New("invalid face box")
	}

	padX := int(float64(w) * 0.35)
	padY := int(float64(h) * 0.35)

	x1 -= padX
	y1 -= padY
	x2 += padX
	y2 += padY

	if x1 < b.Min.X {
		x1 = b.Min.X
	}
	if y1 < b.Min.Y {
		y1 = b.Min.Y
	}
	if x2 > b.Max.X {
		x2 = b.Max.X
	}
	if y2 > b.Max.Y {
		y2 = b.Max.Y
	}

	if x2 <= x1 || y2 <= y1 {
		return nil, errors.New("face crop outside image")
	}

	rect := image.Rect(0, 0, x2-x1, y2-y1)
	dst := image.NewRGBA(rect)

	for y := 0; y < rect.Dy(); y++ {
		for x := 0; x < rect.Dx(); x++ {
			dst.Set(x, y, img.At(x+x1, y+y1))
		}
	}
	return upscaleToMin(dst, minEmbedSide), nil
}

// upscaleToMin scales img up so both sides are at least minSide, preserving
// aspect ratio. Images already large enough are returned unchanged.
func upscaleToMin(img image.Image, minSide int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w >= minSide && h >= minSide {
		return img
	}

	scale := math.Max(float64(minSide)/float64(w), float64(minSide)/float64(h))
	nw := max(1, int(math.Ceil(float64(w)*scale)))
	nh := max(1, int(math.Ceil(float64(h)*scale)))
	resized := image.NewRGBA(image.Rect(0, 0, nw, nh))

	for y := 0; y < nh; y++ {
		sy := b.Min.Y + y*h/nh
		for x := 0; x < nw; x++ {
			sx := b.Min.X + x*w/nw
			resized.Set(x, y, img.At(sx, sy))
		}
	}
	return resized
}

func imageToRGB(img image.Image) []byte {
	b := img.Bounds()
	rgb := make([]byte, b.Dx()*b.Dy()*3)
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			rgb[i] = byte(r >> 8)
			rgb[i+1] = byte(g >> 8)
			rgb[i+2] = byte(b >> 8)
			i += 3
		}
	}
	return rgb
}

func encodePreview(src image.Image) ([]byte, error) {
	const maxSide = 256

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("empty face")
	}

	scale := 1.0
	if w > maxSide || h > maxSide {
		if w > h {
			scale = float64(maxSide) / float64(w)
		} else {
			scale = float64(maxSide) / float64(h)
		}
	}

	nw := max(1, int(float64(w)*scale))
	nh := max(1, int(float64(h)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))

	for y := 0; y < nh; y++ {
		sy := b.Min.Y + y*h/nh
		for x := 0; x < nw; x++ {
			sx := b.Min.X + x*w/nw
			dst.Set(x, y, src.At(sx, sy))
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func writeTempImage(data []byte) (string, error) {
	f, err := os.CreateTemp("", "face-detect-*.img")
	if err != nil {
		return "", err
	}
	name := f.Name()

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func cString(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	p := unsafe.Pointer(ptr)
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}

func Similarity(a, b []float32) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}

	var dot, aa, bb float64
	for i := range a {
		x := float64(a[i])
		y := float64(b[i])
		dot += x * y
		aa += x * x
		bb += y * y
	}

	if aa == 0 || bb == 0 {
		return -1
	}
	return float32(dot / math.Sqrt(aa*bb))
}
