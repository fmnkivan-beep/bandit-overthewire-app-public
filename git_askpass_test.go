package main

import "testing"

func TestGitAskpassTokenIsOneTime(t *testing.T) {
	app := NewApp(newEventHub(), "http://127.0.0.1:4173/internal/askpass")
	token, cleanup, err := app.startGitAskpass("secret")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	password, ok := app.consumeGitAskpass(token)
	if !ok || string(password) != "secret" {
		t.Fatalf("first askpass read = %q, %t", password, ok)
	}
	clear(password)
	if password, ok := app.consumeGitAskpass(token); ok || password != nil {
		t.Fatalf("second askpass read = %q, %t", password, ok)
	}
}
