package database

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"

	"crypto/sha256"
	"golang.org/x/crypto/pbkdf2"
)

const passwordIterations = 260000

func HashPassword(password string) (string, string, error) {
	saltBytes := make([]byte, 16)
	if _, err := rand.Read(saltBytes); err != nil {
		return "", "", err
	}
	salt := hex.EncodeToString(saltBytes)
	hash := pbkdf2.Key([]byte(password), []byte(salt), passwordIterations, 32, sha256.New)
	return hex.EncodeToString(hash), salt, nil
}

func VerifyPassword(password, salt, expected string) bool {
	actual := pbkdf2.Key([]byte(password), []byte(salt), passwordIterations, 32, sha256.New)
	actualHex := hex.EncodeToString(actual)
	return subtle.ConstantTimeCompare([]byte(actualHex), []byte(expected)) == 1
}
