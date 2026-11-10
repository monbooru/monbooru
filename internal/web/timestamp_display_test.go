package web

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

const tzProbeEnv = "MONBOORU_TZ_PROBE"

func TestMain(m *testing.M) {
	if os.Getenv(tzProbeEnv) == "1" {
		time.Local = time.FixedZone("TEST-3", -3*60*60)
	}
	os.Exit(m.Run())
}

func inTZProbe(t *testing.T) bool {
	t.Helper()
	if os.Getenv(tzProbeEnv) == "1" {
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), tzProbeEnv+"=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("timezone probe: %v\n%s", err, out)
	}
	return false
}

func TestTimestampHelpers_RenderInLocalZone(t *testing.T) {
	if !inTZProbe(t) {
		return
	}

	if got := humanISOTime("2026-07-05T19:51:03Z"); got != "2026-07-05 16:51:03" {
		t.Errorf("humanISOTime = %q, want 2026-07-05 16:51:03", got)
	}

	if got := humanISODate("2026-07-05T01:30:00Z"); got != "2026-07-04" {
		t.Errorf("humanISODate = %q, want 2026-07-04", got)
	}

	ingested := time.Date(2026, 7, 5, 19, 51, 3, 0, time.UTC)
	local := templateFuncs()["localTime"].(func(time.Time) string)
	if got := local(ingested); got != "2026-07-05 16:51:03" {
		t.Errorf("localTime = %q, want 2026-07-05 16:51:03", got)
	}
	localPtr := templateFuncs()["localTimePtr"].(func(*time.Time) string)
	if got := localPtr(&ingested); got != "2026-07-05 16:51:03" {
		t.Errorf("localTimePtr = %q, want 2026-07-05 16:51:03", got)
	}
	if got := localPtr(nil); got != "" {
		t.Errorf("localTimePtr(nil) = %q, want empty", got)
	}
}
