// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package macaroon

import (
	"bytes"

	"github.com/btcsuite/btcutil/base58"
	"github.com/gogo/protobuf/proto"
	"github.com/zeebo/errs"
)

var (
	Error           = errs.Class("api key error")
	ErrFormat       = errs.Class("api key format error")
	ErrInvalid      = errs.Class("api key invalid error")
	ErrUnauthorized = errs.Class("api key unauthorized error")
)

// APIKey implements a Macaroon-backed Storj-v3 API key.
type APIKey struct {
	mac *Macaroon
}

// ParseAPIKey parses a given api key string and returns an APIKey if the
// APIKey was correctly formatted. It does not validate the key.
func ParseAPIKey(key string) (*APIKey, error) {
	data, version, err := base58.CheckDecode(key)
	if err != nil || version != 0 {
		return nil, ErrFormat.New("invalid api key format")
	}
	mac, err := ParseMacaroon(data)
	if err != nil {
		return nil, ErrFormat.Wrap(err)
	}
	return &APIKey{mac: mac}, nil
}

// NewAPIKey generates a brand new unrestricted API key given the provided
// server project secret
func NewAPIKey(secret []byte) (*APIKey, error) {
	mac, err := NewUnrestricted(secret)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	return &APIKey{mac: mac}, nil
}

// Check makes sure that the key authorizes the provided action given the root
// project secret and any possible revocations, returning an error if the action
// is not authorized. 'revoked' is a list of either revoked heads or revoked
// tails. Keep in mind that any key generated from a key with a revoked head
// _or_ tail will also be considered revoked.
func (a *APIKey) Check(secret []byte, action Action, revoked [][]byte) error {
	if !a.mac.Validate(secret) {
		return ErrInvalid.New("macaroon unauthorized")
	}

	for _, cavbuf := range a.mac.Caveats() {
		var cav Caveat
		err := proto.Unmarshal(cavbuf, &cav)
		if err != nil {
			return ErrFormat.New("invalid caveat format")
		}
		if !cav.Allows(action) {
			return ErrUnauthorized.New("action disallowed")
		}
	}

	if len(revoked) > 0 {
		revokedMap := make(map[string]struct{}, len(revoked))
		for _, revokedID := range revoked {
			revokedMap[string(revokedID)] = struct{}{}
		}
		if _, exists := revokedMap[string(a.mac.Head())]; exists {
			return ErrUnauthorized.New("macaroon head revoked")
		}
		for _, tail := range a.mac.Tails(secret) {
			if _, exists := revokedMap[string(tail)]; exists {
				return ErrUnauthorized.New("macaroon tail revoked")
			}
		}
	}

	return nil
}

// Restrict generates a new APIKey with the provided Caveat attached.
func (a *APIKey) Restrict(caveat Caveat) (*APIKey, error) {
	buf, err := proto.Marshal(&caveat)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	mac, err := a.mac.AddFirstPartyCaveat(buf)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	return &APIKey{mac: mac}, nil
}

// Head returns the identifier for this macaroon's root ancestor.
func (a *APIKey) Head() []byte {
	return a.mac.Head()
}

// Tail returns the identifier for this macaroon only.
func (a *APIKey) Tail() []byte {
	return a.mac.Tail()
}

// Serialize serializes the API Key to a string
func (a *APIKey) Serialize() (string, error) {
	return base58.CheckEncode(a.mac.Serialize(), 0), nil
}

// Allows returns true if the provided action is allowed by the caveat.
func (c *Caveat) Allows(action Action) bool {
	switch action.Op {
	case Action_READ:
		if c.DisallowReads {
			return false
		}
	case Action_WRITE:
		if c.DisallowWrites {
			return false
		}
	case Action_LIST:
		if c.DisallowLists {
			return false
		}
	case Action_DELETE:
		if c.DisallowDeletes {
			return false
		}
	default:
		return false
	}

	// a timestamp is always required on an action
	if action.Time == nil {
		return false
	}

	// if the action is after the caveat's "not after" field, then it is invalid
	if c.NotAfter != nil && action.Time.After(*c.NotAfter) {
		return false
	}
	// if the caveat's "not before" field is *after* the action, then the action
	// is before the "not before" field and it is invalid
	if c.NotBefore != nil && c.NotBefore.After(*action.Time) {
		return false
	}

	if len(c.Buckets) > 0 {
		found := false
		for _, bucket := range c.Buckets {
			if bytes.Equal(action.Bucket, bucket) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	if len(c.EncryptedPathPrefixes) > 0 {
		found := false
		for _, path := range c.EncryptedPathPrefixes {
			if bytes.HasPrefix(action.EncryptedPath, path) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}
