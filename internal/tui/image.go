package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/gif"  // registered so DecodeConfig can size a GIF
	_ "image/jpeg" // and a JPEG
	"image/png"
	"strconv"
	"strings"
)

// imageProtocol is how a terminal draws an image inline, if it can at all.
type imageProtocol int

const (
	imageNone imageProtocol = iota
	imageKitty
	imageITerm2
)

// imageMaxRows caps how much of the screen one image takes. A screenshot
// pasted into a conversation is a reference, not the conversation.
const imageMaxRows = 20

// detectImageProtocol works out what the terminal will accept.
//
// There is no reliable way to ask. A terminal that does not understand a
// capability query prints it, so probing costs a line of garbage in exactly the
// terminals that cannot draw the image anyway — the environment is the safer
// signal, and anything unrecognised falls back to a text placeholder.
func detectImageProtocol(getenv func(string) string) imageProtocol {
	// An escape hatch for a terminal that lies, or a multiplexer that does not
	// forward what its host can do.
	if getenv("TAU_NO_IMAGES") != "" {
		return imageNone
	}
	program := getenv("TERM_PROGRAM")
	switch {
	case getenv("KITTY_WINDOW_ID") != "", getenv("TERM") == "xterm-kitty":
		return imageKitty
	case getenv("GHOSTTY_RESOURCES_DIR") != "", strings.EqualFold(program, "ghostty"):
		return imageKitty
	// WezTerm speaks both; kitty's protocol is the one that can place an image
	// in a fixed box.
	case strings.EqualFold(program, "WezTerm"):
		return imageKitty
	case program == "iTerm.app", getenv("LC_TERMINAL") == "iTerm2":
		return imageITerm2
	}
	return imageNone
}

// renderImage encodes an image for the terminal, or describes it when the
// terminal cannot draw one.
//
// The description is not a failure mode to apologize for: it is what tau prints
// under tmux, in CI, and in every terminal older than about 2018, and knowing an
// image was there matters more than seeing it.
func renderImage(proto imageProtocol, data []byte, mime string, maxCols int) string {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		// Undecodable here does not mean undecodable everywhere — tau reads
		// three formats and a terminal may know more — but without dimensions
		// there is no box to draw it in.
		return imagePlaceholder(len(data), mime, 0, 0)
	}
	cols, rows := imageBox(cfg.Width, cfg.Height, maxCols)

	switch proto {
	case imageKitty:
		png, err := asPNG(data, mime)
		if err != nil {
			break
		}
		return kittyEscape(png, cols, rows)
	case imageITerm2:
		return iterm2Escape(data, cols, rows)
	}
	return imagePlaceholder(len(data), mime, cfg.Width, cfg.Height)
}

// imageBox sizes the image in terminal cells.
//
// A cell is about twice as tall as it is wide, which is the only reason the
// halving is here: without it every image comes out stretched to twice its
// height.
func imageBox(w, h, maxCols int) (cols, rows int) {
	if maxCols < 4 {
		maxCols = 4
	}
	cols = maxCols
	rows = max(1, cols*h/(w*2))
	if rows > imageMaxRows {
		rows = imageMaxRows
		cols = max(4, rows*2*w/h)
	}
	return cols, rows
}

// asPNG re-encodes anything that is not already a PNG, because kitty's direct
// transfer format is PNG and nothing else that is worth the trouble.
func asPNG(data []byte, mime string) ([]byte, error) {
	if strings.Contains(mime, "png") {
		return data, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// kittyChunk is the payload size kitty's protocol asks transfers to be split
// into. It is a protocol constant, not a tuning knob.
const kittyChunk = 4096

// kittyEscape builds a kitty graphics transmit-and-display sequence.
//
// a=T transmits and draws in one go, f=100 says the payload is a PNG, and c/r
// place it in a box of cells so the image scales to the transcript rather than
// to its own pixel size. m=1 on every chunk but the last is how the protocol
// spells "more coming".
func kittyEscape(png []byte, cols, rows int) string {
	payload := base64.StdEncoding.EncodeToString(png)

	var b strings.Builder
	first := true
	for len(payload) > 0 {
		n := min(kittyChunk, len(payload))
		chunk := payload[:n]
		payload = payload[n:]

		more := "0"
		if len(payload) > 0 {
			more = "1"
		}
		b.WriteString("\x1b_G")
		if first {
			fmt.Fprintf(&b, "a=T,f=100,c=%d,r=%d,", cols, rows)
			first = false
		}
		b.WriteString("m=" + more + ";")
		b.WriteString(chunk)
		b.WriteString("\x1b\\")
	}
	return b.String()
}

// iterm2Escape builds an iTerm2 inline-image sequence.
//
// width and height are in cells when written bare, which is what keeps the
// image inside the transcript's column width. preserveAspectRatio stops the
// box from stretching it.
func iterm2Escape(data []byte, cols, rows int) string {
	var b strings.Builder
	b.WriteString("\x1b]1337;File=inline=1;size=")
	b.WriteString(strconv.Itoa(len(data)))
	b.WriteString(";width=" + strconv.Itoa(cols))
	b.WriteString(";height=" + strconv.Itoa(rows))
	b.WriteString(";preserveAspectRatio=1:")
	b.WriteString(base64.StdEncoding.EncodeToString(data))
	b.WriteString("\a")
	return b.String()
}

// imagePlaceholder describes an image that cannot be drawn. Unknown dimensions
// are left out rather than printed as zeros.
func imagePlaceholder(size int, mime string, w, h int) string {
	kind := mime
	if i := strings.LastIndex(kind, "/"); i >= 0 {
		kind = kind[i+1:]
	}
	if kind == "" {
		kind = "image"
	}
	if w > 0 && h > 0 {
		return fmt.Sprintf("[%s %d×%d, %s]", kind, w, h, humanBytes(size))
	}
	return fmt.Sprintf("[%s, %s]", kind, humanBytes(size))
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f kB", float64(n)/(1<<10))
	default:
		return strconv.Itoa(n) + " B"
	}
}
