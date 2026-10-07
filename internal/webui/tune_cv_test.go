package webui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rdone44/trading-agent-go/internal/config"
	"github.com/rdone44/trading-agent-go/internal/model"
	"github.com/rdone44/trading-agent-go/internal/tune"
)

func TestTuneCVFoldsForwarded(t *testing.T) {
	server, _ := newTestServer(t)
	called := false
	server.TuneRunner = func(cfg config.Config, series model.Series, opts tune.Options) (tune.Report, error) {
		called = true
		if opts.CVFolds != 3 {
			t.Fatalf("CVFolds=%d", opts.CVFolds)
		}
		return tune.Report{}, nil
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/tune", strings.NewReader(`{"symbol":"TEST","days":600,"cv_folds":3}`)))
	if recorder.Code != http.StatusOK || !called {
		t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestTuneInvalidCVReturnsBadRequest(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	for _, body := range []string{`{"cv_folds":-1}`, `{"cv_folds":33}`, `{"days":20,"cv_folds":3}`} {
		server, _ := newTestServer(t)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/tune", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d: %s", body, recorder.Code, recorder.Body.String())
		}
	}
}
