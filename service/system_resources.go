package service

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	gnet "github.com/shirou/gopsutil/net"
)

const SystemResourceRetention = 24 * time.Hour

type SystemResourceSnapshot struct {
	Node       string                     `json:"node"`
	ServerTime int64                      `json:"server_time"`
	Sample     *model.SystemResourcePoint `json:"sample"`
}

var (
	systemResourceMonitorOnce sync.Once
	latestSystemResources     atomic.Pointer[model.SystemResourcePoint]
)

func GetSystemResources() SystemResourceSnapshot {
	return SystemResourceSnapshot{Node: common.GetNodeIdentity().Name, ServerTime: time.Now().Unix(), Sample: latestSystemResources.Load()}
}

// This sampler is independent of the overload guard: turning protection off
// must not turn monitoring into zero-valued measurements. CPU and RAM describe
// the host visible to the process, not the Go heap or the cgroup quota.
func StartSystemResourceMonitor() {
	systemResourceMonitorOnce.Do(func() {
		go func() {
			var sampler systemResourceSampler
			var savedMinute int64 = -1
			var cleanedAt time.Time
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				point := sampler.collect()
				latestSystemResources.Store(&point)
				if minute := point.Timestamp / 60; minute != savedMinute {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					err := model.SaveSystemResourceSample(ctx, common.GetNodeIdentity().Name, point)
					cancel()
					if err != nil {
						common.SysError(common.LogText("failed to persist system resource sample: %s", err.Error()))
					} else {
						savedMinute = minute
					}
				}
				if time.Since(cleanedAt) >= time.Hour {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					err := model.DeleteExpiredSystemResourceSamples(ctx, time.Now().Add(-SystemResourceRetention).Unix())
					cancel()
					cleanedAt = time.Now()
					if err != nil {
						common.SysError(common.LogText("failed to prune system resource samples: %s", err.Error()))
					}
				}
				<-ticker.C
			}
		}()
	})
}

type systemResourceSampler struct {
	previousCPU     []cpu.TimesStat
	previousNetwork map[string]gnet.IOCountersStat
	previousTime    time.Time
}

func (sampler *systemResourceSampler) collect() model.SystemResourcePoint {
	cpuTimes, cpuErr := cpu.Times(false)
	if cpuErr != nil {
		cpuTimes = nil
	}
	var network map[string]gnet.IOCountersStat
	interfaces, interfacesErr := net.Interfaces()
	counters, countersErr := gnet.IOCounters(true)
	if interfacesErr == nil && countersErr == nil {
		eligible := make(map[string]bool, len(interfaces))
		for _, nic := range interfaces {
			eligible[nic.Name] = nic.Flags&net.FlagLoopback == 0 && nic.Flags&net.FlagUp != 0
		}
		network = make(map[string]gnet.IOCountersStat)
		for _, counter := range counters {
			if eligible[counter.Name] {
				network[counter.Name] = counter
			}
		}
	}
	point := sampler.advance(time.Now(), cpuTimes, network)
	if memory, err := mem.VirtualMemory(); err == nil && memory.Total > 0 {
		point.MemoryPercent = &memory.UsedPercent
		point.MemoryUsed = &memory.Used
		point.MemoryTotal = &memory.Total
	}
	if disk := common.GetDiskSpaceInfo(); disk.Total > 0 {
		point.DiskPercent = &disk.UsedPercent
		point.DiskUsed = &disk.Used
		point.DiskTotal = &disk.Total
	}
	return point
}

// Counter resets, interface changes, failed reads and the first sample break
// the baseline. They must not appear as negative rates or invented zeroes.
func (sampler *systemResourceSampler) advance(now time.Time, cpuTimes []cpu.TimesStat, network map[string]gnet.IOCountersStat) model.SystemResourcePoint {
	point := model.SystemResourcePoint{Timestamp: now.Unix()}
	elapsed := now.Sub(sampler.previousTime).Seconds()
	if !sampler.previousTime.IsZero() && elapsed > 0 && elapsed <= 30 {
		if len(cpuTimes) == 1 && len(sampler.previousCPU) == 1 {
			current, previous := cpuTimes[0], sampler.previousCPU[0]
			total := current.Total() - previous.Total()
			idle := current.Idle - previous.Idle
			if total > 0 && idle >= 0 && idle <= total {
				percent := (total - idle) / total * 100
				point.CPUPercent = &percent
			}
		}
		if len(network) > 0 && len(network) == len(sampler.previousNetwork) {
			var received, sent float64
			valid := true
			for name, current := range network {
				previous, exists := sampler.previousNetwork[name]
				if !exists || current.BytesRecv < previous.BytesRecv || current.BytesSent < previous.BytesSent {
					valid = false
					break
				}
				received += float64(current.BytesRecv-previous.BytesRecv) / elapsed
				sent += float64(current.BytesSent-previous.BytesSent) / elapsed
			}
			if valid {
				point.NetworkReceive = &received
				point.NetworkSend = &sent
			}
		}
	}
	sampler.previousCPU = cpuTimes
	sampler.previousNetwork = network
	sampler.previousTime = now
	return point
}
