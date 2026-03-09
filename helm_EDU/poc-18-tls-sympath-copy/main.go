// Package main은 Helm의 TLS 유틸리티, Sympath(심볼릭 링크 Walk), CopyStructure의
// 핵심 개념을 시뮬레이션한다.
//
// 시뮬레이션하는 핵심 개념:
// 1. TLS 설정 (함수형 옵션 패턴, 에러 수집, X.509 인증서 체인)
// 2. Sympath Walk (심볼릭 링크를 따라가는 재귀 순회, Lstat vs Stat)
// 3. CopyStructure (리플렉션 기반 딥 카피, 타입별 분기 처리)
//
// 실행: go run main.go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// ─────────────────────────────────────────────
// 1. TLS 유틸리티 (함수형 옵션 패턴)
// ─────────────────────────────────────────────

// TLSConfigOptions는 TLS 설정 옵션을 담는다.
// 실제 소스: internal/tlsutil/tls.go TLSConfigOptions
type TLSConfigOptions struct {
	insecureSkipTLSVerify     bool
	certPEMBlock, keyPEMBlock []byte
	caPEMBlock                []byte
}

// TLSConfigOption은 함수형 옵션 패턴이다.
// 실제 소스: internal/tlsutil/tls.go TLSConfigOption
type TLSConfigOption func(options *TLSConfigOptions) error

// WithInsecureSkipVerify는 TLS 검증 건너뛰기 옵션이다.
func WithInsecureSkipVerify(insecure bool) TLSConfigOption {
	return func(options *TLSConfigOptions) error {
		options.insecureSkipTLSVerify = insecure
		return nil
	}
}

// WithCertKeyPair는 인증서/키 쌍을 설정한다.
// 실제 소스는 파일에서 읽지만, 시뮬레이션에서는 바이트를 직접 받는다.
func WithCertKeyPair(cert, key []byte) TLSConfigOption {
	return func(options *TLSConfigOptions) error {
		if len(cert) == 0 && len(key) == 0 {
			return nil
		}
		if len(cert) == 0 {
			return fmt.Errorf("cert is empty but key is provided")
		}
		if len(key) == 0 {
			return fmt.Errorf("key is empty but cert is provided")
		}
		options.certPEMBlock = cert
		options.keyPEMBlock = key
		return nil
	}
}

// WithCertKeyPairFiles는 파일 경로로 인증서/키를 설정한다.
// 실제 소스: internal/tlsutil/tls.go WithCertKeyPairFiles
func WithCertKeyPairFiles(certFile, keyFile string) TLSConfigOption {
	return func(options *TLSConfigOptions) error {
		if certFile == "" && keyFile == "" {
			return nil
		}
		certPEM, err := os.ReadFile(certFile)
		if err != nil {
			return fmt.Errorf("unable to read cert file: %q: %w", certFile, err)
		}
		keyPEM, err := os.ReadFile(keyFile)
		if err != nil {
			return fmt.Errorf("unable to read key file: %q: %w", keyFile, err)
		}
		options.certPEMBlock = certPEM
		options.keyPEMBlock = keyPEM
		return nil
	}
}

// WithCAFile는 CA 인증서 파일을 설정한다.
// 실제 소스: internal/tlsutil/tls.go WithCAFile
func WithCAFile(caFile string) TLSConfigOption {
	return func(options *TLSConfigOptions) error {
		if caFile == "" {
			return nil
		}
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return fmt.Errorf("can't read CA file: %q: %w", caFile, err)
		}
		options.caPEMBlock = caPEM
		return nil
	}
}

// SimulatedTLSConfig는 시뮬레이션된 TLS 설정 결과이다.
type SimulatedTLSConfig struct {
	InsecureSkipVerify bool
	HasCertKeyPair     bool
	HasCA              bool
	CertSize           int
	KeySize            int
	CASize             int
}

// NewTLSConfig는 함수형 옵션을 적용하여 TLS 설정을 생성한다.
// 실제 소스: internal/tlsutil/tls.go NewTLSConfig
// 핵심 패턴: 에러 수집 → errors.Join 대신 슬라이스로 모든 에러를 모은다.
func NewTLSConfig(options ...TLSConfigOption) (*SimulatedTLSConfig, error) {
	to := TLSConfigOptions{}

	// 에러 수집 패턴: 모든 옵션을 실행하고 에러를 모은다
	var errs []error
	for _, option := range options {
		err := option(&to)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		// errors.Join과 동일한 효과
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return nil, fmt.Errorf("TLS config errors: %s", strings.Join(msgs, "; "))
	}

	config := &SimulatedTLSConfig{
		InsecureSkipVerify: to.insecureSkipTLSVerify,
	}

	if len(to.certPEMBlock) > 0 && len(to.keyPEMBlock) > 0 {
		config.HasCertKeyPair = true
		config.CertSize = len(to.certPEMBlock)
		config.KeySize = len(to.keyPEMBlock)
	}

	if len(to.caPEMBlock) > 0 {
		config.HasCA = true
		config.CASize = len(to.caPEMBlock)
	}

	return config, nil
}

// ─────────────────────────────────────────────
// 2. Sympath Walk (심볼릭 링크 따라가기)
// ─────────────────────────────────────────────

// FileEntry는 Walk 결과를 담는다.
type FileEntry struct {
	Path       string
	IsDir      bool
	IsSymlink  bool
	ResolvedTo string // 심볼릭 링크인 경우 원본 경로
	Size       int64
}

// SimulatedFS는 인메모리 파일 시스템을 시뮬레이션한다.
type SimulatedFS struct {
	files    map[string]*simFile
	symlinks map[string]string // symlink path → target path
}

type simFile struct {
	isDir   bool
	content string
}

func NewSimulatedFS() *SimulatedFS {
	return &SimulatedFS{
		files:    make(map[string]*simFile),
		symlinks: make(map[string]string),
	}
}

func (fs *SimulatedFS) AddDir(path string) {
	fs.files[path] = &simFile{isDir: true}
}

func (fs *SimulatedFS) AddFile(path, content string) {
	fs.files[path] = &simFile{isDir: false, content: content}
}

func (fs *SimulatedFS) AddSymlink(path, target string) {
	fs.symlinks[path] = target
}

// IsSymlink은 경로가 심볼릭 링크인지 확인한다.
// 실제 소스: internal/sympath/walk.go IsSymlink
// 핵심: fi.Mode()&os.ModeSymlink != 0
func (fs *SimulatedFS) IsSymlink(path string) bool {
	_, ok := fs.symlinks[path]
	return ok
}

// EvalSymlinks는 심볼릭 링크를 해석한다.
// 실제 소스: filepath.EvalSymlinks 호출
func (fs *SimulatedFS) EvalSymlinks(path string) (string, error) {
	target, ok := fs.symlinks[path]
	if !ok {
		return path, nil
	}
	// 재귀 해석 (체인된 심볼릭 링크)
	return fs.EvalSymlinks(target)
}

// readDirNames는 디렉토리 내 항목을 정렬하여 반환한다.
// 실제 소스: internal/sympath/walk.go readDirNames
// 핵심: sort.Strings(names) — 결정적(deterministic) 출력을 보장
func (fs *SimulatedFS) readDirNames(dirname string) []string {
	var names []string
	prefix := dirname + "/"
	seen := make(map[string]bool)
	for path := range fs.files {
		if strings.HasPrefix(path, prefix) {
			rest := path[len(prefix):]
			parts := strings.SplitN(rest, "/", 2)
			if !seen[parts[0]] {
				seen[parts[0]] = true
				names = append(names, parts[0])
			}
		}
	}
	for path := range fs.symlinks {
		if strings.HasPrefix(path, prefix) {
			rest := path[len(prefix):]
			parts := strings.SplitN(rest, "/", 2)
			if !seen[parts[0]] {
				seen[parts[0]] = true
				names = append(names, parts[0])
			}
		}
	}
	sort.Strings(names)
	return names
}

// Walk는 심볼릭 링크를 따라가며 파일 트리를 순회한다.
// 실제 소스: internal/sympath/walk.go Walk
// 핵심 차이점: filepath.Walk는 심볼릭 링크를 따라가지 않지만,
// sympath.Walk는 Lstat → 심볼릭 링크 감지 → EvalSymlinks → 재귀 순회
func (fs *SimulatedFS) Walk(root string) ([]FileEntry, error) {
	var entries []FileEntry
	err := fs.symwalk(root, &entries, 0)
	return entries, err
}

func (fs *SimulatedFS) symwalk(path string, entries *[]FileEntry, depth int) error {
	if depth > 10 {
		return fmt.Errorf("symlink depth exceeded: %s", path)
	}

	// 심볼릭 링크인 경우: Lstat로 감지 → EvalSymlinks → 재귀
	if fs.IsSymlink(path) {
		resolved, err := fs.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("error evaluating symlink %s: %w", path, err)
		}
		fmt.Printf("  [sympath] 심볼릭 링크 발견: %s → %s\n", path, resolved)
		*entries = append(*entries, FileEntry{
			Path:       path,
			IsSymlink:  true,
			ResolvedTo: resolved,
		})
		// 해석된 경로로 재귀 순회
		return fs.symwalk(resolved, entries, depth+1)
	}

	f, ok := fs.files[path]
	if !ok {
		return fmt.Errorf("not found: %s", path)
	}

	entry := FileEntry{
		Path:  path,
		IsDir: f.isDir,
		Size:  int64(len(f.content)),
	}
	*entries = append(*entries, entry)

	if !f.isDir {
		return nil
	}

	// 디렉토리: 정렬된 항목 순회
	names := fs.readDirNames(path)
	for _, name := range names {
		filename := filepath.Join(path, name)
		if err := fs.symwalk(filename, entries, depth); err != nil {
			return err
		}
	}
	return nil
}

// ─────────────────────────────────────────────
// 3. CopyStructure (리플렉션 기반 딥 카피)
// ─────────────────────────────────────────────

// Copy는 주어진 값의 딥 카피를 수행한다.
// 실제 소스: internal/copystructure/copystructure.go Copy
// 핵심: nil 입력 → 빈 map[string]any 반환 (Helm 특화)
func Copy(src any) (any, error) {
	if src == nil {
		return make(map[string]any), nil
	}
	return copyValue(reflect.ValueOf(src))
}

// copyValue는 리플렉션으로 값을 재귀적으로 복사한다.
// 실제 소스: internal/copystructure/copystructure.go copyValue
// 핵심 패턴: Kind() 기반 타입 스위치 → 각 타입별 딥 카피 전략
func copyValue(original reflect.Value) (any, error) {
	switch original.Kind() {
	// 기본 타입: 값 타입이므로 그대로 반환
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32,
		reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64, reflect.Float32, reflect.Float64, reflect.String:
		return original.Interface(), nil

	// Interface: nil 검사 후 Elem()으로 실제 값 추출
	case reflect.Interface:
		if original.IsNil() {
			return original.Interface(), nil
		}
		return copyValue(original.Elem())

	// Map: 새 맵 생성 → 각 키-값 재귀 복사
	// 핵심: nil 인터페이스 값의 특별 처리 (value.Kind() == reflect.Interface && value.IsNil())
	case reflect.Map:
		if original.IsNil() {
			return original.Interface(), nil
		}
		copied := reflect.MakeMap(original.Type())
		iter := original.MapRange()
		for iter.Next() {
			key := iter.Key()
			value := iter.Value()
			// nil 인터페이스 값 특별 처리
			if value.Kind() == reflect.Interface && value.IsNil() {
				copied.SetMapIndex(key, value)
				continue
			}
			child, err := copyValue(value)
			if err != nil {
				return nil, err
			}
			copied.SetMapIndex(key, reflect.ValueOf(child))
		}
		return copied.Interface(), nil

	// Pointer: Elem()으로 가리키는 값 복사 → 새 포인터에 Set
	case reflect.Pointer:
		if original.IsNil() {
			return original.Interface(), nil
		}
		copied, err := copyValue(original.Elem())
		if err != nil {
			return nil, err
		}
		ptr := reflect.New(original.Type().Elem())
		ptr.Elem().Set(reflect.ValueOf(copied))
		return ptr.Interface(), nil

	// Slice: MakeSlice → 각 요소 재귀 복사
	case reflect.Slice:
		if original.IsNil() {
			return original.Interface(), nil
		}
		copied := reflect.MakeSlice(original.Type(), original.Len(), original.Cap())
		for i := 0; i < original.Len(); i++ {
			elem := original.Index(i)
			if elem.Kind() == reflect.Interface && elem.IsNil() {
				copied.Index(i).Set(elem)
				continue
			}
			val, err := copyValue(elem)
			if err != nil {
				return nil, err
			}
			copied.Index(i).Set(reflect.ValueOf(val))
		}
		return copied.Interface(), nil

	// Struct: New → 각 필드 재귀 복사
	case reflect.Struct:
		copied := reflect.New(original.Type()).Elem()
		for i := 0; i < original.NumField(); i++ {
			elem, err := copyValue(original.Field(i))
			if err != nil {
				return nil, err
			}
			copied.Field(i).Set(reflect.ValueOf(elem))
		}
		return copied.Interface(), nil

	default:
		return original.Interface(), fmt.Errorf("unsupported type %v", original.Kind())
	}
}

// ─────────────────────────────────────────────
// 메인 함수
// ─────────────────────────────────────────────

func main() {
	fmt.Println("╔══════════════════════════════════════════════════╗")
	fmt.Println("║  Helm TLS, Sympath, CopyStructure 시뮬레이션     ║")
	fmt.Println("╚══════════════════════════════════════════════════╝")
	fmt.Println()

	// === 1. TLS 유틸리티 데모 ===
	demoTLS()

	// === 2. Sympath Walk 데모 ===
	demoSympath()

	// === 3. CopyStructure 데모 ===
	demoCopyStructure()

	fmt.Println("시뮬레이션 완료.")
}

func demoTLS() {
	fmt.Println("━━━ 1. TLS 유틸리티 (함수형 옵션 패턴) ━━━")

	// 테스트 1: 기본 설정 (옵션 없음)
	fmt.Println("  [테스트 1] 기본 설정:")
	config, err := NewTLSConfig()
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    InsecureSkipVerify: %v\n", config.InsecureSkipVerify)
		fmt.Printf("    HasCertKeyPair:     %v\n", config.HasCertKeyPair)
		fmt.Printf("    HasCA:              %v\n", config.HasCA)
	}

	// 테스트 2: InsecureSkipVerify + 인증서
	fmt.Println("  [테스트 2] InsecureSkipVerify + 인증서:")
	config, err = NewTLSConfig(
		WithInsecureSkipVerify(true),
		WithCertKeyPair(
			[]byte("-----BEGIN CERTIFICATE-----\nMIIC...simulated...cert\n-----END CERTIFICATE-----"),
			[]byte("-----BEGIN RSA PRIVATE KEY-----\nMIIE...simulated...key\n-----END RSA PRIVATE KEY-----"),
		),
	)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    InsecureSkipVerify: %v\n", config.InsecureSkipVerify)
		fmt.Printf("    HasCertKeyPair:     %v (cert=%d, key=%d bytes)\n",
			config.HasCertKeyPair, config.CertSize, config.KeySize)
	}

	// 테스트 3: 에러 수집 패턴 — 여러 옵션이 실패해도 모두 수집
	fmt.Println("  [테스트 3] 에러 수집 패턴 (다중 에러):")
	config, err = NewTLSConfig(
		WithCertKeyPairFiles("/nonexistent/cert.pem", "/nonexistent/key.pem"),
		WithCAFile("/nonexistent/ca.pem"),
	)
	if err != nil {
		fmt.Printf("    에러 수집 결과: %v\n", err)
	}

	// 테스트 4: 빈 경로는 무시 (no-op)
	fmt.Println("  [테스트 4] 빈 경로 (no-op):")
	config, err = NewTLSConfig(
		WithCertKeyPairFiles("", ""),
		WithCAFile(""),
	)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    HasCertKeyPair: %v, HasCA: %v (모두 건너뜀)\n",
			config.HasCertKeyPair, config.HasCA)
	}

	// 테스트 5: cert만 있고 key가 없는 경우
	fmt.Println("  [테스트 5] cert만 있고 key 없음:")
	_, err = NewTLSConfig(
		WithCertKeyPair([]byte("some-cert"), nil),
	)
	if err != nil {
		fmt.Printf("    에러: %v\n", err)
	}

	fmt.Println()
}

func demoSympath() {
	fmt.Println("━━━ 2. Sympath Walk (심볼릭 링크 순회) ━━━")

	// 인메모리 파일 시스템 구성
	fs := NewSimulatedFS()

	// 디렉토리 구조:
	// /chart/
	// ├── Chart.yaml
	// ├── values.yaml
	// ├── templates/
	// │   ├── deployment.yaml
	// │   └── service.yaml
	// ├── common → /shared/common  (심볼릭 링크)
	// └── config → /chart/values.yaml (파일 심볼릭 링크)
	fs.AddDir("/chart")
	fs.AddFile("/chart/Chart.yaml", "apiVersion: v2\nname: myapp")
	fs.AddFile("/chart/values.yaml", "replicaCount: 3")
	fs.AddDir("/chart/templates")
	fs.AddFile("/chart/templates/deployment.yaml", "kind: Deployment")
	fs.AddFile("/chart/templates/service.yaml", "kind: Service")
	fs.AddDir("/shared")
	fs.AddDir("/shared/common")
	fs.AddFile("/shared/common/helpers.tpl", "{{/* helper */}}")
	fs.AddSymlink("/chart/common", "/shared/common")
	fs.AddSymlink("/chart/config", "/chart/values.yaml")

	fmt.Println("  [파일 시스템 구조]")
	fmt.Println("  /chart/")
	fmt.Println("  ├── Chart.yaml")
	fmt.Println("  ├── values.yaml")
	fmt.Println("  ├── templates/")
	fmt.Println("  │   ├── deployment.yaml")
	fmt.Println("  │   └── service.yaml")
	fmt.Println("  ├── common → /shared/common  (심볼릭 링크)")
	fmt.Println("  └── config → /chart/values.yaml (파일 심볼릭 링크)")
	fmt.Println()

	fmt.Println("  [Walk 결과]")
	entries, err := fs.Walk("/chart")
	if err != nil {
		fmt.Printf("  오류: %v\n", err)
		return
	}

	for _, e := range entries {
		marker := "  "
		if e.IsSymlink {
			marker = "→ "
		} else if e.IsDir {
			marker = "📁"
		} else {
			marker = "📄"
		}
		if e.IsSymlink {
			fmt.Printf("    %s %s → %s\n", marker, e.Path, e.ResolvedTo)
		} else {
			fmt.Printf("    %s %s (size=%d)\n", marker, e.Path, e.Size)
		}
	}

	// Lstat vs Stat 비교
	fmt.Println()
	fmt.Println("  [Lstat vs Stat 비교]")
	fmt.Println("  ┌────────────────────────────────────────────────────────┐")
	fmt.Println("  │ Lstat: 심볼릭 링크 자체의 정보 반환 (ModeSymlink 비트)   │")
	fmt.Println("  │ Stat:  심볼릭 링크를 따라간 대상의 정보 반환              │")
	fmt.Println("  │ sympath.Walk: Lstat → IsSymlink → EvalSymlinks → 재귀  │")
	fmt.Println("  └────────────────────────────────────────────────────────┘")

	// IsSymlink 판별
	fmt.Println()
	fmt.Println("  [IsSymlink 판별]")
	testPaths := []string{"/chart/Chart.yaml", "/chart/common", "/chart/config", "/chart/templates"}
	for _, p := range testPaths {
		isSym := fs.IsSymlink(p)
		fmt.Printf("    %-30s → IsSymlink: %v\n", p, isSym)
	}

	// 순환 심볼릭 링크 감지
	fmt.Println()
	fmt.Println("  [순환 심볼릭 링크 감지]")
	fs2 := NewSimulatedFS()
	fs2.AddDir("/a")
	fs2.AddSymlink("/a/b", "/a/c")
	fs2.AddSymlink("/a/c", "/a/b")
	_, err = fs2.Walk("/a")
	if err != nil {
		fmt.Printf("    순환 감지 오류: %v\n", err)
	}
	fmt.Println()
}

func demoCopyStructure() {
	fmt.Println("━━━ 3. CopyStructure (리플렉션 기반 딥 카피) ━━━")

	// 테스트 1: nil 입력 → 빈 map 반환 (Helm 특화 동작)
	fmt.Println("  [테스트 1] nil 입력:")
	result, err := Copy(nil)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
	} else {
		fmt.Printf("    결과 타입: %T, 값: %v\n", result, result)
	}

	// 테스트 2: 기본 타입 (값 타입)
	fmt.Println("  [테스트 2] 기본 타입:")
	intVal := 42
	intCopy, _ := Copy(intVal)
	fmt.Printf("    int: 원본=%d, 복사=%d, 동일=%v\n", intVal, intCopy, intVal == intCopy.(int))

	strVal := "hello"
	strCopy, _ := Copy(strVal)
	fmt.Printf("    string: 원본=%q, 복사=%q, 동일=%v\n", strVal, strCopy, strVal == strCopy.(string))

	// 테스트 3: Map 딥 카피 (Helm values.yaml의 핵심)
	fmt.Println("  [테스트 3] Map 딥 카피:")
	original := map[string]any{
		"replicaCount": 3,
		"image": map[string]any{
			"repository": "nginx",
			"tag":        "latest",
		},
		"labels": map[string]any{
			"app":     "myapp",
			"version": "v1",
		},
		"nilValue": nil,
	}

	copied, err := Copy(original)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
		return
	}
	copiedMap := copied.(map[string]any)

	// 독립성 검증: 복사본 수정이 원본에 영향 없음
	copiedImage := copiedMap["image"].(map[string]any)
	copiedImage["tag"] = "v2.0"

	fmt.Printf("    원본 image.tag: %v\n", original["image"].(map[string]any)["tag"])
	fmt.Printf("    복사본 image.tag: %v\n", copiedImage["tag"])
	fmt.Printf("    독립성 확인: %v (서로 다르면 성공)\n",
		original["image"].(map[string]any)["tag"] != copiedImage["tag"])

	// nil 값 처리
	fmt.Printf("    nil 값 처리: 원본=%v, 복사=%v\n", original["nilValue"], copiedMap["nilValue"])

	// 테스트 4: Slice 딥 카피
	fmt.Println("  [테스트 4] Slice 딥 카피:")
	origSlice := []any{"a", "b", map[string]any{"key": "value"}}
	sliceCopy, err := Copy(origSlice)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
		return
	}
	copiedSlice := sliceCopy.([]any)
	// 수정 독립성 검증
	copiedSlice[0] = "modified"
	innerMap := copiedSlice[2].(map[string]any)
	innerMap["key"] = "modified"
	fmt.Printf("    원본[0]: %v, 복사[0]: %v\n", origSlice[0], copiedSlice[0])
	fmt.Printf("    원본[2].key: %v, 복사[2].key: %v\n",
		origSlice[2].(map[string]any)["key"], innerMap["key"])

	// 테스트 5: Struct 딥 카피
	fmt.Println("  [테스트 5] Struct 딥 카피:")
	type ChartMeta struct {
		Name    string
		Version string
		Tags    []string
	}
	origStruct := ChartMeta{
		Name:    "myapp",
		Version: "1.0.0",
		Tags:    []string{"stable", "production"},
	}
	structCopy, err := Copy(origStruct)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
		return
	}
	copiedStruct := structCopy.(ChartMeta)
	copiedStruct.Tags[0] = "modified"
	fmt.Printf("    원본 Tags[0]: %v, 복사 Tags[0]: %v\n",
		origStruct.Tags[0], copiedStruct.Tags[0])
	fmt.Printf("    독립성 확인: %v\n", origStruct.Tags[0] != copiedStruct.Tags[0])

	// 테스트 6: Pointer 딥 카피
	fmt.Println("  [테스트 6] Pointer 딥 카피:")
	origInt := 100
	ptrOrig := &origInt
	ptrCopy, err := Copy(ptrOrig)
	if err != nil {
		fmt.Printf("    오류: %v\n", err)
		return
	}
	copiedPtr := ptrCopy.(*int)
	*copiedPtr = 200
	fmt.Printf("    원본: %d, 복사: %d (독립적 수정)\n", *ptrOrig, *copiedPtr)

	// 타입별 처리 요약
	fmt.Println()
	fmt.Println("  [타입별 딥 카피 전략]")
	fmt.Println("  ┌───────────────┬──────────────────────────────────────────────┐")
	fmt.Println("  │ Kind          │ 전략                                         │")
	fmt.Println("  ├───────────────┼──────────────────────────────────────────────┤")
	fmt.Println("  │ Bool,Int,     │ Interface()로 값 반환 (값 타입이므로 복사 불필요) │")
	fmt.Println("  │ String 등     │                                              │")
	fmt.Println("  ├───────────────┼──────────────────────────────────────────────┤")
	fmt.Println("  │ Interface     │ IsNil → 반환, Elem()으로 실제 값 추출 후 재귀   │")
	fmt.Println("  ├───────────────┼──────────────────────────────────────────────┤")
	fmt.Println("  │ Map           │ MakeMap → MapRange → 키-값 재귀 복사           │")
	fmt.Println("  ├───────────────┼──────────────────────────────────────────────┤")
	fmt.Println("  │ Pointer       │ Elem 복사 → New → Set                        │")
	fmt.Println("  ├───────────────┼──────────────────────────────────────────────┤")
	fmt.Println("  │ Slice         │ MakeSlice → 각 요소 재귀 복사                  │")
	fmt.Println("  ├───────────────┼──────────────────────────────────────────────┤")
	fmt.Println("  │ Struct        │ New → 각 필드 재귀 복사                        │")
	fmt.Println("  └───────────────┴──────────────────────────────────────────────┘")
	fmt.Println()
}
