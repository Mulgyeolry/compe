package authn

import "testing"

func TestManagerTokens(t *testing.T) {
	manager, err := New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	code, err := manager.NewVerificationCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 6 {
		t.Fatalf("verification code length = %d", len(code))
	}
	token, err := manager.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if token == "" || manager.SessionHash(token) == manager.SessionHash(token+"x") {
		t.Fatal("session hash is not bound to token")
	}
	csrf := manager.CSRFToken(token)
	if !manager.VerifyCSRF(token, csrf) || manager.VerifyCSRF(token+"x", csrf) {
		t.Fatal("CSRF validation failed")
	}
}

func TestUnsubscribeTokenRejectsTampering(t *testing.T) {
	manager, err := New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	token := manager.UnsubscribeToken(42, "USER@example.com")
	userID, email, valid := manager.VerifyUnsubscribeToken(token)
	if !valid || userID != 42 || email != "user@example.com" {
		t.Fatalf("unexpected token result: %d %q %v", userID, email, valid)
	}
	if _, _, valid := manager.VerifyUnsubscribeToken(token + "x"); valid {
		t.Fatal("tampered token was accepted")
	}
}

func TestCompetitionChoiceTokenRejectsTampering(t *testing.T) {
	manager, err := New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	token := manager.CompetitionChoiceToken(42, 99, "participating")
	userID, competitionID, decision, valid := manager.VerifyCompetitionChoiceToken(token)
	if !valid || userID != 42 || competitionID != 99 || decision != "participating" {
		t.Fatalf("unexpected choice token values: user=%d competition=%d decision=%q valid=%v", userID, competitionID, decision, valid)
	}
	if _, _, _, valid := manager.VerifyCompetitionChoiceToken(token + "x"); valid {
		t.Fatal("tampered choice token was accepted")
	}
}

func TestShortSecretRejected(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("short APP_SECRET must be rejected")
	}
}
