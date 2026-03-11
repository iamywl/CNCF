package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Grafana Envelope Encryption (DEK/KEK 패턴) 시뮬레이션
// =============================================================================
//
// Grafana는 Envelope Encryption으로 민감한 데이터(데이터소스 비밀번호 등)를 보호한다.
// 핵심 패턴:
//   - DEK (Data Encryption Key): 실제 데이터를 암호화하는 키 (매번 새로 생성)
//   - KEK (Key Encryption Key): DEK를 암호화하는 마스터 키
//   - 저장 시: 데이터 → DEK로 암호화 → DEK는 KEK로 암호화 → 둘 다 저장
//   - 조회 시: KEK로 DEK 복호화 → DEK로 데이터 복호화
//
// 실제 코드 참조:
//   - pkg/services/encryption/: 암호화 서비스
//   - pkg/services/secrets/: 시크릿 관리
//   - pkg/services/kmsproviders/: KMS 통합
// =============================================================================

// --- KEK Provider (KMS 시뮬레이션) ---

type KEKProvider interface {
	Name() string
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// LocalKEKProvider는 로컬 마스터 키를 사용하는 KEK 프로바이더이다.
// 프로덕션에서는 AWS KMS, GCP KMS, HashiCorp Vault 등을 사용한다.
type LocalKEKProvider struct {
	name string
	key  []byte // 256-bit master key
}

func NewLocalKEKProvider(name string, masterSecret string) *LocalKEKProvider {
	// 마스터 시크릿에서 키 유도 (실제로는 PBKDF2/scrypt 사용)
	hash := sha256.Sum256([]byte(masterSecret))
	return &LocalKEKProvider{name: name, key: hash[:]}
}

func (p *LocalKEKProvider) Name() string { return p.name }

func (p *LocalKEKProvider) Encrypt(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(p.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (p *LocalKEKProvider) Decrypt(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(p.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

// --- DEK 생성 ---

func generateDEK() ([]byte, error) {
	dek := make([]byte, 32) // 256-bit
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, err
	}
	return dek, nil
}

// encryptWithDEK는 DEK로 데이터를 암호화한다 (AES-GCM).
func encryptWithDEK(dek, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptWithDEK(dek, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

// --- Encrypted Data 구조 ---

type EncryptedData struct {
	EncryptedDEK   string `json:"encrypted_dek"`   // KEK로 암호화된 DEK (base64)
	EncryptedValue string `json:"encrypted_value"` // DEK로 암호화된 데이터 (base64)
	KEKProvider    string `json:"kek_provider"`    // 사용된 KEK 프로바이더
	CreatedAt      time.Time `json:"created_at"`
}

// --- Envelope Encryption Service ---

type EnvelopeEncryptionService struct {
	mu           sync.RWMutex
	providers    map[string]KEKProvider
	activeKEK    string
	store        map[string]*EncryptedData // key -> encrypted data
	dekCache     map[string][]byte         // encrypted_dek -> decrypted dek (캐시)
	stats        EncryptionStats
}

type EncryptionStats struct {
	Encryptions   int
	Decryptions   int
	DEKGenerated  int
	DEKCacheHits  int
	DEKCacheMisses int
}

func NewEnvelopeEncryptionService(activeKEK string) *EnvelopeEncryptionService {
	return &EnvelopeEncryptionService{
		providers: make(map[string]KEKProvider),
		activeKEK: activeKEK,
		store:     make(map[string]*EncryptedData),
		dekCache:  make(map[string][]byte),
	}
}

func (svc *EnvelopeEncryptionService) RegisterProvider(provider KEKProvider) {
	svc.providers[provider.Name()] = provider
}

// Encrypt는 Envelope Encryption으로 데이터를 암호화한다.
func (svc *EnvelopeEncryptionService) Encrypt(key string, plaintext []byte) (*EncryptedData, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	provider, ok := svc.providers[svc.activeKEK]
	if !ok {
		return nil, fmt.Errorf("KEK provider %s not found", svc.activeKEK)
	}

	// 1. 새 DEK 생성
	dek, err := generateDEK()
	if err != nil {
		return nil, fmt.Errorf("failed to generate DEK: %v", err)
	}
	svc.stats.DEKGenerated++

	// 2. DEK로 데이터 암호화
	encryptedValue, err := encryptWithDEK(dek, plaintext)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt data: %v", err)
	}

	// 3. KEK로 DEK 암호화
	encryptedDEK, err := provider.Encrypt(dek)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt DEK: %v", err)
	}

	data := &EncryptedData{
		EncryptedDEK:   base64.StdEncoding.EncodeToString(encryptedDEK),
		EncryptedValue: base64.StdEncoding.EncodeToString(encryptedValue),
		KEKProvider:    provider.Name(),
		CreatedAt:      time.Now(),
	}

	svc.store[key] = data
	svc.stats.Encryptions++

	// DEK 캐시에 저장
	svc.dekCache[data.EncryptedDEK] = dek

	return data, nil
}

// Decrypt는 Envelope Encryption으로 데이터를 복호화한다.
func (svc *EnvelopeEncryptionService) Decrypt(key string) ([]byte, error) {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	data, ok := svc.store[key]
	if !ok {
		return nil, fmt.Errorf("key %s not found", key)
	}

	// 1. DEK 캐시 확인
	var dek []byte
	if cached, ok := svc.dekCache[data.EncryptedDEK]; ok {
		dek = cached
		svc.stats.DEKCacheHits++
	} else {
		// 2. KEK로 DEK 복호화
		provider, ok := svc.providers[data.KEKProvider]
		if !ok {
			return nil, fmt.Errorf("KEK provider %s not found", data.KEKProvider)
		}

		encryptedDEK, err := base64.StdEncoding.DecodeString(data.EncryptedDEK)
		if err != nil {
			return nil, err
		}

		dek, err = provider.Decrypt(encryptedDEK)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt DEK: %v", err)
		}

		svc.dekCache[data.EncryptedDEK] = dek
		svc.stats.DEKCacheMisses++
	}

	// 3. DEK로 데이터 복호화
	encryptedValue, err := base64.StdEncoding.DecodeString(data.EncryptedValue)
	if err != nil {
		return nil, err
	}

	plaintext, err := decryptWithDEK(dek, encryptedValue)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt data: %v", err)
	}

	svc.stats.Decryptions++
	return plaintext, nil
}

// RotateKEK는 KEK를 교체한다 (re-encryption).
func (svc *EnvelopeEncryptionService) RotateKEK(newProviderName string) error {
	svc.mu.Lock()
	defer svc.mu.Unlock()

	newProvider, ok := svc.providers[newProviderName]
	if !ok {
		return fmt.Errorf("new KEK provider %s not found", newProviderName)
	}

	fmt.Printf("    KEK rotation: %s -> %s\n", svc.activeKEK, newProviderName)

	rotated := 0
	for key, data := range svc.store {
		if data.KEKProvider == newProviderName {
			continue // 이미 새 KEK로 암호화됨
		}

		// 기존 KEK로 DEK 복호화
		oldProvider := svc.providers[data.KEKProvider]
		encryptedDEK, _ := base64.StdEncoding.DecodeString(data.EncryptedDEK)
		dek, err := oldProvider.Decrypt(encryptedDEK)
		if err != nil {
			return fmt.Errorf("failed to decrypt DEK for %s: %v", key, err)
		}

		// 새 KEK로 DEK 재암호화
		newEncryptedDEK, err := newProvider.Encrypt(dek)
		if err != nil {
			return fmt.Errorf("failed to re-encrypt DEK for %s: %v", key, err)
		}

		data.EncryptedDEK = base64.StdEncoding.EncodeToString(newEncryptedDEK)
		data.KEKProvider = newProviderName
		rotated++
	}

	svc.activeKEK = newProviderName
	// 캐시 무효화
	svc.dekCache = make(map[string][]byte)

	fmt.Printf("    Rotated %d entries\n", rotated)
	return nil
}

func main() {
	fmt.Println("=== Grafana Envelope Encryption (DEK/KEK) 시뮬레이션 ===")
	fmt.Println()

	// --- KEK Provider 등록 ---
	fmt.Println("[1] KEK Provider 등록")
	fmt.Println(strings.Repeat("-", 60))

	svc := NewEnvelopeEncryptionService("local-kek-v1")
	svc.RegisterProvider(NewLocalKEKProvider("local-kek-v1", "grafana-secret-key-v1"))
	svc.RegisterProvider(NewLocalKEKProvider("local-kek-v2", "grafana-secret-key-v2-rotated"))
	fmt.Println("  Registered: local-kek-v1 (active)")
	fmt.Println("  Registered: local-kek-v2 (standby)")
	fmt.Println()

	// --- 데이터소스 시크릿 암호화 ---
	fmt.Println("[2] 데이터소스 시크릿 암호화")
	fmt.Println(strings.Repeat("-", 60))

	secrets := map[string]string{
		"ds.prometheus.password":     "prom-secret-123!",
		"ds.mysql.password":          "mysql-root-p@ss",
		"ds.elasticsearch.api_key":   "ES-API-KEY-abcdef1234567890",
		"ds.cloudwatch.secret_key":   "aws-secret-access-key-xyz",
		"plugin.slack.webhook_url":   "https://hooks.slack.com/services/T00000000/B00000000/XXXX",
	}

	for key, value := range secrets {
		data, err := svc.Encrypt(key, []byte(value))
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			continue
		}
		fmt.Printf("  %s:\n", key)
		fmt.Printf("    KEK Provider: %s\n", data.KEKProvider)
		fmt.Printf("    Encrypted DEK: %s...\n", data.EncryptedDEK[:32])
		fmt.Printf("    Encrypted Value: %s...\n", data.EncryptedValue[:32])
	}
	fmt.Println()

	// --- 복호화 ---
	fmt.Println("[3] 시크릿 복호화")
	fmt.Println(strings.Repeat("-", 60))

	for key, originalValue := range secrets {
		decrypted, err := svc.Decrypt(key)
		if err != nil {
			fmt.Printf("  Error decrypting %s: %v\n", key, err)
			continue
		}
		match := string(decrypted) == originalValue
		masked := strings.Repeat("*", len(originalValue)-4) + originalValue[len(originalValue)-4:]
		fmt.Printf("  %s: %s (match=%v)\n", key, masked, match)
	}
	fmt.Println()

	// --- DEK 독립성 검증 ---
	fmt.Println("[4] DEK 독립성 검증 (각 시크릿마다 다른 DEK)")
	fmt.Println(strings.Repeat("-", 60))

	dekSet := make(map[string]bool)
	for key := range secrets {
		data := svc.store[key]
		dekHash := sha256.Sum256([]byte(data.EncryptedDEK))
		dekID := hex.EncodeToString(dekHash[:8])
		isDuplicate := dekSet[dekID]
		dekSet[dekID] = true
		fmt.Printf("  %s -> DEK hash: %s (duplicate=%v)\n", key, dekID, isDuplicate)
	}
	fmt.Println()

	// --- KEK 로테이션 ---
	fmt.Println("[5] KEK 로테이션")
	fmt.Println(strings.Repeat("-", 60))

	err := svc.RotateKEK("local-kek-v2")
	if err != nil {
		fmt.Printf("  Error: %v\n", err)
	}
	fmt.Println()

	// 로테이션 후 복호화 검증
	fmt.Println("[6] 로테이션 후 복호화 검증")
	fmt.Println(strings.Repeat("-", 60))

	allMatch := true
	for key, originalValue := range secrets {
		decrypted, err := svc.Decrypt(key)
		if err != nil {
			fmt.Printf("  Error: %v\n", err)
			allMatch = false
			continue
		}
		match := string(decrypted) == originalValue
		if !match {
			allMatch = false
		}
		fmt.Printf("  %s: provider=%s, match=%v\n", key, svc.store[key].KEKProvider, match)
	}
	fmt.Printf("\n  모든 시크릿 복호화 성공: %v\n", allMatch)
	fmt.Println()

	// --- 통계 ---
	fmt.Println("[7] 암호화 통계")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("  Encryptions: %d\n", svc.stats.Encryptions)
	fmt.Printf("  Decryptions: %d\n", svc.stats.Decryptions)
	fmt.Printf("  DEK Generated: %d\n", svc.stats.DEKGenerated)
	fmt.Printf("  DEK Cache Hits: %d\n", svc.stats.DEKCacheHits)
	fmt.Printf("  DEK Cache Misses: %d\n", svc.stats.DEKCacheMisses)
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
