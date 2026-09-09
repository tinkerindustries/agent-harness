package attachment

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The rejection messages are asserted by substring elsewhere, so they are
// pinned here rather than left to drift.
func TestValidateRejections(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte("mockup bytes"))
	cases := []struct {
		name, mime, data string
		maxBytes         int
		want             string
	}{
		{name: "", mime: "", data: valid, maxBytes: 1024, want: "name is required"},
		{name: "mockup.gif", mime: "image/gif", data: valid, maxBytes: 1024, want: "only PNG, JPEG, and WebP"},
		{name: "mockup.png", mime: "image/webp", data: valid, maxBytes: 1024, want: "does not match"},
		{name: "../mockup.png", mime: "image/png", data: valid, maxBytes: 1024, want: "plain file name"},
		{name: "mockup.png", mime: "image/png", data: "!!!not base64!!!", maxBytes: 1024, want: "not valid base64"},
		{name: "mockup.png", mime: "image/png", data: valid, maxBytes: 3, want: "per-file limit"},
		{name: "mockup.png", mime: "image/png", data: "", maxBytes: 1024, want: "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			_, _, _, err := Validate(tc.name, tc.mime, tc.data, tc.maxBytes)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate(%q) error = %v, want it to contain %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsEachImageType(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString([]byte("mockup bytes"))
	for name, wantMIME := range map[string]string{
		"mockup.png":  "image/png",
		"mockup.jpg":  "image/jpeg",
		"mockup.jpeg": "image/jpeg",
		"mockup.webp": "image/webp",
	} {
		t.Run(name, func(t *testing.T) {
			gotName, gotMIME, data, err := Validate(name, "", valid, 1024)
			if err != nil {
				t.Fatalf("Validate(%q): %v", name, err)
			}
			if gotName != name || gotMIME != wantMIME {
				t.Fatalf("Validate(%q) = (%q, %q), want (%q, %q)", name, gotName, gotMIME, name, wantMIME)
			}
			if string(data) != "mockup bytes" {
				t.Fatalf("Validate(%q) data = %q, want the decoded bytes", name, data)
			}
		})
	}
}

func TestMIMETypeUnknownExtension(t *testing.T) {
	if _, ok := MIMEType("mockup.gif"); ok {
		t.Fatal("MIMEType(mockup.gif) = ok, want an unknown extension refused")
	}
}
