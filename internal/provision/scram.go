package provision

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// scramIterations matches PostgreSQL's own default (scram_iterations, 4096).
const scramIterations = 4096

// scramVerifier computes the SCRAM-SHA-256 verifier PostgreSQL stores for a password
// (RFC 5802/7677, in PostgreSQL's `SCRAM-SHA-256$<iter>:<salt>$<StoredKey>:<ServerKey>` form).
//
// Why this exists at all: `CREATE ROLE ... PASSWORD '<plaintext>'` sends the plaintext inside a
// DDL statement, and anything that records statements — log_statement=ddl, pg_stat_statements,
// an operator's audit extension on an external cluster — would then hold a live credential,
// breaking contracts/credential-broker.md's "never written to any log". PostgreSQL accepts a
// precomputed verifier in place of a password and stores it as-is, so the plaintext never
// leaves this process except in the broker response to the requester.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	return scramVerifierWithSalt(password, salt, scramIterations)
}

func scramVerifierWithSalt(password string, salt []byte, iterations int) (string, error) {
	// PostgreSQL SASLprep-normalizes the password first; ours are base64url, which SASLprep
	// leaves unchanged, so no normalization step is needed.
	salted, err := pbkdf2.Key(sha256.New, password, salt, iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("deriving salted password: %w", err)
	}
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, b64(salt), b64(storedKey[:]), b64(serverKey)), nil
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// newPassword is a lease's password: 32 random bytes, base64url (no characters that need
// escaping in a libpq connection string or URL).
func newPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
