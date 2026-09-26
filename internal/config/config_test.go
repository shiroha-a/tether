package config

import (
	"testing"
	"time"
)

func setEnv(t *testing.T, idle string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("TETHER_ROOT", dir)
	t.Setenv("TETHER_DATA_DIR", t.TempDir())
	t.Setenv("TETHER_IDLE_TIMEOUT", idle)
	t.Setenv("TETHER_AUTH", "")
	t.Setenv("TETHER_TOKEN", "")
}

func TestIdleTimeoutDefaultsTo24h(t *testing.T) {
	setEnv(t, "")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.IdleTimeout != 24*time.Hour {
		t.Fatalf("IdleTimeout = %v, want 24h", c.IdleTimeout)
	}
}

func TestIdleTimeoutOverride(t *testing.T) {
	setEnv(t, "0")
	if c, err := Load(); err != nil || c.IdleTimeout != 0 {
		t.Fatalf("TETHER_IDLE_TIMEOUT=0: %v %v", c, err)
	}
	setEnv(t, "90m")
	if c, err := Load(); err != nil || c.IdleTimeout != 90*time.Minute {
		t.Fatalf("TETHER_IDLE_TIMEOUT=90m: %v %v", c, err)
	}
	setEnv(t, "forever")
	if _, err := Load(); err == nil {
		t.Fatal("invalid duration accepted")
	}
}

func TestAuthDefaultsOff(t *testing.T) {
	setEnv(t, "")
	t.Setenv("TETHER_AUTH", "")
	// トークンが設定されていても、TETHER_AUTHを指定しなければ認証は無効
	t.Setenv("TETHER_TOKEN", "secret")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth || c.AuthToken() != "" {
		t.Fatalf("auth should default to off (auth=%v, token passed to server=%q)", c.Auth, c.AuthToken())
	}
}

func TestAuthOn(t *testing.T) {
	for _, v := range []string{"on", "ON", "true", "1", "yes"} {
		setEnv(t, "")
		t.Setenv("TETHER_AUTH", v)
		t.Setenv("TETHER_TOKEN", "secret")
		c, err := Load()
		if err != nil || !c.Auth || c.AuthToken() != "secret" {
			t.Fatalf("TETHER_AUTH=%s: auth=%v token=%q err=%v", v, c != nil && c.Auth, c.Token, err)
		}
	}
	for _, v := range []string{"off", "false", "0", "no"} {
		setEnv(t, "")
		t.Setenv("TETHER_AUTH", v)
		if c, err := Load(); err != nil || c.Auth {
			t.Fatalf("TETHER_AUTH=%s: %v %v", v, c, err)
		}
	}
}

func TestAuthOnWithoutTokenFails(t *testing.T) {
	setEnv(t, "")
	t.Setenv("TETHER_AUTH", "on")
	t.Setenv("TETHER_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("TETHER_AUTH=on without a token must fail")
	}
}

func TestAuthInvalidValue(t *testing.T) {
	setEnv(t, "")
	t.Setenv("TETHER_AUTH", "maybe")
	t.Setenv("TETHER_TOKEN", "secret")
	if _, err := Load(); err == nil {
		t.Fatal("invalid TETHER_AUTH accepted")
	}
}
