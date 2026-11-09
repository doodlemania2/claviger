package backupenc_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/doodlemania2/claviger/tools/backupenc"
	"github.com/doodlemania2/claviger/tools/keys"
)

func newKey(t testing.TB) (keys.KeySource, string) {
	t.Helper()

	kek := make([]byte, keys.KEKLength)
	if _, err := crand.Read(kek); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(kek)

	src, err := keys.LoadFromEnv(func(k string) string {
		if k == keys.EnvKEK {
			return b64
		}
		return ""
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	return src, b64
}

func seal(t testing.TB, src keys.KeySource, plain []byte, chunk int) []byte {
	t.Helper()

	var out bytes.Buffer
	w, err := backupenc.NewWriter(context.Background(), &out, src, backupenc.Options{ChunkSize: chunk})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return out.Bytes()
}

func open(src keys.KeySource, enc []byte) ([]byte, error) {
	var out bytes.Buffer
	_, err := backupenc.Decrypt(context.Background(), &out, bytes.NewReader(enc), src)
	return out.Bytes(), err
}

func TestRoundTripSizes(t *testing.T) {
	src, _ := newKey(t)
	const chunk = 1024

	for _, size := range []int{0, 1, chunk - 1, chunk, chunk + 1, 3 * chunk, 3*chunk + 17} {
		plain := make([]byte, size)
		_, _ = crand.Read(plain)

		enc := seal(t, src, plain, chunk)
		if !backupenc.IsEncrypted(enc) {
			t.Fatalf("size %d: missing magic", size)
		}
		if bytes.Contains(enc, plain[:min(size, 32)]) && size >= 32 {
			t.Fatalf("size %d: plaintext leaked", size)
		}

		got, err := open(src, enc)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("size %d: plaintext mismatch", size)
		}
	}
}

func TestSmallWrites(t *testing.T) {
	src, _ := newKey(t)
	plain := bytes.Repeat([]byte("0123456789"), 1000)

	var out bytes.Buffer
	w, _ := backupenc.NewWriter(context.Background(), &out, src, backupenc.Options{ChunkSize: 1024})
	for i := 0; i < len(plain); i += 7 {
		if _, err := w.Write(plain[i:min(i+7, len(plain))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := open(src, out.Bytes())
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("mismatch: %v", err)
	}

	// 1-byte reads
	r, _ := backupenc.NewReader(context.Background(), bytes.NewReader(out.Bytes()), src)
	got, err = io.ReadAll(iotestOneByte{r})
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("one byte reads mismatch: %v", err)
	}
}

type iotestOneByte struct{ r io.Reader }

func (o iotestOneByte) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestTamper(t *testing.T) {
	src, _ := newKey(t)
	const chunk = 1024

	plain := make([]byte, 5*chunk+100)
	_, _ = crand.Read(plain)
	enc := seal(t, src, plain, chunk)
	other := seal(t, src, plain, chunk)

	hdr, err := backupenc.ReadHeader(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	headerLen := len(enc) - (5*(chunk+16) + 100 + 16)
	if headerLen <= len(backupenc.Magic) {
		t.Fatalf("unexpected header length %d (key %s)", headerLen, hdr.KeyID)
	}
	full := chunk + 16

	cases := map[string]func() []byte{
		"flip a byte in the first chunk": func() []byte {
			b := bytes.Clone(enc)
			b[headerLen+5] ^= 1
			return b
		},
		"flip a byte in the last chunk": func() []byte {
			b := bytes.Clone(enc)
			b[len(b)-3] ^= 1
			return b
		},
		"flip a byte in the header chunk size": func() []byte {
			b := bytes.Clone(enc)
			b[13] ^= 0x01
			return b
		},
		"truncate at a chunk boundary": func() []byte {
			return bytes.Clone(enc[:headerLen+2*full])
		},
		"truncate mid chunk": func() []byte {
			return bytes.Clone(enc[:len(enc)-30])
		},
		"truncate to the header": func() []byte {
			return bytes.Clone(enc[:headerLen])
		},
		"truncate the header": func() []byte {
			return bytes.Clone(enc[:headerLen-5])
		},
		"append data": func() []byte {
			return append(bytes.Clone(enc), 1, 2, 3)
		},
		"append a chunk-sized blob": func() []byte {
			return append(bytes.Clone(enc), make([]byte, full)...)
		},
		"reorder chunks": func() []byte {
			b := bytes.Clone(enc)
			c0 := bytes.Clone(b[headerLen : headerLen+full])
			copy(b[headerLen:], b[headerLen+full:headerLen+2*full])
			copy(b[headerLen+full:], c0)
			return b
		},
		"drop a middle chunk": func() []byte {
			b := bytes.Clone(enc[:headerLen+full])
			return append(b, enc[headerLen+2*full:]...)
		},
		"swap in another backup's header": func() []byte {
			b := bytes.Clone(enc)
			copy(b, other[:headerLen])
			return b
		},
		"swap in another backup's chunk": func() []byte {
			b := bytes.Clone(enc)
			copy(b[headerLen+full:headerLen+2*full], other[headerLen+full:headerLen+2*full])
			return b
		},
		"alter the wrapped key": func() []byte {
			b := bytes.Clone(enc)
			b[headerLen-nonceLen-3] ^= 1
			return b
		},
		"not encrypted": func() []byte { return []byte("PK\x03\x04 plain zip") },
		"empty":         func() []byte { return nil },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := open(src, mutate())
			if err == nil {
				t.Fatal("expected an error")
			}
			// whatever was returned before the error is a prefix of the real plaintext
			if !bytes.HasPrefix(plain, got) {
				t.Fatal("returned bytes that are not authentic plaintext")
			}
		})
	}
}

const nonceLen = 12

func TestWrongKey(t *testing.T) {
	src, _ := newKey(t)
	wrong, _ := newKey(t)

	enc := seal(t, src, []byte("secret"), 0)

	if _, err := open(wrong, enc); !errors.Is(err, keys.ErrKeyIDMismatch) {
		t.Fatalf("expected key id mismatch, got %v", err)
	}

	hdr, _ := backupenc.ReadHeader(bytes.NewReader(enc))
	if hdr.KeyID != src.KeyID() {
		t.Fatalf("header key id %q != %q", hdr.KeyID, src.KeyID())
	}
}

func TestRotatedKeyWithOldProvided(t *testing.T) {
	oldSrc, oldB64 := newKey(t)
	newSrc, newB64 := newKey(t)

	enc := seal(t, oldSrc, []byte("old archive"), 0)

	getenv := func(k string) string {
		switch k {
		case keys.EnvKEK:
			return newB64
		case keys.EnvKEKOld:
			return oldB64
		}
		return ""
	}

	multi, err := keys.LoadWithOld(getenv, "")
	if err != nil {
		t.Fatal(err)
	}
	if multi.KeyID() != newSrc.KeyID() {
		t.Fatal("primary should be the new key")
	}

	got, err := open(multi, enc)
	if err != nil || string(got) != "old archive" {
		t.Fatalf("expected the old archive to open with the old key provided, got %q %v", got, err)
	}

	// new archives use the new key id
	enc2 := seal(t, multi, []byte("new"), 0)
	if h, _ := backupenc.ReadHeader(bytes.NewReader(enc2)); h.KeyID != newSrc.KeyID() {
		t.Fatalf("new archive should use the new key id, got %s", h.KeyID)
	}

	// without the old key the old archive reports a mismatch
	if _, err := open(newSrc, enc); !errors.Is(err, keys.ErrKeyIDMismatch) {
		t.Fatalf("expected key id mismatch, got %v", err)
	}
}

func TestChunkSizeBounds(t *testing.T) {
	src, _ := newKey(t)
	for _, cs := range []int{10, 1 << 30} {
		if _, err := backupenc.NewWriter(context.Background(), io.Discard, src, backupenc.Options{ChunkSize: cs}); err == nil {
			t.Fatalf("chunk size %d: expected an error", cs)
		}
	}
}

// the encrypt and decrypt paths hold a few chunks, not the stream
func TestStreamingMemoryBound(t *testing.T) {
	src, _ := newKey(t)

	const total = 96 << 20

	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var s runtime.MemStats
			runtime.ReadMemStats(&s)
			if s.HeapAlloc > peak.Load() {
				peak.Store(s.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	f, err := os.Create(filepath.Join(t.TempDir(), "big.enc"))
	if err != nil {
		t.Fatal(err)
	}

	cw := &maxWriter{w: f}
	w, err := backupenc.NewWriter(context.Background(), cw, src)
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte("claviger"), 8192)
	h := sha256.New()
	for written := 0; written < total; written += len(block) {
		h.Write(block)
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if cw.max > backupenc.DefaultChunkSize+16+256 {
		t.Fatalf("a single write of %d bytes: the stream is not chunked", cw.max)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rh := sha256.New()
	cr := &maxReader{r: f}
	if _, err := backupenc.Decrypt(context.Background(), rh, cr, src); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if cr.max > backupenc.DefaultChunkSize+17 {
		t.Fatalf("a single read of %d bytes: the stream is not chunked", cr.max)
	}
	if !bytes.Equal(h.Sum(nil), rh.Sum(nil)) {
		t.Fatal("hash mismatch")
	}

	close(stop)
	<-done

	if grow := int64(peak.Load()) - int64(base.HeapAlloc); grow > 24<<20 {
		t.Fatalf("heap grew by %d bytes while streaming %d bytes", grow, total)
	}
}

type maxWriter struct {
	w   io.Writer
	max int
}

func (m *maxWriter) Write(p []byte) (int, error) { m.max = max(m.max, len(p)); return m.w.Write(p) }

type maxReader struct {
	r   io.Reader
	max int
}

func (m *maxReader) Read(p []byte) (int, error) { m.max = max(m.max, len(p)); return m.r.Read(p) }
