package assurance

import (
	"bytes"
	"crypto/ed25519"
	"errors"
)

// maxContentPolicyHistoryKeys bounds the operator-pinned historical signer set.
const maxContentPolicyHistoryKeys = 8

// SetContentPolicyHistoryKeys configures the operator-trusted public keys used
// only to verify prior bundle signatures during candidate staging. The current
// candidate key is added internally. This setting is in-memory, copied, and
// immutable after its first successful configuration; keys are never learned
// from stored bundle rows.
func (s *Store) SetContentPolicyHistoryKeys(keys []ed25519.PublicKey) error {
	if s == nil || s.db == nil {
		return errors.New("assurance: store is required to configure content policy history keys")
	}
	if len(keys) == 0 || len(keys) > maxContentPolicyHistoryKeys {
		return errors.New("assurance: content policy history key count is outside the allowed bound")
	}
	copyKeys := make([]ed25519.PublicKey, len(keys))
	for i, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			return errors.New("assurance: content policy history key is not a valid Ed25519 public key")
		}
		for j := 0; j < i; j++ {
			if bytes.Equal(key, keys[j]) {
				return errors.New("assurance: duplicate content policy history key")
			}
		}
		copyKeys[i] = append(ed25519.PublicKey(nil), key...)
	}

	s.contentPolicyHistoryMu.Lock()
	defer s.contentPolicyHistoryMu.Unlock()
	if s.contentPolicyHistorySet {
		return errors.New("assurance: content policy history keys are already configured")
	}
	s.contentPolicyHistoryKeys = copyKeys
	s.contentPolicyHistorySet = true
	return nil
}

func (s *Store) contentPolicyHistoryKeySnapshot(candidateKey ed25519.PublicKey) []ed25519.PublicKey {
	keys := []ed25519.PublicKey{append(ed25519.PublicKey(nil), candidateKey...)}
	if s == nil {
		return keys
	}
	s.contentPolicyHistoryMu.RLock()
	defer s.contentPolicyHistoryMu.RUnlock()
	for _, key := range s.contentPolicyHistoryKeys {
		keys = append(keys, append(ed25519.PublicKey(nil), key...))
	}
	return keys
}
