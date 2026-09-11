// Package seedmark renders version 1 of the Seedmark avatar design without
// JavaScript. It follows the upstream generator at c808a94; fixtures pin its
// visual output. Avatars are decorative and are not identity verification.
package seedmark

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

const Version = 1

var families = [...]string{"masks", "creatures", "botanical", "orbital", "landscapes", "glyphs", "woven", "machines"}

var palettes = [...][4]string{
	{"#142d36", "#f5dfad", "#ed7351", "#65c4ae"},
	{"#24223c", "#f7dfbf", "#a89cfc", "#f7788b"},
	{"#163b32", "#f4eacb", "#b4cf66", "#f69c64"},
	{"#3a2330", "#ffe5b9", "#ef7689", "#8dceba"},
	{"#1d304c", "#e8eff0", "#f4b855", "#72b9d0"},
	{"#422d26", "#f8e8c8", "#e8a04d", "#72afa0"},
	{"#283526", "#f4edcf", "#d3c65e", "#ed8863"},
	{"#292745", "#efe8f5", "#d89ada", "#8fcabc"},
	{"#18343c", "#f8e4d0", "#f88c66", "#a7cee0"},
	{"#393127", "#f4ebd8", "#adb77a", "#da8c6f"},
	{"#2c2440", "#fbe8c2", "#f2b54f", "#b2a1e2"},
	{"#1b3537", "#e9f0d7", "#79c7b8", "#e5a4aa"},
}

type recipe struct {
	Family     string
	Palette    [4]string
	Layout     struct{ Variant, Flip, Tilt int }
	Silhouette struct{ Variant, Width, Height int }
	Features   struct{ Variant, Count, Accent int }
	Texture    struct{ Variant, Marks int }
}

type rng struct{ a, b, c, d uint32 }

func newRNG(seed, namespace string) *rng {
	var framed strings.Builder
	for _, part := range []string{"seedmark", "1", seed, namespace} {
		framed.WriteString(strconv.Itoa(len(utf16.Encode([]rune(part)))))
		framed.WriteByte(':')
		framed.WriteString(part)
	}
	a, b, c, d := uint32(1779033703), uint32(3144134277), uint32(1013904242), uint32(2773480762)
	for _, unit := range utf16.Encode([]rune(framed.String())) {
		k := uint32(unit)
		a = b ^ (a^k)*597399067
		b = c ^ (b^k)*2869860233
		c = d ^ (c^k)*951274213
		d = a ^ (d^k)*2716044179
	}
	a = (c ^ (a >> 18)) * 597399067
	b = (d ^ (b >> 22)) * 2869860233
	c = (a ^ (c >> 17)) * 951274213
	d = (b ^ (d >> 19)) * 2716044179
	return &rng{a ^ b ^ c ^ d, b ^ a, c ^ a, d ^ a}
}

func (r *rng) integer(minimum, maximum int) int {
	t := r.a + r.b + r.d
	r.d++
	r.a = r.b ^ (r.b >> 9)
	r.b = r.c + (r.c << 3)
	r.c = ((r.c << 21) | (r.c >> 11)) + t
	return minimum + int(float64(t)/4294967296*float64(maximum-minimum+1))
}

func recipeFor(seed string) recipe {
	var r recipe
	r.Family = families[newRNG(seed, "family").integer(0, len(families)-1)]
	r.Palette = palettes[newRNG(seed, "palette").integer(0, len(palettes)-1)]
	layout, silhouette := newRNG(seed, "layout"), newRNG(seed, "silhouette")
	features, texture := newRNG(seed, "features"), newRNG(seed, "texture")
	r.Layout.Variant, r.Layout.Flip = layout.integer(0, 3), layout.integer(0, 1)
	r.Layout.Tilt = [...]int{-12, 0, 12}[layout.integer(0, 2)]
	r.Silhouette.Variant = silhouette.integer(0, 3)
	r.Silhouette.Width, r.Silhouette.Height = silhouette.integer(26, 38), silhouette.integer(28, 42)
	r.Features.Variant, r.Features.Count = features.integer(0, 3), features.integer(2, 5)
	r.Features.Accent = features.integer(0, 1)
	r.Texture.Variant, r.Texture.Marks = texture.integer(0, 2), texture.integer(2, 4)
	return r
}

// Avatar returns a standalone 64-pixel SVG for the full seed. The version 1
// geometry remains fixed; CSS may resize the SVG without changing its design.
func Avatar(seed string) string {
	r := recipeFor(seed)
	render := map[string]func(recipe) string{"masks": masks, "creatures": creatures, "botanical": botanical, "orbital": orbital, "landscapes": landscapes, "glyphs": glyphs, "woven": woven, "machines": machines}
	texture := ""
	for i := 0; i < r.Texture.Marks; i++ {
		x := float64(8 + i*6)
		if r.Texture.Variant == 1 {
			texture += circle(x, 6, 1.5, r.Palette[3])
		} else if r.Texture.Variant == 2 {
			texture += rect(x-1, 5, 3, 3, 0, r.Palette[2])
		}
	}
	flip := "1"
	if r.Layout.Flip != 0 {
		flip = "-1"
	}
	transform := "translate(32 32) rotate(" + strconv.Itoa(r.Layout.Tilt) + ") scale(" + flip + " 1) translate(-32 -32)"
	return `<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64" viewBox="0 0 64 64"><rect width="64" height="64" fill="` + r.Palette[0] + `"/>` + texture + group(render[r.Family](r), transform) + `</svg>`
}
