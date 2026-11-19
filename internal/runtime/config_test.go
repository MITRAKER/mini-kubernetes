package runtime

import "testing"

func TestParseMemory(t *testing.T) {
	cases := map[string]int64{
		"":      0,
		"1024":  1024,
		"64Mi":  64 << 20,
		"1Gi":   1 << 30,
		"512Ki": 512 << 10,
		"500k":  500000,
		"2M":    2000000,
	}
	for in, want := range cases {
		got, err := ParseMemory(in)
		if err != nil || got != want {
			t.Errorf("ParseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "-5Mi", "1.5Gi"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("ParseMemory(%q) succeeded, want error", bad)
		}
	}
}

func TestParseCPU(t *testing.T) {
	cases := map[string]int64{"": 0, "500m": 500, "0.5": 500, "2": 2000, "1.25": 1250}
	for in, want := range cases {
		got, err := ParseCPU(in)
		if err != nil || got != want {
			t.Errorf("ParseCPU(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	if _, err := ParseCPU("fast"); err == nil {
		t.Error(`ParseCPU("fast") succeeded, want error`)
	}
}

func TestCPUQuota(t *testing.T) {
	// Half a core may run 50ms out of every 100ms period.
	if got := cpuQuota(500); got != 50000 {
		t.Errorf("cpuQuota(500) = %d, want 50000", got)
	}
}

func TestValidate(t *testing.T) {
	for _, ok := range []Config{
		{ID: "web", RootFS: "/r", Command: []string{"sh"}},
		{ID: "web", RootFS: "/r", Command: []string{"sh"}, MaxPids: MinPids},
	} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("valid config rejected: %v", err)
		}
	}
	bad := []Config{
		{RootFS: "/r", Command: []string{"sh"}},
		{ID: "../escape", RootFS: "/r", Command: []string{"sh"}},
		{ID: "web", Command: []string{"sh"}},
		{ID: "web", RootFS: "/r"},
		{ID: "web", RootFS: "/r", Command: []string{"sh"}, MemoryBytes: -1},
		{ID: "web", RootFS: "/r", Command: []string{"sh"}, MaxPids: MinPids - 1},
	}
	for i, c := range bad {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: invalid config accepted", i)
		}
	}
}
