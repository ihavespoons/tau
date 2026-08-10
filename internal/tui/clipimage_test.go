package tui

import (
	"encoding/base64"
	"testing"

	"github.com/ihavespoons/tau/ai"
)

// osascript has no way to hand back raw bytes: it prints «data PNGf<hex>», and
// the shell-out around this is the only part that cannot be tested without
// clobbering whatever the person running the tests had copied.
func TestParsingAppleScriptData(t *testing.T) {
	out, err := parseAppleScriptData("«data PNGf89504E470D0A1A0A»\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	if string(out) != string(want) {
		t.Errorf("decoded % x, want % x", out, want)
	}
}

func TestParsingSomethingThatIsNotAnImage(t *testing.T) {
	for _, in := range []string{"", "some copied text", "«data PNGf»", "«data PNGfZZ»"} {
		if _, err := parseAppleScriptData(in); err == nil {
			t.Errorf("parsing %q should have failed", in)
		}
	}
}

func TestAttachmentsWaitForAPrompt(t *testing.T) {
	a := &app{}
	png := testPNG(t, 4, 4)

	a.attachImage(clipboardImageMsg{data: png, mime: "image/png"})
	if len(a.pendingImages) != 1 {
		t.Fatalf("%d images attached, want 1", len(a.pendingImages))
	}
	if a.notice == "" {
		t.Error("attaching an image said nothing")
	}

	// Text first: a model reads the instruction before the picture.
	content := a.userContent("what is wrong with this")
	if len(content.Blocks) != 2 {
		t.Fatalf("content has %d blocks, want text plus image", len(content.Blocks))
	}
	if txt, ok := content.Blocks[0].(ai.TextContent); !ok || txt.Text != "what is wrong with this" {
		t.Errorf("first block is %#v, want the prompt", content.Blocks[0])
	}
	if img, ok := content.Blocks[1].(ai.ImageContent); !ok {
		t.Errorf("second block is %#v, want the image", content.Blocks[1])
	} else if img.Data != base64.StdEncoding.EncodeToString(png) {
		t.Error("the attached image is not the one that was pasted")
	}

	// Sending them clears them, or the next message carries them again.
	if len(a.pendingImages) != 0 {
		t.Errorf("%d images still pending after being sent", len(a.pendingImages))
	}
}

// A clipboard with text on it is what the key does when pressed by mistake.
func TestNothingOnTheClipboardIsNotAnError(t *testing.T) {
	a := &app{}
	a.attachImage(clipboardImageMsg{err: errNoClipboardImage})

	if len(a.pendingImages) != 0 {
		t.Error("a failed read attached something")
	}
	if a.notice == "" {
		t.Error("a failed read said nothing at all")
	}
}

// With nothing pasted the message stays a plain string, which is the shape Pi
// writes into a session file for a text-only turn.
func TestAPlainMessageStaysAString(t *testing.T) {
	a := &app{}
	content := a.userContent("just words")

	if content.Blocks != nil {
		t.Errorf("blocks = %#v, want nil", content.Blocks)
	}
	if content.Text != "just words" {
		t.Errorf("text = %q", content.Text)
	}
}
