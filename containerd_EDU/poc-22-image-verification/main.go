package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// =============================================================================
// containerd 이미지 서명 검증 파이프라인 시뮬레이션
// =============================================================================
//
// containerd는 이미지 pull 시 서명을 검증하여 신뢰할 수 있는 이미지만 실행한다.
// Cosign/Notation 등 sigstore 생태계와 통합된다.
//
// 핵심 동작:
//   - 이미지 매니페스트 다이제스트 계산
//   - ECDSA 서명 생성/검증
//   - 신뢰 정책(Trust Policy): 어떤 이미지에 어떤 검증을 적용할지
//   - 검증 파이프라인: 다이제스트 → 서명 조회 → 키 검증 → 정책 평가
//
// 실제 코드 참조:
//   - pkg/imgcrypt/: 이미지 암호화/복호화
//   - Cosign 통합: sigstore/cosign
// =============================================================================

// --- 이미지 모델 ---

type ImageManifest struct {
	MediaType string `json:"mediaType"`
	Config    Descriptor `json:"config"`
	Layers    []Descriptor `json:"layers"`
}

type Descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type ImageReference struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string // sha256:...
}

func (r ImageReference) String() string {
	if r.Digest != "" {
		return fmt.Sprintf("%s/%s@%s", r.Registry, r.Repository, r.Digest)
	}
	return fmt.Sprintf("%s/%s:%s", r.Registry, r.Repository, r.Tag)
}

// --- 서명 모델 ---

type Signature struct {
	Payload    string `json:"payload"` // base64 encoded digest
	SignatureB64 string `json:"signature"` // base64 encoded ECDSA signature
	KeyID      string `json:"keyID"`
	Algorithm  string `json:"algorithm"`
	Timestamp  time.Time `json:"timestamp"`
}

// --- 키 관리 ---

type KeyPair struct {
	ID         string
	PrivateKey *ecdsa.PrivateKey
	PublicKey  *ecdsa.PublicKey
	Owner      string
}

func GenerateKeyPair(id, owner string) (*KeyPair, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &KeyPair{
		ID:         id,
		PrivateKey: privateKey,
		PublicKey:  &privateKey.PublicKey,
		Owner:      owner,
	}, nil
}

// --- 서명 생성 ---

func SignDigest(digest string, kp *KeyPair) (*Signature, error) {
	hash := sha256.Sum256([]byte(digest))
	r, s, err := ecdsa.Sign(rand.Reader, kp.PrivateKey, hash[:])
	if err != nil {
		return nil, err
	}

	// r,s를 바이트로 인코딩
	sigBytes := append(r.Bytes(), s.Bytes()...)

	return &Signature{
		Payload:      base64.StdEncoding.EncodeToString([]byte(digest)),
		SignatureB64: base64.StdEncoding.EncodeToString(sigBytes),
		KeyID:        kp.ID,
		Algorithm:    "ECDSA-P256-SHA256",
		Timestamp:    time.Now(),
	}, nil
}

// --- 서명 검증 ---

func VerifySignature(sig *Signature, pubKey *ecdsa.PublicKey) (bool, error) {
	// 페이로드 디코드
	payloadBytes, err := base64.StdEncoding.DecodeString(sig.Payload)
	if err != nil {
		return false, fmt.Errorf("invalid payload encoding: %v", err)
	}

	// 서명 디코드
	sigBytes, err := base64.StdEncoding.DecodeString(sig.SignatureB64)
	if err != nil {
		return false, fmt.Errorf("invalid signature encoding: %v", err)
	}

	if len(sigBytes) < 2 {
		return false, fmt.Errorf("signature too short")
	}

	mid := len(sigBytes) / 2
	r := new(big.Int).SetBytes(sigBytes[:mid])
	s := new(big.Int).SetBytes(sigBytes[mid:])

	hash := sha256.Sum256(payloadBytes)
	valid := ecdsa.Verify(pubKey, hash[:], r, s)
	return valid, nil
}

// --- Trust Policy ---

type TrustLevel int

const (
	TrustDeny   TrustLevel = iota
	TrustPermit            // 서명 없이도 허용
	TrustVerify            // 서명 검증 필수
)

func (t TrustLevel) String() string {
	switch t {
	case TrustDeny:
		return "DENY"
	case TrustPermit:
		return "PERMIT"
	case TrustVerify:
		return "VERIFY"
	}
	return "UNKNOWN"
}

type TrustPolicyRule struct {
	Pattern    string     // 레지스트리/리포 패턴 (glob)
	TrustLevel TrustLevel
	KeyIDs     []string   // 허용된 서명 키 ID
}

type TrustPolicy struct {
	Rules   []TrustPolicyRule
	Default TrustLevel
}

func (tp *TrustPolicy) Evaluate(ref ImageReference) (TrustLevel, []string) {
	imageStr := ref.Registry + "/" + ref.Repository
	for _, rule := range tp.Rules {
		if matchPattern(rule.Pattern, imageStr) {
			return rule.TrustLevel, rule.KeyIDs
		}
	}
	return tp.Default, nil
}

func matchPattern(pattern, str string) bool {
	if strings.HasSuffix(pattern, "/*") {
		prefix := strings.TrimSuffix(pattern, "/*")
		return strings.HasPrefix(str, prefix+"/")
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return strings.HasPrefix(str, prefix)
	}
	return pattern == str
}

// --- 서명 저장소 (Cosign 스타일) ---

type SignatureStore struct {
	signatures map[string][]*Signature // digest -> signatures
}

func NewSignatureStore() *SignatureStore {
	return &SignatureStore{signatures: make(map[string][]*Signature)}
}

func (ss *SignatureStore) Store(digest string, sig *Signature) {
	ss.signatures[digest] = append(ss.signatures[digest], sig)
}

func (ss *SignatureStore) Lookup(digest string) []*Signature {
	return ss.signatures[digest]
}

// --- 검증 파이프라인 ---

type VerificationResult struct {
	Image   ImageReference
	Digest  string
	Status  string
	Details string
}

type VerificationPipeline struct {
	policy   *TrustPolicy
	sigStore *SignatureStore
	keys     map[string]*ecdsa.PublicKey
}

func NewVerificationPipeline(policy *TrustPolicy, store *SignatureStore) *VerificationPipeline {
	return &VerificationPipeline{
		policy:   policy,
		sigStore: store,
		keys:     make(map[string]*ecdsa.PublicKey),
	}
}

func (vp *VerificationPipeline) AddTrustedKey(id string, key *ecdsa.PublicKey) {
	vp.keys[id] = key
}

func (vp *VerificationPipeline) Verify(ref ImageReference, digest string) VerificationResult {
	result := VerificationResult{Image: ref, Digest: digest}

	// 1. Trust Policy 평가
	level, allowedKeys := vp.policy.Evaluate(ref)
	fmt.Printf("    Policy: %s (level=%s, keys=%v)\n", ref, level, allowedKeys)

	switch level {
	case TrustDeny:
		result.Status = "DENIED"
		result.Details = "이미지가 정책에 의해 차단됨"
		return result
	case TrustPermit:
		result.Status = "PERMITTED"
		result.Details = "서명 검증 없이 허용"
		return result
	}

	// 2. 서명 조회
	sigs := vp.sigStore.Lookup(digest)
	if len(sigs) == 0 {
		result.Status = "FAILED"
		result.Details = "서명을 찾을 수 없음"
		return result
	}
	fmt.Printf("    Found %d signature(s) for digest\n", len(sigs))

	// 3. 서명 검증
	for _, sig := range sigs {
		// 키가 허용 목록에 있는지 확인
		keyAllowed := len(allowedKeys) == 0
		for _, kid := range allowedKeys {
			if kid == sig.KeyID {
				keyAllowed = true
				break
			}
		}
		if !keyAllowed {
			fmt.Printf("    Key %s not in allowed list, skipping\n", sig.KeyID)
			continue
		}

		pubKey, ok := vp.keys[sig.KeyID]
		if !ok {
			fmt.Printf("    Key %s not found in keyring\n", sig.KeyID)
			continue
		}

		valid, err := VerifySignature(sig, pubKey)
		if err != nil {
			fmt.Printf("    Verification error: %v\n", err)
			continue
		}
		if valid {
			result.Status = "VERIFIED"
			result.Details = fmt.Sprintf("서명 검증 성공 (key=%s, algo=%s)", sig.KeyID, sig.Algorithm)
			return result
		}
	}

	result.Status = "FAILED"
	result.Details = "유효한 서명을 찾지 못함"
	return result
}

func computeManifestDigest(manifest ImageManifest) string {
	data, _ := json.Marshal(manifest)
	hash := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", hash)
}

func main() {
	fmt.Println("=== containerd 이미지 서명 검증 시뮬레이션 ===")
	fmt.Println()

	// --- 키 생성 ---
	fmt.Println("[1] 서명 키 생성")
	fmt.Println(strings.Repeat("-", 60))

	releaseKey, _ := GenerateKeyPair("release-key-001", "release-team")
	devKey, _ := GenerateKeyPair("dev-key-001", "dev-team")
	unknownKey, _ := GenerateKeyPair("unknown-key", "unknown")

	for _, kp := range []*KeyPair{releaseKey, devKey, unknownKey} {
		fmt.Printf("  Key: %s (owner: %s, curve: P-256)\n", kp.ID, kp.Owner)
	}
	fmt.Println()

	// --- 이미지 매니페스트 ---
	fmt.Println("[2] 이미지 매니페스트 생성 + 다이제스트 계산")
	fmt.Println(strings.Repeat("-", 60))

	images := []struct {
		ref      ImageReference
		manifest ImageManifest
	}{
		{
			ImageReference{"registry.example.com", "app/web", "v1.0", ""},
			ImageManifest{
				MediaType: "application/vnd.docker.distribution.manifest.v2+json",
				Config:    Descriptor{"application/vnd.docker.container.image.v1+json", "sha256:aaa111", 1234},
				Layers: []Descriptor{
					{"application/vnd.docker.image.rootfs.diff.tar.gzip", "sha256:bbb222", 50000},
					{"application/vnd.docker.image.rootfs.diff.tar.gzip", "sha256:ccc333", 30000},
				},
			},
		},
		{
			ImageReference{"registry.example.com", "app/api", "v2.0", ""},
			ImageManifest{
				MediaType: "application/vnd.docker.distribution.manifest.v2+json",
				Config:    Descriptor{"application/vnd.docker.container.image.v1+json", "sha256:ddd444", 1500},
				Layers:    []Descriptor{{MediaType: "layer", Digest: "sha256:eee555", Size: 70000}},
			},
		},
		{
			ImageReference{"docker.io", "library/nginx", "latest", ""},
			ImageManifest{
				MediaType: "application/vnd.docker.distribution.manifest.v2+json",
				Config:    Descriptor{Digest: "sha256:fff666", Size: 2000},
				Layers:    []Descriptor{{Digest: "sha256:ggg777", Size: 90000}},
			},
		},
		{
			ImageReference{"malicious.io", "bad/image", "latest", ""},
			ImageManifest{Config: Descriptor{Digest: "sha256:hhh888"}},
		},
	}

	sigStore := NewSignatureStore()
	digests := make([]string, len(images))

	for i, img := range images {
		digest := computeManifestDigest(img.manifest)
		digests[i] = digest
		img.ref.Digest = digest
		fmt.Printf("  %s -> %s\n", img.ref, digest[:32]+"...")
	}
	fmt.Println()

	// --- 서명 생성 ---
	fmt.Println("[3] 이미지 서명")
	fmt.Println(strings.Repeat("-", 60))

	// app/web: release key로 서명
	sig1, _ := SignDigest(digests[0], releaseKey)
	sigStore.Store(digests[0], sig1)
	fmt.Printf("  %s 서명 완료 (key: %s)\n", images[0].ref, releaseKey.ID)

	// app/api: dev key로 서명
	sig2, _ := SignDigest(digests[1], devKey)
	sigStore.Store(digests[1], sig2)
	fmt.Printf("  %s 서명 완료 (key: %s)\n", images[1].ref, devKey.ID)

	// nginx: 서명 없음
	fmt.Printf("  %s 서명 없음\n", images[2].ref)

	// malicious: unknown key로 서명
	sig4, _ := SignDigest(digests[3], unknownKey)
	sigStore.Store(digests[3], sig4)
	fmt.Printf("  %s 서명 완료 (key: %s, 비신뢰)\n", images[3].ref, unknownKey.ID)
	fmt.Println()

	// --- Trust Policy 설정 ---
	fmt.Println("[4] Trust Policy 설정")
	fmt.Println(strings.Repeat("-", 60))

	policy := &TrustPolicy{
		Rules: []TrustPolicyRule{
			{"registry.example.com/app/*", TrustVerify, []string{"release-key-001", "dev-key-001"}},
			{"docker.io/library/*", TrustPermit, nil},
			{"malicious.io/**", TrustDeny, nil},
		},
		Default: TrustVerify,
	}

	for _, rule := range policy.Rules {
		fmt.Printf("  Pattern: %-30s Level: %s Keys: %v\n", rule.Pattern, rule.TrustLevel, rule.KeyIDs)
	}
	fmt.Printf("  Default: %s\n", policy.Default)
	fmt.Println()

	// --- 검증 파이프라인 ---
	fmt.Println("[5] 이미지 검증 파이프라인 실행")
	fmt.Println(strings.Repeat("-", 60))

	pipeline := NewVerificationPipeline(policy, sigStore)
	pipeline.AddTrustedKey(releaseKey.ID, releaseKey.PublicKey)
	pipeline.AddTrustedKey(devKey.ID, devKey.PublicKey)
	// unknownKey는 신뢰 키링에 추가하지 않음

	for i, img := range images {
		fmt.Printf("\n  >> 검증: %s\n", img.ref)
		result := pipeline.Verify(img.ref, digests[i])
		icon := "+"
		if result.Status == "FAILED" || result.Status == "DENIED" {
			icon = "X"
		}
		fmt.Printf("  [%s] %s: %s\n", icon, result.Status, result.Details)
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
