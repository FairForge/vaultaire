package produce

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FairForge/vaultaire/internal/api/landing"
)

func TestProduceJSON_Loads(t *testing.T) {
	f := All()
	assert.NotEmpty(t, f.Account)
	assert.NotEmpty(t, f.Retention)
	assert.Equal(t, FlagLegalLEPages, "legal_le_pages")
}

func TestValidate_RefusesWhatTheCopyRulesRefuse(t *testing.T) {
	cases := map[string]func(*File){
		"a mode renamed":       func(f *File) { f.Modes[0] = "secret" },
		"a mode missing":       func(f *File) { f.Modes = f.Modes[1:] },
		"zero-knowledge":       func(f *File) { f.Caveat = "Stored is zero-knowledge." },
		"cannot read, bare":    func(f *File) { f.Kept[0].What = "We cannot read your files." },
		"scrub without column": func(f *File) { f.Retention[0].Action = "scrub" },
		"a period of zero":     func(f *File) { f.Retention[0].Days = 0 },
		"an unknown action":    func(f *File) { f.Retention[0].Action = "archive" },
		"a gate without flag":  func(f *File) { f.AdminReads.RequiresFlag = "" },
	}
	for name, mutate := range cases {
		f, err := Parse(landing.ProduceJSON())
		require.NoError(t, err)
		mutate(&f)
		assert.Error(t, Validate(f), name)
	}
}

func TestParse_RefusesAnUnknownField(t *testing.T) {
	b := strings.Replace(string(landing.ProduceJSON()), `"caveat"`, `"caveats_typo"`, 1)
	_, err := Parse([]byte(b))
	assert.Error(t, err)
}
