package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

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

func TestCommitWall_GatesOnSyncLegsOnly(t *testing.T) {
	slots, err := parseLayout("lyve:2,onedrive:2", 4)
	require.NoError(t, err)
	done := []time.Duration{1 * time.Second, 2 * time.Second, 9 * time.Second, 10 * time.Second}
	res := make([]result, 4)
	assert.Equal(t, 10*time.Second, commitWall(slots, done, res, nil), "no sync set: everything gates")
	assert.Equal(t, 2*time.Second, commitWall(slots, done, res, map[string]bool{"lyve": true}), "sync=lyve ignores onedrive")
	res[1].err = assert.AnError
	assert.Equal(t, 1*time.Second, commitWall(slots, done, res, map[string]bool{"lyve": true}), "failed shards do not count")
}

func TestLegStats_ThresholdNeedsTwoSamplesAndRespectsFloor(t *testing.T) {
	hedgeMul, hedgeFloor = 2.0, 3*time.Second
	st := newLegStats()
	assert.Equal(t, time.Duration(0), st.threshold("x"), "no median yet")
	st.record("x", 1*time.Second)
	assert.Equal(t, time.Duration(0), st.threshold("x"), "one sample is not a median")
	st.record("x", 1*time.Second)
	assert.Equal(t, 3*time.Second, st.threshold("x"), "2×1 s is below the 3 s floor")
	st.record("x", 5*time.Second)
	st.record("x", 5*time.Second)
	assert.Equal(t, 6*time.Second, st.threshold("x"), "median 3 s × 2")
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
