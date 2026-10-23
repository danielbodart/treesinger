package main

import (
	"strings"
	"testing"

	"github.com/danielbodart/treesinger/internal/broker"
)

// The login prompt is what the TypeScript's console.log calls print, blank
// lines included.
func TestLoginPrompt(t *testing.T) {
	var out strings.Builder
	prompt(&out, broker.DeviceAuth{VerificationURI: "https://v", VerificationURIComplete: "https://v?c=X", UserCode: "X"})
	want := "\nTo authorise Treesinger, visit:\n\n" +
		"  https://v?c=X\n" +
		"\nor go to https://v and enter code: X\n\n" +
		"Waiting for approval…\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}
