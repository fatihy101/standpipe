package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// hmacKeyBytes is the key length for inputs_hash. 32 bytes is the SHA-256
// block output size; longer keys buy nothing.
const hmacKeyBytes = 32

// hmacKeyIDBytes is the inputs_hash key id length: 8 CSPRNG bytes, written as 16
// hex characters.
const hmacKeyIDBytes = 8

// inputsHashPrefix is the algorithm tag of "hmac-sha256:<key_id>:<hex>".
const inputsHashPrefix = "hmac-sha256"

// ErrUnknownHMACKey means an inputs_hash names a key id this database does not
// hold. It is deliberately NOT reported as a hash mismatch: a
// mismatch says "this record was tampered with", while the honest answer here
// is "the key that produced this value is not in this database" — the state a
// restore from a snapshot taken before the key was minted leaves behind.
var ErrUnknownHMACKey = errors.New("store: no HMAC key with this key_id")

// InputsHash computes the trigger's `inputs_hash` field over the canonical form
// of the inputs object.
//
// It is HMAC-SHA256, not a plain digest, and the key never leaves this
// package. That is the point: a plain SHA-256 of the inputs would be a
// verification oracle — anyone holding the log could confirm a guessed secret
// value by hashing it. The key lives in the database so it survives a restart
// and is carried by `standpipe migrate`.
//
// The returned form is "hmac-sha256:<key_id>:<hex>", where key_id is 16 random
// hex characters. The id says which key verifies the value, so a rotation
// leaves older values verifiable.
func (db *DB) InputsHash(ctx context.Context, canonicalInputs []byte) (string, error) {
	keyID, key, err := db.activeHMACKey(ctx)
	if err != nil {
		return "", err
	}
	return inputsHashPrefix + ":" + keyID + ":" + macHex(key, canonicalInputs), nil
}

// VerifyInputsHash re-checks a stored inputs_hash against the canonical inputs.
//
// It returns ErrUnknownHMACKey when the value's key id is not in this database,
// which is the distinction the random key id exists to preserve; a wrong digest under a known
// key returns a plain error.
func (db *DB) VerifyInputsHash(ctx context.Context, canonicalInputs []byte, value string) error {
	alg, rest, ok := strings.Cut(value, ":")
	if !ok || alg != inputsHashPrefix {
		return fmt.Errorf("store: %q is not an %s value", value, inputsHashPrefix)
	}
	keyID, want, ok := strings.Cut(rest, ":")
	if !ok {
		return fmt.Errorf("store: %q carries no key_id", value)
	}
	var key []byte
	err := db.reader.QueryRowContext(ctx, `SELECT key FROM hmac_keys WHERE key_id = ?`, keyID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrUnknownHMACKey, keyID)
	}
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(macHex(key, canonicalInputs)), []byte(want)) {
		return fmt.Errorf("store: inputs_hash does not verify under key %s", keyID)
	}
	return nil
}

func macHex(key, msg []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return hex.EncodeToString(mac.Sum(nil))
}

// EnsureHMACKey creates the first key if the database has none and returns its
// key id. It is called once at startup so that the first request does not pay
// for key generation inside the request path.
func (db *DB) EnsureHMACKey(ctx context.Context) (string, error) {
	id, _, err := db.activeHMACKey(ctx)
	return id, err
}

// RotateHMACKeyID generates a new key, makes it active and returns its key id.
// Existing values keep their own key id prefix and stay verifiable. No
// Phase 0 command calls this; it exists so that a rotation does not require a
// schema change.
func (db *DB) RotateHMACKeyID(ctx context.Context) (string, error) {
	var keyID string
	err := db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		var err error
		keyID, err = mintHMACKey(ctx, tx, db.cfg.now().UnixMilli())
		return err
	})
	return keyID, err
}

// RotateHMACKey rotates the key and returns how many keys the database now
// holds.
//
// Deprecated: the returned number is a row count, NOT a key version — key versions
// were removed, because a counter mislabels the key minted after a
// restore from a pre-key snapshot as v1 again. It exists only so
// that callers written against the counter shape keep compiling. Use
// RotateHMACKeyID.
func (db *DB) RotateHMACKey(ctx context.Context) (int, error) {
	if _, err := db.RotateHMACKeyID(ctx); err != nil {
		return 0, err
	}
	var n int
	err := db.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM hmac_keys`).Scan(&n)
	return n, err
}

func (db *DB) activeHMACKey(ctx context.Context) (string, []byte, error) {
	var (
		keyID string
		key   []byte
	)
	err := db.reader.QueryRowContext(ctx,
		`SELECT key_id, key FROM hmac_keys WHERE active = 1`).Scan(&keyID, &key)
	switch {
	case err == nil:
		return keyID, key, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", nil, err
	}

	// No key yet: mint one. The write transaction re-checks, so two goroutines
	// racing here produce one key, not two.
	err = db.writeTx(ctx, func(ctx context.Context, tx *sql.Conn) error {
		err := tx.QueryRowContext(ctx, `SELECT key_id, key FROM hmac_keys WHERE active = 1`).Scan(&keyID, &key)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		keyID, err = mintHMACKey(ctx, tx, db.cfg.now().UnixMilli())
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT key FROM hmac_keys WHERE key_id = ?`, keyID).Scan(&key)
	})
	if err != nil {
		return "", nil, err
	}
	return keyID, key, nil
}

func mintHMACKey(ctx context.Context, tx *sql.Conn, nowMS int64) (string, error) {
	idBytes := make([]byte, hmacKeyIDBytes)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("store: generating HMAC key id: %w", err)
	}
	keyID := hex.EncodeToString(idBytes)

	key := make([]byte, hmacKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("store: generating HMAC key: %w", err)
	}
	// The partial UNIQUE index allows one active row; clear the old one first.
	if _, err := tx.ExecContext(ctx, `UPDATE hmac_keys SET active = 0 WHERE active = 1`); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO hmac_keys (key_id, key, created_at, active) VALUES (?, ?, ?, 1)`,
		keyID, key, nowMS); err != nil {
		return "", err
	}
	return keyID, nil
}
