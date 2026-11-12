package sprinkles

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"time"
)

// buildResourceUsage assembles a task's resource summary from docker's own
// timing/exit report and the metrics poller's final sample.
//
// The two sources are deliberately separate: docker inspect knows when the
// container started and how it exited, while the cgroup counters in the final
// sample cover what it consumed. Splitting them means a container whose cgroup
// has already been torn down still yields correct timing and exit information
// with the counters left nil, rather than nothing at all.
func buildResourceUsage(containerName string, final *MetricSample) *ResourceUsage {
	ru := &ResourceUsage{}

	if state, err := dockerInspectState(containerName); err != nil {
		// Without docker inspect there is no timing at all, but a final sample
		// may still carry usable counters, so this is not fatal.
		log.Printf("resource_usage: docker inspect %s: %v", containerName, err)
	} else {
		ru.StartTime = state.StartedAt
		ru.EndTime = state.FinishedAt
		ru.ElapsedSeconds = state.FinishedAt.Sub(state.StartedAt).Seconds()
		ru.ExitCode = state.ExitCode
		ru.OOMKilled = state.OOMKilled
	}

	if final == nil {
		log.Printf("resource_usage: no final metric sample for container %s; container counters unavailable", containerName)
		return ru
	}

	// Both structs use *int64/nil for "unavailable" and share the same field
	// names, so this is a direct copy -- no sentinel conversion needed.
	ru.ContainerCPUUsageUSec = final.ContainerCPUUsageUSec
	ru.ContainerCPUUserUSec = final.ContainerCPUUserUSec
	ru.ContainerCPUSystemUSec = final.ContainerCPUSystemUSec
	ru.ContainerCPUThrottledUSec = final.ContainerCPUThrottledUSec
	ru.ContainerCPUThrottledPeriods = final.ContainerCPUThrottledPeriods

	ru.ContainerMemoryPeakBytes = final.ContainerMemoryPeakBytes
	ru.ContainerMemoryLimitBytes = final.ContainerMemoryLimitBytes
	ru.ContainerMemoryMajorFaults = final.ContainerMemoryMajorFaults
	ru.ContainerMemoryWorkingsetRefaults = final.ContainerMemoryWorkingsetRefaults
	ru.ContainerMemoryOOMKillCount = final.ContainerMemoryOOMKillCount

	ru.ContainerCPUStallSomeUSec = final.ContainerCPUStallSomeUSec
	ru.ContainerCPUStallFullUSec = final.ContainerCPUStallFullUSec
	ru.ContainerMemoryStallSomeUSec = final.ContainerMemoryStallSomeUSec
	ru.ContainerMemoryStallFullUSec = final.ContainerMemoryStallFullUSec
	ru.ContainerIOStallSomeUSec = final.ContainerIOStallSomeUSec
	ru.ContainerIOStallFullUSec = final.ContainerIOStallFullUSec

	ru.ContainerIOReadBytes = final.ContainerIOReadBytes
	ru.ContainerIOWriteBytes = final.ContainerIOWriteBytes
	ru.ContainerIOReadOps = final.ContainerIOReadOps
	ru.ContainerIOWriteOps = final.ContainerIOWriteOps

	ru.ContainerPidsPeak = final.ContainerPidsPeak

	return ru
}

// oomMessage returns a human-readable line for the task log when the container
// (or any process in its cgroup) was OOM-killed, or "" if it wasn't. Docker's
// OOMKilled flag only reflects the main process, so the cgroup's oom_kill
// counter is checked too to catch a killed child.
func oomMessage(ru *ResourceUsage) string {
	if ru == nil {
		return ""
	}
	children := ru.ContainerMemoryOOMKillCount != nil && *ru.ContainerMemoryOOMKillCount > 0
	if !ru.OOMKilled && !children {
		return ""
	}
	var msg string
	if ru.OOMKilled {
		msg = "the task's main process was killed"
	} else {
		msg = fmt.Sprintf("%d process(es) in the task's container were killed", *ru.ContainerMemoryOOMKillCount)
	}
	msg = "[sprinkles] Out of memory: " + msg + " because the container exhausted its memory"
	if ru.ContainerMemoryLimitBytes != nil && *ru.ContainerMemoryLimitBytes > 0 {
		msg += fmt.Sprintf(" (limit %.2f GiB", float64(*ru.ContainerMemoryLimitBytes)/(1<<30))
		if ru.ContainerMemoryPeakBytes != nil {
			msg += fmt.Sprintf(", peak %.2f GiB", float64(*ru.ContainerMemoryPeakBytes)/(1<<30))
		}
		msg += ")"
	}
	return msg + ". Consider requesting more memory for this job.\n"
}

// dockerState is the subset of `docker inspect .State` we care about.
type dockerState struct {
	StartedAt  time.Time `json:"StartedAt"`
	FinishedAt time.Time `json:"FinishedAt"`
	ExitCode   int       `json:"ExitCode"`
	OOMKilled  bool      `json:"OOMKilled"`
}

func dockerInspectState(name string) (*dockerState, error) {
	out, err := exec.Command(dockerExecutable, "inspect", "--format", "{{json .State}}", name).Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var s dockerState
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("parsing docker inspect output: %w", err)
	}
	return &s, nil
}
