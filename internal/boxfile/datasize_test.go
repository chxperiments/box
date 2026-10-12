package boxfile

import "testing"

func TestDataBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"": 0, "64M": 64 << 20, "512M": 512 << 20, "10G": 10 << 30, "10g": 10 << 30, "1T": 1 << 40,
		" 2G ": 2 << 30, "67108864": 64 << 20,
	} {
		got, err := Spec{DataSize: in}.DataBytes()
		if err != nil || got != want {
			t.Errorf("%q: %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"10X", "G", "-1G", "0", "1M", "63M", "ten", "99999999999T"} {
		if _, err := (Spec{DataSize: bad}).DataBytes(); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
