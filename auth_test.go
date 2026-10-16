package main

import (
	"errors"
	"testing"
)

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !checkPassword(h, "correct horse") {
		t.Fatal("correct password rejected")
	}
	if checkPassword(h, "wrong horse") {
		t.Fatal("wrong password accepted")
	}
	if checkPassword("garbage", "correct horse") {
		t.Fatal("malformed hash accepted")
	}
}

func TestLoginLockout(t *testing.T) {
	h, _ := hashPassword("secret1")
	a := &auth{cfg: &configStore{c: Config{PasswordHash: h, SessionSecret: "00"}}}
	for i := range 5 {
		var ae *apiError
		if err := a.login("nope"); !errors.As(err, &ae) || ae.Code != "password_wrong" {
			t.Fatalf("attempt %d: got %v, want password_wrong", i+1, err)
		}
	}
	var ae *apiError
	if err := a.login("secret1"); !errors.As(err, &ae) || ae.Code != "locked" {
		t.Fatalf("after 5 failures: got %v, want locked", err)
	}
	if ae.Args["seconds"] == nil {
		t.Fatal("locked error carries no retry delay")
	}
}
