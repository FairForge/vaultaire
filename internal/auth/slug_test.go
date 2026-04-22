package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateSlug_BasicCompanyName(t *testing.T) {
	assert.Equal(t, "acme-corp", GenerateSlug("Acme Corp"))
}

func TestGenerateSlug_SpecialCharacters(t *testing.T) {
	assert.Equal(t, "o-brien-sons-llc", GenerateSlug("O'Brien & Sons, LLC"))
}

func TestGenerateSlug_UnicodeStripped(t *testing.T) {
	slug := GenerateSlug("Nöno Labs")
	assert.Equal(t, "n-no-labs", slug)
}

func TestGenerateSlug_CollapseHyphens(t *testing.T) {
	assert.Equal(t, "hello-world", GenerateSlug("hello---world"))
}

func TestGenerateSlug_TrimHyphens(t *testing.T) {
	assert.Equal(t, "hello-world", GenerateSlug("-hello-world-"))
}

func TestGenerateSlug_TooShort(t *testing.T) {
	assert.Equal(t, "x-storage", GenerateSlug("X"))
}

func TestGenerateSlug_Empty(t *testing.T) {
	assert.Equal(t, "tenant", GenerateSlug(""))
}

func TestGenerateSlug_TooLong(t *testing.T) {
	long := "a]bcdefghijklmnopqrstuvwxyz-bcdefghijklmnopqrstuvwxyz-bcdefghijklmnop"
	slug := GenerateSlug(long)
	assert.LessOrEqual(t, len(slug), 63)
	assert.NotEqual(t, "", slug)
}

func TestGenerateSlug_ReservedWord(t *testing.T) {
	assert.Equal(t, "admin-1", GenerateSlug("admin"))
	assert.Equal(t, "cdn-1", GenerateSlug("cdn"))
}

func TestIsReservedSlug(t *testing.T) {
	assert.True(t, IsReservedSlug("admin"))
	assert.True(t, IsReservedSlug("dashboard"))
	assert.False(t, IsReservedSlug("acme"))
}

func TestGenerateSlug_NumericOnly(t *testing.T) {
	assert.Equal(t, "12345", GenerateSlug("12345"))
}

func TestValidateSlug(t *testing.T) {
	assert.True(t, ValidateSlug("acme-corp"))
	assert.True(t, ValidateSlug("ab"))
	assert.True(t, ValidateSlug("12345"))
	assert.False(t, ValidateSlug("a"))
	assert.False(t, ValidateSlug("-bad"))
	assert.False(t, ValidateSlug("bad-"))
	assert.False(t, ValidateSlug(""))
	assert.False(t, ValidateSlug("UPPER"))
}

func TestCanEnablePublicRead_StarterTier(t *testing.T) {
	allowed, reason := CanEnablePublicRead("starter")
	assert.True(t, allowed)
	assert.Empty(t, reason)
}

func TestCanEnablePublicRead_Vault3Tier(t *testing.T) {
	allowed, reason := CanEnablePublicRead("vault3")
	assert.True(t, allowed)
	assert.Empty(t, reason)
}

func TestCanEnablePublicRead_ArchiveTier(t *testing.T) {
	allowed, reason := CanEnablePublicRead("archive")
	assert.False(t, allowed)
	assert.Contains(t, reason, "archive-tier")
}

func TestCanEnablePublicRead_Vault18Tier(t *testing.T) {
	allowed, reason := CanEnablePublicRead("vault18")
	assert.False(t, allowed)
	assert.NotEmpty(t, reason)
}

func TestCanEnablePublicRead_GeyserTier(t *testing.T) {
	allowed, reason := CanEnablePublicRead("geyser")
	assert.False(t, allowed)
	assert.NotEmpty(t, reason)
}

func TestCanEnablePublicRead_EmptyTier(t *testing.T) {
	allowed, reason := CanEnablePublicRead("")
	assert.True(t, allowed)
	assert.Empty(t, reason)
}
