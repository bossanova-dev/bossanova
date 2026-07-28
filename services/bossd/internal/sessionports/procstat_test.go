package sessionports

import "testing"

func TestParseProcStatStartTime(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
		errText string
	}{
		{
			name: "simple comm",
			// Fields 3..22 after comm: state + 18 placeholders + starttime.
			input: "1234 (myproc) R 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 8388608 200 300",
			want:  8388608,
		},
		{
			name: "comm with spaces and parens",
			// The comm field itself contains spaces and a ')' — the parser must
			// key off the LAST ')' so field alignment survives.
			input: "1234 (weird )( name) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 9999999 200 300",
			want:  9999999,
		},
		{
			name:    "missing paren is fatal",
			input:   "1234 no-parens R 1 2 3",
			wantErr: true,
		},
		{
			name:    "too few fields after comm",
			input:   "1234 (proc) R 1 2 3",
			wantErr: true,
		},
		{
			name:    "exactly one field short",
			input:   "1234 (proc) R 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18",
			wantErr: true,
			errText: "parse /proc stat: only 19 fields after comm, need 20",
		},
		{
			name: "comm terminator at first byte",
			// Only absence of ')' is fatal; a terminator at index zero still
			// leaves enough fixed-position fields to extract starttime.
			input: ") R 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 424242",
			want:  424242,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProcStatStartTime([]byte(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseProcStatStartTime() err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errText != "" && err != nil && err.Error() != tt.errText {
				t.Fatalf("parseProcStatStartTime() err = %q, want %q", err, tt.errText)
			}
			if err == nil && got != tt.want {
				t.Fatalf("parseProcStatStartTime() = %d, want %d", got, tt.want)
			}
		})
	}
}
