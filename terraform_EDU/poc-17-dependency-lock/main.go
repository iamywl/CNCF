// Package main은 Terraform의 의존성 잠금 파일(Dependency Lock File) 시스템을
// Go 표준 라이브러리만으로 시뮬레이션하는 PoC이다.
//
// 시뮬레이션하는 핵심 개념:
// 1. Provider 잠금 정보 (버전, 제약 조건, 해시)
// 2. HCL 형식의 잠금 파일 생성/파싱
// 3. SHA256 기반 해시 계산 및 검증 (h1: / zh: 스킴)
// 4. 해시 정규화 (정렬, 중복 제거)
// 5. 잠금 파일 비교 (Equal, ContainsAll)
// 6. Provider 오버라이드 메커니즘
// 7. 원자적 파일 쓰기
//
// 실제 소스 참조:
//   - internal/depsfile/locks.go         (Locks, ProviderLock 구조체)
//   - internal/depsfile/locks_file.go    (HCL 파일 직렬화/역직렬화)
//   - internal/depsfile/paths.go         (파일 경로 상수)
//   - internal/getproviders/providerreqs/hash.go (Hash, HashScheme)
//   - internal/getproviders/hash.go      (PackageHashV1, PackageMatchesAnyHash)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ============================================================================
// 1. 해시 시스템 (Hash / HashScheme)
// ============================================================================

// Hash는 패키지 체크섬을 나타내는 문자열이다.
// 실제 코드: internal/getproviders/providerreqs/hash.go
type Hash string

const NilHash = Hash("")

// HashScheme은 해시 스킴을 나타내는 열거형이다.
type HashScheme string

const (
	// HashScheme1은 디렉토리 내용 해시 (h1:) 스킴이다.
	// 실제 Terraform에서는 Go Modules의 dirhash.Hash1을 사용한다.
	HashScheme1 HashScheme = "h1:"

	// HashSchemeZip은 .zip 아카이브 해시 (zh:) 스킴이다.
	// 레거시 호환용이다.
	HashSchemeZip HashScheme = "zh:"
)

// New는 스킴에 값을 결합하여 Hash를 생성한다.
func (hs HashScheme) New(value string) Hash {
	return Hash(string(hs) + value)
}

// ParseHash는 해시 문자열을 파싱한다.
func ParseHash(s string) (Hash, error) {
	colon := strings.Index(s, ":")
	if colon < 1 {
		return NilHash, fmt.Errorf("해시 문자열은 스킴 키워드와 콜론으로 시작해야 합니다")
	}
	return Hash(s), nil
}

// Scheme은 해시의 스킴을 반환한다.
func (h Hash) Scheme() HashScheme {
	colon := strings.Index(string(h), ":")
	if colon < 0 {
		panic(fmt.Sprintf("잘못된 해시 문자열 %q", h))
	}
	return HashScheme(h[:colon+1])
}

// Value는 해시의 스킴별 값을 반환한다.
func (h Hash) Value() string {
	colon := strings.Index(string(h), ":")
	if colon < 0 {
		panic(fmt.Sprintf("잘못된 해시 문자열 %q", h))
	}
	return string(h[colon+1:])
}

// String은 해시의 문자열 표현을 반환한다.
func (h Hash) String() string {
	return string(h)
}

// PreferredHashes는 지원되는 해시 스킴만 필터링한다.
// 실제 코드: internal/getproviders/providerreqs/hash.go PreferredHashes
func PreferredHashes(given []Hash) []Hash {
	var ret []Hash
	for _, hash := range given {
		switch hash.Scheme() {
		case HashScheme1, HashSchemeZip:
			ret = append(ret, hash)
		}
	}
	return ret
}

// ============================================================================
// 2. Provider 주소
// ============================================================================

// ProviderAddr은 Provider의 정규화된 주소이다.
// 실제 코드: internal/addrs/provider.go
type ProviderAddr struct {
	Hostname  string // 예: registry.terraform.io
	Namespace string // 예: hashicorp
	Type      string // 예: aws
}

func (p ProviderAddr) String() string {
	return fmt.Sprintf("%s/%s/%s", p.Hostname, p.Namespace, p.Type)
}

func (p ProviderAddr) LessThan(other ProviderAddr) bool {
	return p.String() < other.String()
}

// IsBuiltIn은 built-in Provider인지 확인한다.
func (p ProviderAddr) IsBuiltIn() bool {
	return p.Hostname == "terraform.io" && p.Namespace == "builtin"
}

// IsLegacy는 legacy Provider인지 확인한다.
func (p ProviderAddr) IsLegacy() bool {
	return p.Namespace == "-"
}

// ProviderIsLockable은 Provider가 잠금 대상인지 확인한다.
// 실제 코드: internal/depsfile/locks.go ProviderIsLockable
func ProviderIsLockable(addr ProviderAddr) bool {
	return !(addr.IsBuiltIn() || addr.IsLegacy())
}

// ============================================================================
// 3. ProviderLock (개별 Provider 잠금 정보)
// ============================================================================

// ProviderLock은 특정 Provider의 잠금 정보를 나타낸다.
// 실제 코드: internal/depsfile/locks.go ProviderLock
type ProviderLock struct {
	addr               ProviderAddr
	version            string
	versionConstraints string
	hashes             []Hash
}

// NewProviderLock은 ProviderLock을 생성하며, 해시를 정규화(정렬+중복제거)한다.
// 실제 코드: internal/depsfile/locks.go NewProviderLock
func NewProviderLock(addr ProviderAddr, version, constraints string, hashes []Hash) *ProviderLock {
	if !ProviderIsLockable(addr) {
		panic(fmt.Sprintf("잠금 불가능한 Provider: %s", addr))
	}

	// 1. 해시를 사전순으로 정렬
	sort.Slice(hashes, func(i, j int) bool {
		return string(hashes[i]) < string(hashes[j])
	})

	// 2. 인플레이스 중복 제거 (정렬 상태이므로 연속 중복만 확인)
	dedupeHashes := hashes[:0]
	prevHash := NilHash
	for _, hash := range hashes {
		if hash != prevHash {
			dedupeHashes = append(dedupeHashes, hash)
			prevHash = hash
		}
	}

	return &ProviderLock{
		addr:               addr,
		version:            version,
		versionConstraints: constraints,
		hashes:             dedupeHashes,
	}
}

// ContainsAll은 이 잠금의 해시가 target의 모든 해시를 포함하는지 확인한다.
// 정렬된 두 슬라이스의 포함 관계를 O(n+m)에 확인하는 투 포인터 알고리즘.
// 실제 코드: internal/depsfile/locks.go ContainsAll
func (l *ProviderLock) ContainsAll(target *ProviderLock) bool {
	if target == nil || len(target.hashes) == 0 {
		return true
	}

	targetIndex := 0
	for ix := 0; ix < len(l.hashes); ix++ {
		if l.hashes[ix] == target.hashes[targetIndex] {
			targetIndex++
			if targetIndex >= len(target.hashes) {
				return true
			}
		}
	}
	return false
}

// ============================================================================
// 4. Locks (전체 잠금 파일)
// ============================================================================

// Locks는 의존성 잠금 파일의 최상위 구조체이다.
// 실제 코드: internal/depsfile/locks.go Locks
type Locks struct {
	providers           map[string]*ProviderLock       // Provider 주소 -> 잠금 정보
	overriddenProviders map[string]struct{}             // 오버라이드된 Provider (메모리 전용)
}

// NewLocks는 빈 Locks 객체를 생성한다.
func NewLocks() *Locks {
	return &Locks{
		providers: make(map[string]*ProviderLock),
	}
}

// SetProvider는 Provider 잠금을 추가하거나 교체한다.
func (l *Locks) SetProvider(addr ProviderAddr, version, constraints string, hashes []Hash) *ProviderLock {
	if !ProviderIsLockable(addr) {
		panic(fmt.Sprintf("잠금 불가능한 Provider: %s", addr))
	}
	lock := NewProviderLock(addr, version, constraints, hashes)
	l.providers[addr.String()] = lock
	return lock
}

// Provider는 지정된 Provider의 잠금 정보를 반환한다.
func (l *Locks) Provider(addr ProviderAddr) *ProviderLock {
	return l.providers[addr.String()]
}

// RemoveProvider는 Provider 잠금을 제거한다.
func (l *Locks) RemoveProvider(addr ProviderAddr) {
	delete(l.providers, addr.String())
}

// Equal은 두 Locks가 동일한 정보를 나타내는지 비교한다.
// version과 hashes만 비교하고, versionConstraints는 무시한다.
// 실제 코드: internal/depsfile/locks.go Equal
func (l *Locks) Equal(other *Locks) bool {
	if len(l.providers) != len(other.providers) {
		return false
	}
	for key, thisLock := range l.providers {
		otherLock, ok := other.providers[key]
		if !ok {
			return false
		}
		if thisLock.version != otherLock.version {
			return false
		}
		if len(thisLock.hashes) != len(otherLock.hashes) {
			return false
		}
		for i := range thisLock.hashes {
			if thisLock.hashes[i] != otherLock.hashes[i] {
				return false
			}
		}
	}
	return true
}

// EqualProviderAddress는 두 Locks의 Provider 주소 집합이 동일한지 비교한다.
func (l *Locks) EqualProviderAddress(other *Locks) bool {
	if len(l.providers) != len(other.providers) {
		return false
	}
	for key := range l.providers {
		if _, ok := other.providers[key]; !ok {
			return false
		}
	}
	return true
}

// Empty는 잠금이 비어있는지 확인한다.
func (l *Locks) Empty() bool {
	return len(l.providers) == 0
}

// DeepCopy는 Locks의 깊은 복사본을 생성한다.
func (l *Locks) DeepCopy() *Locks {
	ret := NewLocks()
	for _, lock := range l.providers {
		hashes := make([]Hash, len(lock.hashes))
		copy(hashes, lock.hashes)
		ret.SetProvider(lock.addr, lock.version, lock.versionConstraints, hashes)
	}
	return ret
}

// SetProviderOverridden은 Provider를 오버라이드 상태로 표시한다.
// 메모리 전용이며, 파일에 저장되지 않는다.
// 실제 코드: internal/depsfile/locks.go SetProviderOverridden
func (l *Locks) SetProviderOverridden(addr ProviderAddr) {
	if l.overriddenProviders == nil {
		l.overriddenProviders = make(map[string]struct{})
	}
	l.overriddenProviders[addr.String()] = struct{}{}
}

// ProviderIsOverridden은 Provider가 오버라이드되었는지 확인한다.
func (l *Locks) ProviderIsOverridden(addr ProviderAddr) bool {
	_, ret := l.overriddenProviders[addr.String()]
	return ret
}

// ============================================================================
// 5. 잠금 파일 직렬화/역직렬화 (HCL 형식 시뮬레이션)
// ============================================================================

const LockFilePath = ".terraform.lock.hcl"

// SaveLocksToString은 Locks를 HCL 형식의 문자열로 직렬화한다.
// 실제 코드: internal/depsfile/locks_file.go SaveLocksToBytes
func SaveLocksToString(locks *Locks) string {
	var buf strings.Builder

	// 파일 헤더
	buf.WriteString("# This file is maintained automatically by \"terraform init\".\n")
	buf.WriteString("# Manual edits may be lost in future updates.\n")

	// Provider를 정렬하여 일관된 출력 보장
	var addrs []string
	for key := range locks.providers {
		addrs = append(addrs, key)
	}
	sort.Strings(addrs)

	for _, addr := range addrs {
		lock := locks.providers[addr]
		buf.WriteString("\n")
		buf.WriteString(fmt.Sprintf("provider %q {\n", lock.addr.String()))
		buf.WriteString(fmt.Sprintf("  version     = %q\n", lock.version))
		if lock.versionConstraints != "" {
			buf.WriteString(fmt.Sprintf("  constraints = %q\n", lock.versionConstraints))
		}
		if len(lock.hashes) > 0 {
			buf.WriteString("  hashes = [\n")
			for _, hash := range lock.hashes {
				buf.WriteString(fmt.Sprintf("    %q,\n", hash.String()))
			}
			buf.WriteString("  ]\n")
		}
		buf.WriteString("}\n")
	}

	return buf.String()
}

// ParseLocksFromString은 HCL 형식 문자열에서 Locks를 역직렬화한다.
// (간략화된 파서 - 실제는 HCL 라이브러리 사용)
func ParseLocksFromString(content string) (*Locks, error) {
	locks := NewLocks()
	lines := strings.Split(content, "\n")

	var currentAddr ProviderAddr
	var currentVersion string
	var currentConstraints string
	var currentHashes []Hash
	inProvider := false
	inHashes := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// 주석 또는 빈 줄 건너뛰기
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if strings.HasPrefix(trimmed, "provider ") {
			// provider "registry.terraform.io/hashicorp/aws" {
			parts := strings.SplitN(trimmed, "\"", 3)
			if len(parts) >= 2 {
				addrStr := parts[1]
				addrParts := strings.Split(addrStr, "/")
				if len(addrParts) == 3 {
					currentAddr = ProviderAddr{
						Hostname:  addrParts[0],
						Namespace: addrParts[1],
						Type:      addrParts[2],
					}
					currentVersion = ""
					currentConstraints = ""
					currentHashes = nil
					inProvider = true
				}
			}
		} else if inProvider && strings.HasPrefix(trimmed, "version") {
			parts := strings.SplitN(trimmed, "\"", 3)
			if len(parts) >= 2 {
				currentVersion = parts[1]
			}
		} else if inProvider && strings.HasPrefix(trimmed, "constraints") {
			parts := strings.SplitN(trimmed, "\"", 3)
			if len(parts) >= 2 {
				currentConstraints = parts[1]
			}
		} else if inProvider && strings.HasPrefix(trimmed, "hashes") {
			inHashes = true
		} else if inHashes && trimmed == "]" {
			inHashes = false
		} else if inHashes {
			// "h1:abc...",
			hashStr := strings.Trim(trimmed, "\",")
			if hashStr != "" {
				h, err := ParseHash(hashStr)
				if err == nil {
					currentHashes = append(currentHashes, h)
				}
			}
		} else if inProvider && trimmed == "}" {
			if currentVersion != "" {
				locks.SetProvider(currentAddr, currentVersion, currentConstraints, currentHashes)
			}
			inProvider = false
		}
	}

	return locks, nil
}

// ============================================================================
// 6. 해시 계산 (패키지 해시)
// ============================================================================

// ComputeHashV1은 디렉토리의 내용을 h1: 형식으로 해시한다.
// 실제 코드: internal/getproviders/hash.go PackageHashV1
// (실제는 dirhash.HashDir을 사용하지만, 여기서는 직접 구현)
func ComputeHashV1(dirPath string) (Hash, error) {
	// 1단계: 디렉토리의 모든 파일에 대해 SHA256 해시 계산
	var entries []string

	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(dirPath, path)
		if err != nil {
			return err
		}

		// 파일 내용의 SHA256 해시 계산
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}

		fileHash := hex.EncodeToString(h.Sum(nil))
		entries = append(entries, fmt.Sprintf("%s\t%s\n", relPath, fileHash))
		return nil
	})
	if err != nil {
		return NilHash, err
	}

	// 2단계: 정렬
	sort.Strings(entries)

	// 3단계: 전체 문자열의 SHA256 해시
	combined := strings.Join(entries, "")
	finalHash := sha256.Sum256([]byte(combined))

	return HashScheme1.New(hex.EncodeToString(finalHash[:])), nil
}

// ComputeHashZip은 파일의 SHA256 해시를 zh: 형식으로 계산한다.
// 실제 코드: internal/getproviders/hash.go PackageHashLegacyZipSHA
func ComputeHashZip(filePath string) (Hash, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return NilHash, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return NilHash, err
	}

	return HashSchemeZip.New(fmt.Sprintf("%x", h.Sum(nil))), nil
}

// MatchesAnyHash는 주어진 해시 중 하나라도 일치하면 true를 반환한다.
// 실제 코드: internal/getproviders/hash.go PackageMatchesAnyHash
func MatchesAnyHash(computedHash Hash, allowed []Hash) bool {
	for _, want := range allowed {
		if computedHash.Scheme() == want.Scheme() && computedHash == want {
			return true
		}
	}
	return false
}

// ============================================================================
// 7. 원자적 파일 쓰기
// ============================================================================

// AtomicWriteFile은 원자적으로 파일을 쓴다.
// 실제 코드: internal/replacefile/replacefile.go AtomicWriteFile
func AtomicWriteFile(filename string, content []byte, perm os.FileMode) error {
	// 1. 임시 파일에 쓰기
	dir := filepath.Dir(filename)
	tmpFile, err := os.CreateTemp(dir, ".terraform-lock-*.tmp")
	if err != nil {
		return fmt.Errorf("임시 파일 생성 실패: %w", err)
	}
	tmpName := tmpFile.Name()

	// 실패 시 임시 파일 정리
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmpFile.Write(content); err != nil {
		tmpFile.Close()
		return fmt.Errorf("임시 파일 쓰기 실패: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("임시 파일 닫기 실패: %w", err)
	}

	// 2. 권한 설정
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("권한 설정 실패: %w", err)
	}

	// 3. 원자적 이름 변경 (rename은 POSIX에서 원자적)
	if err := os.Rename(tmpName, filename); err != nil {
		return fmt.Errorf("파일 교체 실패: %w", err)
	}
	tmpName = "" // 성공 시 정리 방지

	return nil
}

// ============================================================================
// 메인: 시뮬레이션 실행
// ============================================================================

func main() {
	fmt.Println("=== Terraform 의존성 잠금 파일 (depsfile) 시뮬레이션 ===")
	fmt.Println()

	// --- 1단계: Provider 정의 ---
	fmt.Println("--- 1단계: Provider 잠금 생성 ---")

	awsProvider := ProviderAddr{
		Hostname:  "registry.terraform.io",
		Namespace: "hashicorp",
		Type:      "aws",
	}

	randomProvider := ProviderAddr{
		Hostname:  "registry.terraform.io",
		Namespace: "hashicorp",
		Type:      "random",
	}

	builtinProvider := ProviderAddr{
		Hostname:  "terraform.io",
		Namespace: "builtin",
		Type:      "terraform",
	}

	fmt.Printf("  AWS Provider: %s (잠금 가능: %v)\n", awsProvider, ProviderIsLockable(awsProvider))
	fmt.Printf("  Random Provider: %s (잠금 가능: %v)\n", randomProvider, ProviderIsLockable(randomProvider))
	fmt.Printf("  Built-in Provider: %s (잠금 가능: %v)\n", builtinProvider, ProviderIsLockable(builtinProvider))
	fmt.Println()

	// --- 2단계: 잠금 파일 생성 ---
	fmt.Println("--- 2단계: Locks 객체 생성 ---")

	locks := NewLocks()
	fmt.Printf("  초기 상태 - 비어있음: %v\n", locks.Empty())

	// 해시 생성 (중복 포함하여 정규화 테스트)
	awsHashes := []Hash{
		HashScheme1.New("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"),
		HashSchemeZip.New("fedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"),
		HashScheme1.New("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"), // 중복
		HashScheme1.New("1111111111111111111111111111111111111111111111111111111111111111"),
	}

	fmt.Printf("  입력 해시 수 (중복 포함): %d\n", len(awsHashes))
	awsLock := locks.SetProvider(awsProvider, "4.67.0", ">= 4.0.0, < 5.0.0", awsHashes)
	fmt.Printf("  정규화 후 해시 수 (중복 제거): %d\n", len(awsLock.hashes))

	randomHashes := []Hash{
		HashScheme1.New("random111111111111111111111111111111111111111111111111111111111111"),
	}
	locks.SetProvider(randomProvider, "3.5.1", "~> 3.0", randomHashes)

	fmt.Printf("  Provider 수: %d\n", len(locks.providers))
	fmt.Printf("  비어있음: %v\n", locks.Empty())
	fmt.Println()

	// --- 3단계: 잠금 파일 직렬화 ---
	fmt.Println("--- 3단계: 잠금 파일 직렬화 (HCL 형식) ---")

	lockContent := SaveLocksToString(locks)
	fmt.Println(lockContent)

	// --- 4단계: 잠금 파일 역직렬화 ---
	fmt.Println("--- 4단계: 잠금 파일 역직렬화 ---")

	parsedLocks, err := ParseLocksFromString(lockContent)
	if err != nil {
		fmt.Printf("  파싱 에러: %v\n", err)
		return
	}

	awsParsed := parsedLocks.Provider(awsProvider)
	if awsParsed != nil {
		fmt.Printf("  AWS 버전: %s\n", awsParsed.version)
		fmt.Printf("  AWS 제약 조건: %s\n", awsParsed.versionConstraints)
		fmt.Printf("  AWS 해시 수: %d\n", len(awsParsed.hashes))
		for _, h := range awsParsed.hashes {
			fmt.Printf("    - %s (스킴: %s)\n", h, h.Scheme())
		}
	}
	fmt.Println()

	// --- 5단계: Locks 비교 ---
	fmt.Println("--- 5단계: Locks 비교 ---")

	fmt.Printf("  원본 == 파싱: %v\n", locks.Equal(parsedLocks))
	fmt.Printf("  주소 동일: %v\n", locks.EqualProviderAddress(parsedLocks))

	// 버전 변경 테스트
	modifiedLocks := parsedLocks.DeepCopy()
	modifiedLocks.SetProvider(awsProvider, "4.68.0", ">= 4.0.0, < 5.0.0", awsHashes[:1])
	fmt.Printf("  원본 == 수정: %v (버전 변경)\n", locks.Equal(modifiedLocks))
	fmt.Printf("  주소 동일: %v\n", locks.EqualProviderAddress(modifiedLocks))

	// Provider 추가 테스트
	extraLocks := parsedLocks.DeepCopy()
	extraProvider := ProviderAddr{
		Hostname:  "registry.terraform.io",
		Namespace: "hashicorp",
		Type:      "google",
	}
	extraLocks.SetProvider(extraProvider, "5.0.0", "~> 5.0", []Hash{
		HashScheme1.New("google11111111111111111111111111111111111111111111111111111111111"),
	})
	fmt.Printf("  원본 == 추가: %v (Provider 추가)\n", locks.Equal(extraLocks))
	fmt.Printf("  주소 동일: %v\n", locks.EqualProviderAddress(extraLocks))
	fmt.Println()

	// --- 6단계: ContainsAll 검증 ---
	fmt.Println("--- 6단계: ContainsAll 해시 포함 검증 ---")

	fullLock := NewProviderLock(awsProvider, "4.67.0", "", []Hash{
		HashScheme1.New("aaaa"),
		HashScheme1.New("bbbb"),
		HashScheme1.New("cccc"),
	})

	subsetLock := NewProviderLock(awsProvider, "4.67.0", "", []Hash{
		HashScheme1.New("aaaa"),
		HashScheme1.New("cccc"),
	})

	disjointLock := NewProviderLock(awsProvider, "4.67.0", "", []Hash{
		HashScheme1.New("xxxx"),
	})

	fmt.Printf("  {a,b,c} ContainsAll {a,c}: %v (기대: true)\n", fullLock.ContainsAll(subsetLock))
	fmt.Printf("  {a,c} ContainsAll {a,b,c}: %v (기대: false)\n", subsetLock.ContainsAll(fullLock))
	fmt.Printf("  {a,b,c} ContainsAll {x}: %v (기대: false)\n", fullLock.ContainsAll(disjointLock))
	fmt.Printf("  {a,b,c} ContainsAll nil: %v (기대: true)\n", fullLock.ContainsAll(nil))
	fmt.Println()

	// --- 7단계: PreferredHashes 필터링 ---
	fmt.Println("--- 7단계: PreferredHashes 필터링 ---")

	mixedHashes := []Hash{
		HashScheme1.New("h1hash1"),
		HashSchemeZip.New("ziphash1"),
		Hash("h2:futurehash1"), // 미래의 알 수 없는 스킴
		HashScheme1.New("h1hash2"),
	}

	preferred := PreferredHashes(mixedHashes)
	fmt.Printf("  입력 해시 수: %d\n", len(mixedHashes))
	fmt.Printf("  선호 해시 수: %d\n", len(preferred))
	for _, h := range preferred {
		fmt.Printf("    - %s\n", h)
	}
	fmt.Println()

	// --- 8단계: Provider 오버라이드 ---
	fmt.Println("--- 8단계: Provider 오버라이드 ---")

	locks.SetProviderOverridden(awsProvider)
	fmt.Printf("  AWS 오버라이드: %v\n", locks.ProviderIsOverridden(awsProvider))
	fmt.Printf("  Random 오버라이드: %v\n", locks.ProviderIsOverridden(randomProvider))

	// 오버라이드 전파
	newLocks := NewLocks()
	newLocks.SetProvider(randomProvider, "3.5.1", "", randomHashes)
	fmt.Printf("  전파 전 - Random 오버라이드: %v\n", newLocks.ProviderIsOverridden(randomProvider))
	// SetSameOverriddenProviders 시뮬레이션
	for key := range locks.overriddenProviders {
		if newLocks.overriddenProviders == nil {
			newLocks.overriddenProviders = make(map[string]struct{})
		}
		newLocks.overriddenProviders[key] = struct{}{}
	}
	fmt.Printf("  전파 후 - AWS 오버라이드: %v\n", newLocks.ProviderIsOverridden(awsProvider))
	fmt.Println()

	// --- 9단계: 해시 매칭 ---
	fmt.Println("--- 9단계: 해시 매칭 ---")

	computedHash := HashScheme1.New("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	allowedHashes := []Hash{
		HashScheme1.New("00000000000000000000000000000000000000000000000000000000000000"),
		HashScheme1.New("abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"),
		HashSchemeZip.New("fedcba0987654321fedcba0987654321fedcba0987654321fedcba0987654321"),
	}

	fmt.Printf("  계산된 해시: %s\n", computedHash)
	fmt.Printf("  허용 목록에서 매칭: %v\n", MatchesAnyHash(computedHash, allowedHashes))

	wrongHash := HashScheme1.New("wrong_hash_value")
	fmt.Printf("  잘못된 해시 매칭: %v\n", MatchesAnyHash(wrongHash, allowedHashes))
	fmt.Println()

	// --- 10단계: 원자적 파일 쓰기 ---
	fmt.Println("--- 10단계: 원자적 파일 쓰기 ---")

	tmpDir, err := os.MkdirTemp("", "terraform-lock-poc-*")
	if err != nil {
		fmt.Printf("  임시 디렉토리 생성 실패: %v\n", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	lockFilePath := filepath.Join(tmpDir, LockFilePath)
	content := SaveLocksToString(locks)

	err = AtomicWriteFile(lockFilePath, []byte(content), 0644)
	if err != nil {
		fmt.Printf("  원자적 쓰기 실패: %v\n", err)
		return
	}

	// 파일 읽기 검증
	readContent, err := os.ReadFile(lockFilePath)
	if err != nil {
		fmt.Printf("  파일 읽기 실패: %v\n", err)
		return
	}

	fmt.Printf("  파일 경로: %s\n", lockFilePath)
	fmt.Printf("  파일 크기: %d 바이트\n", len(readContent))
	fmt.Printf("  내용 일치: %v\n", string(readContent) == content)

	// 파일 재파싱 검증
	reParsed, err := ParseLocksFromString(string(readContent))
	if err != nil {
		fmt.Printf("  재파싱 실패: %v\n", err)
		return
	}
	fmt.Printf("  원본 == 재파싱: %v\n", locks.Equal(reParsed))
	fmt.Println()

	// --- 11단계: lockfile=readonly 시뮬레이션 ---
	fmt.Println("--- 11단계: lockfile=readonly 시뮬레이션 ---")

	previousLocks := locks.DeepCopy()
	newConfigLocks := locks.DeepCopy()

	// 사례 1: 변경 없음
	if newConfigLocks.Equal(previousLocks) {
		fmt.Println("  사례 1: 변경 없음 → 잠금 파일 유지")
	}

	// 사례 2: Provider 추가 (readonly에서 에러)
	newConfigLocks.SetProvider(extraProvider, "5.0.0", "", []Hash{HashScheme1.New("xxx")})
	flagLockfile := "readonly"
	if !newConfigLocks.Equal(previousLocks) {
		if flagLockfile == "readonly" {
			if !newConfigLocks.EqualProviderAddress(previousLocks) {
				fmt.Println("  사례 2: [에러] Provider 의존성 변경 감지, readonly 모드에서 거부")
			} else {
				fmt.Println("  사례 2: [경고] 선택 변경 감지, readonly 모드에서 저장 안 함")
			}
		}
	}

	// 사례 3: 해시만 변경 (readonly에서 경고)
	hashOnlyChange := locks.DeepCopy()
	awsLockCopy := hashOnlyChange.Provider(awsProvider)
	if awsLockCopy != nil {
		newHashes := append(awsLockCopy.hashes, HashScheme1.New("newhash"))
		hashOnlyChange.SetProvider(awsProvider, awsLockCopy.version, awsLockCopy.versionConstraints, newHashes)
	}
	if !hashOnlyChange.Equal(previousLocks) {
		if flagLockfile == "readonly" {
			if !hashOnlyChange.EqualProviderAddress(previousLocks) {
				fmt.Println("  사례 3: [에러] Provider 의존성 변경 감지")
			} else {
				fmt.Println("  사례 3: [경고] 해시 변경 감지, readonly 모드에서 저장 안 함")
			}
		}
	}
	fmt.Println()

	fmt.Println("=== 시뮬레이션 완료 ===")
}
