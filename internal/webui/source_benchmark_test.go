package webui

import (
	"strings"
	"testing"
)

// Keep source previews inexpensive even when a small file contains very many
// lines. Rendered bytes matter here as well as server time and allocations.
func BenchmarkSourcePreview(b *testing.B) {
	for _, fixture := range []struct{ name, content string }{
		{"go", strings.Repeat("func main() { fmt.Println(\"hello\") }\n", 3000)},
		{"short_lines", strings.Repeat("\n", sourcePreviewLimit)},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			value := map[string]any{"path": "main.go", "content": fixture.content}
			b.ReportAllocs()
			b.SetBytes(int64(len(fixture.content)))
			var size int
			for b.Loop() {
				size = len(sourceHTML(value))
			}
			b.ReportMetric(float64(size), "rendered-bytes")
		})
	}
}
