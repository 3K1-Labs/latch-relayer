package greetings

import (
	"errors"
	"fmt"
	"math/rand"
)

// Hello returns a greeting for the named person.
func Hello(name string) (string, error) {
	// if no name was given, return an error with a message.
	if name == "" {
		return "", errors.New("empty name")
	}

	// if a name was received, create a greeting message and return it.
	message := fmt.Sprintf(randomFormat(), name)
	return message, nil
}

// Hellos returns a map that associates each of the named people
//with a greeting message. If a name is an empty string, an error is returned
// in place of a message.
func Hellos(names []string) (map[string]string, error) {
	// A map to associate names with messages.
	messages := make(map[string]string)

	// Loop through the received slice of names, calling
	// the Hello function to get a message for each name.
	for _, name := range names {
		message, err := Hello(name)
		if err != nil {
			return nil, err // if an error was returned, return it to the caller.
		}
		messages[name] = message // store the returned message in the map.
	}
	return messages, nil
}

// randomFormat returns one of a set of greeting messages. The returned message
// is selected at random.
func randomFormat() string {
	// A slice of message formats.
	formats := []string{
		"Hi, %v. Welcome!",
		"Great to see you, %v!",
		"Hey, %v! I miss you!",
	}

	// Return a randomly selected message format by specifying
	// a random index for the slice of formats.
	return formats[rand.Intn(len(formats))]
}
