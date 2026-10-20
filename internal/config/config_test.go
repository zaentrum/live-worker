package config

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"LIVE_WORKER_MEDIA_ROOTS": "/media:/library"}))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Supervisor
	if c.Listen != ":8090" || s.GPUSlots != 4 || s.CPUSlots != 2 || s.HW != "cuda" || s.Attempts != 3 ||
		s.Deadline != 12*time.Second || s.BreakerFailures != 3 || s.PrefetchReserve != 1 || s.HedgeAfter != 0 {
		t.Errorf("defaults: %+v", s)
	}
	if len(s.MediaRoots) != 2 || s.MediaRoots[1] != "/library" {
		t.Errorf("roots %v", s.MediaRoots)
	}
}

func TestOverridesAndErrors(t *testing.T) {
	c, err := Load(env(map[string]string{
		"LIVE_WORKER_MEDIA_ROOTS": "/media", "LIVE_WORKER_GPU_SLOTS": "0", "LIVE_WORKER_CPU_SLOTS": "3",
		"LIVE_WORKER_HEDGE_AFTER": "2500ms", "LIVE_WORKER_SLOT_MAX_RSS_MB": "3072", "LIVE_WORKER_HW": "none",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s := c.Supervisor; s.GPUSlots != 0 || s.CPUSlots != 3 || s.HedgeAfter != 2500*time.Millisecond || s.SlotMaxRSS != 3072<<20 {
		t.Errorf("overrides: %+v", s)
	}
	for name, m := range map[string]map[string]string{
		"no roots":      {},
		"bad number":    {"LIVE_WORKER_MEDIA_ROOTS": "/m", "LIVE_WORKER_GPU_SLOTS": "four"},
		"bad duration":  {"LIVE_WORKER_MEDIA_ROOTS": "/m", "LIVE_WORKER_DEADLINE": "soon"},
		"no slots":      {"LIVE_WORKER_MEDIA_ROOTS": "/m", "LIVE_WORKER_GPU_SLOTS": "0", "LIVE_WORKER_CPU_SLOTS": "0"},
		"bad hw":        {"LIVE_WORKER_MEDIA_ROOTS": "/m", "LIVE_WORKER_HW": "opencl"},
		"bad tonemap":   {"LIVE_WORKER_MEDIA_ROOTS": "/m", "LIVE_WORKER_GPU_TONEMAP": "magic"},
		"negative slot": {"LIVE_WORKER_MEDIA_ROOTS": "/m", "LIVE_WORKER_CPU_SLOTS": "-1"},
	} {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
