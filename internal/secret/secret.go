// Package secret provides small AES-256-GCM helpers used to encrypt secrets
// stored in the database and the contents of short-lived cookies.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
)

// prefix marks encrypted values so plain values (e.g. written by an older
// version or by hand) can still be read.
const prefix = "enc:v1:"

// Encrypt encrypts plaintext with key and returns a printable string.
// An empty plaintext is returned unchanged.
func Encrypt(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	b, err := Seal(key, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Decrypt reverses Encrypt. Values without the encryption prefix are returned
// as-is.
func Decrypt(key []byte, value string) (string, error) {
	if len(value) < len(prefix) || value[:len(prefix)] != prefix {
		return value, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[len(prefix):])
	if err != nil {
		return "", err
	}
	b, err := Open(key, raw)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Seal encrypts data with AES-256-GCM; the random nonce is prepended.
func Seal(key, data []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, data, nil), nil
}

// Open decrypts data produced by Seal.
func Open(key, data []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("secret: ciphertext too short")
	}
	return gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("secret: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// RandomToken returns a URL-safe random token with n bytes of entropy.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
