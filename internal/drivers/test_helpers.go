package drivers

import "io"

// mustClose is a test helper that closes and ignores errors
func mustClose(c io.Closer) {
	_ = c.Close()
}

// mustCopy is a test helper for io.Copy in tests
func mustCopy(dst io.Writer, src io.Reader) int64 {
	n, _ := io.Copy(dst, src)
	return n
}

// mustWrite is a test helper for Write operations in tests
// Returns both values to match Write signature
func mustWrite(w io.Writer, data []byte) (int, error) {
	return w.Write(data)
}
