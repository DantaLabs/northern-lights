package assurance

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"unicode/utf8"
)

var contentStorageEnvelopeMagic = []byte{'N', 'L', 'C', 'S', 1}

const contentStorageKeyIDMax = 128

// SetContentStorageKey enables authenticated encryption for source bytes.
// Configuration is immutable for the lifetime of a Store. The key is copied
// and must be a raw AES-256 key; key material is never serialized or logged.
func (s *Store) SetContentStorageKey(keyID string, key []byte) error {
	if s == nil || s.db == nil {
		return errors.New("assurance: content storage requires an initialized store")
	}
	if !validContentStorageKeyID(keyID) || len(key) != 32 {
		return errors.New("assurance: content storage requires a valid key ID and 32-byte AES-256 key")
	}
	s.contentStorageModeMu.Lock()
	defer s.contentStorageModeMu.Unlock()
	s.contentCryptoMu.Lock()
	defer s.contentCryptoMu.Unlock()
	if s.contentCryptoConfigured {
		return errors.New("assurance: content storage key is already configured")
	}
	s.contentCryptoKeyID = keyID
	s.contentCryptoKey = append([]byte(nil), key...)
	s.contentCryptoConfigured = true
	return nil
}

// ContentStorageEncrypted reports whether this Store has a configured source
// storage key. It remains true for the Store's lifetime after configuration.
func (s *Store) ContentStorageEncrypted() bool {
	if s == nil {
		return false
	}
	s.contentStorageModeMu.RLock()
	defer s.contentStorageModeMu.RUnlock()
	s.contentCryptoMu.RLock()
	defer s.contentCryptoMu.RUnlock()
	return s.contentCryptoConfigured
}

func validContentStorageKeyID(keyID string) bool {
	if keyID == "" || len(keyID) > contentStorageKeyIDMax || !utf8.ValidString(keyID) {
		return false
	}
	for _, r := range keyID {
		alphaNumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		allowedPunctuation := r == '-' || r == '_' || r == '.' || r == ':'
		if !alphaNumeric && !allowedPunctuation {
			return false
		}
	}
	return true
}

func (s *Store) sealContentSource(tenant, actor, sourceID, sourceHash string, plaintext []byte) ([]byte, error) {
	keyID, key, configured := s.contentStorageKeySnapshot()
	if !configured {
		return append([]byte(nil), plaintext...), nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("assurance: content source encryption is unavailable")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("assurance: content source encryption is unavailable")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("assurance: content source encryption is unavailable")
	}
	sealed := aead.Seal(nil, nonce, plaintext, contentStorageAAD(tenant, actor, sourceID, sourceHash, keyID))
	if len(keyID) > int(^uint16(0)) || len(nonce) > 255 {
		return nil, errors.New("assurance: content source encryption metadata is invalid")
	}
	envelope := make([]byte, 0, len(contentStorageEnvelopeMagic)+2+1+len(keyID)+len(nonce)+len(sealed))
	envelope = append(envelope, contentStorageEnvelopeMagic...)
	var keyIDLen [2]byte
	binary.BigEndian.PutUint16(keyIDLen[:], uint16(len(keyID)))
	envelope = append(envelope, keyIDLen[:]...)
	envelope = append(envelope, byte(len(nonce)))
	envelope = append(envelope, keyID...)
	envelope = append(envelope, nonce...)
	envelope = append(envelope, sealed...)
	return envelope, nil
}

func (s *Store) openContentSource(tenant, actor, sourceID, sourceHash string, stored []byte) ([]byte, error) {
	keyID, key, configured := s.contentStorageKeySnapshot()
	if !bytes.HasPrefix(stored, contentStorageEnvelopeMagic) {
		if configured {
			return nil, errors.New("assurance: plaintext source row rejected while encrypted storage is configured")
		}
		return append([]byte(nil), stored...), nil
	}
	if !configured {
		return nil, errors.New("assurance: encrypted source row requires a configured key")
	}
	minimum := len(contentStorageEnvelopeMagic) + 3
	if len(stored) < minimum || !bytes.HasPrefix(stored, contentStorageEnvelopeMagic) {
		return nil, errors.New("assurance: encrypted source row is malformed")
	}
	offset := len(contentStorageEnvelopeMagic)
	keyIDLen := int(binary.BigEndian.Uint16(stored[offset : offset+2]))
	offset += 2
	nonceLen := int(stored[offset])
	offset++
	if keyIDLen < 1 || keyIDLen > contentStorageKeyIDMax || nonceLen == 0 || len(stored) < offset+keyIDLen+nonceLen {
		return nil, errors.New("assurance: encrypted source row is malformed")
	}
	storedKeyID := string(stored[offset : offset+keyIDLen])
	offset += keyIDLen
	if storedKeyID != keyID {
		return nil, errors.New("assurance: encrypted source key ID is unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("assurance: content source decryption is unavailable")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("assurance: content source decryption is unavailable")
	}
	if nonceLen != aead.NonceSize() || len(stored) < offset+nonceLen+aead.Overhead() {
		return nil, errors.New("assurance: encrypted source row is malformed")
	}
	nonce := stored[offset : offset+nonceLen]
	ciphertext := stored[offset+nonceLen:]
	plaintext, err := aead.Open(nil, nonce, ciphertext, contentStorageAAD(tenant, actor, sourceID, sourceHash, keyID))
	if err != nil {
		return nil, errors.New("assurance: encrypted source authentication failed")
	}
	if digestHex(HashBytes(plaintext)) != sourceHash {
		return nil, errors.New("assurance: decrypted source digest verification failed")
	}
	return plaintext, nil
}

func (s *Store) contentStorageKeySnapshot() (string, []byte, bool) {
	if s == nil {
		return "", nil, false
	}
	s.contentCryptoMu.RLock()
	defer s.contentCryptoMu.RUnlock()
	if !s.contentCryptoConfigured {
		return "", nil, false
	}
	return s.contentCryptoKeyID, append([]byte(nil), s.contentCryptoKey...), true
}

func contentStorageAAD(tenant, actor, sourceID, sourceHash, keyID string) []byte {
	parts := []string{tenant, actor, sourceID, sourceHash, keyID}
	var out bytes.Buffer
	for _, part := range parts {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(part)))
		out.Write(size[:])
		out.WriteString(part)
	}
	return out.Bytes()
}
