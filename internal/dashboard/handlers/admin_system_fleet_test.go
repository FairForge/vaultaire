package handlers

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// WP-FLEET-10: the System page's fleet card — one row per permafrost account
// (drive, quota used / bin / total, throttles in the last hour, licence),
// from the driver; no card without a fleet.

func fleetCardTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl := template.Must(template.New("admin").Parse(`{{define "admin"}}{{block "content" .}}{{end}}{{end}}`))
	template.Must(tmpl.Parse(`{{define "content"}}{{range .Fleet}}<tr class="acct">{{.Account}}|{{.Drive}}|{{.UsedFmt}}|{{.BinFmt}}|{{.TotalFmt}}|{{.Throttles1h}}|{{.Probe}}|{{.Licence}}</tr>{{end}}{{if not .Fleet}}no fleet{{end}}{{end}}`))
	return tmpl
}

func TestHandleAdminSystem_TheFleetCardListsEveryAccount(t *testing.T) {
	fleet := func() []drivers.FleetAccount {
		return []drivers.FleetAccount{
			{Account: "tenant-1", DriveID: "b!abcdefghijklmnopqrstuvwxyz7MF", Probed: true, Up: true, QuotaKnown: true,
				QuotaTotal: 5 << 40, QuotaUsed: 21 << 30, QuotaDeleted: 68 << 30, Throttles1h: 4},
			{Account: "tenant-2", Probed: true, LastError: "onedrive throttled [tenant-2] (429)"},
		}
	}
	handler := HandleAdminSystem(fleetCardTemplate(t), nil, zap.NewNop(), fleet)
	req := httptest.NewRequest("GET", "/admin/system", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `tenant-1|…z7MF|21 GB|68 GB|5 TB|4|up|—`)
	assert.Contains(t, body, `tenant-2|—|—|—|—|0|failing|—`)
}

func TestHandleAdminSystem_NoFleetNoCard(t *testing.T) {
	handler := HandleAdminSystem(fleetCardTemplate(t), nil, zap.NewNop())
	req := httptest.NewRequest("GET", "/admin/system", nil).WithContext(adminCtx(t))
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	assert.Contains(t, w.Body.String(), "no fleet")
}
