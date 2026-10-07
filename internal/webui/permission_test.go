package webui_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestSessionStartRequiresProcessOptIn(t *testing.T) {
	// Empty credentials prevent any exchange access even if a guard regresses.
	t.Setenv("BINANCE_API_KEY", "")
	t.Setenv("BINANCE_SECRET_KEY", "")
	for _, value := range []string{"unset", "", "0", "true", "01", " 1", "1 ", "1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TA_ALLOW_LIVE", value)
			if value == "unset" {
				if err := os.Unsetenv("TA_ALLOW_LIVE"); err != nil {
					t.Fatal(err)
				}
			}
			for _, futures := range []string{"false", "true"} {
				server, _ := newTestServer(t)
				response := postJSON(t, server, "/api/session/start",
					`{"symbol":"TEST","strategy":"ma_cross","days":120,"execute":true,"confirm":"确认实盘","futures":`+futures+`}`)
				if response.Code != http.StatusBadRequest {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				want := "TA_ALLOW_LIVE=1"
				if value == "1" {
					// Opt-in passes only this gate, not the credentials guard.
					want = "BINANCE_API_KEY"
				}
				if !strings.Contains(response.Body.String(), want) {
					t.Fatalf("want guard %s, got %s", want, response.Body.String())
				}
				if getSession(t, server).Running {
					t.Fatal("rejected start left a running session")
				}
			}
		})
	}
}

func TestPaperSessionDoesNotRequireProcessOptIn(t *testing.T) {
	t.Setenv("TA_ALLOW_LIVE", "0")
	server := newLiveTestServer(t)
	response := postJSON(t, server, "/api/session/start",
		`{"symbol":"TEST","strategy":"ma_cross","days":120,"interval_seconds":3600}`)
	if response.Code != http.StatusOK {
		t.Fatalf("paper start=%d body=%s", response.Code, response.Body.String())
	}
	defer postJSON(t, server, "/api/session/stop", "")
	if status := getSession(t, server); !status.Running || status.Mode != "paper" {
		t.Fatalf("paper status=%+v", status)
	}
}
