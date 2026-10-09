package basic

import "testing"

func TestDescribe(t *testing.T) {
	if got := describe(1); got != "n=1" {
		t.Fatalf("describe(1) = %q, want %q", got, "n=1")
	}
}
