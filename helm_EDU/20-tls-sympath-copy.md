# 20. TLS Util, Sympath, CopyStructure — 내부 유틸리티 Deep Dive

## 목차

1. [개요](#1-개요)
2. [TLS Util 아키텍처](#2-tls-util-아키텍처)
3. [TLS 옵션 패턴](#3-tls-옵션-패턴)
4. [TLS 설정 생성 흐름](#4-tls-설정-생성-흐름)
5. [인증서 체인 검증](#5-인증서-체인-검증)
6. [Sympath Walk 시스템](#6-sympath-walk-시스템)
7. [심볼릭 링크 재귀 처리](#7-심볼릭-링크-재귀-처리)
8. [보안 고려사항](#8-보안-고려사항)
9. [CopyStructure 깊은 복사](#9-copystructure-깊은-복사)
10. [리플렉션 기반 타입 처리](#10-리플렉션-기반-타입-처리)
11. [Helm에서의 CopyStructure 활용](#11-helm에서의-copystructure-활용)
12. [통합 설계 분석](#12-통합-설계-분석)

---

## 1. 개요

이 문서에서 다루는 세 가지 유틸리티는 Helm의 **내부 인프라 계층**을 구성한다:

| 유틸리티 | 패키지 | 역할 |
|---------|--------|------|
| **TLS Util** | `internal/tlsutil/` | TLS 인증서 설정 생성 |
| **Sympath** | `internal/sympath/` | 심볼릭 링크를 따라가는 파일 순회 |
| **CopyStructure** | `internal/copystructure/` | Go 값의 깊은 복사 |

### 왜 이 유틸리티들이 필요한가

```
┌─────────────────────────────────────────────────────────┐
│                    Helm 내부 계층                         │
│                                                         │
│  helm repo add (HTTPS)  → TLS Util  → TLS 설정 생성     │
│  helm install (차트 로딩) → Sympath   → 심링크 해석       │
│  helm upgrade (값 처리)  → CopyStructure → 안전한 복사    │
└─────────────────────────────────────────────────────────┘
```

---

## 2. TLS Util 아키텍처

### 소스 코드 구조

```
internal/tlsutil/
├── tls.go       ← TLSConfigOptions, 옵션 함수, NewTLSConfig
└── tls_test.go  ← 테스트
```

### 핵심 타입 정의

`internal/tlsutil/tls.go` 라인 28-34:

```go
type TLSConfigOptions struct {
    insecureSkipTLSVerify     bool
    certPEMBlock, keyPEMBlock []byte
    caPEMBlock                []byte
}

type TLSConfigOption func(options *TLSConfigOptions) error
```

**왜 함수형 옵션 패턴인가?**

기존 Helm 3.x까지는 `NewClientTLS(certFile, keyFile, caFile string)` 같은 위치 기반 파라미터를 사용했다. Helm 4.x에서 **함수형 옵션 패턴**으로 전환한 이유:

1. **선택적 파라미터**: CA 파일 없이 클라이언트 인증서만 사용하는 경우 처리
2. **에러 수집**: 여러 파일 읽기 오류를 한 번에 수집하여 보고
3. **확장성**: 새 옵션(예: OCSP, Certificate Pinning) 추가 시 API 비호환 없음

---

## 3. TLS 옵션 패턴

### WithInsecureSkipVerify

```go
func WithInsecureSkipVerify(insecureSkipTLSVerify bool) TLSConfigOption {
    return func(options *TLSConfigOptions) error {
        options.insecureSkipTLSVerify = insecureSkipTLSVerify
        return nil
    }
}
```

### WithCertKeyPairFiles

```go
func WithCertKeyPairFiles(certFile, keyFile string) TLSConfigOption {
    return func(options *TLSConfigOptions) error {
        if certFile == "" && keyFile == "" {
            return nil  // 양쪽 모두 빈 값이면 무시
        }

        certPEMBlock, err := os.ReadFile(certFile)
        if err != nil {
            return fmt.Errorf("unable to read cert file: %q: %w", certFile, err)
        }

        keyPEMBlock, err := os.ReadFile(keyFile)
        if err != nil {
            return fmt.Errorf("unable to read key file: %q: %w", keyFile, err)
        }

        options.certPEMBlock = certPEMBlock
        options.keyPEMBlock = keyPEMBlock
        return nil
    }
}
```

### WithCAFile

```go
func WithCAFile(caFile string) TLSConfigOption {
    return func(options *TLSConfigOptions) error {
        if caFile == "" {
            return nil
        }

        caPEMBlock, err := os.ReadFile(caFile)
        if err != nil {
            return fmt.Errorf("can't read CA file: %q: %w", caFile, err)
        }

        options.caPEMBlock = caPEMBlock
        return nil
    }
}
```

**왜 빈 값 체크를 각 옵션에서 하는가?** 호출자가 선택적으로 인증서를 제공할 수 있도록 하기 위함이다. `WithCertKeyPairFiles("", "")`는 에러가 아니라 "클라이언트 인증서를 사용하지 않겠다"는 의미이다.

---

## 4. TLS 설정 생성 흐름

### NewTLSConfig

`internal/tlsutil/tls.go` 라인 84-122:

```go
func NewTLSConfig(options ...TLSConfigOption) (*tls.Config, error) {
    to := TLSConfigOptions{}

    // 1. 모든 옵션 적용 (에러 수집)
    errs := []error{}
    for _, option := range options {
        err := option(&to)
        if err != nil {
            errs = append(errs, err)
        }
    }
    if len(errs) > 0 {
        return nil, errors.Join(errs...)
    }

    // 2. 기본 TLS 설정
    config := tls.Config{
        InsecureSkipVerify: to.insecureSkipTLSVerify,
    }

    // 3. 클라이언트 인증서 설정 (선택)
    if len(to.certPEMBlock) > 0 && len(to.keyPEMBlock) > 0 {
        cert, err := tls.X509KeyPair(to.certPEMBlock, to.keyPEMBlock)
        if err != nil {
            return nil, fmt.Errorf("unable to load cert from key pair: %w", err)
        }
        config.Certificates = []tls.Certificate{cert}
    }

    // 4. CA 인증서 풀 설정 (선택)
    if len(to.caPEMBlock) > 0 {
        cp := x509.NewCertPool()
        if !cp.AppendCertsFromPEM(to.caPEMBlock) {
            return nil, errors.New("failed to append certificates from pem block")
        }
        config.RootCAs = cp
    }

    return &config, nil
}
```

### 실행 시퀀스

```
NewTLSConfig(
    WithInsecureSkipVerify(false),
    WithCertKeyPairFiles("client.crt", "client.key"),
    WithCAFile("ca.crt"),
)
    │
    ├─ 1. 빈 TLSConfigOptions 생성
    │
    ├─ 2. 각 옵션 순차 적용
    │   ├─ insecureSkipTLSVerify = false
    │   ├─ certPEMBlock = <client.crt 내용>
    │   ├─ keyPEMBlock = <client.key 내용>
    │   └─ caPEMBlock = <ca.crt 내용>
    │
    ├─ 3. 에러 체크 (모두 성공)
    │
    ├─ 4. tls.Config 생성
    │   ├─ InsecureSkipVerify: false
    │   ├─ Certificates: [X509KeyPair]
    │   └─ RootCAs: [ca.crt]
    │
    └─ 반환: *tls.Config
```

### 에러 수집 패턴

```go
errs := []error{}
for _, option := range options {
    err := option(&to)
    if err != nil {
        errs = append(errs, err)
    }
}
if len(errs) > 0 {
    return nil, errors.Join(errs...)
}
```

**왜 에러를 수집하는가?** 인증서 파일, 키 파일, CA 파일이 모두 잘못된 경우, 하나씩 에러를 보고하면 사용자가 3번 수정해야 한다. 에러를 수집하여 한 번에 보고하면 모든 문제를 한 번에 해결할 수 있다.

---

## 5. 인증서 체인 검증

### TLS 설정 시나리오

| 시나리오 | InsecureSkipVerify | Certificates | RootCAs | 용도 |
|----------|-------------------|-------------|---------|------|
| 기본 HTTPS | false | 없음 | 없음 | 시스템 CA 사용 |
| 사설 CA | false | 없음 | 설정 | 자체 CA 서명 인증서 |
| mTLS | false | 설정 | 설정 | 양방향 인증 |
| 개발/테스트 | true | 없음 | 없음 | 인증서 검증 건너뛰기 |

### X.509 인증서 체인

```
┌───────────────┐
│   Root CA     │ ← config.RootCAs
└───────┬───────┘
        │ 서명
┌───────▼───────┐
│ Intermediate  │
│     CA        │
└───────┬───────┘
        │ 서명
┌───────▼───────┐
│ Server Cert   │ ← 서버가 제공
└───────────────┘

┌───────────────┐
│ Client Cert   │ ← config.Certificates (mTLS)
│ + Private Key │
└───────────────┘
```

---

## 6. Sympath Walk 시스템

### 소스 코드 구조

```
internal/sympath/
├── walk.go       ← Walk, symwalk, IsSymlink 함수
└── walk_test.go  ← 테스트
```

### Walk 함수

`internal/sympath/walk.go` 라인 36-47:

```go
func Walk(root string, walkFn filepath.WalkFunc) error {
    info, err := os.Lstat(root)
    if err != nil {
        err = walkFn(root, nil, err)
    } else {
        err = symwalk(root, info, walkFn)
    }
    if err == filepath.SkipDir {
        return nil
    }
    return err
}
```

**왜 `os.Lstat`을 사용하는가?** `os.Stat`은 심볼릭 링크를 따라가지만, `os.Lstat`은 심볼릭 링크 자체의 정보를 반환한다. Walk에서는 먼저 링크 자체를 인식한 후, 필요하면 명시적으로 해석한다.

### 표준 filepath.Walk와의 차이

| 기능 | filepath.Walk | sympath.Walk |
|------|-------------|-------------|
| 심볼릭 링크 | **무시** (건너뜀) | **따라감** (재귀적) |
| 정렬 | 사전순 | 사전순 |
| 에러 처리 | walkFn에 위임 | walkFn에 위임 |
| 보안 로깅 | 없음 | 심링크 발견 시 로그 |

---

## 7. 심볼릭 링크 재귀 처리

### symwalk 함수

`internal/sympath/walk.go` 라인 66-114:

```go
func symwalk(path string, info os.FileInfo, walkFn filepath.WalkFunc) error {
    // 심볼릭 링크인 경우
    if IsSymlink(info) {
        resolved, err := filepath.EvalSymlinks(path)
        if err != nil {
            return fmt.Errorf("error evaluating symlink %s: %w", path, err)
        }

        // 보안 경고 로깅
        slog.Info("found symbolic link in path. Contents of linked file included and used",
            "path", path, "resolved", resolved)

        if info, err = os.Lstat(resolved); err != nil {
            return err
        }
        if err := symwalk(path, info, walkFn); err != nil && err != filepath.SkipDir {
            return err
        }
        return nil
    }

    // 일반 파일/디렉토리
    if err := walkFn(path, info, nil); err != nil {
        return err
    }

    if !info.IsDir() {
        return nil
    }

    // 디렉토리 내용 순회 (사전순 정렬)
    names, err := readDirNames(path)
    if err != nil {
        return walkFn(path, info, err)
    }

    for _, name := range names {
        filename := filepath.Join(path, name)
        fileInfo, err := os.Lstat(filename)
        if err != nil {
            if err := walkFn(filename, fileInfo, err); err != nil && err != filepath.SkipDir {
                return err
            }
        } else {
            err = symwalk(filename, fileInfo, walkFn)
            // ...
        }
    }
    return nil
}
```

### 처리 흐름

```
symwalk("/charts/mychart", dirInfo, walkFn)
    │
    ├─ IsSymlink(dirInfo)?
    │   ├─ Yes: EvalSymlinks() → 실제 경로 해석 → 재귀
    │   └─ No: walkFn() 호출
    │
    ├─ IsDir?
    │   ├─ Yes: readDirNames() → 정렬 → 각 엔트리에 대해 재귀
    │   └─ No: 완료
    │
    └─ 각 하위 엔트리:
        └─ os.Lstat() → symwalk() 재귀
```

### readDirNames

```go
func readDirNames(dirname string) ([]string, error) {
    f, err := os.Open(dirname)
    if err != nil {
        return nil, err
    }
    names, err := f.Readdirnames(-1)
    f.Close()
    if err != nil {
        return nil, err
    }
    sort.Strings(names)  // 결정적 출력을 위한 정렬
    return names, nil
}
```

### IsSymlink

```go
func IsSymlink(fi os.FileInfo) bool {
    return fi.Mode()&os.ModeSymlink != 0
}
```

---

## 8. 보안 고려사항

### 심볼릭 링크 보안 경고

소스 코드 라인 74:

```go
slog.Info("found symbolic link in path. Contents of linked file included and used",
    "path", path, "resolved", resolved)
```

**왜 이 로그가 필요한가?** 차트 패키지에 심볼릭 링크가 포함되면:

1. **경로 탈출(Path Traversal)**: `../../etc/passwd`를 가리키는 심링크로 민감한 파일 접근
2. **의도치 않은 포함**: 차트 외부의 파일이 배포에 포함될 수 있음
3. **감사 추적**: 어떤 심링크가 해석되었는지 기록

### TLS 보안 가이드

| 설정 | 프로덕션 | 개발 |
|------|---------|------|
| InsecureSkipVerify | false (필수) | true (편의) |
| 클라이언트 인증서 | 필요 시 사용 | 일반적으로 불필요 |
| CA 파일 | 사설 CA 시 필수 | 불필요 |

---

## 9. CopyStructure 깊은 복사

### 소스 코드 구조

```
internal/copystructure/
├── copystructure.go       ← Copy 함수, copyValue 재귀
└── copystructure_test.go  ← 테스트
```

### Copy 함수

`internal/copystructure/copystructure.go` 라인 26-31:

```go
func Copy(src any) (any, error) {
    if src == nil {
        return make(map[string]any), nil
    }
    return copyValue(reflect.ValueOf(src))
}
```

**왜 nil이면 빈 맵을 반환하는가?** Helm에서 Values가 nil인 경우, 빈 맵으로 초기화하여 이후 코드에서 nil 체크 없이 안전하게 접근할 수 있도록 한다.

---

## 10. 리플렉션 기반 타입 처리

### copyValue 함수

`internal/copystructure/copystructure.go` 라인 34-128:

```go
func copyValue(original reflect.Value) (any, error) {
    switch original.Kind() {
    // 기본 타입: 값 자체 반환 (이미 복사됨)
    case reflect.Bool, reflect.Int, ..., reflect.String, reflect.Array:
        return original.Interface(), nil

    // 인터페이스: nil 체크 후 내부 값 복사
    case reflect.Interface:
        if original.IsNil() {
            return original.Interface(), nil
        }
        return copyValue(original.Elem())

    // 맵: 새 맵 생성 후 각 키-값 재귀 복사
    case reflect.Map:
        if original.IsNil() {
            return original.Interface(), nil
        }
        copied := reflect.MakeMap(original.Type())
        iter := original.MapRange()
        for iter.Next() {
            child, err := copyValue(iter.Value())
            if err != nil {
                return nil, err
            }
            copied.SetMapIndex(iter.Key(), reflect.ValueOf(child))
        }
        return copied.Interface(), nil

    // 포인터: nil 체크, 내부 값 복사, 새 포인터 생성
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

    // 슬라이스: 새 슬라이스 생성 후 각 요소 재귀 복사
    case reflect.Slice:
        if original.IsNil() {
            return original.Interface(), nil
        }
        copied := reflect.MakeSlice(original.Type(), original.Len(), original.Cap())
        for i := 0; i < original.Len(); i++ {
            val, err := copyValue(original.Index(i))
            if err != nil {
                return nil, err
            }
            copied.Index(i).Set(reflect.ValueOf(val))
        }
        return copied.Interface(), nil

    // 구조체: 새 구조체, 각 필드 재귀 복사
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

    // 함수/채널: 참조만 복사 (깊은 복사 불가)
    case reflect.Func, reflect.Chan, reflect.UnsafePointer:
        return original.Interface(), nil

    default:
        return original.Interface(), fmt.Errorf("unsupported type %v", original)
    }
}
```

### 타입별 처리 전략

| 타입 | 전략 | 이유 |
|------|------|------|
| 기본형 (bool, int, string...) | 값 자체 반환 | Go에서 값 타입은 할당 시 자동 복사 |
| Interface | Elem() 추출 후 재귀 | 내부 구체 타입을 복사해야 함 |
| Map | MakeMap + 각 키-값 재귀 | 맵은 참조 타입이므로 새 맵 필요 |
| Pointer | New + Elem 재귀 | 포인터가 가리키는 값을 복사 |
| Slice | MakeSlice + 각 요소 재귀 | 슬라이스는 참조 타입 |
| Struct | New + 각 필드 재귀 | 구조체 내부의 참조 타입 필드 처리 |
| Func, Chan | 참조 공유 | 함수와 채널은 깊은 복사 불가 |

### nil 인터페이스 처리

```go
// 맵 값에서 nil 인터페이스 처리
if value.Kind() == reflect.Interface && value.IsNil() {
    copied.SetMapIndex(key, value)
    continue  // 재귀 복사 건너뛰기
}
```

**왜 nil 인터페이스를 특별 처리하는가?** `reflect.ValueOf(nil)`은 invalid Value를 반환하여 `copyValue`에서 패닉이 발생한다. nil 인터페이스는 그대로 복사하면 된다.

---

## 11. Helm에서의 CopyStructure 활용

### Values 처리에서의 사용

```
chart.yaml의 values:
  image:
    repository: nginx
    tag: "1.21"
  replicas: 3

사용자의 --set:
  replicas: 5
```

Values 병합 시 원본을 변경하지 않기 위해 깊은 복사가 필요하다:

```go
// 원본 보호를 위한 깊은 복사
copiedValues, err := copystructure.Copy(chart.Values)
if err != nil {
    return err
}
// 복사본에 사용자 값 병합
mergedValues := mergeMaps(copiedValues.(map[string]interface{}), userValues)
```

### 얕은 복사의 문제점

```go
// 얕은 복사 (문제 발생)
copiedMap := map[string]interface{}{}
for k, v := range originalMap {
    copiedMap[k] = v  // 중첩 맵/슬라이스는 참조만 복사됨!
}

copiedMap["image"].(map[string]interface{})["tag"] = "1.22"
// 원본의 image.tag도 "1.22"로 변경됨!

// 깊은 복사 (안전)
copiedMap, _ := copystructure.Copy(originalMap)
copiedMap.(map[string]interface{})["image"].(map[string]interface{})["tag"] = "1.22"
// 원본은 변경되지 않음
```

---

## 12. 통합 설계 분석

### Helm 워크플로우에서의 역할

```
helm install mychart --set replicas=3 --tls-cert=client.crt
    │
    ├─ TLS Util: HTTPS 연결 설정
    │   NewTLSConfig(
    │       WithCertKeyPairFiles("client.crt", "client.key"),
    │       WithCAFile("ca.crt"))
    │
    ├─ Sympath: 차트 디렉토리 순회
    │   Walk("./mychart/", func(path, info, err) {
    │       // 심링크 따라가며 모든 파일 수집
    │   })
    │
    └─ CopyStructure: Values 안전한 복사
        Copy(chart.Values) → 원본 보호
```

### 유틸리티 특성 비교

| 특성 | TLS Util | Sympath | CopyStructure |
|------|----------|---------|---------------|
| 패턴 | 함수형 옵션 | 재귀 순회 | 리플렉션 재귀 |
| 외부 의존 | crypto/tls, x509 | os, filepath | reflect |
| 에러 처리 | 수집 후 일괄 | walkFn 위임 | 재귀 전파 |
| 보안 | TLS 설정 | 심링크 로깅 | nil 안전성 |
| 성능 | 한 번 실행 | I/O 바운드 | CPU 바운드 |

### internal 패키지의 의미

세 유틸리티 모두 `internal/` 패키지에 위치한다. Go의 `internal` 패키지는 **외부 모듈에서 임포트할 수 없다**. 이는 의도적인 설계로:

1. **API 안정성 보장 불필요**: 내부 구현이므로 자유롭게 변경 가능
2. **Helm 전용 최적화**: 범용이 아닌 Helm 사용 패턴에 맞춤
3. **의존성 격리**: 외부 코드가 이 유틸리티에 의존하지 않도록 방지

---

## 부록: 주요 소스 파일 참조

| 파일 | 설명 |
|------|------|
| `internal/tlsutil/tls.go` | TLS 옵션 함수, NewTLSConfig (123줄) |
| `internal/sympath/walk.go` | Walk, symwalk, IsSymlink (120줄) |
| `internal/copystructure/copystructure.go` | Copy, copyValue 재귀 (129줄) |
