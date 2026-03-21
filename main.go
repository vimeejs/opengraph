package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"sync"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	"golang.org/x/image/font"
	xdraw "golang.org/x/image/draw"
)

//go:embed fonts/Jersey15-Regular.ttf
var fontData []byte

//go:embed vimee.svg
var iconSVG []byte

// ── Canvas ────────────────────────────────────────────────────────────────

const (
	imgW     = 2400 // internal render width (2x)
	imgH     = 1260 // internal render height (2x)
	outW     = 1200 // final output width (1x)
	outH     = 630  // final output height (1x)
	blurR    = 40   // gaussian blur radius for background
	numBlobs = 7    // number of color blobs
)

// ── Text ──────────────────────────────────────────────────────────────────

const (
	titleSize    = 130.0 // title font size in points (at 2x)
	subtitleSize = 80.0  // subtitle font size in points (at 2x)
)

// ── Grain ─────────────────────────────────────────────────────────────────

const (
	grainDensity  = 0.50 // fraction of pixels affected (0.0-1.0)
	grainStrength = 18   // max multiplicative noise percentage (+/-)
)

// ── Shader math ───────────────────────────────────────────────────────────

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

// prand derives a pseudo-random float in [0,1) from a seed and an integer index.
func prand(seed float64, idx int) float64 {
	h := math.Float64bits(seed) ^ uint64(idx)*2654435761
	h ^= h >> 17
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 31
	h *= 0x94d049bb133111eb
	h ^= h >> 32
	return float64(h&0xFFFFFF) / float64(0x1000000)
}

func ihash(ix, iy int) float64 {
	h := ix*374761393 + iy*668265263
	h ^= h >> 13
	h *= 1274126177
	h ^= h >> 16
	return float64(uint32(h)) / 4294967296.0
}

// noise returns smooth value noise using quintic interpolation.
func noise(x, y float64) float64 {
	ix := int(math.Floor(x))
	iy := int(math.Floor(y))
	fx := x - math.Floor(x)
	fy := y - math.Floor(y)
	ux := fx * fx * fx * (fx*(fx*6-15) + 10)
	uy := fy * fy * fy * (fy*(fy*6-15) + 10)
	return lerp(
		lerp(ihash(ix, iy), ihash(ix+1, iy), ux),
		lerp(ihash(ix, iy+1), ihash(ix+1, iy+1), ux),
		uy,
	)
}

// ── Color palette & blobs ─────────────────────────────────────────────────

// Dark purple/blue palette based on #A5B4FC, #C4B5FD, #B4BEFE, #A78BFA, #C0CAF5
var palette = [][3]float64{
	{0.35, 0.30, 0.55}, // dark #A5B4FC
	{0.42, 0.35, 0.58}, // dark #C4B5FD
	{0.38, 0.38, 0.60}, // dark #B4BEFE
	{0.30, 0.25, 0.52}, // dark #A78BFA
	{0.40, 0.42, 0.56}, // dark #C0CAF5
	{0.32, 0.28, 0.55}, // dark purple
	{0.25, 0.22, 0.48}, // deep blue-purple
	{0.18, 0.16, 0.35}, // very deep purple
}

type colorBlob struct {
	px, py float64
	radius float64
	col    [3]float64
}

func generateBlobs(seed float64) []colorBlob {
	blobs := make([]colorBlob, numBlobs)
	for i := range numBlobs {
		ci := int(prand(seed, i*7+3) * float64(len(palette)))
		if ci >= len(palette) {
			ci = len(palette) - 1
		}
		blobs[i] = colorBlob{
			px:     prand(seed, i*7) * imgW,
			py:     prand(seed, i*7+1) * imgH,
			radius: 180 + prand(seed, i*7+2)*350,
			col:    palette[ci],
		}
	}
	return blobs
}

// shadePixel blends soft radial blooms over a dark base,
// then adds low-frequency noise for subtle organic variation.
func shadePixel(x, y int, seed float64, blobs []colorBlob) (float64, float64, float64) {
	// base: dark purple-blue
	col := [3]float64{0.08, 0.08, 0.15}

	px, py := float64(x), float64(y)
	for _, b := range blobs {
		dx := px - b.px
		dy := py - b.py
		w := math.Exp(-(dx*dx + dy*dy) / (2 * b.radius * b.radius))
		s := w * 0.55
		col[0] = lerp(col[0], b.col[0], s)
		col[1] = lerp(col[1], b.col[1], s)
		col[2] = lerp(col[2], b.col[2], s)
	}

	n := noise(float64(x)*0.003+seed*97, float64(y)*0.003+seed*53)
	col[0] += (n - 0.5) * 0.04
	col[1] += (n - 0.5) * 0.03
	col[2] += (n - 0.5) * 0.04

	return col[0], col[1], col[2]
}

// ── Float buffer for blur pipeline ────────────────────────────────────────

type floatBuf struct {
	w, h int
	pix  []float64
}

func newFloatBuf(w, h int) *floatBuf {
	return &floatBuf{w: w, h: h, pix: make([]float64, w*h*3)}
}

func (f *floatBuf) set(x, y int, r, g, b float64) {
	i := (y*f.w + x) * 3
	f.pix[i] = r
	f.pix[i+1] = g
	f.pix[i+2] = b
}

func (f *floatBuf) get(x, y int) (float64, float64, float64) {
	i := (y*f.w + x) * 3
	return f.pix[i], f.pix[i+1], f.pix[i+2]
}

func (f *floatBuf) toRGBA() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, f.w, f.h))
	for y := range f.h {
		for x := range f.w {
			r, g, b := f.get(x, y)
			off := img.PixOffset(x, y)
			img.Pix[off] = uint8(clamp01(r) * 255)
			img.Pix[off+1] = uint8(clamp01(g) * 255)
			img.Pix[off+2] = uint8(clamp01(b) * 255)
			img.Pix[off+3] = 255
		}
	}
	return img
}

// ── Separable box blur (3-pass approximates gaussian) ─────────────────────

func boxBlurH(src, dst *floatBuf, r int) {
	w, h := src.w, src.h
	d := 1.0 / float64(2*r+1)
	var wg sync.WaitGroup
	for row := range h {
		wg.Add(1)
		go func(y int) {
			defer wg.Done()
			var sr, sg, sb float64
			for i := -r; i <= r; i++ {
				cr, cg, cb := src.get(max(0, min(i, w-1)), y)
				sr += cr
				sg += cg
				sb += cb
			}
			dst.set(0, y, sr*d, sg*d, sb*d)
			for x := 1; x < w; x++ {
				ar, ag, ab := src.get(min(x+r, w-1), y)
				rr, rg, rb := src.get(max(x-r-1, 0), y)
				sr += ar - rr
				sg += ag - rg
				sb += ab - rb
				dst.set(x, y, sr*d, sg*d, sb*d)
			}
		}(row)
	}
	wg.Wait()
}

func boxBlurV(src, dst *floatBuf, r int) {
	w, h := src.w, src.h
	d := 1.0 / float64(2*r+1)
	var wg sync.WaitGroup
	for col := range w {
		wg.Add(1)
		go func(x int) {
			defer wg.Done()
			var sr, sg, sb float64
			for i := -r; i <= r; i++ {
				cr, cg, cb := src.get(x, max(0, min(i, h-1)))
				sr += cr
				sg += cg
				sb += cb
			}
			dst.set(x, 0, sr*d, sg*d, sb*d)
			for y := 1; y < h; y++ {
				ar, ag, ab := src.get(x, min(y+r, h-1))
				rr, rg, rb := src.get(x, max(y-r-1, 0))
				sr += ar - rr
				sg += ag - rg
				sb += ab - rb
				dst.set(x, y, sr*d, sg*d, sb*d)
			}
		}(col)
	}
	wg.Wait()
}

func gaussianBlur(buf *floatBuf, radius int) {
	tmp := newFloatBuf(buf.w, buf.h)
	for range 3 {
		boxBlurH(buf, tmp, radius)
		boxBlurV(tmp, buf, radius)
	}
}

// ── Render ────────────────────────────────────────────────────────────────

func renderBackground(seed float64) *image.RGBA {
	blobs := generateBlobs(seed)
	buf := newFloatBuf(imgW, imgH)

	var wg sync.WaitGroup
	for row := range imgH {
		wg.Add(1)
		go func(y int) {
			defer wg.Done()
			for x := range imgW {
				r, g, b := shadePixel(x, y, seed, blobs)
				buf.set(x, y, r, g, b)
			}
		}(row)
	}
	wg.Wait()

	gaussianBlur(buf, blurR)

	return buf.toRGBA()
}

// ── Grain ─────────────────────────────────────────────────────────────────

// addGrain applies multiplicative noise that preserves hue.
func addGrain(img *image.RGBA, rng *rand.Rand) {
	for y := range imgH {
		for x := range imgW {
			if rng.Float64() < grainDensity {
				factor := 1.0 + float64(rng.Intn(grainStrength*2+1)-grainStrength)/100.0
				i := img.PixOffset(x, y)
				img.Pix[i] = clamp8(int16(float64(img.Pix[i]) * factor))
				img.Pix[i+1] = clamp8(int16(float64(img.Pix[i+1]) * factor))
				img.Pix[i+2] = clamp8(int16(float64(img.Pix[i+2]) * factor))
			}
		}
	}
}

func clamp8(v int16) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// ── Overlay (icon + text) ─────────────────────────────────────────────────

func drawOverlay(img *image.RGBA, title, subtitle string) {
	dc := gg.NewContextForRGBA(img)
	f := loadFont()
	fw, fh := float64(imgW), float64(imgH)
	maxW := fw * 0.8

	// lay out icon + title side by side (vertically centered, with gap)
	icon, iconErr := rasterizeSVG(iconSVG, 140, 140)

	// determine title font size (auto-shrink to fit)
	size := titleSize
	for size > 20 {
		face := truetype.NewFace(f, &truetype.Options{Size: size, DPI: 72, Hinting: font.HintingFull})
		dc.SetFontFace(face)
		tw, _ := dc.MeasureString(title)
		if tw <= maxW {
			break
		}
		size -= 4
	}

	// measure title width
	tw, _ := dc.MeasureString(title)

	gap := 48.0
	iconW := 0.0
	iconH := 0.0
	if iconErr == nil {
		iconW = float64(icon.Bounds().Dx())
		iconH = float64(icon.Bounds().Dy())
	}

	// center the icon+title row horizontally
	totalW := iconW + gap + tw
	startX := (fw - totalW) / 2

	// vertical center for the title row
	centerY := fh * 0.42

	// draw icon (vertically centered with title)
	if iconErr == nil {
		ix := int(startX)
		iy := int(centerY - iconH/2)
		dc.DrawImage(icon, ix, iy)
	}

	// draw title (right of icon, vertically centered)
	textX := startX + iconW + gap + tw/2
	dc.SetColor(color.White)
	dc.DrawStringAnchored(title, textX, centerY, 0.5, 0.3)

	// draw subtitle (centered below)
	if subtitle != "" {
		drawCenteredText(dc, f, subtitle, subtitleSize, maxW, fw/2, centerY+240)
	}
}

func drawCenteredText(dc *gg.Context, f *truetype.Font, text string, size, maxW, cx, cy float64) {
	for size > 20 {
		face := truetype.NewFace(f, &truetype.Options{Size: size, DPI: 72, Hinting: font.HintingFull})
		dc.SetFontFace(face)
		tw, _ := dc.MeasureString(text)
		if tw <= maxW {
			break
		}
		size -= 4
	}

	dc.SetColor(color.White)
	dc.DrawStringAnchored(text, cx, cy, 0.5, 0.5)
}

// ── SVG rasterizer ────────────────────────────────────────────────────────

func rasterizeSVG(svgData []byte, w, h int) (image.Image, error) {
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svgData))
	if err != nil {
		return nil, err
	}
	icon.SetTarget(0, 0, float64(w), float64(h))
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	icon.Draw(rasterx.NewDasher(w, h, rasterx.NewScannerGV(w, h, rgba, rgba.Bounds())), 1)
	return rgba, nil
}

func loadFont() *truetype.Font {
	f, err := truetype.Parse(fontData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "font parse error: %v\n", err)
		os.Exit(1)
	}
	return f
}

// ── Seed ──────────────────────────────────────────────────────────────────

// titleSeed generates a deterministic seed in [0,1) from the input text (FNV-1a).
func titleSeed(title string) float64 {
	var h uint64 = 14695981039346656037
	for _, c := range title {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return float64(h&0xFFFFFF) / float64(0x1000000)
}

// ── Main ──────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: opengraph <title> [subtitle]")
		os.Exit(1)
	}

	title := os.Args[1]
	subtitle := ""
	if len(os.Args) >= 3 {
		subtitle = os.Args[2]
	}

	seed := titleSeed(title + subtitle)
	rng := rand.New(rand.NewSource(int64(seed * 1e9)))

	img := renderBackground(seed)
	addGrain(img, rng)
	drawOverlay(img, title, subtitle)

	// downscale 2x -> 1x with bilinear filtering
	out := image.NewRGBA(image.Rect(0, 0, outW, outH))
	xdraw.BiLinear.Scale(out, out.Bounds(), img, img.Bounds(), xdraw.Over, nil)

	outPath := "og.png"
	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	if err := png.Encode(f, out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(outPath)
}
