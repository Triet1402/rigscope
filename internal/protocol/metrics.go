package protocol

import (
	"encoding/json"
	"errors"
)

// Unavailable is the shared {status:"unavailable", reason} metric variant. It
// is used by every metric family: a family that is unsupported or failed to
// collect carries a reason and no readings.
type Unavailable struct {
	Reason string
}

// MarshalJSON always emits the unavailable variant.
func (u Unavailable) MarshalJSON() ([]byte, error) {
	type plain struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	return json.Marshal(plain{string(MetricStatusUnavailable), u.Reason})
}

// UnmarshalJSON accepts only the unavailable variant.
func (u *Unavailable) UnmarshalJSON(data []byte) error {
	v, err := decodeJSONValue(data)
	if err != nil {
		return err
	}
	p := newParser()
	o, err := p.object(v)
	if err != nil {
		return err
	}
	if err := p.keys(o, []string{"status", "reason"}, nil); err != nil {
		return err
	}
	if _, err := p.child("status").oneOf(o["status"], string(MetricStatusUnavailable)); err != nil {
		return err
	}
	reason, err := p.child("reason").text(o["reason"], 0, MaxTextLength)
	if err != nil {
		return err
	}
	u.Reason = reason
	return nil
}

// ResourceUnavailable is the unavailable variant of a resource reading. The
// stable resource id is present on both variants.
type ResourceUnavailable struct {
	ID     string
	Reason string
}

// MarshalJSON always emits the unavailable variant.
func (r ResourceUnavailable) MarshalJSON() ([]byte, error) {
	type plain struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	return json.Marshal(plain{r.ID, string(MetricStatusUnavailable), r.Reason})
}

// UnmarshalJSON accepts only the unavailable resource variant.
func (r *ResourceUnavailable) UnmarshalJSON(data []byte) error {
	v, err := decodeJSONValue(data)
	if err != nil {
		return err
	}
	p := newParser()
	o, err := p.object(v)
	if err != nil {
		return err
	}
	if err := p.keys(o, []string{"id", "status", "reason"}, nil); err != nil {
		return err
	}
	id, err := p.child("id").resourceID(o["id"])
	if err != nil {
		return err
	}
	if _, err := p.child("status").oneOf(o["status"], string(MetricStatusUnavailable)); err != nil {
		return err
	}
	reason, err := p.child("reason").text(o["reason"], 0, MaxTextLength)
	if err != nil {
		return err
	}
	r.ID, r.Reason = id, reason
	return nil
}

// CpuMetric is available or unavailable CPU usage.
type CpuMetric interface{ isCpuMetric() }

// CpuAvailable is a readable CPU reading.
type CpuAvailable struct {
	Status              MetricStatus       `json:"status"`
	UsagePercent        float64            `json:"usagePercent"`
	PerCore             []*float64         `json:"perCore"`
	TemperatureC        *Optional[float64] `json:"temperatureC,omitempty"`
	PowerWatts          *Optional[float64] `json:"powerWatts,omitempty"`
	IowaitPercent       *Optional[float64] `json:"iowaitPercent,omitempty"`
	PressureSomePercent *Optional[float64] `json:"pressureSomePercent,omitempty"`
}

func (CpuAvailable) isCpuMetric() {}
func (Unavailable) isCpuMetric()  {}

// MarshalJSON forces status "ok" and keeps perCore an array.
func (c CpuAvailable) MarshalJSON() ([]byte, error) {
	c.Status = MetricStatusOK
	if c.PerCore == nil {
		c.PerCore = []*float64{}
	}
	type plain CpuAvailable
	return json.Marshal(plain(c))
}

// UnmarshalJSON accepts only an available CPU reading.
func (c *CpuAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseCpuMetric)
	if err != nil {
		return err
	}
	v, ok := out.(CpuAvailable)
	if !ok {
		return errors.New("protocol: expected an available cpu reading")
	}
	*c = v
	return nil
}

// BytesUsageMetric is available or unavailable used/total bytes (swap).
type BytesUsageMetric interface{ isBytesUsageMetric() }

// BytesUsageAvailable is a readable used/total bytes reading.
type BytesUsageAvailable struct {
	Status     MetricStatus `json:"status"`
	UsedBytes  int64        `json:"usedBytes"`
	TotalBytes int64        `json:"totalBytes"`
}

func (BytesUsageAvailable) isBytesUsageMetric() {}
func (Unavailable) isBytesUsageMetric()         {}

// MarshalJSON forces status "ok".
func (b BytesUsageAvailable) MarshalJSON() ([]byte, error) {
	b.Status = MetricStatusOK
	type plain BytesUsageAvailable
	return json.Marshal(plain(b))
}

// UnmarshalJSON accepts only an available usage reading.
func (b *BytesUsageAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseSwapMetric)
	if err != nil {
		return err
	}
	v, ok := out.(BytesUsageAvailable)
	if !ok {
		return errors.New("protocol: expected an available usage reading")
	}
	*b = v
	return nil
}

// MemoryMetric is available or unavailable memory usage.
type MemoryMetric interface{ isMemoryMetric() }

// MemoryAvailable is a readable memory reading with optional PSI pressure.
type MemoryAvailable struct {
	Status              MetricStatus       `json:"status"`
	UsedBytes           int64              `json:"usedBytes"`
	TotalBytes          int64              `json:"totalBytes"`
	PressureSomePercent *Optional[float64] `json:"pressureSomePercent,omitempty"`
	PressureFullPercent *Optional[float64] `json:"pressureFullPercent,omitempty"`
}

func (MemoryAvailable) isMemoryMetric() {}
func (Unavailable) isMemoryMetric()     {}

// MarshalJSON forces status "ok".
func (m MemoryAvailable) MarshalJSON() ([]byte, error) {
	m.Status = MetricStatusOK
	type plain MemoryAvailable
	return json.Marshal(plain(m))
}

// UnmarshalJSON accepts only an available memory reading.
func (m *MemoryAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseMemoryMetric)
	if err != nil {
		return err
	}
	v, ok := out.(MemoryAvailable)
	if !ok {
		return errors.New("protocol: expected an available memory reading")
	}
	*m = v
	return nil
}

// GpuReading is available or unavailable telemetry for one GPU.
type GpuReading interface{ isGpuReading() }

// GpuAvailable is a readable GPU reading. Nullable fields are null when the
// driver does not expose them.
type GpuAvailable struct {
	Status               MetricStatus       `json:"status"`
	ID                   string             `json:"id"`
	UtilizationPercent   *float64           `json:"utilizationPercent"`
	MemoryUsedBytes      *int64             `json:"memoryUsedBytes"`
	MemoryTotalBytes     *int64             `json:"memoryTotalBytes"`
	TemperatureC         *float64           `json:"temperatureC"`
	PowerWatts           *float64           `json:"powerWatts"`
	JunctionTemperatureC *Optional[float64] `json:"junctionTemperatureC,omitempty"`
	VramTemperatureC     *Optional[float64] `json:"vramTemperatureC,omitempty"`
}

func (GpuAvailable) isGpuReading()        {}
func (ResourceUnavailable) isGpuReading() {}

// MarshalJSON forces status "ok".
func (g GpuAvailable) MarshalJSON() ([]byte, error) {
	g.Status = MetricStatusOK
	type plain GpuAvailable
	return json.Marshal(plain(g))
}

// UnmarshalJSON accepts only an available GPU reading.
func (g *GpuAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseGpuReading)
	if err != nil {
		return err
	}
	v, ok := out.(GpuAvailable)
	if !ok {
		return errors.New("protocol: expected an available gpu reading")
	}
	*g = v
	return nil
}

// GpusMetric is available or unavailable GPU telemetry.
type GpusMetric interface{ isGpusMetric() }

// GpusAvailable is the readable GPU list.
type GpusAvailable struct {
	Status  MetricStatus `json:"status"`
	Devices []GpuReading `json:"devices"`
}

func (GpusAvailable) isGpusMetric() {}
func (Unavailable) isGpusMetric()   {}

// MarshalJSON forces status "ok" and keeps devices an array. It rejects nil and
// typed-nil device members instead of emitting JSON null.
func (g GpusAvailable) MarshalJSON() ([]byte, error) {
	g.Status = MetricStatusOK
	if g.Devices == nil {
		g.Devices = []GpuReading{}
	}
	if err := checkNoNilElements("gpus.devices", g.Devices); err != nil {
		return nil, err
	}
	type plain GpusAvailable
	return json.Marshal(plain(g))
}

// UnmarshalJSON accepts only an available GPU list.
func (g *GpusAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseGpusMetric)
	if err != nil {
		return err
	}
	v, ok := out.(GpusAvailable)
	if !ok {
		return errors.New("protocol: expected an available gpus reading")
	}
	*g = v
	return nil
}

// FilesystemReading is available or unavailable usage for one filesystem.
type FilesystemReading interface{ isFilesystemReading() }

// FilesystemAvailable is a readable filesystem usage reading.
type FilesystemAvailable struct {
	Status      MetricStatus     `json:"status"`
	ID          string           `json:"id"`
	UsedBytes   int64            `json:"usedBytes"`
	TotalBytes  int64            `json:"totalBytes"`
	InodesUsed  *Optional[int64] `json:"inodesUsed,omitempty"`
	InodesTotal *Optional[int64] `json:"inodesTotal,omitempty"`
}

func (FilesystemAvailable) isFilesystemReading() {}
func (ResourceUnavailable) isFilesystemReading() {}

// MarshalJSON forces status "ok".
func (f FilesystemAvailable) MarshalJSON() ([]byte, error) {
	f.Status = MetricStatusOK
	type plain FilesystemAvailable
	return json.Marshal(plain(f))
}

// UnmarshalJSON accepts only an available filesystem reading.
func (f *FilesystemAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseFilesystemReading)
	if err != nil {
		return err
	}
	v, ok := out.(FilesystemAvailable)
	if !ok {
		return errors.New("protocol: expected an available filesystem reading")
	}
	*f = v
	return nil
}

// DiskIoReading is available or unavailable I/O for one block device.
type DiskIoReading interface{ isDiskIoReading() }

// DiskIoAvailable is a readable per-device I/O rate reading.
type DiskIoAvailable struct {
	Status               MetricStatus       `json:"status"`
	ID                   string             `json:"id"`
	ReadBytesPerSec      float64            `json:"readBytesPerSec"`
	WriteBytesPerSec     float64            `json:"writeBytesPerSec"`
	ReadLatencyMs        *Optional[float64] `json:"readLatencyMs,omitempty"`
	WriteLatencyMs       *Optional[float64] `json:"writeLatencyMs,omitempty"`
	IoUtilizationPercent *Optional[float64] `json:"ioUtilizationPercent,omitempty"`
}

func (DiskIoAvailable) isDiskIoReading()     {}
func (ResourceUnavailable) isDiskIoReading() {}

// MarshalJSON forces status "ok".
func (d DiskIoAvailable) MarshalJSON() ([]byte, error) {
	d.Status = MetricStatusOK
	type plain DiskIoAvailable
	return json.Marshal(plain(d))
}

// UnmarshalJSON accepts only an available device reading.
func (d *DiskIoAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseDiskIoReading)
	if err != nil {
		return err
	}
	v, ok := out.(DiskIoAvailable)
	if !ok {
		return errors.New("protocol: expected an available disk reading")
	}
	*d = v
	return nil
}

// DisksMetric is available or unavailable disk telemetry.
type DisksMetric interface{ isDisksMetric() }

// DisksAvailable is the readable filesystem and device lists with optional PSI.
type DisksAvailable struct {
	Status              MetricStatus        `json:"status"`
	Filesystems         []FilesystemReading `json:"filesystems"`
	Devices             []DiskIoReading     `json:"devices"`
	PressureSomePercent *Optional[float64]  `json:"pressureSomePercent,omitempty"`
	PressureFullPercent *Optional[float64]  `json:"pressureFullPercent,omitempty"`
}

func (DisksAvailable) isDisksMetric() {}
func (Unavailable) isDisksMetric()    {}

// MarshalJSON forces status "ok" and keeps both lists arrays. It rejects nil and
// typed-nil members instead of emitting JSON null.
func (d DisksAvailable) MarshalJSON() ([]byte, error) {
	d.Status = MetricStatusOK
	if d.Filesystems == nil {
		d.Filesystems = []FilesystemReading{}
	}
	if d.Devices == nil {
		d.Devices = []DiskIoReading{}
	}
	if err := checkNoNilElements("disks.filesystems", d.Filesystems); err != nil {
		return nil, err
	}
	if err := checkNoNilElements("disks.devices", d.Devices); err != nil {
		return nil, err
	}
	type plain DisksAvailable
	return json.Marshal(plain(d))
}

// UnmarshalJSON accepts only an available disk payload.
func (d *DisksAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseDisksMetric)
	if err != nil {
		return err
	}
	v, ok := out.(DisksAvailable)
	if !ok {
		return errors.New("protocol: expected an available disks reading")
	}
	*d = v
	return nil
}

// InterfaceReading is available or unavailable I/O for one interface.
type InterfaceReading interface{ isInterfaceReading() }

// InterfaceAvailable is a readable per-interface rate reading.
type InterfaceAvailable struct {
	Status        MetricStatus `json:"status"`
	ID            string       `json:"id"`
	RxBytesPerSec float64      `json:"rxBytesPerSec"`
	TxBytesPerSec float64      `json:"txBytesPerSec"`
}

func (InterfaceAvailable) isInterfaceReading()  {}
func (ResourceUnavailable) isInterfaceReading() {}

// MarshalJSON forces status "ok".
func (i InterfaceAvailable) MarshalJSON() ([]byte, error) {
	i.Status = MetricStatusOK
	type plain InterfaceAvailable
	return json.Marshal(plain(i))
}

// UnmarshalJSON accepts only an available interface reading.
func (i *InterfaceAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseInterfaceReading)
	if err != nil {
		return err
	}
	v, ok := out.(InterfaceAvailable)
	if !ok {
		return errors.New("protocol: expected an available interface reading")
	}
	*i = v
	return nil
}

// NetworkMetric is available or unavailable network telemetry.
type NetworkMetric interface{ isNetworkMetric() }

// NetworkAvailable is the readable interface list.
type NetworkAvailable struct {
	Status     MetricStatus       `json:"status"`
	Interfaces []InterfaceReading `json:"interfaces"`
}

func (NetworkAvailable) isNetworkMetric() {}
func (Unavailable) isNetworkMetric()      {}

// MarshalJSON forces status "ok" and keeps interfaces an array. It rejects nil
// and typed-nil members instead of emitting JSON null.
func (n NetworkAvailable) MarshalJSON() ([]byte, error) {
	n.Status = MetricStatusOK
	if n.Interfaces == nil {
		n.Interfaces = []InterfaceReading{}
	}
	if err := checkNoNilElements("network.interfaces", n.Interfaces); err != nil {
		return nil, err
	}
	type plain NetworkAvailable
	return json.Marshal(plain(n))
}

// UnmarshalJSON accepts only an available network payload.
func (n *NetworkAvailable) UnmarshalJSON(data []byte) error {
	out, err := unmarshalMetric(data, (*parser).parseNetworkMetric)
	if err != nil {
		return err
	}
	v, ok := out.(NetworkAvailable)
	if !ok {
		return errors.New("protocol: expected an available network reading")
	}
	*n = v
	return nil
}

// MetricsPayload is the canonical full metrics field set. Every family is
// present; unavailable families carry no values.
type MetricsPayload struct {
	CPU     CpuMetric        `json:"cpu"`
	Memory  MemoryMetric     `json:"memory"`
	Swap    BytesUsageMetric `json:"swap"`
	GPUs    GpusMetric       `json:"gpus"`
	Disks   DisksMetric      `json:"disks"`
	Network NetworkMetric    `json:"network"`
}

// MarshalJSON refuses a payload with a missing family instead of emitting null,
// which the wire contract forbids. A typed-nil family (for example a nil
// *CpuAvailable stored in the interface) is treated as missing.
func (m MetricsPayload) MarshalJSON() ([]byte, error) {
	switch {
	case isNilValue(m.CPU):
		return nil, errors.New("protocol: metrics payload is missing cpu")
	case isNilValue(m.Memory):
		return nil, errors.New("protocol: metrics payload is missing memory")
	case isNilValue(m.Swap):
		return nil, errors.New("protocol: metrics payload is missing swap")
	case isNilValue(m.GPUs):
		return nil, errors.New("protocol: metrics payload is missing gpus")
	case isNilValue(m.Disks):
		return nil, errors.New("protocol: metrics payload is missing disks")
	case isNilValue(m.Network):
		return nil, errors.New("protocol: metrics payload is missing network")
	}
	type plain MetricsPayload
	return json.Marshal(plain(m))
}

// unmarshalMetric decodes a standalone metric value and converts its result.
func unmarshalMetric[T any](data []byte, parse func(*parser, any) (T, error)) (any, error) {
	v, err := decodeJSONValue(data)
	if err != nil {
		return nil, err
	}
	return parse(newParser(), v)
}
