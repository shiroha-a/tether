package sysinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func fakeProc(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRead(t *testing.T) {
	proc := fakeProc(t, map[string]string{
		"loadavg": "0.52 1.25 2.00 3/1234 56789\n",
		"meminfo": "MemTotal:       32000000 kB\nMemFree:         1000000 kB\nMemAvailable:   20000000 kB\nBuffers: x kB\n",
		"uptime":  "86461.25 170000.00\n",
	})
	disk := t.TempDir()
	s := (&Reader{ProcDir: proc, DiskPath: disk}).Read()
	if s.Load != [3]float64{0.52, 1.25, 2.00} {
		t.Fatalf("load = %v", s.Load)
	}
	if s.MemTotal != 32000000*1024 || s.MemAvail != 20000000*1024 {
		t.Fatalf("mem = %d/%d", s.MemAvail, s.MemTotal)
	}
	if s.UptimeSec != 86461.25 {
		t.Fatalf("uptime = %v", s.UptimeSec)
	}
	if s.CPUs < 1 || s.DiskTotal == 0 || s.DiskFree == 0 || s.DiskFree > s.DiskTotal || s.DiskPath != disk {
		t.Fatalf("cpus/disk = %+v", s)
	}
}

func TestReadToleratesMissingOrMalformedSources(t *testing.T) {
	proc := fakeProc(t, map[string]string{
		"loadavg": "garbage\n",
		"meminfo": "MemFree: 10 kB\n",
	})
	s := (&Reader{ProcDir: proc}).Read()
	if s.Load != [3]float64{} || s.MemTotal != 0 || s.UptimeSec != 0 || s.DiskTotal != 0 {
		t.Fatalf("expected zero values, got %+v", s)
	}
}
