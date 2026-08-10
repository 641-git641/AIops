package infra

import "testing"

func TestParsePercent(t *testing.T) {
	cases := map[string]float64{"0.08%": 0.08, "100.00%": 100, "0%": 0, " 12.5% ": 12.5, "bad": 0}
	for in, want := range cases {
		if got := parsePercent(in); got != want {
			t.Errorf("parsePercent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseMemUsedMB(t *testing.T) {
	cases := map[string]float64{
		"170.8MiB / 15.48GiB": 170.8,
		"1.2GiB / 8GiB":       1.2 * 1024,
		"512KiB / 1GiB":       0.5,
		"256MB / 1GB":         256,
		"bad":                 0,
	}
	for in, want := range cases {
		if got := parseMemUsedMB(in); got != want {
			t.Errorf("parseMemUsedMB(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseInt(t *testing.T) {
	if parseInt("8") != 8 || parseInt("x") != 0 {
		t.Fatal("parseInt wrong")
	}
}
