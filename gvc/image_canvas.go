package gvc

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// imageCanvas draws the paths, text and images of a page onto an RGBA image with golang.org/x/image: vector fills
// the paths, and strokes are filled as the polygons that cover them, with round caps and joins. It offers what
// ImageRenderer needs, in the way fogleman/gg did it before.
type imageCanvas struct {
	img      *image.RGBA
	state    canvasState
	stack    []canvasState
	subpaths []canvasSubpath
	current  [2]float64
	started  bool
	// rasterizer is reused from one fill to the next.
	rasterizer *vector.Rasterizer
}

// canvasState is what Push saves and Pop restores.
type canvasState struct {
	translateX, translateY float64
	color                  color.NRGBA
	lineWidth              float64
	dash                   []float64 // alternating lengths of dashes and gaps; empty for solid lines
	face                   font.Face
}

// canvasSubpath is a flattened subpath of the current path, in pixels.
type canvasSubpath struct {
	points [][2]float64
	closed bool
}

func newImageCanvas(width, height int) *imageCanvas {
	return &imageCanvas{
		img:   image.NewRGBA(image.Rect(0, 0, width, height)),
		state: canvasState{color: color.NRGBA{A: 255}, lineWidth: 1},
	}
}

func (c *imageCanvas) Image() image.Image { return c.img }

func (c *imageCanvas) Push() {
	saved := c.state
	saved.dash = append([]float64(nil), c.state.dash...)
	c.stack = append(c.stack, saved)
}

func (c *imageCanvas) Pop() {
	if len(c.stack) == 0 {
		return
	}
	c.state = c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
}

// Translate moves the origin of the coordinates of the following drawing.
func (c *imageCanvas) Translate(x, y float64) {
	c.state.translateX += x
	c.state.translateY += y
}

// SetRGB sets an opaque colour from components between 0 and 1.
func (c *imageCanvas) SetRGB(r, g, b float64) {
	c.state.color = color.NRGBA{R: component(r), G: component(g), B: component(b), A: 255}
}

func component(value float64) uint8 {
	return uint8(math.Round(math.Max(0, math.Min(1, value)) * 255))
}

func (c *imageCanvas) SetLineWidth(width float64) { c.state.lineWidth = width }

func (c *imageCanvas) SetDash(dashes ...float64) { c.state.dash = dashes }

func (c *imageCanvas) SetFontFace(face font.Face) { c.state.face = face }

func (c *imageCanvas) point(x, y float64) [2]float64 {
	return [2]float64{x + c.state.translateX, y + c.state.translateY}
}

func (c *imageCanvas) MoveTo(x, y float64) {
	p := c.point(x, y)
	c.subpaths = append(c.subpaths, canvasSubpath{points: [][2]float64{p}})
	c.current, c.started = p, true
}

func (c *imageCanvas) LineTo(x, y float64) {
	if !c.started {
		c.MoveTo(x, y)
		return
	}
	c.lineTo(c.point(x, y))
}

func (c *imageCanvas) lineTo(p [2]float64) {
	last := &c.subpaths[len(c.subpaths)-1]
	last.points = append(last.points, p)
	c.current = p
}

// CubicTo adds a cubic Bézier curve, flattened into lines short enough to look smooth.
func (c *imageCanvas) CubicTo(x1, y1, x2, y2, x3, y3 float64) {
	if !c.started {
		c.MoveTo(x1, y1)
	}
	p0, p1, p2, p3 := c.current, c.point(x1, y1), c.point(x2, y2), c.point(x3, y3)
	length := distance(p0, p1) + distance(p1, p2) + distance(p2, p3)
	steps := int(math.Max(4, math.Ceil(length/2)))
	for i := 1; i <= steps; i++ {
		t := float64(i) / float64(steps)
		u := 1 - t
		c.lineTo([2]float64{
			u*u*u*p0[0] + 3*u*u*t*p1[0] + 3*u*t*t*p2[0] + t*t*t*p3[0],
			u*u*u*p0[1] + 3*u*u*t*p1[1] + 3*u*t*t*p2[1] + t*t*t*p3[1],
		})
	}
}

func (c *imageCanvas) ClosePath() {
	if len(c.subpaths) > 0 {
		last := &c.subpaths[len(c.subpaths)-1]
		last.closed = true
		c.current = last.points[0]
	}
}

// DrawEllipse adds an ellipse as a closed subpath.
func (c *imageCanvas) DrawEllipse(x, y, rx, ry float64) {
	steps := int(math.Max(16, math.Ceil(math.Pi*(math.Abs(rx)+math.Abs(ry))/2)))
	c.MoveTo(x+rx, y)
	for i := 1; i < steps; i++ {
		angle := 2 * math.Pi * float64(i) / float64(steps)
		c.LineTo(x+rx*math.Cos(angle), y+ry*math.Sin(angle))
	}
	c.ClosePath()
}

func (c *imageCanvas) clearPath() {
	c.subpaths, c.started = nil, false
}

// FillPreserve fills the current path and keeps it.
func (c *imageCanvas) FillPreserve() {
	polygons := make([][][2]float64, 0, len(c.subpaths))
	for _, subpath := range c.subpaths {
		polygons = append(polygons, subpath.points)
	}
	c.fill(polygons, false)
}

// Fill fills the current path, then forgets it.
func (c *imageCanvas) Fill() {
	c.FillPreserve()
	c.clearPath()
}

// Stroke strokes the current path with the line width and dashes, then forgets it.
func (c *imageCanvas) Stroke() {
	polygons := make([][][2]float64, 0)
	halfWidth := c.state.lineWidth / 2
	for _, subpath := range c.subpaths {
		points := subpath.points
		if subpath.closed && len(points) > 1 {
			points = append(points[:len(points):len(points)], points[0])
		}
		for _, line := range c.dashed(points) {
			polygons = append(polygons, strokePolygons(line, halfWidth)...)
		}
	}
	c.fill(polygons, true)
	c.clearPath()
}

// fill draws the polygons in one pass in the current colour. Strokes turn each polygon the same way round, so that
// where they overlap the coverage adds up rather than cancelling out, and is still blended once. Only the bounding
// box of the polygons is rasterized, as a page is often far larger than what one call draws.
func (c *imageCanvas) fill(polygons [][][2]float64, sameWayRound bool) {
	box, ok := c.boundingBox(polygons)
	if !ok {
		return
	}
	if c.rasterizer == nil {
		c.rasterizer = vector.NewRasterizer(box.Dx(), box.Dy())
	} else {
		c.rasterizer.Reset(box.Dx(), box.Dy())
	}
	offsetX, offsetY := float32(box.Min.X), float32(box.Min.Y)
	for _, polygon := range polygons {
		if len(polygon) < 3 {
			continue
		}
		if sameWayRound && signedArea(polygon) < 0 {
			polygon = reversed(polygon)
		}
		c.rasterizer.MoveTo(float32(polygon[0][0])-offsetX, float32(polygon[0][1])-offsetY)
		for _, p := range polygon[1:] {
			c.rasterizer.LineTo(float32(p[0])-offsetX, float32(p[1])-offsetY)
		}
		c.rasterizer.ClosePath()
	}
	c.rasterizer.DrawOp = draw.Over
	c.rasterizer.Draw(c.img, box, image.NewUniform(c.state.color), image.Point{})
}

// boundingBox is the box of whole pixels that the polygons cover, within the image, and false when they cover
// nothing there.
func (c *imageCanvas) boundingBox(polygons [][][2]float64) (image.Rectangle, bool) {
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, polygon := range polygons {
		if len(polygon) < 3 {
			continue
		}
		for _, p := range polygon {
			minX, minY = math.Min(minX, p[0]), math.Min(minY, p[1])
			maxX, maxY = math.Max(maxX, p[0]), math.Max(maxY, p[1])
		}
	}
	if math.IsInf(minX, 1) {
		return image.Rectangle{}, false
	}
	box := image.Rect(int(math.Floor(minX)), int(math.Floor(minY)), int(math.Ceil(maxX)), int(math.Ceil(maxY))).
		Intersect(c.img.Bounds())
	return box, !box.Empty()
}

// dashed splits a polyline into its dashes, or returns it whole for a solid line.
func (c *imageCanvas) dashed(points [][2]float64) [][][2]float64 {
	dash := c.state.dash
	if len(dash) == 0 || len(points) < 2 {
		return [][][2]float64{points}
	}
	dashes := make([][][2]float64, 0)
	index, left, on := 0, dash[0], true
	current := [][2]float64{points[0]}
	for i := 1; i < len(points); i++ {
		from, to := points[i-1], points[i]
		segment := distance(from, to)
		for segment > left {
			t := left / segment
			split := [2]float64{from[0] + (to[0]-from[0])*t, from[1] + (to[1]-from[1])*t}
			if on {
				dashes = append(dashes, append(current, split))
			}
			current, on = [][2]float64{split}, !on
			from, segment = split, segment-left
			index = (index + 1) % len(dash)
			left = dash[index]
		}
		left -= segment
		current = append(current, to)
	}
	if on && len(current) > 1 {
		dashes = append(dashes, current)
	}
	return dashes
}

// strokePolygons are the polygons covering a polyline drawn with the half width: a rectangle along each segment, a
// disc at each joint where the line turns (a round join) and a disc at each end (a round cap).
func strokePolygons(points [][2]float64, halfWidth float64) [][][2]float64 {
	polygons := make([][][2]float64, 0, 2*len(points))
	for i := 1; i < len(points); i++ {
		from, to := points[i-1], points[i]
		length := distance(from, to)
		if length == 0 {
			continue
		}
		nx, ny := -(to[1]-from[1])/length*halfWidth, (to[0]-from[0])/length*halfWidth
		polygons = append(polygons, [][2]float64{
			{from[0] + nx, from[1] + ny}, {to[0] + nx, to[1] + ny}, {to[0] - nx, to[1] - ny}, {from[0] - nx, from[1] - ny},
		})
		if i+1 < len(points) && halfWidth > 0.5 && turns(from, to, points[i+1]) {
			polygons = append(polygons, disc(to, halfWidth))
		}
	}
	if len(points) > 1 && halfWidth > 0.5 {
		polygons = append(polygons, disc(points[0], halfWidth), disc(points[len(points)-1], halfWidth))
	}
	return polygons
}

// turns tells whether the polyline turns at the middle point by more than a few degrees, enough for the gap between
// the rectangles of its two segments to show; a flattened curve turns by less at each of its points.
func turns(from, middle, to [2]float64) bool {
	ax, ay := middle[0]-from[0], middle[1]-from[1]
	bx, by := to[0]-middle[0], to[1]-middle[1]
	return math.Abs(math.Atan2(ax*by-ay*bx, ax*bx+ay*by)) > 5*math.Pi/180
}

func disc(centre [2]float64, radius float64) [][2]float64 {
	steps := int(math.Max(8, math.Ceil(2*math.Pi*radius/2)))
	points := make([][2]float64, steps)
	for i := range points {
		angle := 2 * math.Pi * float64(i) / float64(steps)
		points[i] = [2]float64{centre[0] + radius*math.Cos(angle), centre[1] + radius*math.Sin(angle)}
	}
	return points
}

func distance(a, b [2]float64) float64 {
	return math.Hypot(b[0]-a[0], b[1]-a[1])
}

func signedArea(polygon [][2]float64) float64 {
	area := 0.0
	for i, p := range polygon {
		next := polygon[(i+1)%len(polygon)]
		area += p[0]*next[1] - next[0]*p[1]
	}
	return area / 2
}

func reversed(polygon [][2]float64) [][2]float64 {
	result := make([][2]float64, len(polygon))
	for i, p := range polygon {
		result[len(polygon)-1-i] = p
	}
	return result
}

// DrawString draws the text in the current font and colour, with its baseline starting at the point.
func (c *imageCanvas) DrawString(text string, x, y float64) {
	p := c.point(x, y)
	drawer := font.Drawer{
		Dst:  c.img,
		Src:  image.NewUniform(c.state.color),
		Face: c.state.face,
		Dot:  fixed.Point26_6{X: fixed.Int26_6(math.Round(p[0] * 64)), Y: fixed.Int26_6(math.Round(p[1] * 64))},
	}
	drawer.DrawString(text)
}

// DrawImageBottomLeft draws the image with its bottom left corner at the point.
func (c *imageCanvas) DrawImageBottomLeft(img image.Image, x, y int) {
	p := c.point(float64(x), float64(y))
	size := img.Bounds().Size()
	topLeft := image.Pt(int(math.Round(p[0])), int(math.Round(p[1]))-size.Y)
	draw.Draw(c.img, image.Rectangle{Min: topLeft, Max: topLeft.Add(size)}, img, img.Bounds().Min, draw.Over)
}

func (c *imageCanvas) EncodePNG(w io.Writer) error {
	return png.Encode(w, c.img)
}

func (c *imageCanvas) SavePNG(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := c.EncodePNG(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
