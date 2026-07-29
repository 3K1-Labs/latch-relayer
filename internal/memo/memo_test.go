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
		// A numeric MEMO_TEXT is accepted: on-ramps and wallet UIs send the tag
		// as text, and refusing it here means the deposit is swept to recovery
		// rather than credited. Horizon renders a text memo as the raw UTF-8
		// string in `memo` (the base64 form lives in `memo_bytes`), so the digits
		// arrive here verbatim.
		{
			name:     "numeric MEMO_TEXT accepted",
			memoType: "text",
			memo:     "3891273648291034",
			want:     3891273648291034,
		},
		{
			name:     "MEMO_TEXT with surrounding whitespace accepted",
			memoType: "text",
			memo:     " 3891273648291034 ",
			want:     3891273648291034,
		},
		{
			name:     "non-numeric MEMO_TEXT rejected",
			memoType: "text",
			memo:     "some text",
			wantErr:  ErrInvalidMemoID,
		},
		{
			name:     "empty MEMO_TEXT rejected",
			memoType: "text",
			memo:     "",
			wantErr:  ErrInvalidMemoID,
		},
		{
			name:     "no memo",
			memoType: "none",
			memo:     "",
			wantErr:  ErrUnsupportedMemoType,
		},
		{
			name:     "hash memo rejected",
			memoType: "hash",
			memo:     "NTI1OTM3MjgyMDg1ODUyMzI=",
			wantErr:  ErrUnsupportedMemoType,
		},
		{
			name:     "return memo rejected",
			memoType: "return",
			memo:     "NTI1OTM3MjgyMDg1ODUyMzI=",
			wantErr:  ErrUnsupportedMemoType,
		},
		{
			name:     "id type but invalid value",
			memoType: "id",
			memo:     "not-a-number",
			wantErr:  ErrInvalidMemoID,
		},
		// uint64 boundary — an overflow that silently wrapped would map a deposit
		// onto an unrelated intent, so it must be rejected outright.
		{
			name:     "uint64 max accepted",
			memoType: "text",
			memo:     "18446744073709551615",
			want:     18446744073709551615,
		},
		{
			name:     "uint64 overflow rejected",
			memoType: "text",
			memo:     "18446744073709551616",
			wantErr:  ErrInvalidMemoID,
		},
		{
			name:     "negative value rejected",
			memoType: "text",
			memo:     "-1",
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
