package throughput

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddKeepsAWeekPerModelAndEvictsOnlyTheUsedModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "julia", "throughput.json")
	if log, err := Load(path); err != nil || len(log) != 0 {
		t.Fatalf("missing file: %v %v", log, err)
	}
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	add := func(model string, at time.Time, output int, seconds float64) {
		t.Helper()
		if err := Add(path, model, Sample{At: at, Output: output, Seconds: seconds}); err != nil {
			t.Fatal(err)
		}
	}
	add("codex/gpt-6-luna", now.Add(-40*24*time.Hour), 1000, 10)
	add("codex/gpt-6-luna", now.Add(-39*24*time.Hour), 500, 10)
	add("compatible/minimax/minimax-m3", now.Add(-8*24*time.Hour), 300, 10)
	add("compatible/minimax/minimax-m3", now.Add(-time.Hour), 900, 10)
	add("compatible/minimax/minimax-m3", now, 100, 10)
	// Ignored: nothing was produced, or no time was measured.
	add("compatible/minimax/minimax-m3", now, 0, 10)
	add("compatible/minimax/minimax-m3", now, 10, 0)

	log, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	idle, ok := log.Stats("codex/gpt-6-luna")
	if !ok || idle.Calls != 2 || math.Abs(idle.TokensPerSecond-75) > 1e-9 {
		t.Fatalf("idle model lost its old samples: %+v %v", idle, ok)
	}
	used, ok := log.Stats("compatible/minimax/minimax-m3")
	if !ok || used.Calls != 2 || math.Abs(used.TokensPerSecond-50) > 1e-9 || !used.LastUsed.Equal(now) {
		t.Fatalf("used model = %+v %v, want the last week only", used, ok)
	}
	if _, ok := log.Stats("codex/never"); ok {
		t.Fatal("unknown model has stats")
	}
	if all := log.All(); len(all) != 2 {
		t.Fatalf("All() = %v", all)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("permissions %o", perm)
	}
}

func TestAddBoundsSamplesPerModel(t *testing.T) {
	log := Log{}
	now := time.Now()
	for i := 0; i < maxSamples+10; i++ {
		log.add("m", Sample{At: now, Output: i + 1, Seconds: 1})
	}
	if len(log["m"]) != maxSamples || log["m"][0].Output != 11 {
		t.Fatalf("kept %d samples, first %+v", len(log["m"]), log["m"][0])
	}
}

func TestLoadRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "throughput.json")
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("corrupt file loaded")
	}
	// A corrupt file is replaced rather than left blocking every record.
	if err := Add(path, "m", Sample{At: time.Now(), Output: 1, Seconds: 1}); err != nil {
		t.Fatal(err)
	}
	if log, err := Load(path); err != nil || len(log["m"]) != 1 {
		t.Fatalf("after replacing: %v %v", log, err)
	}
}
