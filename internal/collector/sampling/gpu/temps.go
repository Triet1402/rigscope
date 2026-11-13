package gpu

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Triet1402/rigscope/internal/protocol"
)

// NVIDIA junction and VRAM temperature sampling — read-only PCI mappings.
//
// Reads the same undocumented GPU thermal registers as
// github.com/ThomasBaruzier/gddr6-core-junction-vram-temps:
//
//   - GDDR6/GDDR6X junction: BAR0 + 0x0002046C, °C at bits 8..15
//   - GDDR6/GDDR6X VRAM:     BAR0 + 0x0000E2A8, 12-bit ADC / 32 (°C)
//   - Blackwell junction:    BAR0 + 0x00AD0A90, 4 die channels and a hardware
//     hotspot at +0x10, Q8.8 fixed point
//   - GDDR7 VRAM:            BAR0 + 0x009024C0, per-FBPA DQR registers
//     (stride 0x4000): temperature code at bits 16..23, 20 = 0 °C, 2 °C per
//     code; validity bits at +0x10, poison pattern 0xBADF00xx
//
// Access goes through /sys/bus/pci/devices/<bdf>/resource0, the kernel's
// mapped view of BAR0, via a bounded read-only [HelperSource] that runs the
// mapping in the `rigscope internal pci-read` child. Reading that file needs
// permission: it is root-owned 0600 by default, and
// packaging/system/90-rigscope-gpu-temps.rules grants a dedicated group read
// access (`rigscope gpu-temps enable` installs it).
//
// The feature degrades silently: without the ACL the reads fail, the fields
// stay nil and the poller only logs once per state change. GPU core
// temperature keeps coming from nvidia-smi.

const (
	gddr6TempLimitC  = 0x7f
	gddr7ZeroCCode   = 20
	gddr7CodeMin     = 21
	gddr7CodeMax     = 80
	gddr7PoisonMask  = 0xffff0000
	gddr7PoisonValue = 0xbadf0000
	// EnvGPUTemps is the register-temperature kill switch, e.g.
	// COLLECTOR_GPU_TEMPS=off.
	EnvGPUTemps = "COLLECTOR_GPU_TEMPS"
)

// SensorProfile selects the register map for a device.
type SensorProfile string

const (
	ProfileGDDR6 SensorProfile = "gddr6"
	ProfileGDDR7 SensorProfile = "gddr7"
)

// blackwellDeviceIDs are the PCI device ids whose thermal registers follow
// the GDDR7 map.
var blackwellDeviceIDs = map[uint32]struct{}{
	0x2b85: {}, // RTX 5090
	0x2b82: {}, // RTX 5080
	0x2c05: {}, // RTX 5070 Ti
	0x2c02: {}, // RTX 5070
	0x2d04: {}, // RTX 5060 Ti
	0x2d19: {}, // RTX 5060 Laptop
	0x2d59: {}, // RTX 5060 Laptop variant
	0x2c33: {}, // RTX PRO 4000 SFF (Blackwell generation)
}

// SelectSensorProfile picks the register map for a PCI device id.
func SelectSensorProfile(deviceID uint32) SensorProfile {
	if _, ok := blackwellDeviceIDs[deviceID]; ok {
		return ProfileGDDR7
	}
	return ProfileGDDR6
}

// SensorValues is one device's register temperatures. nil means the register
// was unreadable or the decoded value was invalid.
type SensorValues struct {
	JunctionTemperatureC *float64
	VRAMTemperatureC     *float64
}

// SensorMap is keyed by normalized PCI bus id ("01:00.0").
type SensorMap map[string]SensorValues

// SampleScope names the PCI devices one sensor cycle may touch. The zero
// value is "ungated": every NVIDIA display device may be sampled. A scope
// built by [PermittedScope] contains exactly the devices the runtime-PM gate
// allowed this cycle, so a suspended or paused GPU is never touched — neither
// by a register read nor by the permission probe.
type SampleScope struct {
	permitted map[string]struct{}
	// live, when set, is the poller-owned authorization a cycle must consult
	// before every device access. A cycle started under an older decision sees
	// the current one, so narrowing the permitted set takes effect at once.
	live *liveAuthorization
	// access, when set, is the shared per-device arbiter. Every BAR read and
	// permission probe also needs a lease from it, so a revocation racing the
	// scope check still fails closed.
	access *AccessArbiter
}

// liveAuthorization is the poller-owned, mutable authorization for register
// sampling. Publishing a new decision advances the generation so the poller
// can tell that a running cycle's scope was replaced.
type liveAuthorization struct {
	mu         sync.RWMutex
	generation uint64
	current    SampleScope
}

func newLiveAuthorization() *liveAuthorization {
	return &liveAuthorization{current: AllDevices()}
}

// scope returns a live view of the authorization.
func (l *liveAuthorization) scope() SampleScope { return SampleScope{live: l} }

func (l *liveAuthorization) allows(bdf string) bool {
	l.mu.RLock()
	current := l.current
	l.mu.RUnlock()
	return current.Allows(bdf)
}

func (l *liveAuthorization) gated() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.current.permitted != nil
}

// publish replaces the current decision. It reports the resulting generation
// and whether the permitted set actually changed, so the caller can cancel a
// cycle that was started under the previous decision.
func (l *liveAuthorization) publish(scope SampleScope) (uint64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := !scope.samePermitted(l.current)
	if changed {
		l.generation++
	}
	l.current = scope
	return l.generation, changed
}

// samePermitted reports whether two scopes permit exactly the same devices.
// The gated/ungated distinction matters: an ungated scope and an empty gated
// scope both allow nothing to be read, but only the latter is a gate decision.
func (s SampleScope) samePermitted(other SampleScope) bool {
	if (s.permitted == nil) != (other.permitted == nil) {
		return false
	}
	if len(s.permitted) != len(other.permitted) {
		return false
	}
	for key := range s.permitted {
		if _, ok := other.permitted[key]; !ok {
			return false
		}
	}
	return true
}

// AllDevices is the ungated scope: no runtime-PM gate is configured.
func AllDevices() SampleScope { return SampleScope{} }

// PermittedScope restricts sampling to the given PCI addresses (any spelling
// [CanonicalPCIAddress] accepts). Authorization uses the full canonical
// domain:bus:device.function address, so permitting "0000:01:00.0" does not
// also permit "0001:01:00.0". An empty list permits nothing, which is the
// "gate excluded every device" case.
func PermittedScope(addresses []string) SampleScope {
	permitted := make(map[string]struct{}, len(addresses))
	for _, address := range addresses {
		if key, ok := CanonicalPCIAddress(address); ok {
			permitted[key] = struct{}{}
		}
	}
	return SampleScope{permitted: permitted}
}

// Gated reports whether the scope came from a runtime-PM decision.
func (s SampleScope) Gated() bool {
	if s.live != nil {
		return s.live.gated()
	}
	return s.permitted != nil
}

// Allows reports whether the device may be touched now. A live scope consults
// the CURRENT authorization, so a device access that was permitted when the
// cycle started is refused after a later gate decision excludes it.
func (s SampleScope) Allows(bdf string) bool {
	if s.live != nil {
		return s.live.allows(bdf)
	}
	if s.permitted == nil {
		return true
	}
	key, ok := CanonicalPCIAddress(bdf)
	if !ok {
		return false
	}
	_, allowed := s.permitted[key]
	return allowed
}

// SensorSource is the register-temperature source the poller runs alongside
// nvidia-smi. [TempsPoller] implements it; tests inject a fixed map. The scope
// carries the runtime-PM decision for this cycle: a source must never read or
// probe a device the scope excludes.
type SensorSource interface {
	PollOnce(ctx context.Context, scope SampleScope) error
	Latest() SensorMap
}

// copySensorMap returns an independent copy so callers cannot mutate the
// poller's cache, including through the optional temperature pointers.
func copySensorMap(sensors SensorMap) SensorMap {
	copied := make(SensorMap, len(sensors))
	for key, values := range sensors {
		copied[key] = copySensorValues(values)
	}
	return copied
}

// copySensorValues copies the pointed-to temperatures, which are shared with
// the cached map otherwise.
func copySensorValues(values SensorValues) SensorValues {
	copied := SensorValues{}
	if values.JunctionTemperatureC != nil {
		copied.JunctionTemperatureC = pointer(*values.JunctionTemperatureC)
	}
	if values.VRAMTemperatureC != nil {
		copied.VRAMTemperatureC = pointer(*values.VRAMTemperatureC)
	}
	return copied
}

// DecodeGDDR6Junction decodes the junction byte at bits 8..15.
//
// All-zero readings mean the therm registers are not answering (an
// unsupported card such as the RTX 3070, or a block that reads zeros), not a
// real 0 °C: a running card never sits at the freezing point. Like
// [DecodeBlackwell], reject zero so unsupported cards report nothing.
func DecodeGDDR6Junction(raw *uint32) *float64 {
	if raw == nil || *raw == 0 || *raw == 0xffffffff {
		return nil
	}
	celsius := (*raw >> 8) & 0xff
	if celsius == 0 || celsius >= gddr6TempLimitC {
		return nil
	}
	return pointer(float64(celsius))
}

// DecodeGDDR6VRAM decodes the 12-bit ADC divided by 32.
func DecodeGDDR6VRAM(raw *uint32) *float64 {
	if raw == nil || *raw == 0 || *raw == 0xffffffff {
		return nil
	}
	celsius := float64(*raw&0xfff) / 32
	if celsius == 0 || celsius >= gddr6TempLimitC {
		return nil
	}
	return pointer(roundTenths(celsius))
}

// DecodeBlackwell decodes a Q8.8 die-channel or hardware-hotspot register.
func DecodeBlackwell(raw *uint32) *float64 {
	if raw == nil || *raw == 0xffffffff {
		return nil
	}
	fixed := *raw & 0xffff
	if fixed == 0 || fixed > 150*256 {
		return nil
	}
	return pointer(roundTenths(float64(fixed) / 256))
}

// IsGDDR7Poison reports the driver's poison pattern for a DQR register.
func IsGDDR7Poison(raw uint32) bool {
	return raw&gddr7PoisonMask == gddr7PoisonValue
}

// DecodeGDDR7 decodes a per-FBPA DQR temperature: 2 °C per code, 20 = 0 °C.
func DecodeGDDR7(raw *uint32) *float64 {
	if raw == nil || *raw == 0xffffffff || IsGDDR7Poison(*raw) {
		return nil
	}
	code := (*raw >> 16) & 0xff
	if code < gddr7CodeMin || code > gddr7CodeMax {
		return nil
	}
	return pointer(float64((code - gddr7ZeroCCode) * 2))
}

func roundTenths(value float64) float64 {
	return float64(int64(value*10+0.5)) / 10
}

var busFunctionPattern = regexp.MustCompile(`^[0-9a-f]{2}\.[0-7]$`)
var busNumberPattern = regexp.MustCompile(`^[0-9a-f]{1,4}$`)
var pciDomainPattern = regexp.MustCompile(`^[0-9a-f]{1,8}$`)

// CanonicalPCIAddress is the authorization key for one PCI function:
// "domain:bus:device.function" with a four-digit lower-case domain, e.g.
// "0000:01:00.0". It is deliberately domain-preserving: permitting
// "0000:01:00.0" must never permit the same bus/function in another PCI
// domain. Input may carry a zero-padded domain ("00000000:01:00.0" as
// nvidia-smi prints it) or none at all, which means domain 0.
//
// Do not use this for telemetry joins: nvidia-smi and sysfs disagree about
// domains in ways [NormalizeBusFunctionKey] absorbs.
func CanonicalPCIAddress(raw string) (string, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(raw)), ":")
	var domainPart string
	switch len(parts) {
	case 3:
		domainPart = parts[0]
	case 2:
		domainPart = "0"
	default:
		return "", false
	}
	bus := parts[len(parts)-2]
	deviceFunction := parts[len(parts)-1]
	if !pciDomainPattern.MatchString(domainPart) || !busNumberPattern.MatchString(bus) ||
		!busFunctionPattern.MatchString(deviceFunction) {
		return "", false
	}
	domain, err := strconv.ParseUint(domainPart, 16, 32)
	if err != nil || domain > 0xffff {
		return "", false
	}
	busNumber, err := strconv.ParseUint(bus, 16, 16)
	if err != nil || busNumber > 0xff {
		return "", false
	}
	return fmt.Sprintf("%04x:%02x:%s", domain, busNumber, deviceFunction), true
}

// NormalizeBusFunctionKey is the merge key for joining register readings with
// nvidia-smi rows: the trailing bus:device.function part in lower case, so
// "00000000:01:00.0" (nvidia-smi) and "0000:01:00.0" (sysfs) both become
// "01:00.0".
//
// Deliberately not the inventory's PCI bus id normalization, which preserves
// the domain: this key drops it on purpose so the two spellings join. Do not
// unify them.
func NormalizeBusFunctionKey(raw string) (string, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(raw)), ":")
	if len(parts) < 2 {
		return "", false
	}
	bus := parts[len(parts)-2]
	deviceFunction := parts[len(parts)-1]
	if !busNumberPattern.MatchString(bus) || !busFunctionPattern.MatchString(deviceFunction) {
		return "", false
	}
	return bus + ":" + deviceFunction, true
}

// RegisterSource reads 32-bit little-endian words from a PCI resource file.
// A nil element means that offset could not be read; an error means the whole
// resource could not be accessed. [MappedSource] is the production
// implementation.
type RegisterSource interface {
	ReadRegisters(ctx context.Context, resourcePath string, offsets []int64) ([]*uint32, error)
}

// NVDisplayDevice is one NVIDIA display function discovered under sysfs.
type NVDisplayDevice struct {
	BDF      string
	DeviceID uint32
}

var hexAttributePattern = regexp.MustCompile(`^0x([0-9a-fA-F]+)$`)

func parseHexAttribute(raw string, ok bool) (uint32, bool) {
	if !ok {
		return 0, false
	}
	match := hexAttributePattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return 0, false
	}
	value, err := strconv.ParseUint(match[1], 16, 32)
	if err != nil {
		return 0, false
	}
	return uint32(value), true
}

// isDisplayClass reports PCI base class/sub-class 0x0300 VGA or 0x0302 3D
// controller.
func isDisplayClass(classCode uint32) bool {
	base := (classCode >> 8) & 0xffff
	return base == 0x0300 || base == 0x0302
}

// DiscoverNvidiaDisplays lists NVIDIA display functions under sysfs. Audio
// functions of the same chip and other vendors' GPUs are skipped.
func DiscoverNvidiaDisplays(sysfs SysFS, sysfsDir string) []NVDisplayDevice {
	if sysfsDir == "" {
		sysfsDir = DefaultDRMDevicesDir
	}
	names, err := sysfs.ReadDir(sysfsDir)
	if err != nil {
		return nil
	}
	if len(names) > 256 {
		names = names[:256]
	}
	devices := make([]NVDisplayDevice, 0, 1)
	for _, name := range names {
		dir := sysfsDir + "/" + name
		vendor, ok := parseHexAttribute(readTextTrimmed(sysfs, dir+"/vendor"))
		if !ok || vendor != 0x10de {
			continue
		}
		classCode, ok := parseHexAttribute(readTextTrimmed(sysfs, dir+"/class"))
		if !ok || !isDisplayClass(classCode) {
			continue
		}
		deviceID, ok := parseHexAttribute(readTextTrimmed(sysfs, dir+"/device"))
		if !ok {
			continue
		}
		devices = append(devices, NVDisplayDevice{BDF: name, DeviceID: deviceID})
		if len(devices) >= protocol.MaxGPUs {
			break
		}
	}
	return devices
}

// scopedDisplays lists the NVIDIA display devices the scope permits, in sysfs
// order. Discovery itself only reads vendor/class/device attributes, which do
// not resume a suspended GPU; the caller must still restrict every BAR access
// and permission probe to this list.
func scopedDisplays(sysfs SysFS, sysfsDir string, scope SampleScope) []NVDisplayDevice {
	devices := DiscoverNvidiaDisplays(sysfs, sysfsDir)
	if !scope.Gated() {
		return devices
	}
	permitted := make([]NVDisplayDevice, 0, len(devices))
	for _, device := range devices {
		if scope.Allows(device.BDF) {
			permitted = append(permitted, device)
		}
	}
	return permitted
}

// --- register map ----------------------------------------------------------

const (
	gddr6JunctionThermOffset = 0x0002046c
	gddr6VRAMADCOffset       = 0x0000e2a8

	blackwellThermOffset       = 0x00ad0a90
	blackwellThermChannelCount = 4
	blackwellThermHWSpotOffset = 0x10

	gddr7DQROffset           = 0x009024c0
	gddr7DQRValidOffset      = 0x10
	gddr7DQRStride           = 0x4000
	gddr7DQRSubregisterCount = 4
	gddr7MaxFBPACount        = 16
)

func blackwellThermOffsets() []int64 {
	offsets := make([]int64, 0, blackwellThermChannelCount+1)
	for channel := 0; channel < blackwellThermChannelCount; channel++ {
		offsets = append(offsets, blackwellThermOffset+int64(channel)*4)
	}
	return append(offsets, blackwellThermOffset+blackwellThermHWSpotOffset)
}

func gddr7StatusOffsets() []int64 {
	offsets := make([]int64, 0, gddr7MaxFBPACount)
	for fbpa := 0; fbpa < gddr7MaxFBPACount; fbpa++ {
		offsets = append(offsets, gddr7DQROffset+int64(fbpa)*gddr7DQRStride+gddr7DQRValidOffset)
	}
	return offsets
}

// gddr7TemperatureOffsets lists the DQR temperature entries the statuses
// advertise as valid.
func gddr7TemperatureOffsets(statuses []*uint32) []int64 {
	offsets := make([]int64, 0, len(statuses)*gddr7DQRSubregisterCount)
	for fbpa, statusPtr := range statuses {
		if statusPtr == nil || *statusPtr == 0xffffffff || IsGDDR7Poison(*statusPtr) {
			continue
		}
		status := *statusPtr
		base := int64(fbpa) * gddr7DQRStride
		for sub := 0; sub < gddr7DQRSubregisterCount; sub++ {
			if status&(1<<uint(24+sub)) == 0 {
				continue
			}
			offsets = append(offsets, gddr7DQROffset+base+int64(sub)*4)
		}
	}
	return offsets
}

// sensorDiagnostics records why a register batch did not produce readings, so
// the poller can distinguish a stuck helper from a permission failure instead
// of blaming the hardware for both.
type sensorDiagnostics struct {
	stuck  bool
	denied bool
	// deniedReason is the refusal's user-facing reason: a powered-down device
	// and a gate denial are operationally different, even though both fail
	// closed.
	deniedReason string
	err          error
}

func (d *sensorDiagnostics) observe(err error) {
	if d == nil || err == nil {
		return
	}
	switch {
	case errors.Is(err, errChildNotReaped):
		d.stuck = true
	case errors.Is(err, ErrAccessDenied):
		d.denied = true
		d.deniedReason = accessDeniedReason(err)
	default:
		d.err = err
	}
}

// deniedDiagnostics is the diagnostic published for an arbiter refusal.
func (d *sensorDiagnostics) deniedDiagnostics() SensorDiagnostics {
	reason := AccessDeniedReason
	if d != nil && d.deniedReason != "" {
		reason = d.deniedReason
	}
	return SensorDiagnostics{Reason: reason, Denied: true}
}

// readRegisters reads a batch and normalizes every failure to nil values: a
// device whose BAR0 is inaccessible simply has no register temperatures.
//
// The authorization is checked immediately before the access, not once per
// cycle, and the batch runs under an access lease. A revocation that starts
// after the cycle began therefore refuses the next batch and waits for this one
// to settle. The returned slice always has one element per offset, so callers
// may slice and index it without a nil check.
func readRegisters(ctx context.Context, source RegisterSource, resourcePath string, offsets []int64,
	allowed func() bool, access *AccessArbiter, bdf string, diag *sensorDiagnostics) []*uint32 {
	values := make([]*uint32, len(offsets))
	if len(offsets) == 0 || !allowed() {
		return values
	}
	if access != nil {
		lease, err := access.Acquire(ctx, bdf)
		if err != nil {
			diag.observe(err)
			return values
		}
		defer lease.Release()
	}
	read, err := source.ReadRegisters(ctx, resourcePath, offsets)
	if err != nil {
		diag.observe(err)
		return values
	}
	if len(read) != len(offsets) {
		diag.observe(errIncompleteRead)
		return values
	}
	return read
}

// errIncompleteRead marks a source that answered the wrong number of words.
var errIncompleteRead = errors.New("gpu: register read returned the wrong number of values")

func hottest(a, b *float64) *float64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *a >= *b {
		return a
	}
	return b
}

func hottestDecoded(raws []*uint32, decode func(*uint32) *float64) *float64 {
	var best *float64
	for _, raw := range raws {
		best = hottest(best, decode(raw))
	}
	return best
}

func sampleGDDR6(ctx context.Context, source RegisterSource, resourcePath, bdf string,
	allowed func() bool, access *AccessArbiter, diag *sensorDiagnostics) SensorValues {
	values := readRegisters(ctx, source, resourcePath, []int64{gddr6JunctionThermOffset, gddr6VRAMADCOffset},
		allowed, access, bdf, diag)
	return SensorValues{
		JunctionTemperatureC: DecodeGDDR6Junction(valueAt(values, 0)),
		VRAMTemperatureC:     DecodeGDDR6VRAM(valueAt(values, 1)),
	}
}

// sampleGDDR7 uses both the Blackwell thermal block (junction) and the
// per-FBPA DQR registers (VRAM) from the same BAR0. The first batch carries
// the FBPA statuses plus the Blackwell offsets, so the source opens resource0
// at most twice: once here and once for the valid DQR entries.
//
// A batch is the unit of irreversibility: one lease, one dispatch, one reap,
// one release. The second batch re-checks the authorization and takes its own
// lease, so a revocation between the batches refuses the second one.
func sampleGDDR7(ctx context.Context, source RegisterSource, resourcePath, bdf string,
	allowed func() bool, access *AccessArbiter, diag *sensorDiagnostics) SensorValues {
	statusOffsets := gddr7StatusOffsets()
	thermOffsets := blackwellThermOffsets()
	first := readRegisters(ctx, source, resourcePath, append(append([]int64{}, statusOffsets...), thermOffsets...),
		allowed, access, bdf, diag)

	// readRegisters always answers one element per offset; the guards keep a
	// buggy source from turning a short answer into a slice panic.
	statusCount := len(statusOffsets)
	statuses := make([]*uint32, 0)
	thermometers := make([]*uint32, 0)
	if len(first) >= statusCount {
		statuses = first[:statusCount]
		thermometers = first[statusCount:]
	}

	junction := hottestDecoded(thermometers, DecodeBlackwell)

	var vram *float64
	if offsets := gddr7TemperatureOffsets(statuses); len(offsets) > 0 {
		// The second batch is another BAR read: re-authorize and re-lease,
		// because the gate may have revoked this device while the first one
		// was in flight.
		vram = hottestDecoded(readRegisters(ctx, source, resourcePath, offsets,
			allowed, access, bdf, diag), DecodeGDDR7)
	}
	return SensorValues{JunctionTemperatureC: junction, VRAMTemperatureC: vram}
}

func valueAt(values []*uint32, index int) *uint32 {
	if index < 0 || index >= len(values) {
		return nil
	}
	return values[index]
}

func sampleDevice(ctx context.Context, source RegisterSource, deviceDir, bdf string, deviceID uint32,
	allowed func() bool, access *AccessArbiter, diag *sensorDiagnostics) (SensorValues, bool) {
	resourcePath := deviceDir + "/resource0"
	var values SensorValues
	if SelectSensorProfile(deviceID) == ProfileGDDR7 {
		values = sampleGDDR7(ctx, source, resourcePath, bdf, allowed, access, diag)
	} else {
		values = sampleGDDR6(ctx, source, resourcePath, bdf, allowed, access, diag)
	}
	// Unreadable registers stay absent: "no sensor" must not become a
	// permanent nil series, and machines without the ACL look untouched.
	if values.JunctionTemperatureC == nil && values.VRAMTemperatureC == nil {
		return SensorValues{}, false
	}
	return values, true
}

// ReadGpuRegisterTemps samples the NVIDIA display devices the scope permits.
// Devices the runtime-PM gate excluded are never opened or probed: their BAR
// may be powered down, and a read would resume them.
func ReadGpuRegisterTemps(ctx context.Context, source RegisterSource, sysfs SysFS, sysfsDir string,
	scope SampleScope) SensorMap {
	return readGpuRegisterTemps(ctx, source, sysfs, sysfsDir, scope, nil)
}

func readGpuRegisterTemps(ctx context.Context, source RegisterSource, sysfs SysFS, sysfsDir string,
	scope SampleScope, diag *sensorDiagnostics) SensorMap {
	if sysfsDir == "" {
		sysfsDir = DefaultDRMDevicesDir
	}
	devices := scopedDisplays(sysfs, sysfsDir, scope)
	sensors := make(SensorMap, len(devices))
	for _, device := range devices {
		if !scope.Allows(device.BDF) {
			continue
		}
		key, ok := NormalizeBusFunctionKey(device.BDF)
		if !ok {
			continue
		}
		allowed := func() bool { return scope.Allows(device.BDF) }
		values, ok := sampleDevice(ctx, source, sysfsDir+"/"+device.BDF, device.BDF, device.DeviceID,
			allowed, scope.access, diag)
		if !ok {
			continue
		}
		sensors[key] = values
	}
	return sensors
}

// canReadResource probes whether resource0 can be read at all. The
// distinction matters for diagnostics: "no permission" and "this card has no
// supported thermal registers" need different user action. The probe is a BAR
// access like any other, so it re-checks the current authorization and takes
// an access lease first.
func canReadResource(ctx context.Context, source RegisterSource, resourcePath, bdf string,
	allowed func() bool, access *AccessArbiter) (bool, error) {
	if !allowed() {
		return false, nil
	}
	if access != nil {
		lease, err := access.Acquire(ctx, bdf)
		if err != nil {
			return false, err
		}
		defer lease.Release()
	}
	values, err := source.ReadRegisters(ctx, resourcePath, []int64{0})
	if err != nil {
		return false, err
	}
	if len(values) != 1 {
		return false, errIncompleteRead
	}
	return values[0] != nil, nil
}

// --- poller ------------------------------------------------------------------

// TempsPollerOptions configures a [TempsPoller].
type TempsPollerOptions struct {
	// FS is the root the sysfs attributes are read from; nil means the host's
	// "/" filesystem. Register reads always go through [HelperSource] against
	// the host root, because BAR0 cannot be mapped through an fs.FS.
	FS fs.FS
	// Sysfs overrides the attribute reader (tests, or a caller that already
	// has one). When nil it is built from FS.
	Sysfs    SysFS
	Source   RegisterSource
	SysfsDir string
	// Timeout bounds one register batch, 1.5 s by default. It is the helper
	// child's hard deadline.
	Timeout time.Duration
	// Access is the shared per-device arbiter. nil leaves direct calls
	// unleased; a [Poller] binds its own arbiter when it owns this source.
	Access  *AccessArbiter
	OnEvent func(string)
}

// tempsState tracks the last diagnostic state so each line is logged once per
// change, never per cycle.
type tempsState string

const (
	tempsStateUnknown     tempsState = "unknown"
	tempsStateDevices     tempsState = "devices"
	tempsStateNone        tempsState = "none"
	tempsStateAccess      tempsState = "access"
	tempsStateUnsupported tempsState = "unsupported"
	tempsStateStuck       tempsState = "stuck"
	tempsStateDenied      tempsState = "denied"
	tempsStateFault       tempsState = "fault"
)

// SensorDiagnostics is the last distinct reason register sampling could not
// produce values. A stuck helper is deliberately separate from a permissions
// failure: the user action is to wait for the child to be reaped, not to
// change the udev rule.
type SensorDiagnostics struct {
	Reason  string
	Stuck   bool
	Denied  bool
	Faulted bool
}

// TempsPoller reads register temperatures on demand: [Poller] drives the
// cadence by calling PollOnce each cycle it already runs for nvidia-smi, so
// this poller owns no timer of its own.
type TempsPoller struct {
	sysfs    SysFS
	source   RegisterSource
	sysfsDir string
	access   *AccessArbiter
	onEvent  func(string)

	cycleMu  sync.Mutex
	inflight *tempsCycle

	mu          sync.Mutex
	sensors     SensorMap
	state       tempsState
	diagnostics SensorDiagnostics
}

type tempsCycle struct {
	done chan struct{}
}

// NewTempsPoller builds a register-temperature poller. The default source is
// the self-exec helper, so a hung BAR cannot stall or crash the collector.
func NewTempsPoller(options TempsPollerOptions) *TempsPoller {
	rootFS := options.FS
	if rootFS == nil {
		rootFS = os.DirFS("/")
	}
	source := options.Source
	if source == nil {
		source = HelperSource{Timeout: options.Timeout, Access: options.Access}
	}
	sysfsDir := options.SysfsDir
	if sysfsDir == "" {
		sysfsDir = DefaultDRMDevicesDir
	}
	var sysfs SysFS
	if options.Sysfs != nil {
		sysfs = options.Sysfs
	} else {
		sysfs = RootFS{FS: rootFS}
	}
	return &TempsPoller{
		sysfs:    sysfs,
		source:   source,
		sysfsDir: sysfsDir,
		access:   options.Access,
		onEvent:  options.OnEvent,
		sensors:  make(SensorMap),
		state:    tempsStateUnknown,
	}
}

// bindAccess attaches the poller-owned arbiter to this source after
// construction. A [HelperSource] keeps the lease until its child is reaped, so
// it must know about the shared arbiter rather than only the scope's.
func (p *TempsPoller) bindAccess(access *AccessArbiter) {
	if access == nil {
		return
	}
	p.access = access
	switch source := p.source.(type) {
	case HelperSource:
		source.Access = access
		p.source = source
	case *HelperSource:
		source.Access = access
	}
}

// Diagnostics returns the last distinct reason register sampling was
// unavailable, including the stuck-helper state.
func (p *TempsPoller) Diagnostics() SensorDiagnostics {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.diagnostics
}

// PollOnce samples the devices the scope permits once. Concurrent calls share
// the in-flight cycle, and a caller whose context is cancelled stops waiting
// without leaving the cycle behind: [Poller] owns the cycle and its cancel.
func (p *TempsPoller) PollOnce(ctx context.Context, scope SampleScope) error {
	p.cycleMu.Lock()
	if running := p.inflight; running != nil {
		p.cycleMu.Unlock()
		select {
		case <-running.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	running := &tempsCycle{done: make(chan struct{})}
	p.inflight = running
	p.cycleMu.Unlock()

	defer func() {
		p.cycleMu.Lock()
		p.inflight = nil
		p.cycleMu.Unlock()
		close(running.done)
	}()
	p.runCycle(ctx, scope)
	return nil
}

// Latest returns a copy of the last register readings.
func (p *TempsPoller) Latest() SensorMap {
	p.mu.Lock()
	defer p.mu.Unlock()
	return copySensorMap(p.sensors)
}

// HelperStuckReason is reported when the previous pci-read child has not been
// reaped. It is a distinct state, not a permissions failure: the helper slot
// stays held until the kernel releases the child.
const HelperStuckReason = "GPU register helper stuck (previous child not reaped)"

func (p *TempsPoller) runCycle(ctx context.Context, scope SampleScope) {
	// A sensor read is best effort: a surprise panic in a reader must not take
	// the collector down with it (the prototype swallowed reader throws too).
	// A panic makes the current readings untrustworthy, so they are dropped
	// rather than left behind as stale values.
	defer func() {
		if recovered := recover(); recovered != nil {
			p.mu.Lock()
			p.sensors = make(SensorMap)
			p.diagnostics = SensorDiagnostics{
				Reason:  "GPU register sampling failed; register values unavailable",
				Faulted: true,
			}
			p.mu.Unlock()
			p.emitOnStateChange(tempsStateFault,
				"gpu register temps: sampling failed; register values unavailable")
		}
	}()

	devices := scopedDisplays(p.sysfs, p.sysfsDir, scope)
	if scope.Gated() && len(devices) == 0 {
		// The runtime-PM gate excluded every device: drop the stale readings
		// and stay quiet. Reporting "no NVIDIA display devices" here would
		// blame the hardware for a deliberate decision.
		p.mu.Lock()
		p.sensors = make(SensorMap)
		p.diagnostics = SensorDiagnostics{Denied: true, Reason: AccessDeniedReason}
		p.mu.Unlock()
		return
	}

	diag := &sensorDiagnostics{}
	sensors := readGpuRegisterTemps(ctx, p.source, p.sysfs, p.sysfsDir, scope, diag)
	p.mu.Lock()
	p.sensors = sensors
	p.mu.Unlock()

	// A gate decision taken while the cycle was running can exclude every
	// device that discovery saw. Stay quiet rather than blaming the hardware
	// for a deliberate decision, and do not probe a device that is no longer
	// permitted.
	if len(sensors) == 0 && scope.Gated() && !anyPermitted(devices, scope) {
		p.mu.Lock()
		p.sensors = make(SensorMap)
		p.diagnostics = SensorDiagnostics{Denied: true, Reason: AccessDeniedReason}
		p.mu.Unlock()
		return
	}

	if len(sensors) > 0 {
		p.mu.Lock()
		p.diagnostics = SensorDiagnostics{}
		p.mu.Unlock()
		p.emitOnStateChange(tempsStateDevices, fmt.Sprintf("gpu register temps: %d device(s)", len(sensors)))
		return
	}

	// A stuck helper and an arbiter refusal are operationally different from
	// a permissions failure. Report them before the permission probe, which
	// would otherwise blame the udev rule for both.
	if diag.stuck {
		p.setDiagnostics(SensorDiagnostics{Reason: HelperStuckReason, Stuck: true})
		p.emitOnStateChange(tempsStateStuck,
			"gpu register temps: helper stuck (previous child not reaped); waiting for it to exit")
		return
	}
	if diag.denied {
		denied := diag.deniedDiagnostics()
		p.setDiagnostics(denied)
		p.emitOnStateChange(tempsStateDenied,
			fmt.Sprintf("gpu register temps: %s", denied.Reason))
		return
	}

	// Nothing decoded: distinguish "no NVIDIA GPU", "no permission" and a
	// readable device whose thermal registers are unsupported. Probe only the
	// devices this cycle was allowed to touch, so a suspended GPU is never
	// touched just to explain a diagnostic.
	if len(devices) == 0 {
		p.mu.Lock()
		p.diagnostics = SensorDiagnostics{Reason: "no NVIDIA display devices"}
		p.mu.Unlock()
		p.emitOnStateChange(tempsStateNone, "gpu register temps: no NVIDIA display devices")
		return
	}
	for _, device := range devices {
		if !scope.Allows(device.BDF) {
			continue
		}
		allowed := func() bool { return scope.Allows(device.BDF) }
		readable, err := canReadResource(ctx, p.source,
			p.sysfsDir+"/"+device.BDF+"/resource0", device.BDF, allowed, p.access)
		if err != nil {
			diag.observe(err)
			if diag.stuck {
				p.setDiagnostics(SensorDiagnostics{Reason: HelperStuckReason, Stuck: true})
				p.emitOnStateChange(tempsStateStuck,
					"gpu register temps: helper stuck (previous child not reaped); waiting for it to exit")
				return
			}
			if diag.denied {
				denied := diag.deniedDiagnostics()
				p.setDiagnostics(denied)
				p.emitOnStateChange(tempsStateDenied,
					fmt.Sprintf("gpu register temps: %s", denied.Reason))
				return
			}
		}
		if readable {
			p.mu.Lock()
			p.diagnostics = SensorDiagnostics{Reason: "no supported thermal registers"}
			p.mu.Unlock()
			p.emitOnStateChange(tempsStateUnsupported,
				fmt.Sprintf("gpu register temps: no supported thermal registers on %d device(s)", len(devices)))
			return
		}
	}
	p.mu.Lock()
	p.diagnostics = SensorDiagnostics{Reason: "GPU BAR0 mapping failed (needs read access)"}
	p.mu.Unlock()
	p.emitOnStateChange(tempsStateAccess, "gpu register temps: GPU BAR0 mapping failed (needs read access); "+
		"run rigscope gpu-temps status. If access was granted after this collector started, it applies after a reboot")
}

// setDiagnostics publishes the last distinct unavailability reason.
func (p *TempsPoller) setDiagnostics(diagnostics SensorDiagnostics) {
	p.mu.Lock()
	p.diagnostics = diagnostics
	p.mu.Unlock()
}

// anyPermitted reports whether the current authorization still allows at least
// one of the discovered devices.
func anyPermitted(devices []NVDisplayDevice, scope SampleScope) bool {
	for _, device := range devices {
		if scope.Allows(device.BDF) {
			return true
		}
	}
	return false
}

// emitOnStateChange logs the line only when the cycle's state differs from
// the last one.
func (p *TempsPoller) emitOnStateChange(next tempsState, line string) {
	p.mu.Lock()
	changed := p.state != next
	p.state = next
	p.mu.Unlock()
	if !changed || p.onEvent == nil {
		return
	}
	defer func() { _ = recover() }()
	p.onEvent(line)
}

// TempsEnabled reports whether register temperatures are enabled. The kill
// switch is COLLECTOR_GPU_TEMPS=off.
func TempsEnabled(getenv func(string) string) bool {
	if getenv == nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(getenv(EnvGPUTemps))) != "off"
}
