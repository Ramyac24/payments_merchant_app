package payments

import "testing"

func TestTransitions(t *testing.T) {
	allowed := [][2]Status{
		{StatusCreated, StatusProcessing},
		{StatusCreated, StatusSucceeded}, // webhook raced ahead of create
		{StatusProcessing, StatusSucceeded},
		{StatusProcessing, StatusFailed},
		{StatusFailed, StatusSucceeded}, // customer retried with another card
		{StatusProcessing, StatusCanceled},
	}
	for _, tr := range allowed {
		if !CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s should be allowed", tr[0], tr[1])
		}
	}
	forbidden := [][2]Status{
		{StatusSucceeded, StatusFailed},
		{StatusSucceeded, StatusProcessing},
		{StatusSucceeded, StatusCanceled},
		{StatusCanceled, StatusSucceeded},
		{StatusProcessing, StatusCreated},
	}
	for _, tr := range forbidden {
		if CanTransition(tr[0], tr[1]) {
			t.Errorf("%s -> %s should be forbidden", tr[0], tr[1])
		}
	}
}
