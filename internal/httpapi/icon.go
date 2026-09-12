package httpapi

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"
	"sync"
)

// The favicon is the site chart in miniature: three rising bars in the chart's
// blue, the newest one in the green the pages use for "someone is here now".
// It is drawn from one 32-unit grid for both the SVG and the PNG, so a tab, a
// bookmark and an iPhone home screen all show the same icon.
var (
	iconBG   = color.RGBA{0x10, 0x10, 0x14, 0xff}
	iconBlue = color.RGBA{0x3b, 0x6e, 0xa5, 0xff}
	iconLive = color.RGBA{0x5c, 0xd5, 0x8c, 0xff}
)

// iconBars are x0,y0,x1,y1 rectangles on the 32x32 grid.
var iconBars = []struct {
	x0, y0, x1, y1 int
	fill           color.RGBA
}{
	{4, 18, 11, 27, iconBlue},
	{13, 12, 20, 27, iconBlue},
	{22, 6, 29, 27, iconLive},
}

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32">` +
	`<rect width="32" height="32" rx="7" fill="#101014"/>` +
	`<rect x="4" y="18" width="7" height="9" rx="1.5" fill="#3b6ea5"/>` +
	`<rect x="13" y="12" width="7" height="15" rx="1.5" fill="#3b6ea5"/>` +
	`<rect x="22" y="6" width="7" height="21" rx="1.5" fill="#5cd58c"/>` +
	`</svg>`

// iconPNG rasterises the same drawing for the browsers that still ignore an
// SVG icon. Plain rectangles keep it dependency-free.
func iconPNG(size int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), &image.Uniform{iconBG}, image.Point{}, draw.Src)
	scale := func(v int) int { return v * size / 32 }
	for _, b := range iconBars {
		r := image.Rect(scale(b.x0), scale(b.y0), scale(b.x1), scale(b.y1))
		draw.Draw(img, r, &image.Uniform{b.fill}, image.Point{}, draw.Src)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// Both sizes are encoded on first request and then held, since the drawing
// never changes while the process runs.
var (
	faviconPNG    = sync.OnceValue(func() []byte { return iconPNG(32) })
	appleIconPNG  = sync.OnceValue(func() []byte { return iconPNG(180) })
	iconCacheTime = "public, max-age=604800"
)

func (s *Server) handleFaviconSVG(w http.ResponseWriter, _ *http.Request) {
	writeIcon(w, "image/svg+xml", []byte(faviconSVG))
}

func (s *Server) handleFaviconPNG(w http.ResponseWriter, _ *http.Request) {
	writeIcon(w, "image/png", faviconPNG())
}

func (s *Server) handleAppleIcon(w http.ResponseWriter, _ *http.Request) {
	writeIcon(w, "image/png", appleIconPNG())
}

func writeIcon(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", iconCacheTime)
	_, _ = w.Write(body)
}
