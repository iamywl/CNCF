# PoC-15: Helm Getter 체인

## 개요

Helm의 Getter 시스템은 URL 스킴(http, https, oci, file 등)에 따라 적절한 다운로더를 선택하는 Provider 패턴을 사용한다. 플러그인으로 커스텀 스킴(s3, gs 등)을 추가할 수 있다.

## 참조 소스코드

| 파일 | 설명 |
|------|------|
| `pkg/getter/getter.go` | Getter 인터페이스, Provider, Providers, Option |
| `pkg/getter/httpgetter.go` | HTTPGetter (HTTP/HTTPS 다운로드) |
| `pkg/getter/ocigetter.go` | OCIGetter (OCI 레지스트리) |

## 핵심 개념

### 1. Getter 인터페이스
```
type Getter interface {
    Get(url string, options ...Option) (*bytes.Buffer, error)
}
```

### 2. Provider (스킴 -> Getter 매핑)
- http/https -> HTTPGetter
- oci -> OCIGetter
- file -> FileGetter
- s3/gs -> PluginGetter (플러그인 확장)

### 3. Functional Options 패턴
WithBasicAuth, WithTimeout, WithUserAgent, WithTLSClientConfig 등

### 4. 플러그인 확장
getter/v1 타입 플러그인이 protocols 필드로 스킴 등록

## 실행

```bash
go run main.go
```

## 시뮬레이션 내용

1. Getter/Provider 인터페이스 소개
2. HTTP Getter (httptest 서버로 실제 다운로드)
3. OCI Getter (레지스트리 시뮬레이션)
4. File Getter (로컬 파일)
5. 미지원 스킴 에러 처리
6. Functional Options 패턴
7. 플러그인으로 S3 Getter 확장
8. 아키텍처 다이어그램
