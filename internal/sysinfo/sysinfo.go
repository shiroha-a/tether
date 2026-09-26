// Package sysinfo reports basic host health (load, memory, disk, uptime) on Linux.
package sysinfo

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Snapshot is the response of GET /api/system.
type Snapshot struct {
	Hostname  string     `json:"hostname"`
	CPUs      int        `json:"cpus"`
	Load      [3]float64 `json:"load"`
	MemTotal  uint64     `json:"memTotal"`
	MemAvail  uint64     `json:"memAvailable"`
	DiskPath  string     `json:"diskPath"`
	DiskTotal uint64     `json:"diskTotal"`
	DiskFree  uint64     `json:"diskFree"`
	UptimeSec float64    `json:"uptimeSec"`
}

// Reader collects snapshots. ProcDir is "/proc" in production.
type Reader struct {
	ProcDir  string
	DiskPath string
}

// Read gathers a snapshot. Missing sources leave their fields zero.
func (r *Reader) Read() Snapshot {
	s := Snapshot{CPUs: runtime.NumCPU(), DiskPath: r.DiskPath}
	s.Hostname, _ = os.Hostname()
	if l, err := r.loadavg(); err == nil {
		s.Load = l
	}
	if t, a, err := r.meminfo(); err == nil {
		s.MemTotal, s.MemAvail = t, a
	}
	if u, err := r.uptime(); err == nil {
		s.UptimeSec = u
	}
	var st syscall.Statfs_t
	if r.DiskPath != "" && syscall.Statfs(r.DiskPath, &st) == nil {
		s.DiskTotal = st.Blocks * uint64(st.Bsize)
		// Bfreeではなく一般ユーザーが使えるBavailを使う（dfの「空き」と同じ）
		s.DiskFree = st.Bavail * uint64(st.Bsize)
	}
	return s
}

func (r *Reader) loadavg() ([3]float64, error) {
	var out [3]float64
	b, err := os.ReadFile(filepath.Join(r.ProcDir, "loadavg"))
	if err != nil {
		return out, err
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return out, errors.New("malformed loadavg")
	}
	for i := range out {
		if out[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return out, err
		}
	}
	return out, nil
}

// meminfo returns MemTotal and MemAvailable in bytes.
func (r *Reader) meminfo() (total, avail uint64, err error) {
	f, err := os.Open(filepath.Join(r.ProcDir, "meminfo"))
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		// 値はkB単位
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total == 0 {
		return 0, 0, errors.New("MemTotal not found")
	}
	return total, avail, sc.Err()
}

func (r *Reader) uptime() (float64, error) {
	b, err := os.ReadFile(filepath.Join(r.ProcDir, "uptime"))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("malformed uptime")
	}
	return strconv.ParseFloat(f[0], 64)
}

// Handler serves GET /api/system.
func (r *Reader) Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(r.Read())
}
