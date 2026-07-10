package memo

import (
	"testing"
)

func TestValidateCAddress(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr error
	}{
		{
			name: "valid C-address",
			addr: "CCOX4AG3XESDAZC7L27AMQZ6KKMUWEU2KCHFXJ2PXNAXMDUCL225MN2P",
		},
		{
			name:    "G-address rejected",
			addr:    "GB3AETG6Q5SYNM36TPHULYXPP364EC77YJJBDMNKDA5CF4QYP3XHJ6I5",
			wantErr: ErrInvalidCAddress,
		},
		{
			name:    "garbage rejected",
			addr:    "not-an-address",
			wantErr: ErrInvalidCAddress,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCAddress(tc.addr)
			if err != tc.wantErr {
				t.Fatalf("ValidateCAddress(%q) = %v, want %v", tc.addr, err, tc.wantErr)
			}
		})
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		name     string
		memoType string
		memo     string
		want     uint64
		wantErr  error
	}{
		{
			name:     "valid MEMO_ID",
			memoType: "id",
			memo:     "3891273648291034",
			want:     3891273648291034,
		},
		{
			name:     "wrong memo type",
			memoType: "text",
			memo:     "some text",
			wantErr:  ErrNotMemoID,
		},
		{
			name:     "no memo",
			memoType: "none",
			memo:     "",
			wantErr:  ErrNotMemoID,
		},
		{
			name:     "id type but invalid value",
			memoType: "id",
			memo:     "not-a-number",
			wantErr:  ErrInvalidMemoID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseID(tc.memoType, tc.memo)
			if tc.wantErr != nil {
				if err != tc.wantErr {
					t.Fatalf("ParseID() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseID() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseID() = %d, want %d", got, tc.want)
			}
		})
	}
}
