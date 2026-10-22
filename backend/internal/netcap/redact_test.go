package netcap

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedactBodyPreviewKeepsUTF8Boundary(t *testing.T) {
	for _, runeText := range []string{"中", "é", "🔍"} {
		for offset := 0; offset < len(runeText); offset++ {
			body := strings.Repeat("a", previewLimit-offset) + strings.Repeat(runeText, previewLimit)
			preview := redactBodyPreview([]byte(body), false)
			if !utf8.ValidString(preview) {
				t.Errorf("%q offset %d: preview contains a partial rune", runeText, offset)
			}
			if len(preview) != previewLimit-offset || !strings.HasPrefix(body, preview) {
				t.Errorf("%q offset %d: got %d bytes, want a complete prefix of %d bytes", runeText, offset, len(preview), previewLimit-offset)
			}
		}
	}
}

func TestRedactBodyPreviewPreservesRedactionAndLimits(t *testing.T) {
	for _, tt := range []struct {
		name   string
		body   string
		binary bool
		want   string
	}{
		{name: "empty"},
		{name: "short Chinese", body: "新闻与产品", want: "新闻与产品"},
		{name: "exact limit", body: strings.Repeat("a", previewLimit-3) + "中", want: strings.Repeat("a", previewLimit-3) + "中"},
		{name: "long ASCII", body: strings.Repeat("a", previewLimit+1), want: strings.Repeat("a", previewLimit)},
		{name: "invalid UTF8", body: "\xff", want: "[binary]"},
		{name: "binary", body: "valid text", binary: true, want: "[binary]"},
		{name: "JSON secret", body: `{"password":"do-not-expose","text":"中文"}`, want: `{"password":"[redacted]","text":"中文"}`},
		{name: "form secret", body: "password=do-not-expose&text=test", want: "password=%5Bredacted%5D&text=test"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactBodyPreview([]byte(tt.body), tt.binary); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}

	body := `{"password":"` + strings.Repeat("secret", previewLimit) + `","text":"` + strings.Repeat("中", previewLimit) + `"}`
	preview := redactBodyPreview([]byte(body), false)
	if !utf8.ValidString(preview) || len(preview) > previewLimit || strings.Contains(preview, "secret") || !strings.Contains(preview, "[redacted]") {
		t.Fatal("long JSON preview must be valid UTF8, bounded, and redacted before truncation")
	}
}
