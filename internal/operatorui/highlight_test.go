package operatorui

import (
	"strings"
	"testing"
)

func TestHighlightEscapesAndMarks(t *testing.T) {
	cases := []struct {
		name, text, query, want, notWant string
	}{
		{"no query escapes", `a <b> & c`, "", `a &lt;b&gt; &amp; c`, ""},
		{"case insensitive", "Brown Fox", "brown", `<mark class="hl">Brown</mark> Fox`, ""},
		{"all terms", "alpha beta", "alpha beta", `<mark class="hl">alpha</mark> <mark class="hl">beta</mark>`, ""},
		{"regex meta safe", "a (b) [c] *d*", "(b)", `<mark class="hl">(b)</mark>`, ""},
		{"xss escaped", `<img src=x onerror=alert(1)>`, "img", `<mark class="hl">img</mark> src=x`, "<img"},
		{"cjk", "数据 备份", "备份", `<mark class="hl">备份</mark>`, ""},
	}
	for _, tc := range cases {
		got := string(highlightHTML(tc.text, tc.query))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want substring %q", tc.name, got, tc.want)
		}
		if tc.notWant != "" && strings.Contains(got, tc.notWant) {
			t.Errorf("%s: unsafe content %q in %q", tc.name, tc.notWant, got)
		}
	}
}
