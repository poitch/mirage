package fsx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// ErrNoSuchUpload is returned for a transfer that was never started, or has
// already been assembled or discarded.
var ErrNoSuchUpload = errors.New("no such upload")

// transferIDRe constrains the client-chosen transfer identifier. It becomes a
// directory name, so it must be a single harmless path segment.
var transferIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// chunkNameRe constrains a chunk name. The protocol numbers chunks, and they
// are assembled in the order of their names, so anything non-numeric would have
// no defined position.
var chunkNameRe = regexp.MustCompile(`^[0-9]{1,10}$`)

// ValidTransferID reports whether id is usable as an upload identifier.
func ValidTransferID(id string) bool { return transferIDRe.MatchString(id) }

// ValidChunkName reports whether name is usable as a chunk identifier.
func ValidChunkName(name string) bool { return chunkNameRe.MatchString(name) }

// Chunk is one uploaded piece of a pending transfer.
type Chunk struct {
	Name  string
	Size  int64
	MTime time.Time
	// Number is the chunk's numeric position, used to order assembly.
	Number uint64
}

func uploadPath(id string) string       { return UploadDir + "/" + id }
func chunkPath(id, chunk string) string { return UploadDir + "/" + id + "/" + chunk }

// CreateUpload starts a transfer, creating its directory.
func (s *Storage) CreateUpload(id string) error {
	if !ValidTransferID(id) {
		return fmt.Errorf("%w: malformed transfer id", ErrInvalidPath)
	}
	if err := s.root.MkdirAll(UploadDir, s.dirMode); err != nil {
		return fmt.Errorf("create upload area: %w", err)
	}
	if err := s.root.Mkdir(uploadPath(id), s.dirMode); err != nil {
		return err
	}
	return nil
}

// UploadExists reports whether a transfer directory is present.
func (s *Storage) UploadExists(id string) bool {
	if !ValidTransferID(id) {
		return false
	}
	info, err := s.root.Stat(uploadPath(id))
	return err == nil && info.IsDir()
}

// WriteChunk stores one piece of a transfer.
//
// Chunks are written directly rather than through the atomic path used for real
// files: they are already invisible to clients and to the index, and a chunk
// that fails mid-write is simply re-uploaded.
func (s *Storage) WriteChunk(id, chunk string, r io.Reader, maxBytes int64) (int64, error) {
	if !ValidTransferID(id) || !ValidChunkName(chunk) {
		return 0, fmt.Errorf("%w: malformed chunk reference", ErrInvalidPath)
	}
	if !s.UploadExists(id) {
		return 0, ErrNoSuchUpload
	}

	f, err := s.root.OpenFile(chunkPath(id, chunk), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, s.fileMode)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	src := r
	if maxBytes > 0 {
		src = io.LimitReader(r, maxBytes+1)
	}
	n, err := io.Copy(f, src)
	if err != nil {
		return n, err
	}
	if maxBytes > 0 && n > maxBytes {
		s.root.Remove(chunkPath(id, chunk)) //nolint:errcheck // best effort
		return n, ErrQuotaExceeded
	}
	return n, f.Sync()
}

// ListChunks returns a transfer's chunks in assembly order.
//
// Ordering is numeric, not lexicographic. Clients pad chunk names
// inconsistently, and sorting "10" before "9" would silently corrupt the
// assembled file rather than fail.
func (s *Storage) ListChunks(id string) ([]Chunk, error) {
	if !ValidTransferID(id) {
		return nil, fmt.Errorf("%w: malformed transfer id", ErrInvalidPath)
	}
	f, err := s.root.Open(uploadPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNoSuchUpload
		}
		return nil, err
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}

	chunks := make([]Chunk, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !ValidChunkName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		number, err := strconv.ParseUint(e.Name(), 10, 64)
		if err != nil {
			continue
		}
		chunks = append(chunks, Chunk{
			Name: e.Name(), Size: info.Size(), MTime: info.ModTime(), Number: number,
		})
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].Number < chunks[j].Number })
	return chunks, nil
}

// AssembleUpload concatenates a transfer's chunks into dest and discards the
// transfer.
//
// Assembly reuses the ordinary write path, so the result is placed atomically,
// owned, permissioned and timestamped exactly like a single-request upload, and
// a checksum covers the whole reassembled file rather than any one chunk.
func (s *Storage) AssembleUpload(id, dest string, opts WriteOptions) (WriteResult, error) {
	chunks, err := s.ListChunks(id)
	if err != nil {
		return WriteResult{}, err
	}
	if len(chunks) == 0 {
		return WriteResult{}, fmt.Errorf("%w: no chunks were uploaded", ErrNoSuchUpload)
	}

	readers := make([]io.Reader, 0, len(chunks))
	closers := make([]io.Closer, 0, len(chunks))
	defer func() {
		for _, c := range closers {
			c.Close() //nolint:errcheck // read-only handles
		}
	}()
	for _, c := range chunks {
		f, err := s.root.Open(chunkPath(id, c.Name))
		if err != nil {
			return WriteResult{}, fmt.Errorf("read chunk %s: %w", c.Name, err)
		}
		readers = append(readers, f)
		closers = append(closers, f)
	}

	res, err := s.WriteFile(dest, io.MultiReader(readers...), opts)
	if err != nil {
		// The transfer is deliberately left in place. A rejected checksum or a
		// full disk is often worth retrying, and discarding the chunks would
		// force the client to upload everything again.
		return WriteResult{}, err
	}

	if err := s.DiscardUpload(id); err != nil {
		// The file is already published; failing to tidy up is not worth
		// failing the request over.
		return res, nil
	}
	return res, nil
}

// DiscardUpload removes a transfer and its chunks.
func (s *Storage) DiscardUpload(id string) error {
	if !ValidTransferID(id) {
		return fmt.Errorf("%w: malformed transfer id", ErrInvalidPath)
	}
	return s.root.RemoveAll(uploadPath(id))
}

// PruneUploads deletes transfers untouched since before cutoff, and reports how
// many went.
//
// Abandoned transfers are otherwise permanent: a client that starts a large
// upload and never returns would leave its chunks occupying the user's disk
// forever.
func (s *Storage) PruneUploads(cutoff time.Time) (int, error) {
	f, err := s.root.Open(UploadDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return 0, err
	}

	var removed int
	for _, e := range entries {
		if !e.IsDir() || !ValidTransferID(e.Name()) {
			continue
		}
		// A transfer's age is that of its newest chunk, so an upload still
		// making progress is never collected out from under itself.
		newest, err := s.newestChunkTime(e.Name())
		if err != nil {
			continue
		}
		if newest.After(cutoff) {
			continue
		}
		if err := s.DiscardUpload(e.Name()); err == nil {
			removed++
		}
	}
	return removed, nil
}

func (s *Storage) newestChunkTime(id string) (time.Time, error) {
	info, err := s.root.Stat(uploadPath(id))
	if err != nil {
		return time.Time{}, err
	}
	newest := info.ModTime()
	chunks, err := s.ListChunks(id)
	if err != nil {
		return newest, nil
	}
	for _, c := range chunks {
		if c.MTime.After(newest) {
			newest = c.MTime
		}
	}
	return newest, nil
}
