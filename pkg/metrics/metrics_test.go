package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerExposesEmbervmMetrics(t *testing.T) {
	WatchdogReaps.Add(0) // non-vec instruments export even at zero
	NodesUp.Set(0)

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{"embervm_watchdog_reaps_total", "embervm_nodes_up", "embervm_engine_tick_errors_total"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("/metrics missing %s", want)
		}
	}
}
