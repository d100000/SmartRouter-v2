package model

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const supplierSecretPrefix = "supplier-secret:v1:"

// The key ring is deployment-owned, outside the business database. Old keys
// remain available by ID for tasks that retain an earlier credential version.
func supplierSecretCipher(keyID string) (cipher.AEAD, error) {
	keys := map[string]string{}
	if common.UnmarshalJsonStr(os.Getenv("SUPPLIER_CREDENTIAL_KEYS"), &keys) != nil {
		return nil, errors.New("supplier credential encryption is not configured")
	}
	key, err := base64.StdEncoding.DecodeString(keys[keyID])
	if err != nil || len(key) != 32 {
		return nil, errors.New("supplier credential encryption key is unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("supplier credential encryption key is invalid")
	}
	return cipher.NewGCM(block)
}

func EncryptSupplierSecret(owner, secret string) (string, error) {
	if secret == "" {
		return "", nil
	}
	if strings.HasPrefix(secret, supplierSecretPrefix) {
		return "", errors.New("encrypted credential payloads cannot be submitted")
	}
	keyID := os.Getenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY")
	if keyID == "" {
		keyID = "v1"
	}
	if strings.ContainsAny(keyID, ":\r\n") {
		return "", errors.New("invalid supplier credential key version")
	}
	aead, err := supplierSecretCipher(keyID)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", errors.New("failed to generate credential nonce")
	}
	sealed := aead.Seal(nonce, nonce, []byte(secret), []byte(supplierSecretPrefix+keyID+":"+owner))
	return supplierSecretPrefix + keyID + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

func DecryptSupplierSecret(owner, value string) (string, error) {
	encoded, ok := strings.CutPrefix(value, supplierSecretPrefix)
	if !ok {
		return "", errors.New("supplier credential is not encrypted")
	}
	keyID, payload, ok := strings.Cut(encoded, ":")
	if !ok {
		return "", errors.New("invalid encrypted supplier credential")
	}
	aead, err := supplierSecretCipher(keyID)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return "", errors.New("invalid encrypted supplier credential")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(supplierSecretPrefix+keyID+":"+owner))
	if err != nil {
		return "", errors.New("supplier credential authentication failed")
	}
	return string(plain), nil
}

func (channel *Channel) BeforeSave(_ *gorm.DB) error {
	if channel.OwnerSupplierID != "" {
		channel.SecretsWriteOnly = true
	}
	if !channel.SecretsWriteOnly || channel.Key == "" {
		return nil
	}
	if strings.HasPrefix(channel.Key, supplierSecretPrefix) {
		_, err := DecryptSupplierSecret(channel.OwnerSupplierID, channel.Key)
		return err
	}
	secret, err := EncryptSupplierSecret(channel.OwnerSupplierID, channel.Key)
	if err != nil {
		return err
	}
	channel.Key = secret
	return nil
}

func (channel *Channel) sealSupplierCredential() error {
	if !channel.SecretsWriteOnly || channel.Key == "" {
		return nil
	}
	if !strings.HasPrefix(channel.Key, supplierSecretPrefix) {
		sealed, err := EncryptSupplierSecret(channel.OwnerSupplierID, channel.Key)
		if err != nil {
			return err
		}
		channel.Key = sealed
	}
	channel.Keys = nil
	return nil
}

func (channel *Channel) AfterSave(_ *gorm.DB) error {
	return channel.openSupplierCredential()
}

func (channel *Channel) openSupplierCredential() error {
	if !strings.HasPrefix(channel.Key, supplierSecretPrefix) {
		return nil
	}
	secret, err := DecryptSupplierSecret(channel.OwnerSupplierID, channel.Key)
	if err != nil {
		return err
	}
	channel.Key, channel.SecretsWriteOnly = secret, true
	return nil
}
