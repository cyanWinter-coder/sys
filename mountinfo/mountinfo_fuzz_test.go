//go:build linux

package mountinfo

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// unescapeReference is a verbatim copy of the unescape implementation as
// it existed before the escape handling was hardened with an explicit
// range check. It exists solely to prove, via fuzzing, that the new
// implementation decodes every possible input identically.
func unescapeReference(path string) string {
	if strings.IndexByte(path, '\\') == -1 {
		return path
	}
	buf := make([]byte, len(path))
	bufLen := 0
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '\\' && i+3 < len(path) &&
			(path[i+1] == '0' || path[i+1] == '1') &&
			(path[i+2] >= '0' && path[i+2] <= '7') &&
			(path[i+3] >= '0' && path[i+3] <= '7') {
			c1 := path[i+1] - '0'
			c2 := path[i+2] - '0'
			c3 := path[i+3] - '0'
			c = c1<<6 | c2<<3 | c3
			i += 3
		}
		buf[bufLen] = c
		bufLen++
	}
	return string(buf[:bufLen])
}

func FuzzUnescape(f *testing.F) {
	for _, seed := range []string{
		"",
		"/",
		`/path\040with\040spaces`,
		`\000`,
		`\177`,
		`\200`,
		`\377`,
		`\12`,
		`/\1345`,
		`\x`,
		"\\\\",
		`Это\040тест`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := unescape(in)
		want := unescapeReference(in)
		if got != want {
			t.Fatalf("unescape(%q) = %q, want %q", in, got, want)
		}
		if len(got) > len(in) {
			t.Fatalf("unescape(%q) expanded the input", in)
		}
	})
}

func FuzzParseInfoLine(f *testing.F) {
	for _, seed := range []string{
		`251 15 0:3573 / /mnt/point rw,relatime shared:1 - aufs none rw`,
		fedoraMountinfo,
		`251 15 0\07235 /a\040b /m\040p rw - ext4 /dev/sda1 rw`,
		`malformed`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		// Parsing arbitrary input must never panic; any failure has to
		// be reported as an error instead.
		info, err := parseInfoLine(in)
		if err != nil {
			return
		}
		// Every successfully parsed single-line entry must be
		// reproducible through GetMountsFromReader with identical
		// content. Inputs containing line terminators only exercise
		// the no-panic guarantee, since bufio.Scanner strips trailing
		// '\r' while parseInfoLine treats the string as one raw line.
		if strings.ContainsAny(in, "\n\r") {
			return
		}
		parsed, err := GetMountsFromReader(bytes.NewBufferString(in), nil)
		if err != nil {
			t.Fatalf("parseInfoLine succeeded but GetMountsFromReader failed: %v", err)
		}
		if len(parsed) != 1 || *parsed[0] != *info {
			t.Fatalf("parser mismatch: %#v vs %#v", parsed, info)
		}
	})
}

// TestMountedAndGetMountsConcurrent makes sure mounted detection and
// plain mount table reads can run concurrently without sharing partial
// parse state. Run with -race to make the check meaningful.
func TestMountedAndGetMountsConcurrent(t *testing.T) {
	const goroutines = 16
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*2)

	for g := 0; g < goroutines; g++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				mounted, err := mountedByMountinfo("/")
				if err != nil {
					errCh <- fmt.Errorf("mountedByMountinfo: %w", err)
					return
				}
				if !mounted {
					errCh <- fmt.Errorf("mountedByMountinfo(/) = false, want true")
					return
				}
			}
		}()
		go func(n int) {
			defer wg.Done()
			filter := []FilterFunc{
				nil,
				PrefixFilter("/proc"),
				SingleEntryFilter("/"),
			}[n%3]
			for k := 0; k < 20; k++ {
				if _, err := GetMounts(filter); err != nil {
					errCh <- fmt.Errorf("GetMounts: %w", err)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}
