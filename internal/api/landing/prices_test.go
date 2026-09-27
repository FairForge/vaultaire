package landing

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrices_LoadAndMatchTheSheet(t *testing.T) {
	p := Get()
	require.Greater(t, p.Standard.Annual, 0.0)
	require.Greater(t, p.Vault.Annual, 0.0)
	assert.Less(t, p.Vault.Annual, p.Standard.Annual, "the attic must be the cheaper floor")
	assert.Greater(t, p.Competitors.AWSS3, p.Standard.Annual)
}

// The landing page's receipt for the starter house (6 TB downstairs, 1 TB in
// the attic) reads $28.94 at 4.49 / 2.00; the Go side must agree to the cent.
func TestHouseIntent_MonthlyMatchesTheReceipt(t *testing.T) {
	h := HouseIntent{StdTB: 6, VaultTB: 1}
	assert.Equal(t, 2894, h.MonthlyCents())
	assert.Equal(t, "$28.94", h.Monthly())
	assert.Equal(t, 7, h.TotalTB())
	assert.False(t, h.Empty())
}

func TestParseHouseIntent_ClampsAndFilters(t *testing.T) {
	tests := []struct {
		name             string
		std, vault, room string
		want             HouseIntent
	}{
		{"plain", "6", "1", "v2.midnight.midnight..box-17-88", HouseIntent{6, 1, "v2.midnight.midnight..box-17-88"}},
		{"floats round", "5.6", "0.4", "", HouseIntent{6, 0, ""}},
		{"negative and junk become zero", "-3", "lots", "", HouseIntent{0, 0, ""}},
		{"huge is clamped", "99999", "1e9", "", HouseIntent{MaxIntentTB, MaxIntentTB, ""}},
		{"room must look like a share link", "1", "0", "<script>alert(1)</script>", HouseIntent{1, 0, ""}},
		{"room keeps percent-encoded names", "1", "0", "v2.oat.noir.Zo%C3%AB.box-1-2-p", HouseIntent{1, 0, "v2.oat.noir.Zo%C3%AB.box-1-2-p"}},
		{"empty", "", "", "", HouseIntent{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseHouseIntent(tt.std, tt.vault, tt.room)
			assert.Equal(t, tt.want, got)
		})
	}
	assert.True(t, ParseHouseIntent("", "", "v2.x").Empty())
	assert.Equal(t, "/#room=v2.oat.noir..box-1-2", HouseIntent{Room: "v2.oat.noir..box-1-2"}.RoomURL())
	assert.Equal(t, "", HouseIntent{}.RoomURL())
}
