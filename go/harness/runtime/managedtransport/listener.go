// Package managedtransport authenticates connections from an enrolled local
// execution provider before HTTP or h2c bytes reach a private Harness server.
package managedtransport

import (
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	authenticationTimeout = 2 * time.Second
	clientHelloPrefix     = "KAGENT-MANAGED-RUNTIME/2 HELLO "
	serverChallengePrefix = "KAGENT-MANAGED-RUNTIME/2 CHALLENGE "
	clientProofPrefix     = "KAGENT-MANAGED-RUNTIME/2 PROOF "
	proofSeparator        = " "
	proofSuffix           = "\n"
	hexValueLength        = 64
	clientProofLabel      = "client:"
	serverProofLabel      = "server:"
	clientHelloLength     = len(clientHelloPrefix) + hexValueLength + len(proofSuffix)
	serverChallengeLength = len(serverChallengePrefix) + hexValueLength + len(proofSeparator) + hexValueLength + len(proofSuffix)
	clientProofLength     = len(clientProofPrefix) + hexValueLength + len(proofSuffix)
)

var (
	tokenPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hexValuePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// ErrInvalidToken is deliberately sanitized so a token is never reflected.
	ErrInvalidToken = errors.New("managed runtime transport token is invalid")
	// ErrAuthentication reports every missing, malformed, or incorrect
	// handshake through one fail-closed error.
	ErrAuthentication = errors.New("managed runtime transport authentication failed")
)

// ValidateToken enforces the external-provider protocol's lowercase 256-bit
// hexadecimal token.
func ValidateToken(token string) error {
	if !tokenPattern.MatchString(token) {
		return ErrInvalidToken
	}
	return nil
}

// WrapListener performs a mutual, token-bound handshake on the first Read from
// each accepted connection. The host proves possession without sending the raw
// token, and the listener proves that it is the expected runtime before any
// HTTP or h2c bytes cross the connection.
func WrapListener(listener net.Listener, token string) (net.Listener, error) {
	if listener == nil {
		return nil, ErrAuthentication
	}
	if err := ValidateToken(token); err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(token)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &authenticatedListener{Listener: listener, key: key}, nil
}

type authenticatedListener struct {
	net.Listener
	key []byte
}

func (listener *authenticatedListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, ErrAuthentication
	}
	key := append([]byte(nil), listener.key...)
	return &authenticatedConn{Conn: connection, key: key}, nil
}

type authenticationState uint8

const (
	authenticationPending authenticationState = iota
	authenticationAccepted
	authenticationRejected
)

type authenticatedConn struct {
	net.Conn
	key []byte

	authMu        sync.Mutex
	state         authenticationState
	readDeadline  time.Time
	writeDeadline time.Time
}

func (connection *authenticatedConn) Read(destination []byte) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	if err := connection.authenticate(); err != nil {
		return 0, err
	}
	return connection.Conn.Read(destination)
}

func (connection *authenticatedConn) authenticate() error {
	connection.authMu.Lock()
	defer connection.authMu.Unlock()
	switch connection.state {
	case authenticationAccepted:
		return nil
	case authenticationRejected:
		return ErrAuthentication
	}

	readDeadline := authenticationDeadline(connection.readDeadline)
	writeDeadline := authenticationDeadline(connection.writeDeadline)
	if err := connection.Conn.SetReadDeadline(readDeadline); err != nil {
		return connection.reject()
	}
	if err := connection.Conn.SetWriteDeadline(writeDeadline); err != nil {
		return connection.reject()
	}
	hello := make([]byte, clientHelloLength)
	_, helloReadErr := io.ReadFull(connection.Conn, hello)
	clientNonce, helloValid := parseClientHello(hello)
	clear(hello)
	if helloReadErr != nil || !helloValid {
		clear(clientNonce)
		return connection.reject()
	}
	challenge, serverNonce, challengeErr := makeServerChallenge(connection.key, clientNonce)
	if challengeErr != nil {
		clear(clientNonce)
		return connection.reject()
	}
	challengeWriteErr := writeAll(connection.Conn, challenge)
	clear(challenge)
	if challengeWriteErr != nil {
		clear(clientNonce)
		clear(serverNonce)
		return connection.reject()
	}
	clientProof := make([]byte, clientProofLength)
	_, proofReadErr := io.ReadFull(connection.Conn, clientProof)
	proofValid := verifyClientProof(connection.key, clientProof, clientNonce, serverNonce)
	readClearErr := connection.Conn.SetReadDeadline(connection.readDeadline)
	writeClearErr := connection.Conn.SetWriteDeadline(connection.writeDeadline)
	clear(clientProof)
	clear(clientNonce)
	clear(serverNonce)
	if proofReadErr != nil || readClearErr != nil || writeClearErr != nil || !proofValid {
		return connection.reject()
	}
	clear(connection.key)
	connection.key = nil
	connection.state = authenticationAccepted
	return nil
}

func (connection *authenticatedConn) reject() error {
	connection.state = authenticationRejected
	clear(connection.key)
	connection.key = nil
	_ = connection.Close()
	return ErrAuthentication
}

func (connection *authenticatedConn) SetDeadline(deadline time.Time) error {
	connection.authMu.Lock()
	defer connection.authMu.Unlock()
	if err := connection.Conn.SetDeadline(deadline); err != nil {
		return err
	}
	connection.readDeadline = deadline
	connection.writeDeadline = deadline
	return nil
}

func (connection *authenticatedConn) SetReadDeadline(deadline time.Time) error {
	connection.authMu.Lock()
	defer connection.authMu.Unlock()
	if err := connection.Conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	connection.readDeadline = deadline
	return nil
}

func (connection *authenticatedConn) SetWriteDeadline(deadline time.Time) error {
	connection.authMu.Lock()
	defer connection.authMu.Unlock()
	if err := connection.Conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	connection.writeDeadline = deadline
	return nil
}

func authenticationDeadline(configured time.Time) time.Time {
	deadline := time.Now().Add(authenticationTimeout)
	if !configured.IsZero() && configured.Before(deadline) {
		return configured
	}
	return deadline
}

func parseClientHello(message []byte) ([]byte, bool) {
	if len(message) != clientHelloLength || !strings.HasPrefix(string(message), clientHelloPrefix) || message[len(message)-1] != '\n' {
		return nil, false
	}
	nonce := append([]byte(nil), message[len(clientHelloPrefix):len(message)-len(proofSuffix)]...)
	if !hexValuePattern.Match(nonce) {
		clear(nonce)
		return nil, false
	}
	return nonce, true
}

func makeServerChallenge(key, clientNonce []byte) ([]byte, []byte, error) {
	rawNonce := make([]byte, sha256.Size)
	if _, err := cryptorand.Read(rawNonce); err != nil {
		clear(rawNonce)
		return nil, nil, ErrAuthentication
	}
	serverNonce := make([]byte, hexValueLength)
	hex.Encode(serverNonce, rawNonce)
	clear(rawNonce)
	proofBytes := proof(key, serverProofLabel, clientNonce, serverNonce)
	result := make([]byte, 0, serverChallengeLength)
	result = append(result, serverChallengePrefix...)
	result = append(result, serverNonce...)
	result = append(result, proofSeparator...)
	result = hex.AppendEncode(result, proofBytes)
	result = append(result, proofSuffix...)
	clear(proofBytes)
	return result, serverNonce, nil
}

func verifyClientProof(key, message, clientNonce, serverNonce []byte) bool {
	if len(message) != clientProofLength || !strings.HasPrefix(string(message), clientProofPrefix) || message[len(message)-1] != '\n' {
		return false
	}
	providedHex := message[len(clientProofPrefix) : len(message)-len(proofSuffix)]
	if !hexValuePattern.Match(providedHex) {
		return false
	}
	provided := make([]byte, sha256.Size)
	if _, err := hex.Decode(provided, providedHex); err != nil {
		clear(provided)
		return false
	}
	expected := proof(key, clientProofLabel, clientNonce, serverNonce)
	matched := subtle.ConstantTimeCompare(provided, expected) == 1
	clear(provided)
	clear(expected)
	return matched
}

func proof(key []byte, label string, clientNonce, serverNonce []byte) []byte {
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write([]byte(label))
	_, _ = digest.Write(clientNonce)
	_, _ = digest.Write([]byte(":"))
	_, _ = digest.Write(serverNonce)
	return digest.Sum(nil)
}

func writeAll(writer io.Writer, value []byte) error {
	for written := 0; written < len(value); {
		count, err := writer.Write(value[written:])
		if count < 0 || count > len(value)-written || count == 0 {
			return ErrAuthentication
		}
		written += count
		if err != nil {
			return ErrAuthentication
		}
	}
	return nil
}

var _ net.Listener = (*authenticatedListener)(nil)
var _ net.Conn = (*authenticatedConn)(nil)
