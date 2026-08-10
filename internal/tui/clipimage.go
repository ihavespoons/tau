package tui

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// errNoClipboardImage means the clipboard holds something that is not an
// image. It is the ordinary outcome of pressing the key by mistake, so it is a
// sentinel to check rather than a message to show.
var errNoClipboardImage = errors.New("no image on the clipboard")

// readClipboardImage returns the image on the clipboard as PNG bytes.
//
// There is no portable way to read a clipboard and tau builds without cgo, so
// this shells out to whatever the platform ships — the same bargain already
// made for fd and ripgrep. Each helper is asked for PNG specifically, which
// makes the platform do any conversion.
func readClipboardImage(ctx context.Context) ([]byte, string, error) {
	switch runtime.GOOS {
	case "darwin":
		return macClipboardImage(ctx)
	case "windows":
		return windowsClipboardImage(ctx)
	default:
		return unixClipboardImage(ctx)
	}
}

// macClipboardImage asks AppleScript for the clipboard as a PNG.
//
// osascript prints it as «data PNGf<hex>», which is the only shape it offers
// without writing a temporary file.
func macClipboardImage(ctx context.Context) ([]byte, string, error) {
	out, err := exec.CommandContext(ctx, "osascript", "-e",
		"the clipboard as «class PNGf»").Output()
	if err != nil {
		return nil, "", errNoClipboardImage
	}
	data, err := parseAppleScriptData(string(out))
	if err != nil {
		return nil, "", err
	}
	return data, "image/png", nil
}

// parseAppleScriptData pulls the bytes out of osascript's «data PNGf…» form.
//
// It is split out because it is the only part of the macOS path that can be
// tested: putting an image on a real clipboard to read it back would clobber
// whatever the person running the tests had copied.
func parseAppleScriptData(out string) ([]byte, error) {
	s := strings.TrimSpace(out)
	start := strings.Index(s, "PNGf")
	end := strings.LastIndex(s, "»")
	if start < 0 || end <= start+4 {
		return nil, errNoClipboardImage
	}
	data, err := hex.DecodeString(strings.TrimSpace(s[start+4 : end]))
	if err != nil || len(data) == 0 {
		return nil, errNoClipboardImage
	}
	return data, nil
}

// unixClipboardImage tries Wayland first and X11 second, because a Wayland
// session often still has xclip on PATH talking to an Xwayland clipboard that
// is not the one the screenshot went to.
func unixClipboardImage(ctx context.Context) ([]byte, string, error) {
	for _, c := range [][]string{
		{"wl-paste", "--no-newline", "--type", "image/png"},
		{"xclip", "-selection", "clipboard", "-t", "image/png", "-o"},
	} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		out, err := exec.CommandContext(ctx, c[0], c[1:]...).Output()
		if err == nil && len(out) > 0 {
			return out, "image/png", nil
		}
	}
	return nil, "", errNoClipboardImage
}

// windowsClipboardImage goes through PowerShell, which is the only clipboard
// reader guaranteed to be present.
func windowsClipboardImage(ctx context.Context) ([]byte, string, error) {
	const script = `Add-Type -AssemblyName System.Windows.Forms,System.Drawing
$img = [Windows.Forms.Clipboard]::GetImage()
if ($img -ne $null) {
  $ms = New-Object IO.MemoryStream
  $img.Save($ms, [Drawing.Imaging.ImageFormat]::Png)
  [Convert]::ToBase64String($ms.ToArray())
}`
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", script).Output()
	if err != nil {
		return nil, "", errNoClipboardImage
	}
	// PowerShell wraps long base64 output, so the newlines have to come out
	// before it will decode.
	encoded := strings.Join(strings.Fields(string(out)), "")
	if encoded == "" {
		return nil, "", errNoClipboardImage
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return nil, "", errNoClipboardImage
	}
	return data, "image/png", nil
}
