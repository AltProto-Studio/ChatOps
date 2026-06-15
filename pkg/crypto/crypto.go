package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
	"os"
)

var activeKey []byte
const keyFilePath = ".gopass_secret.key"

func init() {
	envKey := os.Getenv("GOPASS_MASTER_KEY")
	if len(envKey) == 32 {
		activeKey = []byte(envKey)
		return
	} else if len(envKey) > 0 {
		// If provided but not 32 bytes, pad or truncate to 32 bytes
		padded := make([]byte, 32)
		copy(padded, envKey)
		activeKey = padded
		return
	}

	// Try to read the local key file
	if fileData, err := os.ReadFile(keyFilePath); err == nil && len(fileData) == 32 {
		activeKey = fileData
		return
	}

	// Generate a new 32-byte key
	newKey := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, newKey); err != nil {
		panic("crypto/rand failed to generate secure key: " + err.Error())
	}

	// Save to local file with 0400 permissions (read-only for owner)
	if err := os.WriteFile(keyFilePath, newKey, 0400); err != nil {
		panic("failed to save secure key to file: " + err.Error())
	}

	activeKey = newKey
}

// Encrypt encrypts data using AES-GCM
func Encrypt(plaintext []byte) []byte {
	block, err := aes.NewCipher(activeKey)
	if err != nil {
		return plaintext // Fallback to plaintext if cipher fails, though this shouldn't happen with valid key
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return plaintext
	}

	ciphertext := aesGCM.Seal(nonce, nonce, plaintext, nil)
	return ciphertext
}

// Decrypt decrypts data using AES-GCM. Returns error if it fails (e.g., if it's plaintext JSON)
func Decrypt(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(activeKey)
	if err != nil {
		return nil, err
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertextBytes := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}
