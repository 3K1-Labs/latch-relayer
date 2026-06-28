package memo

import (
	"strings"
	"testing"
)

func TestDeriveID(t *testing.T) {
	cAddress := "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAD2KM"

	id := DeriveID(cAddress)

	if id == 0 {
		t.Fatal("DeriveID returned 0, expected a non-zero uint64")
	}

	// same input must always produce the same output
	if id2 := DeriveID(cAddress); id != id2 {
		t.Fatalf("DeriveID is not deterministic: got %d then %d", id, id2)
	}

	// different addresses must produce different IDs
	otherId := DeriveID("CBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	if id == otherId {
		t.Fatal("different C-addresses produced the same memo ID")
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		name      string
		memoType  string
		memo      string
		want      uint64
		wantErr   error
	}{
		{
			name:     "valid MEMO_ID",
			memoType: "id",
			memo:     "3891273648291034",
			want:     3891273648291034,
		},
		{
			name:     "wrong memo type text",
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
			name:     "memo_id but invalid value",
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

func TestToMuxedAddress(t *testing.T) {
	gAddress := "GB3AETG6Q5SYNM36TPHULYXPP364EC77YJJBDMNKDA5CF4QYP3XHJ6I5"
	id := uint64(12345678)

	mAddress, err := ToMuxedAddress(gAddress, id)
	if err != nil {
		t.Fatalf("ToMuxedAddress() error: %v", err)
	}

	if !strings.HasPrefix(mAddress, "M") {
		t.Fatalf("ToMuxedAddress() = %q, want M-prefix address", mAddress)
	}

	// same inputs must always produce the same M-address
	mAddress2, _ := ToMuxedAddress(gAddress, id)
	if mAddress != mAddress2 {
		t.Fatal("ToMuxedAddress() is not deterministic")
	}

	// different IDs must produce different M-addresses
	mOther, _ := ToMuxedAddress(gAddress, id+1)
	if mAddress == mOther {
		t.Fatal("different IDs produced the same M-address")
	}

	// invalid G-address must return an error
	_, err = ToMuxedAddress("not-a-stellar-address", id)
	if err != ErrInvalidAddress {
		t.Fatalf("ToMuxedAddress() with bad address: error = %v, want ErrInvalidAddress", err)
	}
}
