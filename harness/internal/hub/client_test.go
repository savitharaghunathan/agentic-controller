package hub

import "testing"

func TestParseAppID(t *testing.T) {
	tests := []struct {
		input   string
		want    uint
		wantErr bool
	}{
		{"42", 42, false},
		{"0", 0, false},
		{"1000000", 1000000, false},
		{"", 0, true},
		{"abc", 0, true},
		{"-1", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseAppID(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseAppID(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ParseAppID(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
