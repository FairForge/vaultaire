// Package landing holds the generated landing page's sources (see build.py)
// and the ONE price file the product quotes from: prices.json feeds the
// page at build time and, through this package, the Go side at run time
// (waitlist intent, registration, billing), so a price change is one edit.
package landing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
)

//go:embed prices.json
var pricesJSON []byte

// Prices mirrors prices.json. USD per TB per month unless noted.
type Prices struct {
	Standard struct {
		Annual  float64 `json:"annual"`
		Monthly float64 `json:"monthly"`
	} `json:"standard"`
	Vault struct {
		Annual         float64 `json:"annual"`
		Monthly        float64 `json:"monthly"`
		MonthlyMinimum float64 `json:"monthly_minimum"`
	} `json:"vault"`
	PinHot      float64 `json:"pin_hot"`
	Performance float64 `json:"performance"`
	Founders    float64 `json:"founders"`
	// Egress allowances are keyed to the quota (per month) — see
	// prices.json. They used to be typed into the landing copy as
	// "0." + the AWS price multiple (Review R14-09); now they are data.
	Egress struct {
		StandardFreeRatio     float64 `json:"standard_free_ratio"`
		VaultRestoreFreeRatio float64 `json:"vault_restore_free_ratio"`
	} `json:"egress"`
	Competitors struct {
		AWSS3        float64 `json:"aws_s3"`
		BackblazeB2  float64 `json:"backblaze_b2"`
		Wasabi       float64 `json:"wasabi"`
		CloudflareR2 float64 `json:"cloudflare_r2"`
	} `json:"competitors"`
}

var (
	pricesOnce sync.Once
	prices     Prices
)

// Get returns the parsed price file. The file is checked into the binary, so
// a parse failure is a build defect and panics at first use.
func Get() Prices {
	pricesOnce.Do(func() {
		if err := json.Unmarshal(pricesJSON, &prices); err != nil {
			panic("landing/prices.json: " + err.Error())
		}
	})
	return prices
}

// HouseIntent is what a visitor built in the landing page's house: whole TB
// downstairs (Standard) and in the attic (Vault), plus the share-link room so
// the house can be reopened. It rides on waitlist signups pre-launch and on
// the tenant at registration, and is only ever a hint: nothing is billed
// from it.
type HouseIntent struct {
	StdTB   int
	VaultTB int
	Room    string
}

// Bounds: the house holds at most 30 pieces of 10 TB, and a room link is a
// few hundred characters of [A-Za-z0-9._%-] (v2 share format).
const (
	MaxIntentTB   = 300
	maxRoomLength = 800
)

// ParseHouseIntent reads the three fields as they arrive from a form, a
// query string or JSON (already stringified), clamps the TB into range and
// drops a room that is not a plausible share link. Missing or junk values
// yield an empty intent rather than an error: the signup must never fail
// because of the hint.
func ParseHouseIntent(std, vault, room string) HouseIntent {
	h := HouseIntent{StdTB: clampTB(std), VaultTB: clampTB(vault)}
	room = strings.TrimSpace(room)
	if len(room) <= maxRoomLength && strings.HasPrefix(room, "v") && roomCharset(room) {
		h.Room = room
	}
	return h
}

func clampTB(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || f < 0 {
		return 0
	}
	if f > MaxIntentTB {
		return MaxIntentTB
	}
	return int(math.Round(f))
}

func roomCharset(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '%':
		default:
			return false
		}
	}
	return true
}

// Empty reports whether the visitor built nothing billable.
func (h HouseIntent) Empty() bool { return h.StdTB == 0 && h.VaultTB == 0 }

// TotalTB is the quota the house implies.
func (h HouseIntent) TotalTB() int { return h.StdTB + h.VaultTB }

// MonthlyCents prices the house at the annual per-TB rates, in whole cents,
// rounded half up (what the landing page's receipt shows).
func (h HouseIntent) MonthlyCents() int {
	p := Get()
	return int(math.Round((float64(h.StdTB)*p.Standard.Annual + float64(h.VaultTB)*p.Vault.Annual) * 100))
}

// Monthly formats MonthlyCents as "$28.94".
func (h HouseIntent) Monthly() string {
	c := h.MonthlyCents()
	return fmt.Sprintf("$%d.%02d", c/100, c%100)
}

// RoomURL is the landing page link that reopens the house, or "" if none.
func (h HouseIntent) RoomURL() string {
	if h.Room == "" {
		return ""
	}
	return "/#room=" + h.Room
}
