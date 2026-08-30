package managedtransport

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const testNonce = "1111111111111111111111111111111111111111111111111111111111111111"

func TestProtocolVectorsMatchExternalHostClient(t *testing.T) {
	key, err := hex.DecodeString(testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	clientNonce := bytes.Repeat([]byte{'a'}, hexValueLength)
	serverNonce := bytes.Repeat([]byte{'b'}, hexValueLength)
	defer clear(clientNonce)
	defer clear(serverNonce)
	serverProof := proof(key, serverProofLabel, clientNonce, serverNonce)
	clientProof := proof(key, clientProofLabel, clientNonce, serverNonce)
	defer clear(serverProof)
	defer clear(clientProof)
	if got := hex.EncodeToString(serverProof); got != "71401780aa52dd0a7dcc78faf89086aa1a81c4f4b9e0a36a61f225fd9115d2ff" {
		t.Fatalf("server proof = %s", got)
	}
	if got := hex.EncodeToString(clientProof); got != "7c68619ff825af24aac54cbf15d20efddf707d573579eb51788c636d455eecd1" {
		t.Fatalf("client proof = %s", got)
	}
}

func TestWrappedListenerMutuallyAuthenticatesBeforeApplicationBytes(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	wrapper, err := WrapListener(&singleUseListener{connection: server}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := wrapper.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	done := make(chan error, 1)
	go func() {
		if err := performClientHandshake(client, testToken, testNonce); err != nil {
			done <- err
			return
		}
		_, err := client.Write([]byte("GET /readyz HTTP/1.1\r\n"))
		done <- err
	}()
	contents := make([]byte, len("GET /readyz HTTP/1.1\r\n"))
	if _, err := io.ReadFull(connection, contents); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if string(contents) != "GET /readyz HTTP/1.1\r\n" {
		t.Fatalf("application bytes = %q", contents)
	}
}

func TestWrappedListenerRejectsWrongProofWithoutReflection(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	wrapper, err := WrapListener(&singleUseListener{connection: server}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := wrapper.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() {
		if _, err := client.Write(clientHelloMessage(testNonce)); err != nil {
			done <- err
			return
		}
		message, err := clientProofForChallenge(client, testToken, testNonce)
		if err != nil {
			done <- err
			return
		}
		proofStart := len(clientProofPrefix)
		if message[proofStart] == 'f' {
			message[proofStart] = 'e'
		} else {
			message[proofStart] = 'f'
		}
		_, err = client.Write(message)
		done <- err
	}()
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); !errors.Is(err, ErrAuthentication) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("Read() error = %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWrappedListenerRejectsProofReplayedAgainstFreshChallenge(t *testing.T) {
	firstServer, firstClient := net.Pipe()
	firstWrapper, err := WrapListener(&singleUseListener{connection: firstServer}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	firstConnection, err := firstWrapper.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = firstClient.Close()
		_ = firstConnection.Close()
	})
	captured := make(chan []byte, 1)
	firstDone := make(chan error, 1)
	go func() {
		if _, err := firstClient.Write(clientHelloMessage(testNonce)); err != nil {
			firstDone <- err
			return
		}
		proof, err := clientProofForChallenge(firstClient, testToken, testNonce)
		if err != nil {
			firstDone <- err
			return
		}
		captured <- append([]byte(nil), proof...)
		if _, err := firstClient.Write(proof); err != nil {
			firstDone <- err
			return
		}
		_, err = firstClient.Write([]byte{'x'})
		firstDone <- err
	}()
	applicationByte := make([]byte, 1)
	if _, err := io.ReadFull(firstConnection, applicationByte); err != nil || applicationByte[0] != 'x' {
		t.Fatalf("first authenticated Read() = %q, %v", applicationByte, err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	replayedProof := <-captured

	secondServer, secondClient := net.Pipe()
	secondWrapper, err := WrapListener(&singleUseListener{connection: secondServer}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	secondConnection, err := secondWrapper.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = secondClient.Close()
		_ = secondConnection.Close()
	})
	secondDone := make(chan error, 1)
	go func() {
		if _, err := secondClient.Write(clientHelloMessage(testNonce)); err != nil {
			secondDone <- err
			return
		}
		challenge := make([]byte, serverChallengeLength)
		if _, err := io.ReadFull(secondClient, challenge); err != nil {
			secondDone <- err
			return
		}
		_, err := secondClient.Write(replayedProof)
		secondDone <- err
	}()
	if _, err := secondConnection.Read(applicationByte); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("replayed proof Read() error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestWrappedListenerRejectsLegacyRawTokenPreamble(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	wrapper, err := WrapListener(&singleUseListener{connection: server}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := wrapper.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	legacy := []byte("KAGENT-MANAGED-RUNTIME/1 " + testToken + "\n")
	legacy = append(legacy, bytes.Repeat([]byte{'x'}, clientHelloLength-len(legacy))...)
	go func() { _, _ = client.Write(legacy) }()
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); !errors.Is(err, ErrAuthentication) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("Read() error = %v", err)
	}
}

func TestValidateToken(t *testing.T) {
	if err := ValidateToken(testToken); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if err := ValidateToken(token); !errors.Is(err, ErrInvalidToken) || strings.Contains(err.Error(), token) && token != "" {
			t.Fatalf("ValidateToken(%q) error = %v", token, err)
		}
	}
}

func performClientHandshake(connection net.Conn, token, clientNonce string) error {
	hello := clientHelloMessage(clientNonce)
	if bytes.Contains(hello, []byte(token)) {
		return errors.New("client hello disclosed the raw transport token")
	}
	if _, err := connection.Write(hello); err != nil {
		return err
	}
	clientProof, err := clientProofForChallenge(connection, token, clientNonce)
	if err != nil {
		return err
	}
	if bytes.Contains(clientProof, []byte(token)) {
		return errors.New("client proof disclosed the raw transport token")
	}
	_, err = connection.Write(clientProof)
	return err
}

func clientHelloMessage(nonce string) []byte {
	result := make([]byte, 0, clientHelloLength)
	result = append(result, clientHelloPrefix...)
	result = append(result, nonce...)
	return append(result, proofSuffix...)
}

func clientProofForChallenge(connection net.Conn, token, clientNonce string) ([]byte, error) {
	challenge := make([]byte, serverChallengeLength)
	if _, err := io.ReadFull(connection, challenge); err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(challenge, []byte(serverChallengePrefix)) || challenge[len(challenge)-1] != '\n' {
		return nil, errors.New("server challenge has an invalid frame")
	}
	serverNonceStart := len(serverChallengePrefix)
	serverNonceEnd := serverNonceStart + hexValueLength
	serverNonce := challenge[serverNonceStart:serverNonceEnd]
	proofStart := serverNonceEnd + len(proofSeparator)
	providedHex := challenge[proofStart : proofStart+hexValueLength]
	if !hexValuePattern.Match(serverNonce) || !hexValuePattern.Match(providedHex) {
		return nil, errors.New("server challenge has invalid hex")
	}
	key, err := hex.DecodeString(token)
	if err != nil {
		return nil, err
	}
	provided := make([]byte, sha256.Size)
	if _, err := hex.Decode(provided, providedHex); err != nil {
		clear(key)
		return nil, err
	}
	expected := proof(key, serverProofLabel, []byte(clientNonce), serverNonce)
	if !hmac.Equal(provided, expected) {
		clear(key)
		clear(provided)
		clear(expected)
		return nil, errors.New("server challenge proof is invalid")
	}
	clear(provided)
	clear(expected)
	proofBytes := proof(key, clientProofLabel, []byte(clientNonce), serverNonce)
	result := make([]byte, 0, clientProofLength)
	result = append(result, clientProofPrefix...)
	result = hex.AppendEncode(result, proofBytes)
	result = append(result, proofSuffix...)
	clear(key)
	clear(proofBytes)
	if len(result) != clientProofLength {
		return nil, fmt.Errorf("client proof length = %d", len(result))
	}
	return result, nil
}

type singleUseListener struct {
	connection net.Conn
	accepted   bool
}

func (listener *singleUseListener) Accept() (net.Conn, error) {
	if listener.accepted {
		return nil, net.ErrClosed
	}
	listener.accepted = true
	return listener.connection, nil
}

func (*singleUseListener) Close() error   { return nil }
func (*singleUseListener) Addr() net.Addr { return pipeAddress{} }

type pipeAddress struct{}

func (pipeAddress) Network() string { return "pipe" }
func (pipeAddress) String() string  { return "pipe" }
