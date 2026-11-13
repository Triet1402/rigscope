package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// ErrDecompressedLimit is returned when the raw decompressed stream, including
// tar headers, PAX/GNU long-name metadata and padding, exceeds its budget.
var ErrDecompressedLimit = errors.New("archive decompressed byte limit exceeded")

// ExtractOptions bounds tar extraction so a hostile archive cannot exhaust
// memory or CPU.
type ExtractOptions struct {
	// MaxBinary bounds the rigscope member in bytes.
	MaxBinary int64
	// MaxDecompressed bounds the sum of all declared member sizes, including
	// members that are skipped.
	MaxDecompressed int64
	// MaxEntries bounds the number of members.
	MaxEntries int64
	// MaxRawBytes bounds every byte read from the gzip stream after
	// decompression: member bodies, tar headers, PAX/GNU long-name metadata and
	// block padding. Zero selects DefaultMaxRawBytes.
	MaxRawBytes int64
	// MaxBlocks bounds the number of raw 512-byte tar blocks. Zero derives the
	// bound from MaxRawBytes.
	MaxBlocks int64
}

// ExtractBinary returns the contents of the root-level "rigscope" member.
//
// Every member is checked for path traversal before it is skipped, every
// declared size counts against the total decompressed bound, and an entry that
// is too large is rejected before its body is skipped. A counting reader also
// bounds the raw decompressed stream so hidden PAX/GNU metadata, headers and
// padding cannot bypass the limits. The rigscope member must be a regular file.
// Nothing is written to disk.
func ExtractBinary(archive []byte, opts ExtractOptions) ([]byte, error) {
	if opts.MaxBinary <= 0 {
		opts.MaxBinary = DefaultMaxBinary
	}
	if opts.MaxDecompressed <= 0 {
		opts.MaxDecompressed = DefaultMaxDecompressed
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.MaxRawBytes <= 0 {
		opts.MaxRawBytes = DefaultMaxRawBytes
	}
	if opts.MaxBlocks <= 0 {
		opts.MaxBlocks = opts.MaxRawBytes / 512
		if opts.MaxBlocks <= 0 {
			opts.MaxBlocks = 1
		}
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	counter := &countingReader{reader: gz, limit: opts.MaxRawBytes, maxBlocks: opts.MaxBlocks}
	reader := tar.NewReader(counter)
	var total int64
	var entries int64
	var binary []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrDecompressedLimit) {
				return nil, err
			}
			return nil, fmt.Errorf("tar: %w", err)
		}
		entries++
		if entries > opts.MaxEntries {
			return nil, fmt.Errorf("archive has more than %d entries", opts.MaxEntries)
		}
		name, err := checkPathMember(header.Name)
		if err != nil {
			return nil, err
		}
		if header.Size < 0 {
			return nil, fmt.Errorf("archive entry %q has a negative size", header.Name)
		}
		if header.Size > opts.MaxDecompressed-total {
			return nil, fmt.Errorf("archive entries exceed the %d byte decompressed limit", opts.MaxDecompressed)
		}
		total += header.Size
		if name != "rigscope" {
			// tar.Reader discards the body on the next Next call. Its declared
			// size has already been charged against the total bound.
			continue
		}
		if binary != nil {
			return nil, errors.New("archive contains more than one rigscope member")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("archive entry %q is not a regular file", header.Name)
		}
		if header.Size > opts.MaxBinary {
			return nil, fmt.Errorf("rigscope binary exceeds the %d byte limit", opts.MaxBinary)
		}
		data, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil {
			if errors.Is(err, ErrDecompressedLimit) {
				return nil, err
			}
			return nil, fmt.Errorf("read rigscope binary: %w", err)
		}
		if int64(len(data)) > header.Size {
			return nil, errors.New("rigscope binary is larger than its tar header")
		}
		if len(data) == 0 {
			return nil, errors.New("archive contains an empty rigscope binary")
		}
		binary = data
	}
	if binary == nil {
		return nil, errors.New(`archive contains no "rigscope" binary`)
	}
	return binary, nil
}

// countingReader enforces both a byte budget and a raw-block budget on every
// byte read from the decompressed stream.
type countingReader struct {
	reader    io.Reader
	read      int64
	limit     int64
	maxBlocks int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	if r.read >= r.limit {
		return 0, fmt.Errorf("%w (%d bytes)", ErrDecompressedLimit, r.limit)
	}
	remaining := r.limit - r.read
	if int64(len(buffer)) > remaining {
		buffer = buffer[:remaining]
	}
	if blocks := r.read / 512; r.maxBlocks > 0 && blocks >= r.maxBlocks {
		return 0, fmt.Errorf("%w (%d raw tar blocks)", ErrDecompressedLimit, r.maxBlocks)
	}
	n, err := r.reader.Read(buffer)
	r.read += int64(n)
	if r.maxBlocks > 0 && r.read/512 > r.maxBlocks {
		return n, fmt.Errorf("%w (%d raw tar blocks)", ErrDecompressedLimit, r.maxBlocks)
	}
	if err == nil && r.read >= r.limit {
		// The next read reports the exceeded budget.
		return n, nil
	}
	return n, err
}
