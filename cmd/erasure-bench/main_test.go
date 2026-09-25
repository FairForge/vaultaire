package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/klauspost/reedsolomon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLayout_AssignsSlotsInOrder(t *testing.T) {
	slots, err := parseLayout("lyve:6,geyser:4,onedrive:6", 16)
	require.NoError(t, err)
	require.Len(t, slots, 16)
	assert.Equal(t, "lyve", slots[0].backend)
	assert.Equal(t, "geyser", slots[6].backend)
	assert.Equal(t, "onedrive", slots[15].backend)
	assert.Equal(t, 15, slots[15].idx)
	assert.Equal(t, map[string]int{"lyve": 6, "geyser": 4, "onedrive": 6}, perBackend(slots))
}

func TestParseLayout_RejectsWrongTotalAndBadEntries(t *testing.T) {
	_, err := parseLayout("lyve:6,geyser:4", 16)
	assert.Error(t, err)
	_, err = parseLayout("lyve", 1)
	assert.Error(t, err)
	_, err = parseLayout("lyve:x", 1)
	assert.Error(t, err)
}

func TestDescribe_LabelsDataAndParityRuns(t *testing.T) {
	slots, err := parseLayout("lyve:6,geyser:4,onedrive:6", 16)
	require.NoError(t, err)
	assert.Equal(t, "lyve=0-5(data) geyser=6-9(data) onedrive=10-15(parity)", describe(slots, 10))
	mixed, err := parseLayout("a:12,b:4", 16)
	require.NoError(t, err)
	assert.Equal(t, "a=0-11(data+parity) b=12-15(parity)", describe(mixed, 10))
}

func TestReconstruct_RebuildsFromAnyKShards(t *testing.T) {
	enc, err := reedsolomon.New(10, 6)
	require.NoError(t, err)
	payload := make([]byte, 10*4096+123) // not shard-divisible: exercises Split padding
	_, _ = rand.Read(payload)
	sum := sha256.Sum256(payload)
	want := hex.EncodeToString(sum[:])
	shards, err := enc.Split(payload)
	require.NoError(t, err)
	require.NoError(t, enc.Encode(shards))
	shardLen := int64(len(shards[0]))

	// Lose all of the first six (data) shards: survivors are 4 data + 6 parity.
	have := make([][]byte, 16)
	for i := 6; i < 16; i++ {
		have[i] = append([]byte(nil), shards[i]...)
	}
	_, ok := reconstruct(enc, have, 10, shardLen, int64(len(payload)), want)
	assert.True(t, ok)

	// Nine survivors is not enough.
	have = make([][]byte, 16)
	for i := 7; i < 16; i++ {
		have[i] = append([]byte(nil), shards[i]...)
	}
	_, ok = reconstruct(enc, have, 10, shardLen, int64(len(payload)), want)
	assert.False(t, ok)
}
