package systemd

// File names and contents for the system-scope account and hardware-access
// integration. The 1.0 rules port the intent of packaging/system/*.rules:
// read-only group access to RAPL package counters and, only when explicitly
// enabled, NVIDIA BAR0, applied by udev whenever the kernel recreates the
// sysfs nodes.

const (
	// SysusersFileName creates the two service accounts and hardware groups.
	SysusersFileName = "rigscope.conf"
	// RAPLRuleName grants the collector group read access to package energy.
	RAPLRuleName = "90-rigscope-rapl.rules"
	// GPUTempsRuleName grants the collector group read access to NVIDIA BAR0.
	GPUTempsRuleName = "90-rigscope-gpu-temps.rules"
)

// SysusersOptions selects the optional hardware groups. PCI register access is
// opt-in, so a hub-only or default install does not create rigscope-gpu or
// grant the collector any device access.
type SysusersOptions struct {
	RAPL         bool
	GPURegisters bool
}

// RenderSysusers returns /etc/sysusers.d/rigscope.conf. Both service accounts
// exist even when one component is disabled, so it can be enabled later.
func RenderSysusers(options SysusersOptions) string {
	var b []byte
	appendLine := func(line string) {
		b = append(b, line...)
		b = append(b, '\n')
	}
	appendLine(markerPrefix + ` Do not edit; run "rigscope install --system" to rewrite.`)
	appendLine("# Shared group lets both unprivileged services read the 0640 config file.")
	appendLine("g rigscope - -")
	appendLine("# Dedicated unprivileged, non-login service accounts.")
	appendLine("g rigscope-hub - -")
	appendLine(`u rigscope-hub - "Rigscope hub" /nonexistent /usr/sbin/nologin`)
	appendLine("g rigscope-collector - -")
	appendLine(`u rigscope-collector - "Rigscope collector" /nonexistent /usr/sbin/nologin`)
	appendLine("m rigscope-hub rigscope")
	appendLine("m rigscope-collector rigscope")
	if options.RAPL {
		appendLine("# CPU package energy counters are readable by the collector only.")
		appendLine("g rigscope-rapl - -")
		appendLine("m rigscope-collector rigscope-rapl")
	}
	if options.GPURegisters {
		appendLine("# NVIDIA BAR0 register temperatures are opt-in.")
		appendLine("g rigscope-gpu - -")
		appendLine("m rigscope-collector rigscope-gpu")
	}
	return string(b)
}

// HardwareGroups returns the supplementary groups for the collector unit. It
// mirrors exactly the sysusers/udev files the installer wrote.
func HardwareGroups(rapl, gpuRegisters bool) []string {
	var groups []string
	if rapl {
		groups = append(groups, RAPLGroup)
	}
	if gpuRegisters {
		groups = append(groups, GPUGroup)
	}
	return groups
}

// RenderRAPLRule returns the powercap udev rule. Only top-level package
// devices are touched; subdomains, constraints and power-limit writes are not.
func RenderRAPLRule() string {
	return markerPrefix + ` Do not edit; run "rigscope install --system" to rewrite.
# Read-only package energy/range counters for the Rigscope collector group.
SUBSYSTEM=="powercap", ACTION=="add|change", KERNEL=="intel-rapl:[0-9]*", KERNEL!="intel-rapl:*:*", ATTR{name}=="package*", \
  RUN+="/usr/bin/chgrp rigscope-rapl /sys%p/energy_uj", \
  RUN+="/usr/bin/chmod 0440 /sys%p/energy_uj", \
  RUN+="/usr/bin/chgrp rigscope-rapl /sys%p/max_energy_range_uj", \
  RUN+="/usr/bin/chmod 0440 /sys%p/max_energy_range_uj"
`
}

// RenderGPUTempsRule returns the PCI udev rule for NVIDIA BAR0. It matches
// display functions of vendor 0x10de only and is installed only for
// `install --gpu-registers`. In system scope the group is [GPUGroup]; a user
// install passes the invoking user's private primary group when it has one.
// The group is part of the rule text, so both install and doctor can tell
// which group the file grants.
func RenderGPUTempsRule(group string) string {
	return markerPrefix + ` Do not edit; run "rigscope install --gpu-registers" to rewrite.
# Read-only NVIDIA BAR0 access for junction/VRAM register temperatures.
# Group: ` + group + `
ACTION=="add|change", SUBSYSTEM=="pci", ATTR{vendor}=="0x10de", ATTR{class}=="0x03[0-9a-f][0-9a-f][0-9a-f][0-9a-f]", \
  RUN+="/usr/bin/chgrp ` + group + ` /sys%p/resource0", \
  RUN+="/usr/bin/chmod 0440 /sys%p/resource0", \
  OPTIONS+="watch"
`
}
