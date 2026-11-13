package policy

import (
	"github.com/Triet1402/rigscope/internal/collector/processes"
	"github.com/Triet1402/rigscope/internal/protocol"
)

// WithheldReason is the reason carried by a family hidden by the local
// sharing policy.
const WithheldReason = "withheld by local sharing policy"

// Input is a partially specified policy as configuration supplies it. A nil
// field means "use the default"; a pointer distinguishes an explicit false
// from an omitted key.
type Input struct {
	CPU              *bool `json:"cpu,omitempty"`
	Memory           *bool `json:"memory,omitempty"`
	Swap             *bool `json:"swap,omitempty"`
	GPUs             *bool `json:"gpus,omitempty"`
	Disks            *bool `json:"disks,omitempty"`
	Network          *bool `json:"network,omitempty"`
	NetworkAddresses *bool `json:"networkAddresses,omitempty"`
	Processes        *bool `json:"processes,omitempty"`
	ProcessArguments *bool `json:"processArguments,omitempty"`
}

// Policy is the effective local sharing policy. The zero value withholds
// everything and is the fail-closed choice when a policy source breaks.
type Policy struct {
	CPU              bool
	Memory           bool
	Swap             bool
	GPUs             bool
	Disks            bool
	Network          bool
	NetworkAddresses bool
	Processes        bool
	ProcessArguments bool
}

// Default returns the privacy-preserving defaults:
//
//   - metric families, inventory families and process visibility are shared;
//   - swap follows memory (sharing memory is also a swap decision);
//   - network addresses are withheld because interface addresses identify a
//     host on the network;
//   - process arguments are withheld because command lines carry secrets.
func Default() Policy {
	return Policy{
		CPU:              true,
		Memory:           true,
		Swap:             true,
		GPUs:             true,
		Disks:            true,
		Network:          true,
		NetworkAddresses: false,
		Processes:        true,
		ProcessArguments: false,
	}
}

// Normalize fills the defaults for a partially specified policy.
func Normalize(raw *Input) Policy {
	if raw == nil {
		return Default()
	}
	memory := valueOr(raw.Memory, true)
	return Policy{
		CPU:    valueOr(raw.CPU, true),
		Memory: memory,
		// Swap follows memory unless the input says otherwise, matching the
		// prototype's legacy sharing defaults.
		Swap:             valueOr(raw.Swap, memory),
		GPUs:             valueOr(raw.GPUs, true),
		Disks:            valueOr(raw.Disks, true),
		Network:          valueOr(raw.Network, true),
		NetworkAddresses: valueOr(raw.NetworkAddresses, false),
		Processes:        valueOr(raw.Processes, true),
		ProcessArguments: valueOr(raw.ProcessArguments, false),
	}
}

// MetricEnabled reports whether one metric family is shared.
func (p Policy) MetricEnabled(family protocol.MetricFamily) bool {
	switch family {
	case protocol.FamilyCPU:
		return p.CPU
	case protocol.FamilyMemory:
		return p.Memory
	case protocol.FamilySwap:
		return p.Swap
	case protocol.FamilyGPUs:
		return p.GPUs
	case protocol.FamilyDisks:
		return p.Disks
	case protocol.FamilyNetwork:
		return p.Network
	default:
		return false
	}
}

// ProcessesPolicy exposes the policy in the shape the process-detail
// scheduler expects.
func ProcessesPolicy(p Policy) processes.Policy {
	return processes.Policy{Processes: p.Processes, ProcessArguments: p.ProcessArguments}
}

func valueOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func withheld() protocol.Unavailable {
	return protocol.Unavailable{Reason: WithheldReason}
}

func withheldCapability() protocol.CapabilityState {
	return protocol.CapabilityState{Status: protocol.CapabilityWithheld}
}
