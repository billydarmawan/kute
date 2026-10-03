package kube

import "testing"

func TestParseNodeDiskSummary(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    NodeDisk
		wantErr bool
	}{
		{
			name: "node fs present",
			raw:  `{"node":{"nodeName":"n1","fs":{"availableBytes":10,"capacityBytes":100,"usedBytes":42}},"pods":[{"podRef":{"name":"p"}}]}`,
			want: NewNodeDisk(90, 100, 10, 0, 0),
		},
		{
			// usedBytes (statfs total−free) leaves out the 5 root-reserved
			// blocks; capacity−available is what nodefs.available tracks.
			name: "root-reserved blocks count as used",
			raw:  `{"node":{"fs":{"availableBytes":15,"capacityBytes":100,"usedBytes":80}}}`,
			want: NewNodeDisk(85, 100, 15, 0, 0),
		},
		{
			name: "usedBytes is the fallback without available",
			raw:  `{"node":{"fs":{"capacityBytes":100,"usedBytes":42}}}`,
			want: NewNodeDisk(42, 100, 0, 0, 0),
		},
		{
			name: "imagefs on its own disk",
			raw:  `{"node":{"fs":{"availableBytes":60,"capacityBytes":100,"usedBytes":40},"runtime":{"imageFs":{"availableBytes":50,"capacityBytes":200,"usedBytes":120}}}}`,
			want: NewNodeDisk(40, 100, 60, 200, 50),
		},
		{
			name: "imagefs without available is ignored",
			raw:  `{"node":{"fs":{"availableBytes":60,"capacityBytes":100,"usedBytes":40},"runtime":{"imageFs":{"capacityBytes":200}}}}`,
			want: NewNodeDisk(40, 100, 60, 0, 0),
		},
		{name: "fs missing", raw: `{"node":{"nodeName":"n1"}}`, wantErr: true},
		{name: "used and available missing", raw: `{"node":{"fs":{"capacityBytes":100}}}`, wantErr: true},
		{name: "not json", raw: `<html>`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNodeDiskSummary([]byte(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNodeDiskSeparateImageFs(t *testing.T) {
	const gi = int64(1) << 30
	tests := []struct {
		name string
		disk NodeDisk
		want bool
	}{
		{name: "no imagefs", disk: NewNodeDisk(40*gi, 100*gi, 60*gi, 0, 0), want: false},
		{name: "same fs", disk: NewNodeDisk(40*gi, 100*gi, 60*gi, 100*gi, 60*gi), want: false},
		{name: "same fs, free space drifted between samples", disk: NewNodeDisk(40*gi, 100*gi, 60*gi, 100*gi, 60*gi-gi/2), want: false},
		{name: "different size disk", disk: NewNodeDisk(40*gi, 100*gi, 60*gi, 200*gi, 50*gi), want: true},
		{name: "identically sized second disk", disk: NewNodeDisk(40*gi, 100*gi, 60*gi, 100*gi, 10*gi), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.disk.SeparateImageFs(); got != tt.want {
				t.Fatalf("SeparateImageFs() = %v, want %v", got, tt.want)
			}
		})
	}
}
