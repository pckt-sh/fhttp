package http

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func brBody(t testing.TB, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstdBody(t testing.TB, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pooledCodings are the content codings whose decoders are recycled the way
// gzip's is, with what each test needs to know about them.
var pooledCodings = []struct {
	name     string
	compress func(testing.TB, string) []byte
	// holdsDecoder reports whether the body still owns a decoder.
	holdsDecoder func(io.ReadCloser) bool
}{
	{"br", brBody, func(b io.ReadCloser) bool { return b.(*brReader).zr != nil }},
	{"zstd", zstdBody, func(b io.ReadCloser) bool { return b.(*zstdReader).zr != nil }},
}

func decodedBody(coding string, compressed []byte) io.ReadCloser {
	return DecompressBodyByType(io.NopCloser(bytes.NewReader(compressed)), coding)
}

// TestDecoderPoolReuse: a decoder that has decoded one body decodes the next
// ones correctly, whatever their sizes and the order they come in.
func TestDecoderPoolReuse(t *testing.T) {
	bodies := []string{
		"first body",
		strings.Repeat("second body, long enough to span the window ", 20000),
		"",
		"third body",
		strings.Repeat("x", 300000),
		"last",
	}
	for _, c := range pooledCodings {
		t.Run(c.name, func(t *testing.T) {
			for round := 0; round < 3; round++ {
				for _, want := range bodies {
					body := decodedBody(c.name, c.compress(t, want))
					got, err := io.ReadAll(body)
					if err != nil {
						t.Fatalf("ReadAll = %v", err)
					}
					if string(got) != want {
						t.Fatalf("decompressed %d bytes, want %d bytes (mismatch)", len(got), len(want))
					}
					if c.holdsDecoder(body) {
						t.Fatal("decoder retained after EOF; it should have been recycled")
					}
					var buf [1]byte
					if n, err := body.Read(buf[:]); n != 0 || err != io.EOF {
						t.Fatalf("Read after EOF = %v, %v; want 0, io.EOF", n, err)
					}
					if err := body.Close(); err != nil {
						t.Fatalf("Close = %v", err)
					}
				}
			}
		})
	}
}

// TestDecoderPoolAfterBadBodies: bodies that are abandoned midway, cut short
// or not in the coding at all leave nothing behind that a later response
// could decode wrongly.
func TestDecoderPoolAfterBadBodies(t *testing.T) {
	long := strings.Repeat("a body that is long enough to be cut in the middle ", 5000)
	const want = "a completely different body"

	for _, c := range pooledCodings {
		t.Run(c.name, func(t *testing.T) {
			check := func(after string) {
				t.Helper()
				for i := 0; i < 3; i++ {
					got, err := io.ReadAll(decodedBody(c.name, c.compress(t, want)))
					if err != nil || string(got) != want {
						t.Fatalf("after %s: got %q, %v; want %q", after, got, err, want)
					}
				}
			}

			// Abandoned before EOF: the decoder is mid-stream and must stay
			// with the body it belongs to.
			body := decodedBody(c.name, c.compress(t, long))
			var buf [4]byte
			if _, err := body.Read(buf[:]); err != nil {
				t.Fatalf("Read = %v", err)
			}
			if err := body.Close(); err != nil {
				t.Fatalf("Close = %v", err)
			}
			if !c.holdsDecoder(body) {
				t.Fatal("mid-stream decoder was released; it must not be recycled")
			}
			check("an abandoned body")

			// Cut short.
			compressed := c.compress(t, long)
			got, _ := io.ReadAll(decodedBody(c.name, compressed[:len(compressed)/2]))
			if string(got) == long {
				t.Fatal("half of the compressed body decoded to all of it")
			}
			check("a truncated body")

			// Not in the coding.
			if _, err := io.ReadAll(decodedBody(c.name, []byte(strings.Repeat("this is not compressed. ", 100)))); err == nil {
				t.Fatal("ReadAll of a body in no coding returned no error")
			}
			check("a corrupt body")
		})
	}
}

// TestDecoderPoolConcurrentBodies: bodies decoded at the same time each get a
// decoder of their own. Run with -race.
func TestDecoderPoolConcurrentBodies(t *testing.T) {
	for _, c := range pooledCodings {
		t.Run(c.name, func(t *testing.T) {
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					want := strings.Repeat(string(rune('a'+g))+" body ", 2000*(g+1))
					compressed := c.compress(t, want)
					for i := 0; i < 50; i++ {
						got, err := io.ReadAll(decodedBody(c.name, compressed))
						if err != nil || string(got) != want {
							t.Errorf("goroutine %d: got %d bytes, %v; want %d bytes", g, len(got), err, len(want))
							return
						}
					}
				}(g)
			}
			wg.Wait()
		})
	}
}

// TestZstdReaderCloseDuringRead: closing a zstd body while a Read is blocked
// on it is how a caller unblocks that Read, so Close must not touch the
// decoder the Read is using. Run with -race.
func TestZstdReaderCloseDuringRead(t *testing.T) {
	pr, pw := io.Pipe()
	body := &zstdReader{body: pr}

	compressed := zstdBody(t, strings.Repeat("never finished ", 10000))
	go pw.Write(compressed[:len(compressed)/2])

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		done <- err
	}()

	// The Read is, or soon will be, blocked on the half that never comes.
	if err := body.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := <-done; err == nil {
		t.Fatal("reading a body that was closed midway returned no error")
	}
}
