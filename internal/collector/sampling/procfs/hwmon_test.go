package procfs

import (
	"testing"
	"time"
)

func TestReadCPUTemperature(t *testing.T) {
	src := mapSource(map[string]string{
		"/sys/class/hwmon/hwmon0/name":        "nvme\n",
		"/sys/class/hwmon/hwmon0/temp1_label": "Composite\n",
		"/sys/class/hwmon/hwmon0/temp1_input": "90000\n",
		"/sys/class/hwmon/hwmon1/name":        "k10temp\n",
		"/sys/class/hwmon/hwmon1/temp1_label": "Tctl\n",
		"/sys/class/hwmon/hwmon1/temp1_input": "61500\n",
		"/sys/class/hwmon/hwmon1/temp3_label": "Tccd1\n",
		"/sys/class/hwmon/hwmon1/temp3_input": "70000\n",
		"/sys/class/hwmon/hwmon2/name":        "coretemp\n",
		"/sys/class/hwmon/hwmon2/temp1_label": "Package id 0\n",
		"/sys/class/hwmon/hwmon2/temp1_input": "60000\n",
		"/sys/class/hwmon/hwmon2/temp2_label": "Core 0\n",
		"/sys/class/hwmon/hwmon2/temp2_input": "80000\n",
	}, nil)
	// Package/Tctl beats core sensors; the hottest package/Tctl wins.
	if got := ReadCPUTemperature(src); got == nil || *got != 61.5 {
		t.Fatalf("temperature = %v, want 61.5", got)
	}

	unrelated := mapSource(map[string]string{
		"/sys/class/hwmon/hwmon0/name":        "bnxt_en\n",
		"/sys/class/hwmon/hwmon0/temp1_input": "95000\n",
	}, nil)
	if got := ReadCPUTemperature(unrelated); got != nil {
		t.Fatalf("unrelated driver temperature = %v, want nil", got)
	}

	noSensors := mapSource(map[string]string{
		"/sys/class/hwmon/hwmon0/name": "k10temp\n",
	}, nil)
	if got := ReadCPUTemperature(noSensors); got != nil {
		t.Fatalf("sensorless temperature = %v, want nil", got)
	}

	missingRoot := mapSource(nil, nil)
	if got := ReadCPUTemperature(missingRoot); got != nil {
		t.Fatalf("missing hwmon temperature = %v, want nil", got)
	}

	emptySensor := mapSource(map[string]string{
		"/sys/class/hwmon/hwmon0/name":        "k10temp\n",
		"/sys/class/hwmon/hwmon0/temp1_input": "\n",
	}, nil)
	if got := ReadCPUTemperature(emptySensor); got != nil {
		t.Fatalf("empty sensor temperature = %v, want nil (never zero degrees)", got)
	}

	outOfRange := mapSource(map[string]string{
		"/sys/class/hwmon/hwmon0/name":        "k10temp\n",
		"/sys/class/hwmon/hwmon0/temp1_input": "900000\n",
	}, nil)
	if got := ReadCPUTemperature(outOfRange); got != nil {
		t.Fatalf("implausible temperature = %v, want nil", got)
	}
}

func TestReadCPUTemperatureRoundsNegativeHalfStepsLikeJavaScript(t *testing.T) {
	cases := []struct {
		millidegrees string
		want         float64
	}{
		// -5 millidegrees is -0.005 °C; JS Math.round(-0.5) is 0, while Go's
		// math.Round would give -1 (−0.01 °C).
		{"-5\n", 0},
		{"-15\n", -0.01},
		{"-4\n", 0},
		{"-6\n", -0.01},
	}
	for _, tc := range cases {
		src := mapSource(map[string]string{
			"/sys/class/hwmon/hwmon0/name":        "k10temp\n",
			"/sys/class/hwmon/hwmon0/temp1_input": tc.millidegrees,
		}, nil)
		got := ReadCPUTemperature(src)
		if got == nil || *got != tc.want {
			t.Fatalf("temperature at %q = %v, want %v", tc.millidegrees, got, tc.want)
		}
	}
}

func TestRound2MatchesJavaScriptMathRound(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0.5, 1},
		{-0.5, 0},
		{1.5, 2},
		{-1.5, -1},
		{2.5, 3},
		{-2.5, -2},
	}
	for _, tc := range cases {
		if got := jsRound(tc.in); got != tc.want {
			t.Errorf("jsRound(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := round2(-0.005); got != 0 {
		t.Errorf("round2(-0.005) = %v, want 0", got)
	}
	if got := round2(-0.015); got != -0.01 {
		t.Errorf("round2(-0.015) = %v, want -0.01", got)
	}
}

func TestRaplPowerMeter(t *testing.T) {
	base := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	files := map[string]string{
		"/sys/class/powercap/intel-rapl:0/energy_uj":           "1000000\n",
		"/sys/class/powercap/intel-rapl:0/max_energy_range_uj": "10000000\n",
	}
	src := dynamicSource{files: files, dirs: map[string][]string{
		"/sys/class/powercap": {"intel-rapl", "intel-rapl:0", "intel-rapl:0:0"},
	}}
	meter := NewRaplPowerMeter(src)
	if got := meter.Sample(base, true); got != nil {
		t.Fatalf("first interval = %v, want nil", got)
	}
	files["/sys/class/powercap/intel-rapl:0/energy_uj"] = "3000000\n"
	if got := meter.Sample(base.Add(time.Second), true); got == nil || *got != 2 {
		t.Fatalf("2 J over 1 s = %v, want 2 W", got)
	}
	files["/sys/class/powercap/intel-rapl:0/energy_uj"] = "3500000\n"
	if got := meter.Sample(base.Add(2*time.Second), true); got == nil || *got != 0.5 {
		t.Fatalf("500 mJ over 1 s = %v, want 0.5 W", got)
	}
	if got := meter.Sample(base.Add(2*time.Second), true); got != nil {
		t.Fatalf("same timestamp = %v, want nil", got)
	}
	if got := meter.Sample(base.Add(3*time.Second), false); got != nil {
		t.Fatalf("missing clock = %v, want nil", got)
	}
}

func TestRaplPowerMeterWrapAndReset(t *testing.T) {
	base := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	wrapFiles := map[string]string{
		"/sys/class/powercap/intel-rapl:0/energy_uj":           "9500000\n",
		"/sys/class/powercap/intel-rapl:0/max_energy_range_uj": "10000000\n",
	}
	wrapSrc := dynamicSource{files: wrapFiles, dirs: map[string][]string{"/sys/class/powercap": {"intel-rapl:0"}}}
	wrapMeter := NewRaplPowerMeter(wrapSrc)
	if got := wrapMeter.Sample(base, true); got != nil {
		t.Fatalf("wrap baseline = %v, want nil", got)
	}
	wrapFiles["/sys/class/powercap/intel-rapl:0/energy_uj"] = "500000\n"
	if got := wrapMeter.Sample(base.Add(time.Second), true); got == nil || *got != 1 {
		t.Fatalf("(10M - 9.5M) + 0.5M over 1 s = %v, want 1 W", got)
	}

	resetFiles := map[string]string{
		"/sys/class/powercap/intel-rapl:0/energy_uj":           "200000\n",
		"/sys/class/powercap/intel-rapl:0/max_energy_range_uj": "10000000\n",
	}
	resetSrc := dynamicSource{files: resetFiles, dirs: map[string][]string{"/sys/class/powercap": {"intel-rapl:0"}}}
	resetMeter := NewRaplPowerMeter(resetSrc)
	if got := resetMeter.Sample(base, true); got != nil {
		t.Fatalf("reset baseline = %v, want nil", got)
	}
	resetFiles["/sys/class/powercap/intel-rapl:0/energy_uj"] = "0\n"
	if got := resetMeter.Sample(base.Add(time.Second), true); got != nil {
		t.Fatalf("reset = %v, want nil (not a huge wrap)", got)
	}
	resetFiles["/sys/class/powercap/intel-rapl:0/energy_uj"] = "500000\n"
	if got := resetMeter.Sample(base.Add(2*time.Second), true); got == nil || *got != 0.5 {
		t.Fatalf("baseline after reset = %v, want 0.5 W", got)
	}
}

func TestRaplPowerMeterSumsPackagesAndIgnoresSubdomains(t *testing.T) {
	base := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	files := map[string]string{
		"/sys/class/powercap/intel-rapl:0/energy_uj":   "0\n",
		"/sys/class/powercap/intel-rapl:1/energy_uj":   "0\n",
		"/sys/class/powercap/intel-rapl:0:0/energy_uj": "0\n",
	}
	src := dynamicSource{files: files, dirs: map[string][]string{
		"/sys/class/powercap": {"intel-rapl:0", "intel-rapl:1", "intel-rapl:0:0"},
	}}
	meter := NewRaplPowerMeter(src)
	if got := meter.Sample(base, true); got != nil {
		t.Fatalf("first interval = %v, want nil", got)
	}
	files["/sys/class/powercap/intel-rapl:0/energy_uj"] = "2000000\n"
	files["/sys/class/powercap/intel-rapl:1/energy_uj"] = "1000000\n"
	files["/sys/class/powercap/intel-rapl:0:0/energy_uj"] = "5000000\n"
	if got := meter.Sample(base.Add(time.Second), true); got == nil || *got != 3 {
		t.Fatalf("sum = %v, want 3 W", got)
	}

	unreadable := mapSource(nil, map[string][]string{"/sys/class/powercap": {"intel-rapl:0"}})
	noEnergy := NewRaplPowerMeter(unreadable)
	if got := noEnergy.Sample(base, true); got != nil {
		t.Fatalf("unreadable energy = %v, want nil", got)
	}
	if got := noEnergy.Sample(base.Add(time.Second), true); got != nil {
		t.Fatalf("unreadable energy second = %v, want nil", got)
	}

	noDomains := mapSource(nil, map[string][]string{"/sys/class/powercap": {}})
	if got := NewRaplPowerMeter(noDomains).Sample(base, true); got != nil {
		t.Fatalf("no domains = %v, want nil", got)
	}
}
