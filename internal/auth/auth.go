// Package auth provides the user accounts, login sessions and the per-user
// credential vault for the dashboard. It is deliberately stdlib-only:
// passwords are PBKDF2-HMAC-SHA256 (implemented in this file so the project
// keeps its zero-extra-dependency rule), sessions are HMAC-signed tokens in a
// cookie, and everything persists to a single 0600 JSON file so a deployment
// has no database to run.
//
// Per-user credentials (Binance API keys, the LLM endpoint and key) live in
// the same vault. They are injected into the session config by the webui
// layer at start time and are never echoed back: readers get booleans and
// the LLM base URL, not the secret values.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PasswordHashIterations is the default PBKDF2 round count. The count is
// stored per user, so it can be raised later without invalidating accounts.
const PasswordHashIterations = 250_000

// SessionTTL is how long a login cookie stays valid.
const SessionTTL = 24 * time.Hour

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,32}$`)

// User is a snapshot of one account, safe to hand to the trading layer.
// The service is the only writer of the underlying record.
type User struct {
	ID           int
	Username     string
	BinAPIKey    string
	BinSecretKey string
	LLMBaseURL   string
	LLMModel     string
	LLMAPIKey    string
}

// Status reports which credentials are set, without exposing their values.
type Status struct {
	BinAPIKey    bool
	BinSecretKey bool
	LLMAPIKey    bool
	LLMBaseURL   string
	LLMModel     string
}

type userRecord struct {
	ID           int
	Username     string
	Salt         string
	Hash         string
	Iterations   int
	BinAPIKey    string
	BinSecretKey string
	LLMBaseURL   string
	LLMModel     string
	LLMAPIKey    string
	CreatedAt    time.Time
}

type userFile struct {
	SecretHex string
	NextID    int
	Users     []userRecord
}

// Service is the account + credential store.
type Service struct {
	mu     sync.RWMutex
	path   string
	secret []byte
	nextID int
	byName map[string]int
	users  map[int]*userRecord
}

// New opens the vault at path, creating it (with a fresh 32-byte HMAC
// secret) when it does not exist yet.
func New(path string) (*Service, error) {
	s := &Service{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		secret := make([]byte, 32)
		if _, rerr := io.ReadFull(rand.Reader, secret); rerr != nil {
			return nil, rerr
		}
		s.secret = secret
		s.nextID = 1
		s.byName = map[string]int{}
		s.users = map[int]*userRecord{}
		if serr := s.saveLocked(); serr != nil {
			return nil, serr
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var file userFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse user vault %s: %w", path, err)
	}
	secret, err := hex.DecodeString(file.SecretHex)
	if err != nil || len(secret) != 32 {
		return nil, fmt.Errorf("user vault %s has no valid HMAC secret", path)
	}
	s.secret = secret
	s.nextID = max(file.NextID, 1)
	s.byName = map[string]int{}
	s.users = map[int]*userRecord{}
	for _, u := range file.Users {
		if u.Username == "" || u.Salt == "" || u.Hash == "" {
			return nil, fmt.Errorf("user vault %s has an incomplete record", path)
		}
		ur := u
		s.users[ur.ID] = &ur
		s.byName[ur.Username] = ur.ID
		if ur.ID >= s.nextID {
			s.nextID = ur.ID + 1
		}
	}
	return s, nil
}

func (s *Service) saveLocked() error {
	var file userFile
	file.NextID = s.nextID
	for _, ur := range s.users {
		file.Users = append(file.Users, *ur)
	}
	file.SecretHex = hex.EncodeToString(s.secret)
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Register creates a new account.
func (s *Service) Register(username, password string) (*User, error) {
	if !usernamePattern.MatchString(username) {
		return nil, fmt.Errorf("用户名须为 3–32 位，仅字母/数字/下划线/中划线/点")
	}
	if len(password) < 6 {
		return nil, fmt.Errorf("密码至少 6 位")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byName[username]; exists {
		return nil, fmt.Errorf("用户名 %q 已被占用", username)
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	hash := base64.StdEncoding.EncodeToString(pbkdf2SHA256([]byte(password), salt, PasswordHashIterations, 32))
	ur := &userRecord{
		ID: s.nextID, Username: username,
		Salt: base64.StdEncoding.EncodeToString(salt), Hash: hash,
		Iterations: PasswordHashIterations, CreatedAt: time.Now().UTC(),
	}
	s.users[ur.ID] = ur
	s.byName[username] = ur.ID
	s.nextID++
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return s.userCopyLocked(ur), nil
}

// Authenticate checks the password and returns the account.
func (s *Service) Authenticate(username, password string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[username]
	if !ok {
		// Burn the same hashing cost so the response time does not reveal
		// whether the username exists.
		pbkdf2SHA256([]byte(password), make([]byte, 16), PasswordHashIterations, 32)
		return nil, errors.New("用户名或密码错误")
	}
	return s.authenticateLocked(username, s.users[id], password)
}

func (s *Service) authenticateLocked(username string, ur *userRecord, password string) (*User, error) {
	salt, err := base64.StdEncoding.DecodeString(ur.Salt)
	if err != nil {
		return nil, fmt.Errorf("凭证库损坏: %v", err)
	}
	want, err := base64.StdEncoding.DecodeString(ur.Hash)
	if err != nil {
		return nil, fmt.Errorf("凭证库损坏: %v", err)
	}
	iterations := ur.Iterations
	if iterations <= 0 {
		iterations = PasswordHashIterations
	}
	got := pbkdf2SHA256([]byte(password), salt, iterations, len(want))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return nil, errors.New("用户名或密码错误")
	}
	return s.userCopyLocked(ur), nil
}

// SetCredentials stores per-user credentials. Empty values leave the stored
// value unchanged, so the form can post only what the user typed.
func (s *Service) SetCredentials(username, binKey, binSecret, llmURL, llmModel, llmKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byName[username]
	if !ok {
		return fmt.Errorf("用户 %q 不存在", username)
	}
	ur := s.users[id]
	if binKey != "" {
		ur.BinAPIKey = binKey
	}
	if binSecret != "" {
		ur.BinSecretKey = binSecret
	}
	if llmURL != "" {
		ur.LLMBaseURL = llmURL
	}
	if llmKey != "" {
		ur.LLMAPIKey = llmKey
	}
	if llmModel != "" {
		ur.LLMModel = llmModel
	}
	return s.saveLocked()
}

// Status reports which of the user's credentials are set.
func (s *Service) Status(username string) (Status, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[username]
	if !ok {
		return Status{}, fmt.Errorf("用户 %q 不存在", username)
	}
	ur := s.users[id]
	return Status{
		BinAPIKey:    ur.BinAPIKey != "", BinSecretKey: ur.BinSecretKey != "",
		LLMAPIKey:    ur.LLMAPIKey != "", LLMBaseURL: ur.LLMBaseURL, LLMModel: ur.LLMModel,
	}, nil
}

// Get returns a snapshot of the account's credentials for the trading layer.
func (s *Service) Get(username string) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[username]
	if !ok {
		return nil, false
	}
	return s.userCopyLocked(s.users[id]), true
}

func (s *Service) userCopyLocked(ur *userRecord) *User {
	return &User{
		ID: ur.ID, Username: ur.Username,
		BinAPIKey:    ur.BinAPIKey, BinSecretKey: ur.BinSecretKey,
		LLMBaseURL:   ur.LLMBaseURL, LLMModel: ur.LLMModel, LLMAPIKey: ur.LLMAPIKey,
	}
}

// IssueSession returns a signed session token for the cookie.
func (s *Service) IssueSession(username string, ttl time.Duration) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.byName[username]; !ok {
		return "", fmt.Errorf("用户 %q 不存在", username)
	}
	nonce := make([]byte, 8)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	body := fmt.Sprintf("%s|%d|%s", username, time.Now().Add(ttl).Unix(), hex.EncodeToString(nonce))
	payload := base64.RawURLEncoding.EncodeToString([]byte(body))
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil)), nil
}

// VerifySession checks the signature and expiry of a session token.
func (s *Service) VerifySession(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	payload, sig, found := strings.Cut(token, ".")
	if !found || payload == "" || sig == "" {
		return "", false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(payload))
	sigBytes, err := hex.DecodeString(sig)
	if err != nil || subtle.ConstantTimeCompare(mac.Sum(nil), sigBytes) != 1 {
		return "", false
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	fields := strings.Split(string(body), "|")
	if len(fields) != 3 || fields[0] == "" {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	// The account must still exist: a deleted user's token is dead.
	if _, ok := s.byName[fields[0]]; !ok {
		return "", false
	}
	return fields[0], true
}

// ---------------------------------------------------------------- hashing

// pbkdf2SHA256 implements RFC 2898 PBKDF2 with HMAC-SHA256 on top of the
// standard library, so no external crypto package is needed.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	out := make([]byte, 0, keyLen)
	for block := 1; len(out) < keyLen; block++ {
		u := hmacSHA256(password, append(salt, uint32Bytes(uint32(block))...))
		accum := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			u = hmacSHA256(password, u)
			for j := range accum {
				accum[j] ^= u[j]
			}
		}
		out = append(out, accum...)
	}
	return out[:keyLen]
}

func hmacSHA256(key, msg []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(msg)
	return mac.Sum(nil)
}

func uint32Bytes(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}
