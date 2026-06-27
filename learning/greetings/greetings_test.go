package greetings

import (
	"testing"
	"regexp"
)

// TestHelloName calls greetings.Hello with a name, checking for a valid return value.
func TestHelloName(t *testing.T) {
	name := "Amaka"
	want := regexp.MustCompile(`\b` + name + `\b`)
	msg, err := Hello("Amaka")
	if !want.MatchString(msg) || err != nil {
		t.Errorf(`Hello("Amaka") = %q, %v, want match for %#q, nil`, msg, err, want)
	}
}

// TestHelloEmpty calls greetings.Hello with an empty string, checking for an error.
func TestHelloEmpty(t *testing.T) {
	_, err := Hello("")
	if err == nil {
		t.Errorf(`Hello("") = _, %v, want _, error`, err)
	}
}