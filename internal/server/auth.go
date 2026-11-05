package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/agoodley/pathgauge/internal/pki"
	"github.com/agoodley/pathgauge/internal/store"
)

// Roles, lowest to highest.
var roleRank = map[string]int{"viewer": 1, "operator": 2, "admin": 3}

// ValidRole reports whether a role name is known.
func ValidRole(r string) bool { return roleRank[r] > 0 }

// HashPassword returns an Argon2id PHC string.
func HashPassword(pw string) (string, error) {
	if len(pw) < 10 {
		return "", errors.New("password must be at least 10 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, 2, 64*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword verifies a password against an Argon2id PHC string.
func CheckPassword(hash, pw string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func randToken(prefix string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Principal is the authenticated caller.
type Principal struct {
	Name   string `json:"name"`
	Role   string `json:"role"`
	UserID int64  `json:"user_id,omitempty"`
	Token  bool   `json:"api_token,omitempty"`
}

type ctxKey struct{}

func principal(ctx context.Context) Principal {
	p, _ := ctx.Value(ctxKey{}).(Principal)
	return p
}

const sessionCookie = "pg_session"

// authenticate resolves a session cookie or bearer token.
func (s *Server) authenticate(r *http.Request) (Principal, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		name, role, err := s.store.APITokenRole(r.Context(), pki.HashSecret(strings.TrimPrefix(h, "Bearer ")))
		if err != nil {
			return Principal{}, false
		}
		return Principal{Name: "token:" + name, Role: role, Token: true}, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return Principal{}, false
	}
	u, err := s.store.SessionUser(r.Context(), pki.HashSecret(c.Value))
	if err != nil || u.Disabled {
		return Principal{}, false
	}
	return Principal{Name: u.Username, Role: u.Role, UserID: u.ID}, true
}

// require wraps a handler with authentication and a minimum role. Cookie
// sessions must send X-PathGauge on state-changing requests (CSRF
// protection on top of SameSite=Strict cookies).
func (s *Server) require(role string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(r)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "sign in required")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !p.Token && r.Header.Get("X-PathGauge") == "" {
			writeErr(w, http.StatusForbidden, "missing X-PathGauge header")
			return
		}
		if roleRank[p.Role] < roleRank[role] {
			writeErr(w, http.StatusForbidden, "this needs the "+role+" role")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginAllowed rate-limits failed logins per client address.
func (s *Server) loginAllowed(ip string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	var recent []time.Time
	for _, t := range s.loginFails[ip] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.loginFails[ip] = recent
	return len(recent) < 10
}

func (s *Server) loginFailed(ip string) {
	s.loginMu.Lock()
	s.loginFails[ip] = append(s.loginFails[ip], time.Now())
	s.loginMu.Unlock()
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User) error {
	tok := randToken("")
	if err := s.store.CreateSession(r.Context(), pki.HashSecret(tok), u.ID, 7*24*time.Hour); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 3600})
	return nil
}

func hostname() (string, error) { return os.Hostname() }
