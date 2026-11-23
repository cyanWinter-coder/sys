package userns

import (
	"runtime"
	"testing"
)

func TestSupported(t *testing.T) {
	want := runtime.GOOS == "linux"
	if got := Supported(); got != want {
		t.Fatalf("Supported() = %v, want %v on %s", got, want, runtime.GOOS)
	}

	// On an unsupported platform RunningInUserNS must report false.
	if !Supported() && RunningInUserNS() {
		t.Fatal("RunningInUserNS() returned true on an unsupported platform")
	}
}
