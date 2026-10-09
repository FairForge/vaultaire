package engine

import "context"

type expectedSizeKey struct{}

// WithExpectedSize tells a driver the size the caller's record says the
// object has (the head row's size_bytes). A driver that can hold two
// candidate versions of a key at once — the multi-bridge WebDAV backend's
// plain file and striped manifest after an interrupted commit — serves the
// one of that size, never bytes that are not the committed version (Prompt
// 2b B3).
func WithExpectedSize(ctx context.Context, size int64) context.Context {
	return context.WithValue(ctx, expectedSizeKey{}, size)
}

// ExpectedSize is the size WithExpectedSize put on ctx.
func ExpectedSize(ctx context.Context) (int64, bool) {
	n, ok := ctx.Value(expectedSizeKey{}).(int64)
	return n, ok
}
