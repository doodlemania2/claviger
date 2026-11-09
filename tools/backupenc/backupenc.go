// Package backupenc implements the streaming encrypted backup container used
// by Claviger (#78).
//
// An encrypted backup is a plain zip archive run through a chunked AES-256-GCM
// stream. Nothing is buffered beyond one chunk, in either direction.
//
// The format is specified in docs/backup-encryption.md. In short:
//
//	header = magic | version | cipher | chunkSize | kekIdLen | kekId | wrappedLen | wrappedDEK | fileNonce
//	chunk  = AES-256-GCM(streamKey, nonce(fileNonce, i), plaintext, aad = SHA-256(header) | final)
//
// The random per-backup DEK is wrapped by the key-encryption key (see package
// keys), and the AES key of the stream is HKDF-SHA256(DEK, salt=fileNonce,
// info="claviger/backup/v1"). The last chunk is flagged in its AAD, so a
// truncated stream fails authentication instead of looking complete.
package backupenc

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/doodlemania2/claviger/tools/keys"
)

const (
	// Magic starts every encrypted backup (8 bytes).
	Magic = "CLAVBAK\n"

	// Version is the format version written by this package.
	Version = 1

	// CipherAES256GCM is the only cipher id defined by version 1.
	CipherAES256GCM = 1

	// DefaultChunkSize is the plaintext size of every chunk but the last.
	DefaultChunkSize = 64 * 1024

	minChunkSize = 1024
	maxChunkSize = 16 * 1024 * 1024

	maxKeyIDLen   = 256
	maxWrappedLen = 8 * 1024

	nonceSize = 12
	tagSize   = 16
	dekSize   = 32

	hkdfInfo = "claviger/backup/v1"
)

var (
	// ErrNotEncrypted is returned when the stream does not start with [Magic].
	ErrNotEncrypted = errors.New("not an encrypted Claviger backup")

	// ErrUnsupported is returned for a version or cipher this build does not know.
	ErrUnsupported = errors.New("unsupported encrypted backup format")

	// ErrCorrupt is returned when the header is malformed or a chunk fails
	// authentication (altered, reordered, truncated or extended data).
	ErrCorrupt = errors.New("the encrypted backup is corrupted or has been tampered with")
)

// IsEncrypted reports whether the prefix (at least len(Magic) bytes) starts
// an encrypted backup.
func IsEncrypted(prefix []byte) bool {
	return len(prefix) >= len(Magic) && string(prefix[:len(Magic)]) == Magic
}

// Header is the parsed, still unauthenticated, header of an encrypted backup.
type Header struct {
	Version   int
	ChunkSize int
	// KeyID is the id of the KEK that wrapped the DEK.
	KeyID string

	wrapped   []byte
	fileNonce []byte
	raw       []byte
}

// Options tune [NewWriter].
type Options struct {
	// ChunkSize is the plaintext size of a chunk (default [DefaultChunkSize]).
	ChunkSize int
}

func deriveStreamKey(dek, fileNonce []byte) ([]byte, error) {
	return hkdf.Key(sha256.New, dek, fileNonce, hkdfInfo, 32)
}

func newStreamAEAD(dek, fileNonce []byte) (cipher.AEAD, error) {
	key, err := deriveStreamKey(dek, fileNonce)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	return cipher.NewGCM(block)
}

func chunkNonce(dst, fileNonce []byte, index uint64) []byte {
	copy(dst, fileNonce)
	var ctr [8]byte
	binary.BigEndian.PutUint64(ctr[:], index)
	for i := 0; i < 8; i++ {
		dst[nonceSize-8+i] ^= ctr[i]
	}
	return dst
}

func chunkAAD(dst, headerHash []byte, final bool) []byte {
	dst = append(dst[:0], headerHash...)
	if final {
		return append(dst, 1)
	}
	return append(dst, 0)
}

// -------------------------------------------------------------------

type writer struct {
	w          io.Writer
	aead       cipher.AEAD
	fileNonce  []byte
	headerHash []byte
	chunkSize  int
	buf        []byte
	out        []byte
	aad        []byte
	nonce      []byte
	index      uint64
	closed     bool
	err        error
}

// NewWriter writes the header to w and returns a writer that encrypts what is
// written to it. Close must be called to write the final chunk; it does not
// close w.
func NewWriter(ctx context.Context, w io.Writer, src keys.KeySource, opts ...Options) (io.WriteCloser, error) {
	chunkSize := DefaultChunkSize
	if len(opts) > 0 && opts[0].ChunkSize > 0 {
		chunkSize = opts[0].ChunkSize
	}
	if chunkSize < minChunkSize || chunkSize > maxChunkSize {
		return nil, fmt.Errorf("the chunk size must be between %d and %d bytes", minChunkSize, maxChunkSize)
	}

	dek := make([]byte, dekSize)
	if _, err := crand.Read(dek); err != nil {
		return nil, err
	}

	wrapped, err := src.Wrap(ctx, dek)
	if err != nil {
		return nil, fmt.Errorf("failed to wrap the backup key: %w", err)
	}

	kekID := src.KeyID()
	if len(kekID) == 0 || len(kekID) > maxKeyIDLen || len(wrapped) == 0 || len(wrapped) > maxWrappedLen {
		return nil, errors.New("the key source returned a key id or a wrapped key of an unsupported size")
	}

	fileNonce := make([]byte, nonceSize)
	if _, err := crand.Read(fileNonce); err != nil {
		return nil, err
	}

	var hdr bytes.Buffer
	hdr.WriteString(Magic)
	hdr.WriteByte(Version)
	hdr.WriteByte(CipherAES256GCM)
	_ = binary.Write(&hdr, binary.BigEndian, uint32(chunkSize))
	_ = binary.Write(&hdr, binary.BigEndian, uint16(len(kekID)))
	hdr.WriteString(kekID)
	_ = binary.Write(&hdr, binary.BigEndian, uint16(len(wrapped)))
	hdr.Write(wrapped)
	hdr.Write(fileNonce)

	if _, err := w.Write(hdr.Bytes()); err != nil {
		return nil, err
	}

	aead, err := newStreamAEAD(dek, fileNonce)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(hdr.Bytes())

	return &writer{
		w:          w,
		aead:       aead,
		fileNonce:  fileNonce,
		headerHash: sum[:],
		chunkSize:  chunkSize,
		buf:        make([]byte, 0, chunkSize),
		out:        make([]byte, 0, chunkSize+tagSize),
		aad:        make([]byte, 0, sha256.Size+1),
		nonce:      make([]byte, nonceSize),
	}, nil
}

func (e *writer) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.closed {
		return 0, errors.New("backupenc: write on a closed writer")
	}

	total := 0
	for len(p) > 0 {
		// a full buffer is flushed only when more data follows, because the
		// last chunk has to be sealed as final
		if len(e.buf) == e.chunkSize {
			if err := e.flush(false); err != nil {
				return total, err
			}
		}

		n := min(e.chunkSize-len(e.buf), len(p))
		e.buf = append(e.buf, p[:n]...)
		p = p[n:]
		total += n
	}

	return total, nil
}

func (e *writer) flush(final bool) error {
	e.out = e.aead.Seal(
		e.out[:0],
		chunkNonce(e.nonce, e.fileNonce, e.index),
		e.buf,
		chunkAAD(e.aad, e.headerHash, final),
	)
	e.index++
	e.buf = e.buf[:0]

	if _, err := e.w.Write(e.out); err != nil {
		e.err = err
		return err
	}

	return nil
}

func (e *writer) Close() error {
	if e.closed {
		return e.err
	}
	e.closed = true

	if e.err != nil {
		return e.err
	}

	return e.flush(true)
}

// -------------------------------------------------------------------

// ReadHeader reads and parses the header of an encrypted backup without
// unwrapping the key. It reads exactly the header bytes from r.
func ReadHeader(r io.Reader) (*Header, error) {
	fixed := make([]byte, len(Magic)+2+4+2)
	if _, err := io.ReadFull(r, fixed[:len(Magic)]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrNotEncrypted
		}
		return nil, err
	}
	if !IsEncrypted(fixed) {
		return nil, ErrNotEncrypted
	}

	short := func(err error) error {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%w (the header is truncated)", ErrCorrupt)
		}
		return err
	}

	raw := bytes.NewBuffer(append([]byte(nil), fixed[:len(Magic)]...))

	if _, err := io.ReadFull(r, fixed[len(Magic):]); err != nil {
		return nil, short(err)
	}
	raw.Write(fixed[len(Magic):])

	ver, cph := int(fixed[8]), int(fixed[9])
	if ver != Version || cph != CipherAES256GCM {
		return nil, fmt.Errorf("%w (version %d, cipher %d)", ErrUnsupported, ver, cph)
	}

	chunkSize := int(binary.BigEndian.Uint32(fixed[10:14]))
	if chunkSize < minChunkSize || chunkSize > maxChunkSize {
		return nil, fmt.Errorf("%w (invalid chunk size)", ErrCorrupt)
	}

	idLen := int(binary.BigEndian.Uint16(fixed[14:16]))
	if idLen == 0 || idLen > maxKeyIDLen {
		return nil, fmt.Errorf("%w (invalid key id length)", ErrCorrupt)
	}

	rest := make([]byte, idLen+2)
	if _, err := io.ReadFull(r, rest); err != nil {
		return nil, short(err)
	}
	raw.Write(rest)

	kekID := string(rest[:idLen])
	wrappedLen := int(binary.BigEndian.Uint16(rest[idLen:]))
	if wrappedLen == 0 || wrappedLen > maxWrappedLen {
		return nil, fmt.Errorf("%w (invalid wrapped key length)", ErrCorrupt)
	}

	tail := make([]byte, wrappedLen+nonceSize)
	if _, err := io.ReadFull(r, tail); err != nil {
		return nil, short(err)
	}
	raw.Write(tail)

	return &Header{
		Version:   ver,
		ChunkSize: chunkSize,
		KeyID:     kekID,
		wrapped:   tail[:wrappedLen],
		fileNonce: tail[wrappedLen:],
		raw:       raw.Bytes(),
	}, nil
}

type reader struct {
	br         *bufio.Reader
	aead       cipher.AEAD
	fileNonce  []byte
	headerHash []byte
	chunkSize  int
	plain      []byte
	pending    []byte
	aad        []byte
	nonce      []byte
	index      uint64
	done       bool
	err        error
}

// NewReader reads the header from r, unwraps the data key with src and
// returns a reader of the decrypted bytes. A wrong, missing or rotated-away
// key fails here (before any data is returned). Tampering is detected while
// reading, chunk by chunk; the reader never returns unauthenticated bytes.
func NewReader(ctx context.Context, r io.Reader, src keys.KeySource) (io.Reader, error) {
	h, err := ReadHeader(r)
	if err != nil {
		return nil, err
	}

	return newReaderFromHeader(ctx, h, r, src)
}

// NewReaderFromHeader continues after [ReadHeader].
func NewReaderFromHeader(ctx context.Context, h *Header, r io.Reader, src keys.KeySource) (io.Reader, error) {
	return newReaderFromHeader(ctx, h, r, src)
}

func newReaderFromHeader(ctx context.Context, h *Header, r io.Reader, src keys.KeySource) (io.Reader, error) {
	if src == nil {
		return nil, keys.ErrNoKeySource
	}

	dek, err := src.Unwrap(ctx, h.KeyID, h.wrapped)
	if err != nil {
		return nil, err
	}
	if len(dek) != dekSize {
		return nil, fmt.Errorf("%w (unexpected data key size)", ErrCorrupt)
	}

	aead, err := newStreamAEAD(dek, h.fileNonce)
	if err != nil {
		return nil, err
	}

	sum := sha256.Sum256(h.raw)

	return &reader{
		br:         bufio.NewReaderSize(r, h.ChunkSize+tagSize+1),
		aead:       aead,
		fileNonce:  h.fileNonce,
		headerHash: sum[:],
		chunkSize:  h.ChunkSize,
		plain:      make([]byte, 0, h.ChunkSize),
		aad:        make([]byte, 0, sha256.Size+1),
		nonce:      make([]byte, nonceSize),
	}, nil
}

func (d *reader) Read(p []byte) (int, error) {
	for len(d.pending) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			d.err = err
		}
	}

	n := copy(p, d.pending)
	d.pending = d.pending[n:]

	return n, nil
}

func (d *reader) next() error {
	// one byte beyond a full chunk tells whether another chunk follows
	window, err := d.br.Peek(d.chunkSize + tagSize + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	final := len(window) <= d.chunkSize+tagSize
	size := len(window)
	if !final {
		size = d.chunkSize + tagSize
	}

	if size < tagSize {
		return fmt.Errorf("%w (the stream ends before its final chunk: truncated)", ErrCorrupt)
	}

	plain, err := d.aead.Open(
		d.plain[:0],
		chunkNonce(d.nonce, d.fileNonce, d.index),
		window[:size],
		chunkAAD(d.aad, d.headerHash, final),
	)
	if err != nil {
		return fmt.Errorf("%w (chunk %d failed authentication)", ErrCorrupt, d.index)
	}

	if _, err := d.br.Discard(size); err != nil {
		return err
	}

	d.index++
	d.plain = plain
	d.pending = plain
	d.done = final

	return nil
}

// Decrypt decrypts the whole stream from src into dst and returns the number
// of plaintext bytes written. On error the bytes already written are
// authenticated but incomplete, so the caller must discard dst.
func Decrypt(ctx context.Context, dst io.Writer, src io.Reader, ks keys.KeySource) (int64, error) {
	dr, err := NewReader(ctx, src, ks)
	if err != nil {
		return 0, err
	}

	return io.Copy(dst, dr)
}
