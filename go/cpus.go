package main

// GOMAXPROCS from the cgroup CPU quota. Go 1.24 sizes GOMAXPROCS from the host's cores, not from a
// container's quota: in a 2-CPU pod on a many-core host the runtime would run more threads than the quota
// allows and be throttled by CFS, which shows up as latency spikes. Read the quota (cgroup v2 cpu.max,
// else v1 cfs_quota_us / cfs_period_us) and cap GOMAXPROCS to it, unless GOMAXPROCS is set explicitly.

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// cgroupCPUs answers the tightest CPU quota on this process's cgroup path, 0 when there is none.
func cgroupCPUs() int {
	// cgroup v2: walk from this process's own cgroup (/proc/self/cgroup "0::<path>") up to the root,
	// so the quota is found both inside a container's namespace and in a plain scope on a host.
	if b, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		best := 0
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(line, "0::") {
				continue
			}
			p := strings.TrimSpace(line[3:])
			for {
				if data, err := os.ReadFile("/sys/fs/cgroup" + strings.TrimRight(p, "/") + "/cpu.max"); err == nil {
					f := strings.Fields(string(data))
					if len(f) == 2 && f[0] != "max" {
						if n := quotaCPUs(f[0], f[1]); n > 0 && (best == 0 || n < best) {
							best = n
						}
					}
				}
				if p == "/" || p == "" {
					break
				}
				i := strings.LastIndexByte(strings.TrimRight(p, "/"), '/')
				if i <= 0 {
					p = "/"
				} else {
					p = p[:i]
				}
			}
		}
		if best > 0 {
			return best
		}
	}
	q, err1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	p, err2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 == nil && err2 == nil {
		return quotaCPUs(strings.TrimSpace(string(q)), strings.TrimSpace(string(p)))
	}
	return 0
}

func quotaCPUs(quota, period string) int {
	q, err1 := strconv.ParseFloat(quota, 64)
	p, err2 := strconv.ParseFloat(period, 64)
	if err1 != nil || err2 != nil || q <= 0 || p <= 0 {
		return 0
	}
	n := int((q + p - 1) / p) // round up: 1.5 CPUs still gets 2 threads
	if n < 1 {
		n = 1
	}
	return n
}

func fitGOMAXPROCS() {
	if os.Getenv("GOMAXPROCS") != "" {
		return
	}
	if n := cgroupCPUs(); n > 0 && n < runtime.NumCPU() {
		runtime.GOMAXPROCS(n)
	}
}
