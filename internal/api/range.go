package api

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type httpRange struct {
	start  int64
	end    int64
	length int64
}

// errMultiRange marks a syntactically valid multi-range request. We serve
// single ranges only; a caller that sees it ignores the header and answers
// 200 with the whole body, which RFC 9110 §14.2 permits (AWS does the same).
var errMultiRange = errors.New("multi-range request")

// rangeParseErr is parseRangeHeader's error alone, for callers that only
// need to classify the header.
func rangeParseErr(header string, totalSize int64) error {
	_, err := parseRangeHeader(header, totalSize)
	return err
}

func parseRangeHeader(header string, totalSize int64) (*httpRange, error) {
	// The range unit is case-insensitive (RFC 9110 §14.1).
	if header == "" || len(header) < 6 || !strings.EqualFold(header[:6], "bytes=") {
		return nil, fmt.Errorf("invalid range header")
	}

	spec := strings.TrimSpace(header[6:])

	if strings.Contains(spec, ",") {
		return nil, errMultiRange
	}
	if totalSize <= 0 {
		// Nothing satisfies a range on an empty object (AWS: 416).
		return nil, fmt.Errorf("unsatisfiable range on a %d-byte object", totalSize)
	}

	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid range spec %q", spec)
	}

	var start, end int64

	if parts[0] == "" {
		// Suffix range: bytes=-N (last N bytes)
		suffix, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || suffix <= 0 {
			return nil, fmt.Errorf("invalid suffix range %q", spec)
		}
		start = totalSize - suffix
		if start < 0 {
			start = 0
		}
		end = totalSize - 1
	} else {
		var err error
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid range start %q", parts[0])
		}

		if parts[1] == "" {
			// Open-ended: bytes=N-
			end = totalSize - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid range end %q", parts[1])
			}
		}
	}

	if end >= totalSize {
		end = totalSize - 1
	}

	if start > end || start >= totalSize {
		return nil, fmt.Errorf("unsatisfiable range %d-%d/%d", start, end, totalSize)
	}

	return &httpRange{
		start:  start,
		end:    end,
		length: end - start + 1,
	}, nil
}

func serveRange(w http.ResponseWriter, reader io.Reader, rng *httpRange, totalSize int64, contentType string) error {
	if seeker, ok := reader.(io.ReadSeeker); ok {
		if _, err := seeker.Seek(rng.start, io.SeekStart); err != nil {
			return fmt.Errorf("seek to %d: %w", rng.start, err)
		}
	} else if rng.start > 0 {
		if _, err := io.CopyN(io.Discard, reader, rng.start); err != nil {
			return fmt.Errorf("discard %d prefix bytes: %w", rng.start, err)
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rng.start, rng.end, totalSize))
	w.Header().Set("Content-Length", strconv.FormatInt(rng.length, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusPartialContent)

	_, err := io.CopyN(w, reader, rng.length)
	return err
}

func writeRangeNotSatisfiable(w http.ResponseWriter, totalSize int64) {
	w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalSize))
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
}
