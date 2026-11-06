package parfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WP-FLEET-3 D-F3-1: a part whose fetch failed with an error that WRAPS
// io.EOF — io.ReadFull of an empty body, or Go's transport answering `EOF`
// for a connection the server closed — was the stream's clean end to every
// errors.Is(err, io.EOF) check: Reader.WriteTo (what io.Copy uses) returned
// (0, nil), Done() was true, Err() nil. The 2026-11-06 fleet soak saw it 135
// times as `size mismatch: got 0` on a refused range. A part's failure is
// an error, whatever it wraps; the end is only the Stream's own io.EOF.

// eofAt starts a stream of 4 parts whose part `bad` fails with a wrapped
// io.EOF on every attempt (hedging on, so the one retry runs too).
func eofAt(bad int) (*Stream, []byte) {
	data, sizes := parts(4, 1000)
	s := Start(context.Background(), Config{Window: 1 << 20, MaxParts: 4, HedgeAfter: 0}, sizes,
		func(_ context.Context, i int) ([]byte, error) {
			if i == bad {
				return nil, fmt.Errorf("onedrive range %d: %w", i, io.EOF)
			}
			return data[i], nil
		})
	return s, bytes.Join(data[:bad], nil)
}

func TestReader_APartFailingWithAWrappedEOFFailsIOCopy(t *testing.T) {
	for _, bad := range []int{0, 2} {
		t.Run(fmt.Sprintf("part %d", bad), func(t *testing.T) {
			// Arrange
			s, before := eofAt(bad)
			r := NewReader(s, nil)
			defer func() { _ = r.Close() }()
			var got bytes.Buffer

			// Act: io.Copy takes the Reader's WriteTo.
			n, err := io.Copy(&got, r)

			// Assert
			require.Error(t, err, "a refused part is an error, never a short clean body")
			assert.NotErrorIs(t, err, io.EOF)
			assert.ErrorIs(t, err, ErrPartCut)
			assert.Equal(t, int64(len(before)), n)
			assert.Equal(t, string(before), got.String(), "the parts before it, nothing after")
			assert.False(t, r.Done())
			assert.Error(t, r.Err())
		})
	}
}

func TestReader_APartFailingWithAWrappedEOFFailsRead(t *testing.T) {
	// Arrange
	s, before := eofAt(1)
	r := NewReader(s, nil)
	defer func() { _ = r.Close() }()

	// Act: a consumer that reads with io.ReadFull and treats io.EOF as the
	// end (the parity rebuilder's shape).
	buf := make([]byte, len(before))
	_, err := io.ReadFull(r, buf)
	require.NoError(t, err)
	n, err := io.ReadFull(r, make([]byte, 1000))

	// Assert
	assert.Equal(t, 0, n)
	require.Error(t, err)
	assert.False(t, errors.Is(err, io.EOF), "never the end of the stream: %v", err)
	assert.False(t, errors.Is(err, io.ErrUnexpectedEOF), "never a short read either")
}

func TestStream_NextNeverHandsAPartFailureOutAsEOF(t *testing.T) {
	// Arrange
	s, _ := eofAt(0)
	defer s.Close()

	// Act
	_, err := s.Next()

	// Assert
	require.Error(t, err)
	assert.NotErrorIs(t, err, io.EOF)
	assert.ErrorIs(t, err, ErrPartCut)
	assert.Contains(t, err.Error(), "onedrive range 0", "the fetch's own words are kept")
}

func TestStream_TheEndIsStillABareEOF(t *testing.T) {
	data, sizes := parts(2, 10)
	s := Start(context.Background(), Config{}, sizes,
		func(_ context.Context, i int) ([]byte, error) { return data[i], nil })
	defer s.Close()
	_, err := s.Next()
	require.NoError(t, err)
	_, err = s.Next()
	require.NoError(t, err)

	_, err = s.Next()

	assert.Same(t, io.EOF, err) // the identity is the contract
}
