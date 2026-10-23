package e2ee

import (
	"crypto/sha256"
	_ "embed" // the BIP-39 wordlist
	"errors"
	"fmt"
	"strings"
)

// bip39English is BIP-39's English wordlist, byte for byte (MIT; see
// bip39_english.LICENSE). TestMnemonic_WordlistIsBIP39 pins its SHA-256.
//
//go:embed bip39_english.txt
var bip39English string

var (
	bip39Words = strings.Split(strings.TrimSuffix(bip39English, "\n"), "\n")
	bip39Index = func() map[string]int {
		m := make(map[string]int, len(bip39Words))
		for i, w := range bip39Words {
			m[w] = i
		}
		return m
	}()
)

// MnemonicWords is the length of a recovery mnemonic: 256 bits of key and 8
// bits of checksum, 11 bits a word.
const MnemonicWords = 24

var (
	// ErrMnemonicWord: a word that is not in BIP-39's English list, or not 24
	// words.
	ErrMnemonicWord = errors.New("e2ee: not a 24-word recovery phrase")
	// ErrMnemonicChecksum: 24 known words whose checksum does not match — a
	// mistyped word or two words swapped.
	ErrMnemonicChecksum = errors.New("e2ee: the recovery phrase's checksum does not match")
)

// RecoveryMnemonic is the recovery key as the 24 words a person writes
// down: BIP-39 (English list) over the key's 32 bytes, its checksum the
// first byte of SHA-256(key). Only the encoding is BIP-39's: the words are
// never run through BIP-39's PBKDF2 seed derivation, they ARE the key.
func RecoveryMnemonic(recoveryKey []byte) (string, error) {
	if len(recoveryKey) != KeySize {
		return "", ErrKeySize
	}
	sum := sha256.Sum256(recoveryKey)
	bits := append(append([]byte{}, recoveryKey...), sum[0])
	words := make([]string, MnemonicWords)
	for i := range words {
		words[i] = bip39Words[bits11(bits, i)]
	}
	return strings.Join(words, " "), nil
}

// ParseRecoveryMnemonic is the recovery key from its 24 words (any case,
// any whitespace between them). A word outside the list is ErrMnemonicWord;
// a wrong checksum ErrMnemonicChecksum.
func ParseRecoveryMnemonic(phrase string) ([]byte, error) {
	words := strings.Fields(strings.ToLower(phrase))
	if len(words) != MnemonicWords {
		return nil, ErrMnemonicWord
	}
	bits := make([]byte, KeySize+1)
	for i, w := range words {
		idx, ok := bip39Index[w]
		if !ok {
			return nil, fmt.Errorf("%w: word %d", ErrMnemonicWord, i+1)
		}
		for b := 0; b < 11; b++ {
			if idx&(1<<(10-b)) != 0 {
				pos := i*11 + b
				bits[pos/8] |= 0x80 >> (pos % 8)
			}
		}
	}
	key := bits[:KeySize]
	if sum := sha256.Sum256(key); sum[0] != bits[KeySize] {
		return nil, ErrMnemonicChecksum
	}
	return key, nil
}

// bits11 is the i-th 11-bit group of b, most significant bit first.
func bits11(b []byte, i int) int {
	v := 0
	for j := 0; j < 11; j++ {
		pos := i*11 + j
		v <<= 1
		if b[pos/8]&(0x80>>(pos%8)) != 0 {
			v |= 1
		}
	}
	return v
}
