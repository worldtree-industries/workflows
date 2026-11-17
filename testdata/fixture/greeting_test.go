package fixture

import "testing"

func TestGreeting(t *testing.T) {
	if Greeting == "" {
		t.Fatal("Greeting is empty")
	}
}
