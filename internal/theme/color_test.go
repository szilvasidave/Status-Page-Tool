package theme

import (
	"math"
	"testing"
)

func TestNormalizeHex(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"   ", "", true},
		{"#1f5bd8", "#1f5bd8", true},
		{"#1F5BD8", "#1f5bd8", true},
		{"1f5bd8", "#1f5bd8", true},
		{"  #AbC  ", "#aabbcc", true},
		{"abc", "#aabbcc", true},
		{"#000", "#000000", true},
		{"\t#FFFFFF\n", "#ffffff", true},
		{"#", "", false},
		{"##abc", "", false},
		{"#ab", "", false},
		{"#abcd", "", false},
		{"#abcde", "", false},
		{"#1f5bd8f", "", false},
		{"#1f5bd8ff", "", false}, // no alpha
		{"#ggg", "", false},
		{"red", "", false},
		{"rgb(1,2,3)", "", false},
		{"#1f 5bd", "", false},
		{"#1f5bd8;}", "", false},
		{"#ａｂｃ", "", false}, // full-width letters
	}
	for _, tc := range tests {
		got, ok := NormalizeHex(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeHex(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestContrast(t *testing.T) {
	near := func(got, want float64) bool { return math.Abs(got-want) < 0.01 }
	tests := []struct {
		a, b string
		want float64
	}{
		{"#ffffff", "#000000", 21},
		{"#000000", "#ffffff", 21}, // symmetric
		{"#1f5bd8", "#1f5bd8", 1},
		{"#1f5bd8", "#f5f7fa", 5.52},
		{"#7aa9ff", "#1b222c", 6.82},
		{"#767676", "#ffffff", 4.54}, // the well-known AA grey
		{"#FFF", "000", 21},          // any form NormalizeHex accepts
		{"invalid", "#ffffff", 21},   // invalid counts as black
	}
	for _, tc := range tests {
		if got := Contrast(tc.a, tc.b); !near(got, tc.want) {
			t.Errorf("Contrast(%q, %q) = %.3f, want %.2f", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMix(t *testing.T) {
	tests := []struct {
		a, b string
		t    float64
		want string
	}{
		{"#000000", "#ffffff", 0.5, "#808080"}, // 127.5 rounds up
		{"#000000", "#ffffff", 1, "#000000"},
		{"#000000", "#ffffff", 0, "#ffffff"},
		{"#000000", "#1f5bd8", 0.14, "#1b4eba"},
		{"#ffffff", "#002855", 0.78, "#c7d0da"},
	}
	for _, tc := range tests {
		if got := mix(tc.a, tc.b, tc.t); got != tc.want {
			t.Errorf("mix(%s, %s, %v) = %s, want %s", tc.a, tc.b, tc.t, got, tc.want)
		}
	}
}

func TestEnsureContrast(t *testing.T) {
	// Already enough: unchanged (normalised).
	if got := ensureContrast("#1F5BD8", "#f5f7fa", 4.5, black); got != "#1f5bd8" {
		t.Errorf("unchanged colour: got %s", got)
	}
	// Adjusted: the first 2 % step that reaches the target.
	got := ensureContrast("#ffd400", "#f5f7fa", 4.5, black)
	if got == "#ffd400" || Contrast(got, "#f5f7fa") < 4.5 {
		t.Errorf("adjusted colour %s has contrast %.2f", got, Contrast(got, "#f5f7fa"))
	}
	for i := 1; i <= 50; i++ {
		step := mix(black, "#ffd400", float64(i)/50)
		if step == got {
			break
		}
		if Contrast(step, "#f5f7fa") >= 4.5 {
			t.Fatalf("step %d (%s) already reaches 4.5, but got %s", i, step, got)
		}
	}
	// Unreachable: ends at the target colour.
	if got := ensureContrast("#777777", "#808080", 30, white); got != white {
		t.Errorf("unreachable contrast: got %s, want %s", got, white)
	}
}

func TestBestText(t *testing.T) {
	tests := []struct{ bg, dark, want string }{
		{"#ffffff", nearBlack, nearBlack},
		{"#000000", nearBlack, white},
		{"#ffd400", nearBlack, nearBlack},
		{"#002855", nearBlack, white},
		{"#1f5bd8", nearBlack, white},
		{"#808080", headerTextDark, headerTextDark},
	}
	for _, tc := range tests {
		if got := bestText(tc.bg, tc.dark); got != tc.want {
			t.Errorf("bestText(%s, %s) = %s, want %s", tc.bg, tc.dark, got, tc.want)
		}
	}
}
