// internal/container/security_test.go
package container

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVulnerabilitySeverity(t *testing.T) {
	t.Run("severity ordering", func(t *testing.T) {
		assert.True(t, SeverityCritical > SeverityHigh)
		assert.True(t, SeverityHigh > SeverityMedium)
		assert.True(t, SeverityMedium > SeverityLow)
		assert.True(t, SeverityLow > SeverityUnknown)
	})
}

func TestVulnerability(t *testing.T) {
	t.Run("creates vulnerability", func(t *testing.T) {
		vuln := &Vulnerability{
			ID:          "CVE-2024-1234",
			Package:     "openssl",
			Version:     "1.1.1",
			FixedIn:     "1.1.2",
			Severity:    SeverityCritical,
			Description: "Buffer overflow",
			Link:        "https://nvd.nist.gov/vuln/detail/CVE-2024-1234",
		}
		assert.Equal(t, "CVE-2024-1234", vuln.ID)
		assert.Equal(t, SeverityCritical, vuln.Severity)
	})
}

func TestScanResult(t *testing.T) {
	t.Run("creates scan result", func(t *testing.T) {
		result := &ScanResult{
			Image:     "vaultaire:1.0.0",
			Scanner:   "trivy",
			ScannedAt: time.Now(),
			Vulnerabilities: []Vulnerability{
				{ID: "CVE-2024-1234", Severity: SeverityCritical},
				{ID: "CVE-2024-5678", Severity: SeverityHigh},
			},
		}
		assert.Equal(t, 2, len(result.Vulnerabilities))
	})

	t.Run("counts by severity", func(t *testing.T) {
		result := &ScanResult{
			Vulnerabilities: []Vulnerability{
				{Severity: SeverityCritical},
				{Severity: SeverityCritical},
				{Severity: SeverityHigh},
				{Severity: SeverityMedium},
			},
		}
		counts := result.CountBySeverity()
		assert.Equal(t, 2, counts[SeverityCritical])
		assert.Equal(t, 1, counts[SeverityHigh])
		assert.Equal(t, 1, counts[SeverityMedium])
	})

	t.Run("has critical vulnerabilities", func(t *testing.T) {
		result := &ScanResult{
			Vulnerabilities: []Vulnerability{
				{Severity: SeverityCritical},
			},
		}
		assert.True(t, result.HasCritical())
	})

	t.Run("no critical vulnerabilities", func(t *testing.T) {
		result := &ScanResult{
			Vulnerabilities: []Vulnerability{
				{Severity: SeverityHigh},
			},
		}
		assert.False(t, result.HasCritical())
	})
}

func TestScanPolicy(t *testing.T) {
	t.Run("creates scan policy", func(t *testing.T) {
		policy := &ScanPolicy{
			FailOnCritical:   true,
			FailOnHigh:       false,
			MaxCritical:      0,
			MaxHigh:          5,
			IgnoredCVEs:      []string{"CVE-2024-9999"},
			RequiredScanners: []string{"trivy"},
		}
		assert.True(t, policy.FailOnCritical)
		assert.Contains(t, policy.IgnoredCVEs, "CVE-2024-9999")
	})
}

func TestScanPolicy_Evaluate(t *testing.T) {
	t.Run("fails on critical when policy requires", func(t *testing.T) {
		policy := &ScanPolicy{FailOnCritical: true, MaxCritical: 0}
		result := &ScanResult{
			Vulnerabilities: []Vulnerability{
				{ID: "CVE-2024-1234", Severity: SeverityCritical},
			},
		}
		passed, violations := policy.Evaluate(result)
		assert.False(t, passed)
		assert.NotEmpty(t, violations)
	})

	t.Run("passes when no critical", func(t *testing.T) {
		policy := &ScanPolicy{FailOnCritical: true, MaxCritical: 0}
		result := &ScanResult{
			Vulnerabilities: []Vulnerability{
				{ID: "CVE-2024-1234", Severity: SeverityHigh},
			},
		}
		passed, _ := policy.Evaluate(result)
		assert.True(t, passed)
	})

	t.Run("ignores specified CVEs", func(t *testing.T) {
		policy := &ScanPolicy{
			FailOnCritical: true,
			MaxCritical:    0,
			IgnoredCVEs:    []string{"CVE-2024-1234"},
		}
		result := &ScanResult{
			Vulnerabilities: []Vulnerability{
				{ID: "CVE-2024-1234", Severity: SeverityCritical},
			},
		}
		passed, _ := policy.Evaluate(result)
		assert.True(t, passed)
	})

	t.Run("respects max thresholds", func(t *testing.T) {
		policy := &ScanPolicy{MaxHigh: 2}
		result := &ScanResult{
			Vulnerabilities: []Vulnerability{
				{Severity: SeverityHigh},
				{Severity: SeverityHigh},
				{Severity: SeverityHigh},
			},
		}
		passed, violations := policy.Evaluate(result)
		assert.False(t, passed)
		assert.NotEmpty(t, violations)
	})
}

func TestNewImageScanner(t *testing.T) {
	t.Run("creates scanner", func(t *testing.T) {
		scanner := NewImageScanner(&ScannerConfig{
			Scanner: "trivy",
			Timeout: 5 * time.Minute,
		})
		assert.NotNil(t, scanner)
	})
}

func TestImageScanner_ParseTrivyOutput(t *testing.T) {
	scanner := NewImageScanner(&ScannerConfig{Scanner: "trivy"})

	t.Run("parses trivy JSON output", func(t *testing.T) {
		output := `{
			"Results": [{
				"Vulnerabilities": [
					{
						"VulnerabilityID": "CVE-2024-1234",
						"PkgName": "openssl",
						"InstalledVersion": "1.1.1",
						"FixedVersion": "1.1.2",
						"Severity": "CRITICAL",
						"Description": "Buffer overflow"
					}
				]
			}]
		}`
		result, err := scanner.ParseTrivyOutput([]byte(output))
		require.NoError(t, err)
		assert.Len(t, result.Vulnerabilities, 1)
		assert.Equal(t, "CVE-2024-1234", result.Vulnerabilities[0].ID)
	})

	t.Run("handles empty results", func(t *testing.T) {
		output := `{"Results": []}`
		result, err := scanner.ParseTrivyOutput([]byte(output))
		require.NoError(t, err)
		assert.Empty(t, result.Vulnerabilities)
	})
}

func TestImageScanner_BuildCommand(t *testing.T) {
	t.Run("builds trivy command", func(t *testing.T) {
		scanner := NewImageScanner(&ScannerConfig{
			Scanner: "trivy",
			Timeout: 5 * time.Minute,
		})
		cmd := scanner.BuildCommand("vaultaire:1.0.0")
		assert.Contains(t, cmd, "trivy")
		assert.Contains(t, cmd, "image")
		assert.Contains(t, cmd, "vaultaire:1.0.0")
	})
}

func TestSecurityReport(t *testing.T) {
	t.Run("generates security report", func(t *testing.T) {
		result := &ScanResult{
			Image:     "vaultaire:1.0.0",
			Scanner:   "trivy",
			ScannedAt: time.Now(),
			Vulnerabilities: []Vulnerability{
				{ID: "CVE-2024-1234", Package: "openssl", Severity: SeverityCritical},
				{ID: "CVE-2024-5678", Package: "curl", Severity: SeverityHigh},
			},
		}
		report := GenerateSecurityReport(result)
		assert.Contains(t, report, "vaultaire:1.0.0")
		assert.Contains(t, report, "CVE-2024-1234")
		assert.Contains(t, report, "CRITICAL")
	})
}

func TestDefaultScanPolicy(t *testing.T) {
	t.Run("creates default policy", func(t *testing.T) {
		policy := DefaultScanPolicy()
		assert.True(t, policy.FailOnCritical)
		assert.Equal(t, 0, policy.MaxCritical)
	})
}

func TestStrictScanPolicy(t *testing.T) {
	t.Run("creates strict policy", func(t *testing.T) {
		policy := StrictScanPolicy()
		assert.True(t, policy.FailOnCritical)
		assert.True(t, policy.FailOnHigh)
		assert.Equal(t, 0, policy.MaxCritical)
		assert.Equal(t, 0, policy.MaxHigh)
	})
}

func TestScanResult_FilterBySeverity(t *testing.T) {
	result := &ScanResult{
		Vulnerabilities: []Vulnerability{
			{ID: "CVE-1", Severity: SeverityCritical},
			{ID: "CVE-2", Severity: SeverityHigh},
			{ID: "CVE-3", Severity: SeverityMedium},
		},
	}

	t.Run("filters critical and above", func(t *testing.T) {
		filtered := result.FilterBySeverity(SeverityCritical)
		assert.Len(t, filtered, 1)
	})

	t.Run("filters high and above", func(t *testing.T) {
		filtered := result.FilterBySeverity(SeverityHigh)
		assert.Len(t, filtered, 2)
	})
}

func TestScannerConfig(t *testing.T) {
	t.Run("creates config", func(t *testing.T) {
		config := &ScannerConfig{
			Scanner:       "trivy",
			Timeout:       10 * time.Minute,
			CacheDir:      "/tmp/trivy-cache",
			OfflineMode:   false,
			IgnoreUnfixed: true,
		}
		assert.Equal(t, "trivy", config.Scanner)
		assert.True(t, config.IgnoreUnfixed)
	})
}

func TestImageScanner_ScanMock(t *testing.T) {
	t.Run("mock scan returns result", func(t *testing.T) {
		scanner := NewImageScanner(&ScannerConfig{
			Scanner: "mock",
		})
		ctx := context.Background()
		result, err := scanner.Scan(ctx, "test:latest")
		require.NoError(t, err)
		assert.Equal(t, "test:latest", result.Image)
	})
}
