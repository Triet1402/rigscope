package procfs

import (
	"testing"
)

func TestParsePressure(t *testing.T) {
	both, err := ParsePressure([]byte("some avg10=4.25 avg60=2.00 avg300=1.00 total=123\nfull avg10=1.50 avg60=0.50 avg300=0.25 total=45"))
	if err != nil {
		t.Fatalf("ParsePressure: %v", err)
	}
	if both.SomeAvg10 == nil || *both.SomeAvg10 != 4.25 || both.FullAvg10 == nil || *both.FullAvg10 != 1.5 {
		t.Fatalf("both = %+v", both)
	}

	cpu, err := ParsePressure([]byte("some avg10=0.00 avg60=0.00 avg300=0.00 total=0"))
	if err != nil {
		t.Fatalf("ParsePressure cpu: %v", err)
	}
	if cpu.SomeAvg10 == nil || *cpu.SomeAvg10 != 0 || cpu.FullAvg10 != nil {
		t.Fatalf("cpu = %+v", cpu)
	}

	for name, text := range map[string]string{
		"missing avg10": "some avg60=1.00 total=1",
		"nan":           "some avg10=nan total=1",
		"above 100":     "some avg10=101 total=1",
	} {
		t.Run(name, func(t *testing.T) {
			pressure, err := ParsePressure([]byte(text))
			if err != nil {
				t.Fatalf("ParsePressure: %v", err)
			}
			if pressure.SomeAvg10 != nil || pressure.FullAvg10 != nil {
				t.Fatalf("pressure = %+v, want nil values", pressure)
			}
		})
	}

	if _, err := ParsePressure(nil); err == nil {
		t.Fatal("expected an error for an empty file")
	}
	if _, err := ParsePressure([]byte("garbage")); err == nil {
		t.Fatal("expected an error for an unrecognised file")
	}
}
