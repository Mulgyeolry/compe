package authn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const verificationCodeRange = uint32(1_000_000)

type Manager struct {
	secret []byte
}

func New(secret string) (*Manager, error) {
	if len(secret) < 32 {
		return nil, errors.New("APP_SECRET must contain at least 32 characters")
	}
	return &Manager{secret: []byte(secret)}, nil
}

func (m *Manager) NewVerificationCode() (string, error) {
	var buffer [4]byte
	for {
		if _, err := rand.Read(buffer[:]); err != nil {
			return "", fmt.Errorf("generate verification code: %w", err)
		}
		value := binary.BigEndian.Uint32(buffer[:])
		limit := ^uint32(0) - (^uint32(0) % verificationCodeRange)
		if value < limit {
			return fmt.Sprintf("%06d", value%verificationCodeRange), nil
		}
	}
}

func (m *Manager) VerificationHash(email, code string) string {
	return m.hmacHex("verification", strings.ToLower(strings.TrimSpace(email)), code)
}

func (m *Manager) NewSessionToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (m *Manager) SessionHash(token string) string {
	return m.hmacHex("session", token)
}

func (m *Manager) CSRFToken(sessionToken string) string {
	return m.hmacHex("csrf", sessionToken)
}

func (m *Manager) VerifyCSRF(sessionToken, token string) bool {
	expected := m.CSRFToken(sessionToken)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(token)) == 1
}

func (m *Manager) UnsubscribeToken(userID int64, email string) string {
	payload := strconv.FormatInt(userID, 10) + ":" + strings.ToLower(strings.TrimSpace(email))
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return encoded + "." + m.hmacHex("unsubscribe", encoded)
}

func (m *Manager) VerifyUnsubscribeToken(token string) (int64, string, bool) {
	encoded, signature, found := strings.Cut(token, ".")
	if !found || encoded == "" || signature == "" {
		return 0, "", false
	}
	expected := m.hmacHex("unsubscribe", encoded)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
		return 0, "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, "", false
	}
	idText, email, found := strings.Cut(string(payload), ":")
	if !found || email == "" {
		return 0, "", false
	}
	userID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || userID < 1 {
		return 0, "", false
	}
	return userID, email, true
}

func (m *Manager) CompetitionChoiceToken(userID, competitionID int64, decision string) string {
	payload := fmt.Sprintf("%d:%d:%s", userID, competitionID, decision)
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return encoded + "." + m.hmacHex("competition-choice", encoded)
}

func (m *Manager) VerifyCompetitionChoiceToken(token string) (int64, int64, string, bool) {
	encoded, signature, found := strings.Cut(token, ".")
	if !found || encoded == "" || signature == "" {
		return 0, 0, "", false
	}
	expected := m.hmacHex("competition-choice", encoded)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
		return 0, 0, "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return 0, 0, "", false
	}
	parts := strings.Split(string(payload), ":")
	if len(parts) != 3 || (parts[2] != "participating" && parts[2] != "declined") {
		return 0, 0, "", false
	}
	userID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || userID < 1 {
		return 0, 0, "", false
	}
	competitionID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || competitionID < 1 {
		return 0, 0, "", false
	}
	return userID, competitionID, parts[2], true
}

func (m *Manager) hmacHex(parts ...string) string {
	mac := hmac.New(sha256.New, m.secret)
	for _, part := range parts {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil))
}
