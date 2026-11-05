package container

import "testing"

func TestParseSizeReadsDockersUnits(t *testing.T) {
	for in, want := range map[string]int64{
		"0B": 0, "512B": 512, "1.5kB": 1500, "69.21MB": 69210000, "5.2GB": 5200000000,
		"1GiB": 1 << 30, "2.5 MiB": 2621440, "N/A": -1, "": -1, "12XB": -1,
	} {
		if got := ParseSize(in); got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
