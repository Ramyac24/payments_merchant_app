package ledger

import (
	"errors"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := []Entry{
		{Account: "a", Currency: "inr", Amount: 97000},
		{Account: "fees", Currency: "inr", Amount: 3000},
		{Account: "b", Currency: "inr", Amount: -100000},
	}
	if err := Validate(ok); err != nil {
		t.Fatalf("balanced txn rejected: %v", err)
	}
	if err := Validate([]Entry{{Currency: "inr", Amount: 100}, {Currency: "inr", Amount: -99}}); !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("unbalanced: %v", err)
	}
	if err := Validate([]Entry{{Currency: "inr", Amount: 100}, {Currency: "usd", Amount: -100}}); err == nil {
		t.Fatal("mixed currencies accepted")
	}
	if err := Validate([]Entry{{Currency: "inr", Amount: 0}, {Currency: "inr", Amount: 0}}); err == nil {
		t.Fatal("zero entries accepted")
	}
	if err := Validate([]Entry{{Currency: "inr", Amount: 100}}); err == nil {
		t.Fatal("single entry accepted")
	}
}
