package nodestat

import "testing"

func TestParseMeminfo(t *testing.T) {
	const sample = "MemTotal:       8000000 kB\nMemFree:  100 kB\nMemAvailable:   4000000 kB\n"
	total, avail, err := parseMeminfo(sample)
	if err != nil {
		t.Fatal(err)
	}
	if total != 8000000*1024 || avail != 4000000*1024 {
		t.Fatalf("total=%d avail=%d", total, avail)
	}
	if _, _, err := parseMeminfo("MemFree: 1 kB\n"); err == nil {
		t.Fatal("expected error without MemTotal")
	}
}
