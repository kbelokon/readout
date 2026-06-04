package web

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"
)

type sessionCodec struct {
	aead cipher.AEAD
}

type sealedEnvelope struct {
	Expires int64           `json:"expires"`
	Value   json.RawMessage `json:"value"`
}

func newSessionCodec(secret string) (*sessionCodec, error) {
	key := make([]byte, 32)
	if secret == "" {
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
	} else {
		sum := sha256.Sum256([]byte(secret))
		copy(key, sum[:])
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sessionCodec{aead: aead}, nil
}

func (c *sessionCodec) Seal(name string, value any, ttl time.Duration) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	env, err := json.Marshal(sealedEnvelope{
		Expires: time.Now().Add(ttl).Unix(),
		Value:   payload,
	})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := append([]byte(nil), nonce...)
	out = c.aead.Seal(out, nonce, env, []byte(name))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func (c *sessionCodec) Open(name, encoded string, out any) error {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	nonceSize := c.aead.NonceSize()
	if len(data) <= nonceSize {
		return errors.New("sealed value is too short")
	}
	nonce := data[:nonceSize]
	ciphertext := data[nonceSize:]
	plain, err := c.aead.Open(nil, nonce, ciphertext, []byte(name))
	if err != nil {
		return err
	}
	var env sealedEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return err
	}
	if env.Expires <= time.Now().Unix() {
		return errors.New("sealed value expired")
	}
	return json.Unmarshal(env.Value, out)
}
