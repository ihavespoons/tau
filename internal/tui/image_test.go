package tui

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
	"testing"
)

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noisyPNG makes an image PNG cannot compress, which is the only way to get a
// payload past one chunk without making the test slow. The generator is a
// fixed LCG so the bytes are the same on every run.
func noisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(1)
	next := func() uint8 {
		seed = seed*1664525 + 1013904223
		return uint8(seed >> 24)
	}
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{R: next(), G: next(), B: next(), A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func envFrom(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestDetectingTheImageProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want imageProtocol
	}{
		{"kitty by TERM", map[string]string{"TERM": "xterm-kitty"}, imageKitty},
		{"kitty by window id", map[string]string{"KITTY_WINDOW_ID": "1"}, imageKitty},
		{"ghostty", map[string]string{"TERM_PROGRAM": "ghostty"}, imageKitty},
		{"wezterm", map[string]string{"TERM_PROGRAM": "WezTerm"}, imageKitty},
		{"iterm2", map[string]string{"TERM_PROGRAM": "iTerm.app"}, imageITerm2},
		// LC_TERMINAL is the one that survives ssh, where TERM_PROGRAM does not.
		{"iterm2 over ssh", map[string]string{"LC_TERMINAL": "iTerm2"}, imageITerm2},
		{"apple terminal", map[string]string{"TERM_PROGRAM": "Apple_Terminal"}, imageNone},
		{"vscode", map[string]string{"TERM_PROGRAM": "vscode"}, imageNone},
		{"nothing at all", map[string]string{}, imageNone},
		// The escape hatch has to beat a terminal that would otherwise qualify.
		{"opted out", map[string]string{"TERM": "xterm-kitty", "TAU_NO_IMAGES": "1"}, imageNone},
	} {
		if got := detectImageProtocol(envFrom(tc.env)); got != tc.want {
			t.Errorf("%s: protocol = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A cell is about twice as tall as it is wide. Without the halving every image
// comes out stretched to twice its height.
func TestSizingAnImageInCells(t *testing.T) {
	// A square image in 40 columns is 20 rows, not 40.
	if cols, rows := imageBox(100, 100, 40); cols != 40 || rows != 20 {
		t.Errorf("square image = %dx%d cells, want 40x20", cols, rows)
	}
	// A wide image is shorter.
	if _, rows := imageBox(400, 100, 40); rows != 5 {
		t.Errorf("wide image = %d rows, want 5", rows)
	}
}

// A screenshot pasted into a conversation is a reference, not the conversation.
func TestATallImageIsCappedAndStaysInProportion(t *testing.T) {
	cols, rows := imageBox(100, 1000, 80)
	if rows != imageMaxRows {
		t.Errorf("rows = %d, want the cap of %d", rows, imageMaxRows)
	}
	// Narrowed to keep the aspect ratio rather than squashed into 80 columns.
	if cols >= 80 {
		t.Errorf("cols = %d, want it narrowed to match the capped height", cols)
	}
}

func TestKittyTransmitsAndDrawsInOne(t *testing.T) {
	esc := kittyEscape(testPNG(t, 8, 8), 10, 5)

	if !strings.HasPrefix(esc, "\x1b_Ga=T,f=100,c=10,r=5,m=") {
		t.Errorf("escape does not open with a placed transmit-and-display: %q", esc[:min(60, len(esc))])
	}
	if !strings.HasSuffix(esc, "\x1b\\") {
		t.Error("escape is not terminated")
	}
	if !strings.Contains(esc, "m=0;") {
		t.Error("no chunk is marked final, so kitty would wait forever")
	}
}

// The protocol asks for 4096-byte payload chunks, and every chunk but the last
// has to say that more is coming.
func TestKittyChunksALargePayload(t *testing.T) {
	esc := kittyEscape(noisyPNG(t, 80, 80), 40, 20)

	chunks := strings.Count(esc, "\x1b_G")
	if chunks < 2 {
		t.Fatalf("payload was not chunked: %d chunk(s)", chunks)
	}
	if got := strings.Count(esc, "m=1;"); got != chunks-1 {
		t.Errorf("%d chunks marked as continuing, want %d", got, chunks-1)
	}
	if got := strings.Count(esc, "m=0;"); got != 1 {
		t.Errorf("%d chunks marked final, want exactly one", got)
	}
	// Only the first chunk carries the placement keys; repeating them would be
	// a second image.
	if got := strings.Count(esc, "a=T"); got != 1 {
		t.Errorf("a=T appears %d times, want once", got)
	}
}

func TestITerm2CarriesTheSizeAndTheBox(t *testing.T) {
	data := testPNG(t, 8, 8)
	esc := iterm2Escape(data, 10, 5)

	for _, want := range []string{
		"\x1b]1337;File=inline=1;", "size=" + strconv.Itoa(len(data)),
		"width=10", "height=5", "preserveAspectRatio=1:",
	} {
		if !strings.Contains(esc, want) {
			t.Errorf("escape is missing %q", want)
		}
	}
	if !strings.HasSuffix(esc, "\a") {
		t.Error("escape is not terminated with BEL")
	}
	body := esc[strings.Index(esc, ":")+1 : len(esc)-1]
	if _, err := base64.StdEncoding.DecodeString(body); err != nil {
		t.Errorf("payload is not base64: %v", err)
	}
}

// Knowing an image was there matters more than seeing it, and this is what
// tau prints under tmux, in CI, and in most terminals.
func TestATerminalThatCannotDrawGetsADescription(t *testing.T) {
	out := renderImage(imageNone, testPNG(t, 640, 480), "image/png", 80)

	for _, want := range []string{"png", "640×480"} {
		if !strings.Contains(out, want) {
			t.Errorf("placeholder %q is missing %q", out, want)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("a terminal that cannot draw was sent an escape: %q", out)
	}
}

func TestUndecodableDataStillSaysSomething(t *testing.T) {
	out := renderImage(imageITerm2, []byte("not an image"), "image/webp", 80)

	if strings.Contains(out, "\x1b") {
		t.Errorf("undecodable data was sent as an escape: %q", out)
	}
	if !strings.Contains(out, "webp") {
		t.Errorf("placeholder %q does not say what it was", out)
	}
}

// kitty's direct transfer format is PNG, so anything else has to be converted
// rather than dropped.
func TestKittyGetsAPNGEvenFromAJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}

	out, err := asPNG(buf.Bytes(), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("the converted image is not a PNG: %v", err)
	}

	// An image that is already a PNG is passed through untouched: re-encoding
	// it would cost time and change nothing.
	src := testPNG(t, 4, 4)
	same, err := asPNG(src, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(src, same) {
		t.Error("a PNG was needlessly re-encoded")
	}
}
