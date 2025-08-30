//go:build wasm

package main

import (
	"bytes"
	"unsafe"
)

//export malloc
func malloc(size uint32) uint32 {
	buf := make([]byte, size)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

//export transform
func transform(ptr uint32, size uint32) (uint32, uint32) {
	input := make([]byte, size)
	for i := uint32(0); i < size; i++ {
		input[i] = *(*byte)(unsafe.Pointer(uintptr(ptr + i)))
	}

	output := bytes.ToUpper(input)
	outPtr := malloc(uint32(len(output)))

	for i, b := range output {
		*(*byte)(unsafe.Pointer(uintptr(outPtr + uint32(i)))) = b
	}

	return outPtr, uint32(len(output))
}

func main() {}
