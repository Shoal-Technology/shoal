package resource

import "testing"

func TestParseCPU(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "200m", want: 200},
		{in: "0m", want: 0},
		{in: "1", want: 1000},
		{in: "0.5", want: 500},
		{in: ".25", want: 250},
		{in: "1.125", want: 1125},
		{in: " 2 ", want: 2000},
		{in: "", wantErr: true},
		{in: "1.2345", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "-100m", wantErr: true},
		{in: "1.5m", wantErr: true},
		{in: "abc", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseCPU(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseCPU(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseCPU(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseMemory(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1024", want: 1024},
		{in: "128Mi", want: 128 << 20},
		{in: "1.5Gi", want: 3 << 29},
		{in: "64Ki", want: 64 << 10},
		{in: "500M", want: 500_000_000},
		{in: "2G", want: 2_000_000_000},
		{in: "1k", want: 1000},
		{in: "", wantErr: true},
		{in: "-1Mi", wantErr: true},
		{in: "12XB", wantErr: true},
		{in: "Mi", wantErr: true},
		{in: "99999999Ti", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseMemory(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseMemory(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseMemory(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
