package cli

import "testing"

func TestTuneCVFlagRejectsInvalidBeforeLoadingData(t *testing.T) {
	for _, value := range []string{"-1", "33"} {
		if code := runTune([]string{"--cv", value, "--config", "nonexistent-cv-test-config.yaml"}); code != 1 {
			t.Fatalf("cv=%s: exit=%d, want 1", value, code)
		}
	}
}
