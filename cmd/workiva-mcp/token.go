package main

import (
	"crypto/rand"
	"encoding/hex"
)

// randomToken returns a 32-character random hex token for demo mode.
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
